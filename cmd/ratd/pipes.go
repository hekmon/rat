package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"os"
	"sync"
	"syscall"
	"time"

	"github.com/hekmon/rat/connect"
	"github.com/hekmon/rat/internal/fifo"
)

// A transfer may stream through a named pipe the agent made, rather than copy a file: a command in
// a terminal writes into the pipe a download reads (tar -cf - . > ~/data.pipe), or reads from the
// pipe an upload writes into, with no file in between (see Named pipes in the README). ratd only
// opens its end of the pipe (package fifo), never creating nor removing one, nor running anything:
// the commands are the agent's, in its terminals. A client handles pipes only when it says so
// (connect.FeaturePipes): to an older rat, a pipe stays what is not a regular file, refused.

var (
	// errClosed is the cause of a transfer through a pipe stopped by its client, asking to close
	// the pipe (DELETE).
	errClosed = errors.New("pipe closed by the client")
	// errPipeBusy is the error of a transfer through a pipe another transfer goes through.
	errPipeBusy = errors.New("pipe in another transfer")
	// errReplaced is the error of a transfer whose pipe was replaced while it waited for its command.
	errReplaced = errors.New("pipe replaced")
)

// handlesPipes tells whether the client of req handles the transfers through named pipes.
func handlesPipes(req *http.Request) bool {
	return connect.HasFeature(req.Header.Get(connect.HeaderFeatures), connect.FeaturePipes)
}

// pipeTransfer is a transfer through a named pipe, in flight, or held once cut.
type pipeTransfer struct {
	path, session string
	way           direction
	// cancel stops the transfer in flight, with the cause of its stop
	cancel context.CancelCauseFunc
	// done is closed once the transfer in flight returned, its pipe closed or held
	done chan struct{}
	// pipe is the end of the pipe ratd holds, once held, since heldSince
	pipe      *fifo.Pipe
	heldSince time.Time
}

// pipeTransfers are the transfers through named pipes, in flight or held, by the path of their
// pipe: one at a time goes through a pipe, as two would split what its command writes, or mix what
// it reads. A pipe whose transfer was cut (its client gone, ratd stopping) or whose source failed
// is held open, its command waiting rather than taking the end of the pipe for the end of its
// data, until the client closes it (see closePipe) or ratd exits, after stopping the terminals
// (see run). Nothing watches a held pipe for its command leaving: the agent closes what it opened.
type pipeTransfers struct {
	status *status
	mu     sync.Mutex
	byPath map[string]*pipeTransfer
}

func newPipeTransfers(s *status) *pipeTransfers {
	return &pipeTransfers{status: s, byPath: map[string]*pipeTransfer{}}
}

// start records a transfer of session going way through the pipe at path, refused when another
// goes through it, and returns it with its context: done with ctx (its request), once ratd stops
// (stopping), or once the client closes the pipe, its cause telling which.
func (p *pipeTransfers) start(ctx context.Context, stopping <-chan struct{}, path, session string,
	way direction) (*pipeTransfer, context.Context, *result) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.byPath[path] != nil {
		return nil, nil, &result{outcome: failed, err: errPipeBusy,
			text: fmt.Sprintf("%s is already in a transfer: cancel that one first.", path)}
	}
	ctx, cancel := context.WithCancelCause(ctx)
	if stopping != nil {
		go func() {
			select {
			case <-stopping:
				cancel(errStopping)
			case <-ctx.Done():
			}
		}()
	}
	t := &pipeTransfer{path: path, session: session, way: way, cancel: cancel, done: make(chan struct{})}
	p.byPath[path] = t
	return t, ctx, nil
}

// finish records the end of t in flight: pipe held when hold, the pipe gone otherwise, closed by
// the caller, or never opened.
func (p *pipeTransfers) finish(t *pipeTransfer, pipe *fifo.Pipe, hold bool) {
	p.mu.Lock()
	if hold && pipe != nil {
		t.pipe, t.heldSince = pipe, time.Now()
		p.status.pipeHeld()
	} else {
		delete(p.byPath, t.path)
	}
	p.mu.Unlock()
	t.cancel(nil)
	close(t.done)
}

// close closes the pipe at path of a transfer of session going way, held or in flight, and returns
// how long it was held, zero for a transfer in flight, and false when there is none. A transfer in
// flight is stopped, closing its pipe itself, unless a cut held it first: close waits for it, a
// tick of package fifo at most once its client is gone, as rat cuts its request before closing.
func (p *pipeTransfers) close(path, session string, way direction) (held time.Duration, found bool) {
	p.mu.Lock()
	t := p.byPath[path]
	if t == nil || t.session != session || t.way != way {
		p.mu.Unlock()
		return 0, false
	}
	if t.pipe == nil {
		p.mu.Unlock()
		t.cancel(errClosed)
		<-t.done
		p.mu.Lock()
		if p.byPath[path] != t {
			p.mu.Unlock()
			return 0, true
		}
	}
	delete(p.byPath, path)
	p.status.pipeReleased()
	p.mu.Unlock()
	t.pipe.Close()
	return time.Since(t.heldSince), true
}

