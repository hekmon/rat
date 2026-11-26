package connect

import (
	"context"
	"fmt"
	"io"
	"io/fs"
	"net/http"

	"github.com/hekmon/rat/internal/stamp"
)

// A portal is a transfer with a pipe at one end where a file transfer has a file: one request of a
// transfer endpoint asking [HeaderPipe], ratd making the pipe at its path. That request is the
// portal: its answer's headers tell it ready, the pipe made and nothing flowed; its body moves
// what comes through; it ends as the portal closes, how being told after the stream, in the
// [HeaderError] trailer, the status being sent by then. A download from a pipe is one (see
// [StartDownload]): the body of the answer is what a command writes into the pipe. An upload into a
// pipe is the other (see [StartPipeUpload]): the body of the request is what ratd writes into the
// pipe for a command to read.
//
// An upload into a pipe is the one unusual thing a portal does on the wire: ratd answers it before
// it reads any byte of its body, an answer starting before its request was sent whole. HTTP/1.1
// allows it, and few programs expect it. It takes two conditions: the server allowing both
// directions at once (ratd does), and the request asking for no go-ahead (Expect: 100-continue),
// which a final answer cancels, the client then sending no body at all. A proxy between the client
// and ratd may hold the answer until the body ended, and ready with it: a portal is promised on a
// direct connection, which mutual TLS makes the usual one.
//
// A portal is closed by its path rather than by its request, which a client restarted no longer
// has: DELETE on either endpoint of the transfers, naming the path in [HeaderPath] (see
// [ClosePortal]). ratd closes the portal it holds at that path, whichever way it goes, for the
// session of the client certificate, a portal of another session being none, packs it up (its pipe deleted first, then
// its end closed), and answers once done, telling what the command at the pipe saw.

// ClosePortal asks the ratd at server, HOST:PORT, through client (see [HTTPClient]), to close the
// portal it holds at path for the session of the client, on endpoint ([DownloadPath] or
// [UploadPath]), and returns the sentence ratd wrote for the agent, and whether it closed one:
// ratd answers once the portal packed up, its pipe deleted. Holding none there, ratd tells so,
// closed false: nothing to close. A ratd older than the portals, which serves the transfers, holds
// none either, and fails with [ErrNoPortals]; one older than the transfers with [ErrNoTransfers].
// A refusal of ratd (a path it can not read, the checks of the client) is a [Refusal]; ratd not
// reached, the error of net/http.
func ClosePortal(ctx context.Context, client *http.Client, server, endpoint, path string) (text string, closed bool,
	err error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodDelete, transferURL(server, endpoint), nil)
	if err != nil {
		return "", false, err
	}
	req.Header.Set(HeaderPath, EncodePath(path))
	resp, err := client.Do(req)
	if err != nil {
		return "", false, err
	}
	defer resp.Body.Close()
	switch {
	case resp.StatusCode == http.StatusOK:
		return readText(resp.Body), true, nil
	case resp.StatusCode == http.StatusNotFound && resp.Header.Get(HeaderVersion) != "":
		return readText(resp.Body), false, nil
	case resp.StatusCode == http.StatusMethodNotAllowed:
		// the mux of a ratd serving the transfers without the portals: DELETE is not a method of the
		// endpoint, answered before any handler of ratd, without its version
		return "", false, ErrNoPortals
	}
	return "", false, refusal(resp)
}

// PipeUpload is an upload into the pipe of a portal, from [StartPipeUpload]: ratd made its pipe and
// answered ready, and the file is on its way, ratd writing it into the pipe as a command reads it.
// It must be closed.
type PipeUpload struct {
	resp *http.Response
	body *uploadBody
	// cut cuts the request; stop forgets the context of the upload
	cut  context.CancelFunc
	stop func() bool
}

