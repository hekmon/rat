package connect

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/hekmon/rat/internal/testpki"
	"github.com/hekmon/rat/mtls"
)

// TestHTTPClientRevocation guards that the client checks the CRL of its CA on every request, not
// only during the handshake of its connection: a connection kept alive to a ratd revoked since is
// not used, the request failing with mtls.ErrRevoked.
func TestHTTPClientRevocation(t *testing.T) {
	var mu sync.Mutex
	var crl []byte
	crlServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		_, _ = w.Write(crl)
	}))
	defer crlServer.Close()
	pki := testpki.NewSubCA(t, "t", crlServer.URL)
	serverSide, err := mtls.Load(mtls.ServerDir(pki.Dir))
	if err != nil {
		t.Fatal(err)
	}
	config, err := mtls.ServerConfig(serverSide)
	if err != nil {
		t.Fatal(err)
	}
	ratd := httptest.NewUnstartedServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	ratd.TLS = config
	ratd.StartTLS()
	defer ratd.Close()
	alice, err := mtls.Load(mtls.ClientDir(pki.Dir, "alice"))
	if err != nil {
		t.Fatal(err)
	}
	client, err := HTTPClient(alice)
	if err != nil {
		t.Fatal(err)
	}
	get := func() error {
		resp, err := client.Get(ratd.URL)
		if err == nil {
			_ = resp.Body.Close()
		}
		return err
	}

	// a first CRL stale in a second, then one revoking ratd
	start := time.Now()
	mu.Lock()
	crl = pki.CRLAt(start.Add(-time.Hour), start.Add(time.Second))
	mu.Unlock()
	// Fetched before any connection exists, as the client does before dialing: a CRL fetched
	// during a handshake closes the connection once idle (http.Transport.CloseIdleConnections
	// closes those becoming idle too), and the next request would dial, checking it anyway.
	if _, err = alice.CRL(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err = get(); err != nil {
		t.Fatalf("first request: %v", err)
	}
	mu.Lock()
	crl = pki.CRLAt(start, start.Add(time.Hour), "server")
	mu.Unlock()
	time.Sleep(time.Until(start.Add(1200 * time.Millisecond)))
	if err = get(); !errors.Is(err, mtls.ErrRevoked) {
		t.Errorf("ratd revoked since the handshake: expected mtls.ErrRevoked, got %v", err)
	}
}
