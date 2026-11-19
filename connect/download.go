package connect

import (
	"context"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"strconv"
)

// Download is a download under way, from [StartDownload]: the file as ratd opened it, whose
// bytes are still to be read ([Download.WriteTo]). It must be closed.
type Download struct {
	// Mode is the permission bits of the file, for the caller to create the file it writes with.
	Mode fs.FileMode
	// Size is the size of the file when ratd opened it: what a complete download writes.
	Size int64
	resp *http.Response
}

// StartDownload asks the ratd at server, HOST:PORT, through client (see [HTTPClient]), for the
// file at path on its machine, and returns the download once ratd answered with the file's
// headers, before its bytes: the caller learns the permission bits to create its own file with.
// A refusal or failure of ratd is a [Refusal], with the sentence ratd wrote for the agent;
// [ErrNoTransfers] tells a ratd older than the transfers. Cancelling ctx cuts the download, until
// the download is closed.
func StartDownload(ctx context.Context, client *http.Client, server, path string) (*Download, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, transferURL(server, DownloadPath), nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set(HeaderPath, EncodePath(path))
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		defer resp.Body.Close()
		return nil, refusal(resp)
	}
	mode, err := ParseMode(resp.Header.Get(HeaderMode))
	if err != nil {
		_ = resp.Body.Close()
		return nil, err
	}
	size, err := strconv.ParseInt(resp.Header.Get(HeaderSize), 10, 64)
	if err != nil || size < 0 {
		_ = resp.Body.Close()
		return nil, fmt.Errorf("%w %s %q: expected a size in bytes", ErrHeader, HeaderSize, resp.Header.Get(HeaderSize))
	}
	return &Download{Mode: mode, Size: size, resp: resp}, nil
}

// WriteTo writes the file to w, and returns the bytes written, failing unless they are the whole
// file as ratd opened it: a body cut short fails with [ErrCut]; a file ratd found changed while
// reading it, or could not read, with a [Refusal] (status 200) telling why, from the trailer of
// the body; a write failing with the error of w. What was written is then partial, for the caller
// to drop.
func (d *Download) WriteTo(w io.Writer) (int64, error) {
	body := &readErrors{r: d.resp.Body}
	n, err := io.Copy(w, body)
	switch {
	case body.err != nil:
		return n, fmt.Errorf("%w: %w", ErrCut, body.err)
	case err != nil:
		return n, err
	}
	// the trailer is read with the end of the body
	if text := d.resp.Trailer.Get(HeaderError); text != "" {
		return n, &Refusal{Status: d.resp.StatusCode, Text: text}
	}
	if n != d.Size {
		return n, fmt.Errorf("%w: %d bytes received of %d", ErrCut, n, d.Size)
	}
	return n, nil
}

// Close ends the download, cutting it if its body was not read whole.
func (d *Download) Close() error {
	return d.resp.Body.Close()
}
