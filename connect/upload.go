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
// connection lost once ratd gave its go-ahead, before the end of the file.
//
// src is stamped when opened and once read (see package stamp): a file that changed meanwhile (a
// log still written, a build output) ends the body with an error instead of its end, and Upload
// fails with [ErrChanged], ratd writing nothing. Cancelling ctx cuts the upload likewise, unless
// the whole file was sent: from then on, ratd may be writing it, and Upload waits for its answer
// and returns it, so that a cancel never leaves unknown whether the file was written. The choice
// is made at one point, when the end of the file is about to be sent. A cancel cuts the
// connection at once, even stalled, but net/http returns from a request only once the read of
// its body under way returns: a read of src blocked (a file system not answering) delays it.
func Upload(ctx context.Context, client *http.Client, server string, src fs.File, path string,
	overwrite bool) (*Uploaded, error) {
	info, err := src.Stat()
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() {
		return nil, fmt.Errorf("upload of %s: not a regular file", info.Name())
	}
	body := &uploadBody{src: src, opened: stamp.Of(info)}
	// The request outlives ctx once the file is sent whole (see uploadBody.cancel), and is cut at
	// once before: a write blocked on a stalled connection never sees a cancel in the body.
	requestCtx, cut := context.WithCancel(context.WithoutCancel(ctx))
	defer cut()
	stop := context.AfterFunc(ctx, func() {
		if body.cancel(ctx.Err()) {
			cut()
		}
	})
	defer stop()
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
	if failure := body.failure(); failure != nil {
		if err == nil {
			_ = resp.Body.Close()
		}
		return nil, failure
	}
	switch {
	case err != nil && answered.Load() && !body.whole():
		// lost once ratd gave its go-ahead, before the end of the file: cut, ratd writing nothing
		return nil, &TransferError{Kind: ErrCut, Reason: innermost(err)}
	case err != nil:
		// ratd not reached, or lost before its go-ahead; or lost once the file was sent whole, ratd
		// then possibly writing it, which only its answer would tell
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

// uploadBody is the body of an upload: src, read to its end, whose end is told only if the file
// is as it was when opened, and the upload not cancelled. Otherwise the body ends with the error
// telling why, and ratd, seeing it cut, writes nothing. Once its end is told, the whole file is
// sent: nothing cuts the upload anymore.
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

// cancel fails the upload with err unless the whole file was sent, and tells whether it did.
func (b *uploadBody) cancel(err error) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.ended {
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
