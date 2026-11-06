package connect

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
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

// TestHTTPClientRefusesBeforeSending guards that the client gets the CRL of its CA before
// sending each request, refusing it while it can have no valid one: the request, its body
// included (a file written), never reaches a ratd it can not check, even on a connection kept
// alive from when it could.
func TestHTTPClientRefusesBeforeSending(t *testing.T) {
	var mu sync.Mutex
	var crl []byte
	crlServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		if crl == nil {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
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
	alice, err := mtls.Load(mtls.ClientDir(pki.Dir, "alice"))
	if err != nil {
		t.Fatal(err)
	}
	bob, err := mtls.Load(mtls.ClientDir(pki.Dir, "bob"))
	if err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	crl = pki.CRL(false)
	mu.Unlock()
	if _, err = alice.CRL(context.Background()); err != nil {
		t.Fatal(err)
	}
	// the PKI down from now on: bob, holding no CRL, can have none
	mu.Lock()
	crl = nil
	mu.Unlock()
	var bodies []string
	ratd := httptest.NewUnstartedServer(http.HandlerFunc(func(_ http.ResponseWriter, req *http.Request) {
		body, _ := io.ReadAll(req.Body)
		mu.Lock()
		defer mu.Unlock()
		bodies = append(bodies, string(body))
	}))
	ratd.TLS = config
	ratd.StartTLS()
	defer ratd.Close()
	source := &switchingSource{side: alice}
	client, err := HTTPClient(source)
	if err != nil {
		t.Fatal(err)
	}
	post := func(body string) error {
		resp, err := client.Post(ratd.URL, "application/json", strings.NewReader(body))
		if err == nil {
			_, _ = io.Copy(io.Discard, resp.Body)
			_ = resp.Body.Close()
		}
		return err
	}

	// a connection checked and kept alive, then a side with no valid CRL
	if err = post("first"); err != nil {
		t.Fatalf("first request: %v", err)
	}
	source.set(bob)
	if err = post("secret"); !errors.Is(err, mtls.ErrCRL) {
		t.Errorf("no valid CRL: expected mtls.ErrCRL, got %v", err)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(bodies) != 1 {
		t.Errorf("expected ratd to receive the first request only, got %q", bodies)
	}
}
