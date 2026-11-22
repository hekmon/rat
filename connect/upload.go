package connect

import (
	"context"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"net/http/httptrace"
	"strconv"
	"sync"
	"sync/atomic"

	"github.com/hekmon/rat/internal/stamp"
)

// Uploaded is what ratd answered an upload it wrote.
type Uploaded struct {
	// Replaced tells an existing file was replaced (204), rather than created (201).
	Replaced bool
	// Mode is the permission bits of the file as written: those sent minus the umask of ratd for
	// a file created, its own for a file replaced.
	Mode fs.FileMode
	// Sent is the size of the file sent, in bytes.
	Sent int64
	// Note is what ratd tells the agent of a file it created, often nothing: the directories it
	// created for it.
	Note string
}

// Upload copies src, a regular file the caller opened, to path on the machine of the ratd at
// server, HOST:PORT, through client (see [HTTPClient]), replacing an existing file with overwrite
// only, and returns what ratd wrote. The permission bits of src are sent; its bytes are counted
// by its own reads, for a caller wrapping it to tell progress.
//
// It is all or nothing on ratd's side: ratd writes the file once received whole, and writes
// nothing of an upload cut before its end. What its headers alone refuse is refused before any
// byte is sent, client waiting for the go-ahead of ratd ([HTTPClient] waits a second for it). A
// refusal or failure of ratd is a [Refusal], with the sentence ratd wrote for the agent;
// [ErrNoTransfers] tells a ratd older than the transfers; a [TransferError] of kind [ErrCut] the
// connection lost once ratd gave its go-ahead, before the end of the file; of kind
// [ErrAnswerLost] lost once the file was sent whole, before ratd answered, ratd then possibly
// writing it.
//
// src is stamped when opened and once read (see package stamp): a file that changed meanwhile (a
// log still written, a build output) ends the body with an error instead of its end, and Upload
// fails with [ErrChanged], ratd writing nothing.
//
// Two signals stop an upload. Closing cancel (unless nil), as an agent cancels, cuts it, ratd
// writing nothing, Upload returning [context.Canceled], unless the whole file was sent: from then
// on, ratd may be writing it, and Upload waits for its answer and returns it, so that a cancel
// never leaves unknown whether the file was written. The choice is made at one point, when the end
// of the file is about to be sent. Cancelling ctx cuts the upload whatever was sent, a cancel
// having come first or not, returning its cause: nobody waits for the answer any more (a client
// stopping); once ratd answered the file written, it is too late, and Upload returns that answer.
// One signal would not do: a context is done once, and a cancel waiting for the answer would leave
// a stop coming next unseen. Either cuts the connection at once, even stalled, but net/http
// returns from a request only once the read of its body under way returns: a read of src blocked
// (a file system not answering) delays it.
func Upload(ctx context.Context, cancel <-chan struct{}, client *http.Client, server string, src fs.File,
	path string, overwrite bool) (*Uploaded, error) {
	info, err := src.Stat()
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() {
		return nil, fmt.Errorf("upload of %s: not a regular file", info.Name())
	}
	body := &uploadBody{src: src, opened: stamp.Of(info)}
	// The request is cut at once by ctx, whatever was sent, and by cancel unless the file was sent
	// whole (see uploadBody.cancel): a write blocked on a stalled connection never sees a cancel in
	// the body. It is cut by these alone, once they failed the body with why, rather than by ctx
	// itself: Upload then tells why (a cancel, a stop), rather than a connection lost.
	requestCtx, cut := context.WithCancel(context.WithoutCancel(ctx))
	defer cut()
	stop := context.AfterFunc(ctx, func() {
		body.cancel(context.Cause(ctx), true)
		cut()
	})
	defer stop()
	stopCancel := afterClosed(cancel, func() {
		if body.cancel(context.Canceled, false) {
			cut()
		}
	})
	defer stopCancel()
	// the go-ahead of ratd (100): the connection lost after it cuts the transfer ratd answered
	var answered atomic.Bool
	traced := httptrace.WithClientTrace(requestCtx, &httptrace.ClientTrace{
		Got100Continue: func() { answered.Store(true) },
	})
	req, err := http.NewRequestWithContext(traced, http.MethodPost, transferURL(server, UploadPath), body)
	if err != nil {
		return nil, err
	}
	// chunked: its end is told by the last chunk alone, which a failing body never sends
	req.ContentLength = -1
	req.Header.Set("Content-Type", OctetStream)
	req.Header.Set("Expect", "100-continue")
	req.Header.Set(HeaderPath, EncodePath(path))
	req.Header.Set(HeaderMode, FormatMode(info.Mode()))
	req.Header.Set(HeaderOverwrite, strconv.FormatBool(overwrite))
	resp, err := client.Do(req)
	// a stop coming once ratd answered the file written is too late: that answer is returned, the
	// stop cutting at most the read of its note
	done := err == nil && (resp.StatusCode == http.StatusCreated || resp.StatusCode == http.StatusNoContent)
	if failure := body.failure(); failure != nil && !done {
		if err == nil {
			_ = resp.Body.Close()
		}
		return nil, failure
	}
	switch {
	case err != nil && body.whole():
		// lost once the file was sent whole: ratd may be writing it, which only its answer would
		// have told
		return nil, &TransferError{Kind: ErrAnswerLost, Reason: innermost(err)}
	case err != nil && answered.Load():
		// lost once ratd gave its go-ahead, before the end of the file: cut, ratd writing nothing
		return nil, &TransferError{Kind: ErrCut, Reason: innermost(err)}
	case err != nil:
		// ratd not reached, or lost before its go-ahead
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusCreated && resp.StatusCode != http.StatusNoContent {
		return nil, refusal(resp)
	}
	mode, err := ParseMode(resp.Header.Get(HeaderMode))
	if err != nil {
		return nil, err
	}
	return &Uploaded{Replaced: resp.StatusCode == http.StatusNoContent, Mode: mode, Sent: body.read(),
		Note: readText(resp.Body)}, nil
}

// afterClosed calls f in a goroutine of its own once c is closed, unless stop is called first: a
// nil c never closes.
func afterClosed(c <-chan struct{}, f func()) (stop func()) {
	if c == nil {
		return func() {}
	}
	stopped := make(chan struct{})
	go func() {
		select {
		case <-c:
			f()
		case <-stopped:
		}
	}()
	return func() { close(stopped) }
}

// uploadBody is the body of an upload: src, read to its end, whose end is told only if the file
// is as it was when opened, and the upload not cancelled. Otherwise the body ends with the error
// telling why, and ratd, seeing it cut, writes nothing. Once its end is told, the whole file is
// sent: a cancel cuts the upload no more, its context still does (see Upload).
type uploadBody struct {
	src    fs.File
	opened stamp.Stamp
	// mu guards the fields below
	mu sync.Mutex
	// n counts the bytes read from src
	n int64
	// ended tells the end of the file was told: the whole file is sent
	ended bool
	// err is why the body failed: the file changed or could not be read, or the upload was
	// cancelled
	err error
}

func (b *uploadBody) Read(p []byte) (int, error) {
	if err := b.failure(); err != nil {
		return 0, err
	}
	// Not under the lock: a read may block (a slow disk), and a cancel must not wait for it.
	n, err := b.src.Read(p)
	b.mu.Lock()
	defer b.mu.Unlock()
	b.n += int64(n)
	switch {
	case b.err != nil:
		// cancelled while reading
		return 0, b.err
	case err == io.EOF:
		if info, statErr := b.src.Stat(); statErr != nil || !stamp.Of(info).Equal(b.opened) {
			b.err = ErrChanged
			return 0, b.err
		}
		b.ended = true
		return n, io.EOF
	case err != nil:
		b.err = fmt.Errorf("reading the file: %w", err)
		return 0, b.err
	}
	return n, nil
}

// cancel fails the upload with err, and tells whether it did: unless the whole file was sent,
// ratd then writing it, but with evenWhole: stopped by the context of the upload, nobody waiting
// for the answer.
func (b *uploadBody) cancel(err error, evenWhole bool) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.ended && !evenWhole {
		return false
	}
	if b.err == nil {
		b.err = err
	}
	return true
}

// failure returns why the body failed, or nil.
func (b *uploadBody) failure() error {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.err
}

// read returns the bytes read from src.
func (b *uploadBody) read() int64 {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.n
}

// whole tells whether the end of the file was told: the file sent whole.
func (b *uploadBody) whole() bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.ended
}