// closeAll closes every pipe held, once ratd stopped its terminals (see run): a command reading
// one in a terminal stopped with it, rather than seeing the end of its input first.
func (p *pipeTransfers) closeAll() {
	p.mu.Lock()
	defer p.mu.Unlock()
	for path, t := range p.byPath {
		if t.pipe != nil {
			t.pipe.Close()
			delete(p.byPath, path)
			p.status.pipeReleased()
		}
	}
}

// pipeOpener opens the named pipe at path, never following a symbolic link at its end, as a
// download opens its file.
func pipeOpener(path string) fifo.Opener {
	return func(flag int) (*os.File, error) { return os.OpenFile(path, flag|syscall.O_NOFOLLOW, 0) }
}

// openPipe tells the client that ratd waits for the command at the pipe at path (102), then opens
// the pipe once the command opened the other end, checking it is the pipe looked at (info). It
// returns the refusal of a failure otherwise, the wait given up included, with its status.
func (d *daemon) openPipe(ctx context.Context, w http.ResponseWriter, path string, info fs.FileInfo,
	way fifo.Way) (*fifo.Pipe, *result, int) {
	w.WriteHeader(http.StatusProcessing)
	pipe, err := fifo.Open(ctx, pipeOpener(path), way)
	if ctx.Err() != nil {
		if pipe != nil {
			pipe.Close()
		}
		return nil, &result{outcome: failed, err: context.Cause(ctx), text: fmt.Sprintf(
			"The transfer of %s was given up before a command opened the pipe.", path)}, http.StatusConflict
	}
	if err != nil {
		what, how := "Reading "+path, read
		if way == fifo.Write {
			what, how = "Writing "+path, written
		}
		r := d.fileFailure(err, what, how, false)
		return nil, &r, transferStatus(err)
	}
	if opened, err := pipe.Stat(); err != nil || !os.SameFile(opened, info) {
		pipe.Close()
		return nil, &result{outcome: failed, err: errReplaced, text: fmt.Sprintf(
			"%s was replaced while the transfer waited for its command: transfer it again.", path)}, http.StatusConflict
	}
	return pipe, nil, 0
}

// cutCause returns why a transfer through a pipe was cut: the cause of its context once done (its
// client gone, ratd stopping), err otherwise (its connection failing).
func cutCause(ctx context.Context, err error) error {
	if cause := context.Cause(ctx); cause != nil {
		return cause
	}
	return err
}

// downloadPipe sends to a client handling pipes what a command writes into the named pipe at path,
// described by info (from Lstat): first 102, telling that ratd waits for the command, then 200,
// without Rat-Size, once the command opened the pipe, and its bytes as they come, until the
// command closes its end, which ends the download. A cut (the client gone, ratd stopping) and a
// read failing, told in the Rat-Error trailer, hold the pipe (see pipeTransfers); the client
// closing it closes it, its command getting a broken pipe. HEAD answers from info, never opening
// the pipe, which would release a command waiting to write into it.
func (d *daemon) downloadPipe(w http.ResponseWriter, req *http.Request, session, path string, info fs.FileInfo,
	attrs []any) result {
	header := w.Header()
	if req.Method == http.MethodHead {
		header.Set("Content-Type", connect.OctetStream)
		header.Set(connect.HeaderMode, connect.FormatMode(info.Mode()))
		w.WriteHeader(http.StatusOK)
		return result{attrs: append(attrs, "pipe", "unopened", "status", http.StatusOK, "bytes", 0)}
	}
	t, ctx, refused := d.pipes.start(req.Context(), d.stopping, path, session, download)
	if refused != nil {
		return refuse(w, http.StatusConflict, withAttrs(*refused, attrs))
	}
	pipe, refused, status := d.openPipe(ctx, w, path, info, fifo.Read)
	if refused != nil {
		d.pipes.finish(t, nil, false)
		return refuse(w, status, withAttrs(*refused, append(attrs, "pipe", "unopened")))
	}
	header.Set("Content-Type", connect.OctetStream)
	header.Set(connect.HeaderMode, connect.FormatMode(info.Mode()))
	header.Set("Trailer", connect.HeaderError)
	w.WriteHeader(http.StatusOK)
	controller := http.NewResponseController(w)
	// the client learns that the command opened the pipe
	_ = controller.Flush()
	source := &readErrors{r: pipe.Reader(ctx)}
	n, err := io.Copy(flushed{w: w, controller: controller}, source)
	attrs = append(attrs, "status", http.StatusOK, "bytes", n)
	switch {
	case err == nil:
		// the command closed its end: the download is complete
		pipe.Close()
		d.pipes.finish(t, nil, false)
		return result{attrs: append(attrs, "pipe", "closed")}
	case errors.Is(context.Cause(ctx), errClosed):
		pipe.Close()
		d.pipes.finish(t, nil, false)
		return result{outcome: failed, err: errClosed, attrs: append(attrs, "pipe", "closed")}
	case source.err != nil && ctx.Err() == nil:
		// reading the pipe failed: told to the client, the pipe held as for a cut
		failure := d.fileFailure(source.err, "Reading "+path, read, false)
		header.Set(connect.HeaderError, failure.text)
		d.pipes.finish(t, pipe, true)
		failure.outcome, failure.attrs = failed, append(attrs, "pipe", "held")
		return failure
	default:
		d.pipes.finish(t, pipe, true)
		return result{outcome: failed, err: d.cutError(cutCause(ctx, err)), attrs: append(attrs, "pipe", "held")}
	}
}