// StartPipeUpload opens a portal on the ratd at server, HOST:PORT, through client (see
// [HTTPClient]): ratd makes a named pipe at path on its machine, where nothing may be, and src, a
// regular file the caller opened, is sent for ratd to write into the pipe once a command opens it
// for reading. It returns once ratd answered, the portal ready: the pipe exists and nothing was
// written into it yet. [PipeUpload.Wait] then waits for its end. Its bytes are counted by its own
// reads, for a caller wrapping it to tell progress: what was sent, not what a command took.
//
// A function of its own rather than an option of [Upload], which returns once ratd answered the
// file written: a portal is answered before its file is sent, and closes when it is used, two steps
// as a download takes them. It shares the body of an upload: src is stamped when opened and once
// read, a file that changed meanwhile, grew past its size, or failed to read cutting the body (see
// [Upload]), [ErrChanged] or a [TransferError] of kind [ErrRead] telling it.
//
// A refusal of ratd (something at path, a missing directory) is a [Refusal], with the sentence ratd
// wrote for the agent; [ErrNoTransfers] tells a ratd older than the transfers, [ErrNoPortals] one
// older than the portals, whose answer does not echo [HeaderPipe]: such a ratd takes the request
// for a file upload, which it refuses before reading its body, the request carrying no [HeaderMode]
// (see the body). Cancelling ctx cuts the portal, whatever was sent, the upload failing with its
// cause: ratd takes the link lost as a cut, a command reading the pipe then seeing no end, until the
// portal is closed ([ClosePortal]). A caller closing the portal asks ratd first, then cancels.
func StartPipeUpload(ctx context.Context, client *http.Client, server string, src fs.File, path string) (
	*PipeUpload, error) {
	info, err := src.Stat()
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() {
		return nil, fmt.Errorf("upload of %s: not a regular file", info.Name())
	}
	body := &uploadBody{src: src, opened: stamp.Of(info), size: info.Size()}
	// Cut by ctx alone, once it failed the body with its cause: the upload then tells why (a cancel,
	// a stop), rather than a connection lost. There is no cancel apart, as Upload has: a portal
	// closed always closes, whatever was sent.
	requestCtx, cut := context.WithCancel(context.WithoutCancel(ctx))
	stop := context.AfterFunc(ctx, func() {
		body.cancel(context.Cause(ctx), true)
		cut()
	})
	failed := func(err error) (*PipeUpload, error) {
		stop()
		cut()
		return nil, err
	}
	req, err := http.NewRequestWithContext(requestCtx, http.MethodPost, transferURL(server, UploadPath), body)
	if err != nil {
		return failed(err)
	}
	// chunked: its end is told by the last chunk alone, which a failing body never sends
	req.ContentLength = -1
	req.Header.Set("Content-Type", OctetStream)
	req.Header.Set(HeaderPath, EncodePath(path))
	req.Header.Set(HeaderPipe, "true")
	// No Expect: 100-continue, which a file upload sends to be refused before its body: ratd answers
	// a portal before reading its body, a final answer, and Go's client, receiving a final answer
	// where it waits for the go-ahead, sends no body at all (measured with Go 1.27 over TLS: ratd
	// read nothing, its body closed). The body is sent at once, then, as far as the connection holds
	// it, ratd reading none until a command opened the pipe.
	//
	// No Rat-Mode either, meaningless for a pipe ratd makes with its own bits, and required by a
	// file upload: a ratd older than the portals, which reads no Rat-Pipe and takes the request for
	// a file upload, refuses it before reading its body, creating nothing (400, without Rat-Pipe,
	// ErrNoPortals). With it, sent at once without a go-ahead to wait for, the body would be written
	// whole at path as a file, before rat could tell from the answer.
	resp, err := client.Do(req)
	if err != nil {
		if failure := body.failure(); failure != nil {
			return failed(failure)
		}
		return failed(err)
	}
	// Before the status: a refusal of an older ratd would read as the refusal of a portal. Without
	// the version, the answer comes from a ratd older than the transfers, or from its checks of the
	// client, told as they are by refusal.
	if resp.Header.Get(HeaderVersion) != "" && resp.Header.Get(HeaderPipe) != "true" {
		_ = resp.Body.Close()
		return failed(ErrNoPortals)
	}
	if resp.StatusCode != http.StatusOK {
		err := refusal(resp)
		_ = resp.Body.Close()
		return failed(err)
	}
	return &PipeUpload{resp: resp, body: body, cut: cut, stop: stop}, nil
}

// Wait waits for the end of the portal of u, and returns the bytes sent, and nil once ratd ended
// it as done: the file sent whole, all of it written into the pipe, and its end closed, the command
// reading it then seeing the end of its input. Otherwise: the file changing or failing to read, or
// ctx cancelled, as [StartPipeUpload] tells; the outcome ratd told in the [HeaderError] trailer, a
// [Refusal] of status 200 (the command reading left before the end, the portal closed, ratd
// stopping, a write of the pipe failing); the connection lost, a [TransferError] of kind [ErrCut].
func (u *PipeUpload) Wait() (int64, error) {
	answer := &readErrors{r: u.resp.Body}
	// the answer has no body: what comes is discarded, bounded, its end and its trailer awaited
	_, _ = io.Copy(io.Discard, io.LimitReader(answer, textMax))
	if failure := u.body.failure(); failure != nil {
		return u.body.read(), failure
	}
	if answer.err != nil {
		return u.body.read(), &TransferError{Kind: ErrCut, Reason: innermost(answer.err)}
	}
	if text := u.resp.Trailer.Get(HeaderError); text != "" {
		return u.body.read(), &Refusal{Status: u.resp.StatusCode, Text: DecodeError(text)}
	}
	if !u.body.whole() {
		// ratd ends a portal cleanly only once the body ended: this answer is not one
		return u.body.read(), &TransferError{Kind: ErrCut, Reason: fmt.Errorf("the answer ended before the file was "+
			"sent whole, after %d bytes", u.body.read())}
	}
	return u.body.read(), nil
}

// Sent returns the bytes of the file sent so far: read from it and handed to the connection, not
// what ratd wrote into the pipe, nor what a command took.
func (u *PipeUpload) Sent() int64 {
	return u.body.read()
}

// Close cuts the portal of u, unless it ended, and releases its connection: once ratd told its
// outcome, the rest of the file is no longer sent.
func (u *PipeUpload) Close() error {
	u.stop()
	u.cut()
	return u.resp.Body.Close()
}
