package connect

import (
	"bytes"
	"context"
	"errors"
	"io"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeRatd serves handler over TLS as a ratd would, until the test ends, and returns its address
// and a client of it built as HTTPClient builds one: from the default transport, waiting a second
// for the go-ahead of a server before sending a body.
func fakeRatd(t *testing.T, handler http.HandlerFunc) (string, *http.Client) {
	t.Helper()
	srv := httptest.NewTLSServer(handler)
	t.Cleanup(srv.Close)
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.TLSClientConfig = srv.Client().Transport.(*http.Transport).TLSClientConfig.Clone()
	return srv.Listener.Addr().String(), &http.Client{Transport: transport}
}

// hangUp closes the connection of the request w answers, with no answer: the connection lost.
func hangUp(t *testing.T, w http.ResponseWriter) {
	t.Helper()
	conn, _, err := http.NewResponseController(w).Hijack()
	if err != nil {
		t.Error(err)
		return
	}
	_ = conn.Close()
}

// stopOnAnswer calls stop once the answer to its request came, before the client returns it, as a
// stop coming right after ratd answered: the stop running its course meanwhile, in a goroutine of
// its own (context.AfterFunc), for which it waits a little.
type stopOnAnswer struct {
	inner http.RoundTripper
	stop  func()
}

func (s stopOnAnswer) RoundTrip(req *http.Request) (*http.Response, error) {
	resp, err := s.inner.RoundTrip(req)
	s.stop()
	time.Sleep(100 * time.Millisecond)
	return resp, err
}

// testFile is a file to upload: content, or size zero bytes without it. grows tells it grew once
// read to its end, its stamp then changing.
type testFile struct {
	content []byte
	size    int
	mode    fs.FileMode
	grows   bool
	// mu guards off, the bytes read
	mu  sync.Mutex
	off int
}

// length returns the size of the file.
func (f *testFile) length() int {
	if f.content != nil {
		return len(f.content)
	}
	return f.size
}

func (f *testFile) Read(p []byte) (int, error) {
	off := f.taken()
	if off == f.length() {
		return 0, io.EOF
	}
	n := min(len(p), f.length()-off)
	if f.content != nil {
		copy(p, f.content[off:off+n])
	} else {
		clear(p[:n])
	}
	f.mu.Lock()
	f.off += n
	f.mu.Unlock()
	return n, nil
}

func (f *testFile) Stat() (fs.FileInfo, error) {
	size := int64(f.length())
	if f.grows && f.taken() == f.length() {
		size++
	}
	return testInfo{size: size, mode: f.mode}, nil
}

func (f *testFile) Close() error {
	return nil
}

// taken returns the bytes read from the file.
func (f *testFile) taken() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.off
}

// testInfo describes a testFile.
type testInfo struct {
	size int64
	mode fs.FileMode
}

func (i testInfo) Name() string       { return "test.bin" }
func (i testInfo) Size() int64        { return i.size }
func (i testInfo) Mode() fs.FileMode  { return i.mode }
func (i testInfo) ModTime() time.Time { return time.Time{} }
func (i testInfo) IsDir() bool        { return false }
func (i testInfo) Sys() any           { return nil }

