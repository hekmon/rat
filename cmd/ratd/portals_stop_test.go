package main

import (
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/hekmon/rat/connect"
)

// TestPortalUploadStopNotDone guards what the answer of an upload portal tells a client when ratd
// stops, waiting for a command or copying to one, over a real connection: never a portal done, an
// answer ending cleanly with no Rat-Error. The stop cuts the answer most often (see cutOnStop),
// before a client reads its trailer: what the trailer tells is guarded by TestPortalUploadStopTold.
func TestPortalUploadStopNotDone(t *testing.T) {
	for _, phase := range []string{"waiting", "copying"} {
		t.Run(phase, func(t *testing.T) {
			pt := startPortals(t)
			path := filepath.Join(pt.home, "in.pipe")
			resp, send := pt.openUpload(t, pt.alice, path)
			expectPipe(t, path)
			if phase == "copying" {
				r := openReader(t, path)
				pt.waitOpened(t, path)
				if _, err := send.Write([]byte("begin")); err != nil {
					t.Fatal(err)
				}
				r.readUntil(5)
				eventually(t, "the pump waiting in a read of the body", func() bool {
					return pumpAt("net/http.(*body).Read(")
				})
			}
			expectQuickStop(t, pt.r)
			_, err := readBody(t, resp.Body)
			told := connect.DecodeError(resp.Trailer.Get(connect.HeaderError))
			switch {
			case err != nil:
				// cut by the stop
			case told != stoppingText(path):
				t.Errorf("expected the answer cut, or ended telling the stop, got it ended with Rat-Error %q", told)
			}
			pt.r.d.portals.release()
		})
	}
}

// portalRecorder records the answer of a portal served by a handler called directly, standing for
// the connection of net/http: it answers before the body is read (EnableFullDuplex), and a read
// deadline in the past cuts the body, as it cuts the read of a connection. Nothing cuts the answer
// meanwhile, as the stop cuts a connection (see cutOnStop): its trailer is read whole.
type portalRecorder struct {
	*httptest.ResponseRecorder
	body *io.PipeReader
}

func (r portalRecorder) EnableFullDuplex() error { return nil }

func (r portalRecorder) SetReadDeadline(deadline time.Time) error {
	if !deadline.IsZero() && !deadline.After(time.Now()) {
		_ = r.body.CloseWithError(os.ErrDeadlineExceeded)
	}
	return nil
}

func (r portalRecorder) SetWriteDeadline(time.Time) error { return nil }

// TestPortalUploadStopTold guards the Rat-Error trailer of an upload portal ended by ratd stopping,
// waiting for a command or copying to one, the portal then held (Q5): it tells the stop
// (stoppingText), for a client reading the answer before the stop cuts it, an answer ending without
// one being a portal done. The handler is called directly, the answer recorded: over a connection,
// the stop cuts it before a client reads the trailer (see TestPortalUploadStopNotDone).
func TestPortalUploadStopTold(t *testing.T) {
	for _, phase := range []string{"waiting", "copying"} {
		t.Run(phase, func(t *testing.T) {
			stopping := make(chan struct{})
			d := &daemon{logger: slog.New(slog.DiscardHandler), stopping: stopping}
			t.Cleanup(d.portals.release)
			path := filepath.Join(t.TempDir(), "in.pipe")
			body, send := io.Pipe()
			t.Cleanup(func() { _ = send.Close() })
			req := httptest.NewRequest(http.MethodPost, connect.UploadPath, body)
			req.Header.Set("Content-Type", connect.OctetStream)
			req.Header.Set(connect.HeaderPath, connect.EncodePath(path))
			req.Header.Set(connect.HeaderPipe, "true")
			req.TLS = &tls.ConnectionState{PeerCertificates: []*x509.Certificate{{Subject: pkix.Name{CommonName: "alice"}}}}
			w := portalRecorder{ResponseRecorder: httptest.NewRecorder(), body: body}
			served := make(chan result, 1)
			go func() { served <- d.uploadPipe(w, req) }()
			portalAt := func() *portal {
				d.portals.mu.Lock()
				defer d.portals.mu.Unlock()
				return d.portals.open[path]
			}
			eventually(t, "the pump waiting in the open of its end", func() bool {
				return pumpAt(".(*portal).openEnd(")
			})
			if phase == "copying" {
				r := openReader(t, path)
				eventually(t, "ratd opening its end of the pipe", func() bool { return portalAt().opened.Load() })
				if _, err := send.Write([]byte("begin")); err != nil {
					t.Fatal(err)
				}
				r.readUntil(5)
				eventually(t, "the pump waiting in a read of the body", func() bool {
					return pumpAt("io.(*pipe).read(")
				})
			}
			close(stopping)
			var r result
			select {
			case r = <-served:
			case <-time.After(30 * time.Second):
				t.Fatal("the portal did not end with the stop")
			}
			if r.err != errStopping {
				t.Errorf("expected the portal ended by the stop, got %v", r.err)
			}
			told := connect.DecodeError(w.Result().Trailer.Get(connect.HeaderError))
			if want := stoppingText(path); told != want {
				t.Errorf("expected Rat-Error %q, got %q", want, told)
			}
		})
	}
}
