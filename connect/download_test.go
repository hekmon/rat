package connect

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"strconv"
	"testing"
)

// TestDownload guards a download as ratd sends it: a GET of DownloadPath with the path encoded;
// the permission bits and size from its headers, before the bytes; the bytes whole, complete
// without the Rat-Error trailer.
func TestDownload(t *testing.T) {
	content := bytes.Repeat([]byte("data\n"), 100_000)
	server, client := fakeRatd(t, func(w http.ResponseWriter, r *http.Request) {
		if path, err := DecodePath(r.Header.Get(HeaderPath)); r.Method != http.MethodGet || r.URL.Path != DownloadPath ||
			err != nil || path != "~/my dir/app.bin" {
			t.Errorf("unexpected request: %s %s, path %q, %v", r.Method, r.URL.Path, path, err)
		}
		w.Header().Set(HeaderVersion, "v1")
		w.Header().Set(HeaderMode, "0750")
		w.Header().Set(HeaderSize, strconv.Itoa(len(content)))
		w.Header().Set("Trailer", HeaderError)
		_, _ = w.Write(content)
	})
	d, err := StartDownload(context.Background(), client, server, "~/my dir/app.bin")
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	if d.Mode != 0o750 || d.Size != int64(len(content)) {
		t.Errorf("expected the mode 0750 and the size, got %v, %d", d.Mode, d.Size)
	}
	var written bytes.Buffer
	if n, err := d.WriteTo(&written); err != nil || n != int64(len(content)) || !bytes.Equal(written.Bytes(), content) {
		t.Errorf("expected the file whole, got %d bytes, %v", n, err)
	}
}

// TestDownloadFailed guards the failures of a download: refused by ratd from the start (a Refusal
// with its sentence), a ratd older than the transfers (a 404 without its version), the file
// changed while ratd read it (the Rat-Error trailer, a Refusal of status 200), a body cut short,
// and fewer bytes than the size told (ErrCut).
func TestDownloadFailed(t *testing.T) {
	server, client := fakeRatd(t, func(w http.ResponseWriter, r *http.Request) {
		path, _ := DecodePath(r.Header.Get(HeaderPath))
		switch path {
		case "/old":
			http.NotFound(w, r)
			return
		case "/refused":
			w.Header().Set(HeaderVersion, "v1")
			http.Error(w, "/refused is a directory, not a file.", http.StatusConflict)
			return
		}
		w.Header().Set(HeaderVersion, "v1")
		w.Header().Set(HeaderMode, "0644")
		w.Header().Set(HeaderSize, "1000")
		w.Header().Set("Trailer", HeaderError)
		switch path {
		case "/changed":
			_, _ = w.Write(bytes.Repeat([]byte("x"), 1000))
			w.Header().Set(HeaderError, "/changed changed while it was read.")
		case "/cut":
			_, _ = w.Write(bytes.Repeat([]byte("x"), 500))
			http.NewResponseController(w).Flush()
			panic(http.ErrAbortHandler)
		case "/short":
			_, _ = w.Write(bytes.Repeat([]byte("x"), 500))
		}
	})
	if _, err := StartDownload(context.Background(), client, server, "/old"); !errors.Is(err, ErrNoTransfers) {
		t.Errorf("a 404 without the version: expected ErrNoTransfers, got %v", err)
	}
	var refused *Refusal
	if _, err := StartDownload(context.Background(), client, server, "/refused"); !errors.As(err, &refused) ||
		refused.Status != http.StatusConflict || refused.Text != "/refused is a directory, not a file." {
		t.Errorf("expected a refusal with the sentence of ratd, got %v", err)
	}
	for path, expected := range map[string]func(error) bool{
		"/changed": func(err error) bool {
			return errors.As(err, &refused) && refused.Status == http.StatusOK && refused.Text == "/changed changed while it was read."
		},
		"/cut":   func(err error) bool { return errors.Is(err, ErrCut) },
		"/short": func(err error) bool { return errors.Is(err, ErrCut) },
	} {
		d, err := StartDownload(context.Background(), client, server, path)
		if err != nil {
			t.Fatalf("%s: %v", path, err)
		}
		if _, err = d.WriteTo(io.Discard); !expected(err) {
			t.Errorf("%s: unexpected error %v", path, err)
		}
		_ = d.Close()
	}
}