// TestUpload guards an upload as ratd expects it: a POST of UploadPath, chunked, with the path
// encoded, the permission bits of the file, overwrite, and the bytes of the file whole; and what
// ratd answered: a file created with its mode and the note of ratd (201), or replaced (204).
func TestUpload(t *testing.T) {
	content := bytes.Repeat([]byte("data\n"), 100_000)
	server, client := fakeRatd(t, func(w http.ResponseWriter, r *http.Request) {
		path, err := DecodePath(r.Header.Get(HeaderPath))
		if r.Method != http.MethodPost || r.URL.Path != UploadPath || r.Header.Get("Content-Type") != OctetStream ||
			len(r.TransferEncoding) != 1 || r.TransferEncoding[0] != "chunked" || err != nil || path != "~/my dir/app.bin" ||
			r.Header.Get(HeaderMode) != "0750" {
			t.Errorf("unexpected request: %s %s, path %q (%v), %v, %v", r.Method, r.URL.Path, path, err, r.TransferEncoding, r.Header)
		}
		if body, err := io.ReadAll(r.Body); err != nil || !bytes.Equal(body, content) {
			t.Errorf("expected the file whole, got %d bytes, %v", len(body), err)
		}
		if r.Header.Get(HeaderOverwrite) == "true" {
			w.Header().Set(HeaderMode, "0600")
			w.WriteHeader(http.StatusNoContent)
			return
		}
		w.Header().Set(HeaderMode, "0640")
		w.WriteHeader(http.StatusCreated)
		_, _ = io.WriteString(w, "Missing directories created: /x.\n")
	})
	up, err := Upload(context.Background(), nil, client, server, &testFile{content: content, mode: 0o750},
		"~/my dir/app.bin", false)
	if err != nil || *up != (Uploaded{Mode: 0o640, Sent: int64(len(content)), Note: "Missing directories created: /x."}) {
		t.Errorf("expected the file created, got %+v, %v", up, err)
	}
	up, err = Upload(context.Background(), nil, client, server, &testFile{content: content, mode: 0o750},
		"~/my dir/app.bin", true)
	if err != nil || *up != (Uploaded{Replaced: true, Mode: 0o600, Sent: int64(len(content))}) {
		t.Errorf("expected the file replaced, got %+v, %v", up, err)
	}
}

// TestUploadRefused guards the refusals of ratd: from the headers alone, before any byte of the
// file is read, as a Refusal with the sentence of ratd; a 404 without the version of ratd telling
// a ratd older than the transfers, one with it a refusal like the others.
func TestUploadRefused(t *testing.T) {
	server, client := fakeRatd(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.Header.Get(HeaderPath) {
		case "/old":
			http.NotFound(w, r)
		case "/missing":
			w.Header().Set(HeaderVersion, "v1")
			http.Error(w, "/missing does not exist.", http.StatusNotFound)
		default:
			w.Header().Set(HeaderVersion, "v1")
			http.Error(w, "/x exists: set overwrite to replace it.", http.StatusPreconditionFailed)
		}
	})
	file := &testFile{content: bytes.Repeat([]byte("x"), 1<<20), mode: 0o644}
	_, err := Upload(context.Background(), nil, client, server, file, "/x", false)
	var refused *Refusal
	if !errors.As(err, &refused) || !errors.Is(err, ErrRefused) || refused.Status != http.StatusPreconditionFailed ||
		refused.Text != "/x exists: set overwrite to replace it." {
		t.Errorf("expected a refusal with the sentence of ratd, got %v", err)
	}
	if taken := file.taken(); taken != 0 {
		t.Errorf("expected the refusal before any byte was read, %d were", taken)
	}
	if _, err = Upload(context.Background(), nil, client, server, file, "/old", false); !errors.Is(err, ErrNoTransfers) {
		t.Errorf("a 404 without the version: expected ErrNoTransfers, got %v", err)
	}
	if _, err = Upload(context.Background(), nil, client, server, file, "/missing", false); !errors.As(err, &refused) ||
		refused.Status != http.StatusNotFound {
		t.Errorf("a 404 with the version: expected a refusal, got %v", err)
	}
}

