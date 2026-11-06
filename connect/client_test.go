package connect

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/hekmon/rat/internal/testpki"
	"github.com/hekmon/rat/mtls"
)

// switchingSource returns the side the test sets, and accepts CRL events without telling them: the
// sides it returns hold their CRL already.
type switchingSource struct {
	mu   sync.Mutex
	side *mtls.Side
}

func (s *switchingSource) Side() *mtls.Side {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.side
}

func (s *switchingSource) set(side *mtls.Side) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.side = side
}

func (s *switchingSource) OnCRL(mtls.CRLEvents) {}

// TestHTTPClientRevocation guards that the client checks the certificate of ratd after every
// response, not only during the handshake: a server revoked once the response was served is
// refused, the request failing with mtls.ErrRevoked, even though its handshake accepted it. A
// connection kept alive outlives the handshake of its establishment by hours.
func TestHTTPClientRevocation(t *testing.T) {
	var mu sync.Mutex
	var body []byte
	crlServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		_, _ = w.Write(body)
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
	alice, err := mtls.Load(mtls.ClientDir(pki.Dir, "alice"))
	if err != nil {
		t.Fatal(err)
	}
	bob, err := mtls.Load(mtls.ClientDir(pki.Dir, "bob"))
	if err != nil {
		t.Fatal(err)
	}
	// both sides hold their CRL before the request: no fetch happens while it is served, which
	// would close the connection and check ratd during a new handshake
	serve := func(crl []byte) {
		mu.Lock()
		defer mu.Unlock()
		body = crl
	}
	serve(pki.CRL(false))
	if _, err = alice.CRL(context.Background()); err != nil {
		t.Fatal(err)
	}
	serve(pki.CRL(false, "server"))
	if _, err = bob.CRL(context.Background()); err != nil {
		t.Fatal(err)
	}
	source := &switchingSource{side: alice}
	ratd := httptest.NewUnstartedServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		// what the client checks changes once its handshake is over: only the check after the
		// response can see ratd revoked
		source.set(bob)
	}))
	ratd.TLS = config
	ratd.StartTLS()
	defer ratd.Close()
	client, err := HTTPClient(source)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := client.Get(ratd.URL)
	if resp != nil {
		_ = resp.Body.Close()
	}
	if !errors.Is(err, mtls.ErrRevoked) {
		t.Errorf("ratd revoked once the response was served: expected mtls.ErrRevoked, got %v", err)
	}
}