// uploadPipe writes the body of req into the named pipe at path, described by info (from Lstat),
// for a client handling pipes. Rat-Overwrite and Rat-Mode do not apply: a pipe is written into,
// never replaced. First 102, telling that ratd waits for the command; once the command opened the
// pipe, reading the body sends the go-ahead the client waits for (100). The body ending closes the
// pipe, its command seeing the end of its input (200). The command closing its end first fails the
// upload (409), as nothing takes the rest. A cut (the body cut short, the client's file having
// changed included, the client gone, ratd stopping) holds the pipe (see pipeTransfers), its command
// waiting rather than taking what it got for the whole of its input; the client closing it closes
// it.
func (d *daemon) uploadPipe(w http.ResponseWriter, req *http.Request, session, path string, info fs.FileInfo,
	attrs []any) result {
	t, ctx, refused := d.pipes.start(req.Context(), d.stopping, path, session, upload)
	if refused != nil {
		return refuse(w, http.StatusConflict, withAttrs(*refused, attrs))
	}
	pipe, refused, status := d.openPipe(ctx, w, path, info, fifo.Write)
	if refused != nil {
		d.pipes.finish(t, nil, false)
		return refuse(w, status, withAttrs(*refused, append(attrs, "pipe", "unopened")))
	}
	// Reading the body sends the go-ahead the client waits for.
	body := &readErrors{r: req.Body}
	sink := &writeErrors{w: pipe.Writer(ctx)}
	n, err := io.Copy(sink, body)
	attrs = append(attrs, "bytes", n)
	switch {
	case err == nil:
		// the body ended: closing the pipe tells its command the end of its input
		pipe.Close()
		d.pipes.finish(t, nil, false)
		w.WriteHeader(http.StatusOK)
		return result{attrs: append(attrs, "status", http.StatusOK, "pipe", "closed")}
	case errors.Is(context.Cause(ctx), errClosed):
		pipe.Close()
		d.pipes.finish(t, nil, false)
		return refuse(w, http.StatusConflict, result{err: errClosed, attrs: append(attrs, "pipe", "closed"),
			text: fmt.Sprintf("The transfer to %s was closed.", path)})
	case sink.err != nil && ctx.Err() == nil:
		// the command reading the pipe closed its end
		pipe.Close()
		d.pipes.finish(t, nil, false)
		return refuse(w, http.StatusConflict, result{err: sink.err, attrs: append(attrs, "pipe", "closed"),
			text: fmt.Sprintf("The command reading %s stopped after %s, before the end of the source.", path,
				sizeText(int(n)))})
	default:
		d.pipes.finish(t, pipe, true)
		return refuse(w, http.StatusBadRequest, result{err: d.cutError(cutCause(ctx, body.err)),
			attrs: append(attrs, "pipe", "held"), text: fmt.Sprintf("The upload to %s was cut before its end: "+
				"the pipe stays open, its command waiting, until the transfer is closed.", path)})
	}
}

// closePipe returns the handler closing the named pipe a transfer going way holds, or streams,
// for the session asking: rat asks when the agent cancels a transfer through a pipe here, having
// cut its request first. It answers 204 once closed, 404 when there is none (the transfer ended,
// or never started), and logs one line.
func (d *daemon) closePipe(way direction) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		session, ok := transferSession(w, req)
		if !ok {
			return
		}
		attrs := []any{"session", session, "remote", req.RemoteAddr, "direction", way}
		path, refused := transferPath(req)
		if refused != nil {
			http.Error(w, refused.text, http.StatusBadRequest)
			d.logger.Info("pipe close refused", append(attrs, "error", refused.err)...)
			return
		}
		attrs = append(attrs, "path", path)
		held, found := d.pipes.close(path, session, way)
		if !found {
			http.Error(w, fmt.Sprintf("No transfer of %s to close: it ended, or none was started.", path),
				http.StatusNotFound)
			d.logger.Info("pipe close: none", attrs...)
			return
		}
		w.WriteHeader(http.StatusNoContent)
		d.logger.Info("pipe closed", append(attrs, "held", held)...)
	})
}

// flushed writes to w, flushing each write: what a command writes into a pipe comes when it comes,
// and reaches the client then, rather than once a buffer filled.
type flushed struct {
	w          io.Writer
	controller *http.ResponseController
}

func (f flushed) Write(b []byte) (int, error) {
	n, err := f.w.Write(b)
	if err == nil {
		err = f.controller.Flush()
	}
	return n, err
}

// writeErrors writes to w, keeping the error it failed with: an upload through a pipe tells its
// command leaving from its body cut short.
type writeErrors struct {
	w   io.Writer
	err error
}

func (e *writeErrors) Write(b []byte) (int, error) {
	n, err := e.w.Write(b)
	if err != nil {
		e.err = err
	}
	return n, err
}