// TestUploadCut guards an upload whose connection is lost: once ratd gave its go-ahead, before the
// end of the file, a TransferError of kind ErrCut, ratd writing nothing; once the file was sent
// whole, of kind ErrAnswerLost, ratd possibly writing it; both with the reason the system gave
// rather than the request; with no go-ahead of ratd, the error as net/http gives it.
func TestUploadCut(t *testing.T) {
	server, client := fakeRatd(t, func(w http.ResponseWriter, r *http.Request) {
		switch path, _ := DecodePath(r.Header.Get(HeaderPath)); path {
		case "/go-ahead":
			// reading the body sends the go-ahead
			_, _ = io.ReadFull(r.Body, make([]byte, 1024))
		case "/whole":
			_, _ = io.Copy(io.Discard, r.Body)
		}
		hangUp(t, w)
	})
	for _, tc := range []struct {
		path string
		// size is the size of the file: beyond what the buffers of a connection hold, the
		// connection is lost before its end
		size int
		// kind is the kind of TransferError expected, nil for the error of net/http alone
		kind error
	}{
		{"/go-ahead", 64 << 20, ErrCut},
		{"/none", 64 << 20, nil},
		{"/whole", 1024, ErrAnswerLost},
	} {
		file := &testFile{size: tc.size, mode: 0o644}
		_, err := Upload(context.Background(), nil, client, server, file, tc.path, false)
		var failed *TransferError
		switch {
		case err == nil:
			t.Errorf("%s: expected the upload to fail", tc.path)
		case tc.kind == nil && errors.As(err, &failed):
			t.Errorf("%s: expected the error of net/http alone, got %v", tc.path, err)
		case tc.kind != nil && !errors.Is(err, tc.kind):
			t.Errorf("%s: expected %v, got %v", tc.path, tc.kind, err)
		case tc.kind != nil && (!errors.As(err, &failed) || strings.Contains(failed.Reason.Error(), server)):
			t.Errorf("%s: expected the reason alone, rather than the request, got %v", tc.path, err)
		}
	}
}

// TestUploadChanged guards a file changing while it is uploaded: the body ends with an error
// instead of its end, so that ratd sees it cut, and Upload fails with ErrChanged.
func TestUploadChanged(t *testing.T) {
	cut := make(chan error, 1)
	server, client := fakeRatd(t, func(w http.ResponseWriter, r *http.Request) {
		_, err := io.Copy(io.Discard, r.Body)
		cut <- err
	})
	file := &testFile{content: bytes.Repeat([]byte("x"), 64<<10), mode: 0o644, grows: true}
	if _, err := Upload(context.Background(), nil, client, server, file, "/x", false); !errors.Is(err, ErrChanged) {
		t.Errorf("expected ErrChanged, got %v", err)
	}
	if err := <-cut; err == nil {
		t.Error("expected ratd to see the body cut")
	}
}

// TestUploadCancelled guards the two signals stopping an upload. A cancel (its channel closed),
// before the end of the file is sent, cuts the upload at once, even while its connection stalls
// (ratd reading nothing more), and ratd sees it cut; once the whole file is sent, ratd may be
// writing it, and Upload waits for its answer and returns it, rather than leave unknown whether
// the file was written. Its context cuts it whatever was sent, nobody waiting for that answer, a
// cancel having come first or not: a cancel waiting for the answer leaves the context to stop it.
// A stop coming once ratd answered the file written is too late: that answer is returned.
func TestUploadCancelled(t *testing.T) {
	t.Run("before the end", func(t *testing.T) {
		started, release, cut := make(chan struct{}), make(chan struct{}), make(chan error, 1)
		server, client := fakeRatd(t, func(w http.ResponseWriter, r *http.Request) {
			if _, err := io.ReadFull(r.Body, make([]byte, 1024)); err != nil {
				t.Error(err)
			}
			close(started)
			// nothing more read: the buffers of the connection fill, and the client blocks writing
			<-release
			_, err := io.Copy(io.Discard, r.Body)
			cut <- err
		})
		// more than the buffers of a connection hold
		file := &testFile{size: 256 << 20, mode: 0o644}
		cancel := make(chan struct{})
		done := make(chan error, 1)
		go func() {
			_, err := Upload(context.Background(), cancel, client, server, file, "/x", false)
			done <- err
		}()
		<-started
		close(cancel)
		select {
		case err := <-done:
			if !errors.Is(err, context.Canceled) {
				t.Errorf("expected the upload canceled, got %v", err)
			}
		case <-time.After(5 * time.Second):
			t.Fatal("the cancel did not cut the upload at once")
		}
		close(release)
		if err := <-cut; err == nil {
			t.Error("expected ratd to see the body cut")
		}
	})
	t.Run("after the end", func(t *testing.T) {
		received, answer := make(chan struct{}), make(chan struct{})
		server, client := fakeRatd(t, func(w http.ResponseWriter, r *http.Request) {
			if _, err := io.Copy(io.Discard, r.Body); err != nil {
				t.Error(err)
			}
			close(received)
			<-answer
			w.Header().Set(HeaderMode, "0644")
			w.WriteHeader(http.StatusCreated)
		})
		cancel := make(chan struct{})
		type outcome struct {
			up  *Uploaded
			err error
		}
		done := make(chan outcome, 1)
		go func() {
			up, err := Upload(context.Background(), cancel, client, server, &testFile{content: []byte("whole"),
				mode: 0o644}, "/x", false)
			done <- outcome{up, err}
		}()
		<-received
		close(cancel)
		close(answer)
		if o := <-done; o.err != nil || o.up.Replaced || o.up.Sent != 5 {
			t.Errorf("expected the answer of ratd once the file was sent whole, got %+v, %v", o.up, o.err)
		}
	})
	for _, canceled := range []bool{false, true} {
		name := "stopped after the end"
		if canceled {
			name = "canceled, then stopped, after the end"
		}
		t.Run(name, func(t *testing.T) {
			received, answer := make(chan struct{}), make(chan struct{})
			server, client := fakeRatd(t, func(w http.ResponseWriter, r *http.Request) {
				if _, err := io.Copy(io.Discard, r.Body); err != nil {
					t.Error(err)
				}
				close(received)
				// slow to answer, as a ratd writing on a file system that does not answer
				select {
				case <-answer:
				case <-r.Context().Done():
				}
			})
			t.Cleanup(func() { close(answer) })
			ctx, stop := context.WithCancelCause(context.Background())
			cancel := make(chan struct{})
			done := make(chan error, 1)
			go func() {
				_, err := Upload(ctx, cancel, client, server, &testFile{content: []byte("whole"), mode: 0o644}, "/x",
					false)
				done <- err
			}()
			<-received
			if canceled {
				close(cancel)
				select {
				case err := <-done:
					t.Fatalf("canceled once sent whole: expected the upload to wait for the answer, got %v", err)
				case <-time.After(200 * time.Millisecond):
				}
			}
			stopping := errors.New("client stopping")
			stop(stopping)
			select {
			case err := <-done:
				if !errors.Is(err, stopping) {
					t.Errorf("expected the upload stopped, the cause of its context, got %v", err)
				}
			case <-time.After(5 * time.Second):
				t.Fatal("stopped once sent whole: the upload still waits for the answer of ratd")
			}
		})
	}
	t.Run("stopped once answered", func(t *testing.T) {
		server, client := fakeRatd(t, func(w http.ResponseWriter, r *http.Request) {
			if _, err := io.Copy(io.Discard, r.Body); err != nil {
				t.Error(err)
			}
			w.Header().Set(HeaderMode, "0644")
			w.WriteHeader(http.StatusCreated)
		})
		ctx, stop := context.WithCancel(context.Background())
		defer stop()
		client.Transport = stopOnAnswer{inner: client.Transport, stop: stop}
		up, err := Upload(ctx, nil, client, server, &testFile{content: []byte("whole"), mode: 0o644}, "/x", false)
		if err != nil || up.Replaced || up.Sent != 5 {
			t.Errorf("stopped once ratd answered the file written: expected that answer, too late to stop, got %+v, %v",
				up, err)
		}
	})
}
