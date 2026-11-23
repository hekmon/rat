package connect

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

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

func (s *switchingSource) OnWatch(mtls.WatchEvents) {}

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

// TestHTTPClientWaitsForGoAhead guards that the client sends the body of an upload only once ratd
// gave its go-ahead, however long ratd takes to look at its path (a file system slow to answer):
// past Go's default wait of a second, the body would leave unasked, a refusal then coming once
// bytes left, and a file written in place emptied by bytes whose go-ahead may never reach the
// client. Behind a look of a second and a half, a refusal comes before any byte is read, and an
// upload goes on once ratd reads it.
func TestHTTPClientWaitsForGoAhead(t *testing.T) {
	bundle := filepath.Join(t.TempDir(), "bundle")
	if _, err := mtls.Generate(bundle, "t", []string{"alice"}); err != nil {
		t.Fatal(err)
	}
	serverSide, err := mtls.Load(mtls.ServerDir(bundle))
	if err != nil {
		t.Fatal(err)
	}
	config, err := mtls.ServerConfig(serverSide)
	if err != nil {
		t.Fatal(err)
	}
	ratd := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		// slower than Go's default wait for the go-ahead
		time.Sleep(1500 * time.Millisecond)
		w.Header().Set(HeaderVersion, "test")
		if req.Header.Get(HeaderPath) == "/refused" {
			http.Error(w, "/refused can not be written.", http.StatusForbidden)
			return
		}
		// reading the body sends the go-ahead
		_, _ = io.Copy(io.Discard, req.Body)
		w.Header().Set(HeaderMode, "0644")
		w.WriteHeader(http.StatusCreated)
	}))
	ratd.TLS = config
	ratd.StartTLS()
	defer ratd.Close()
	alice, err := mtls.Load(mtls.ClientDir(bundle, "alice"))
	if err != nil {
		t.Fatal(err)
	}
	client, err := HTTPClient(alice)
	if err != nil {
		t.Fatal(err)
	}
	server := ratd.Listener.Addr().String()
	file := &testFile{content: bytes.Repeat([]byte("x"), 1<<20), mode: 0o644}
	_, err = Upload(context.Background(), nil, client, server, file, "/refused", UploadOptions{})
	var refused *Refusal
	if !errors.As(err, &refused) || refused.Status != http.StatusForbidden {
		t.Errorf("expected the refusal, got %v", err)
	}
	if taken := file.taken(); taken != 0 {
		t.Errorf("a refusal behind a slow look: expected it before any byte was read, %d were", taken)
	}
	file = &testFile{content: bytes.Repeat([]byte("x"), 1<<20), mode: 0o644}
	if up, err := Upload(context.Background(), nil, client, server, file, "/created", UploadOptions{}); err != nil ||
		up.Sent != 1<<20 {
		t.Errorf("an upload behind a slow look: expected it sent whole, got %+v, %v", up, err)
	}
}

// TestHTTPClientRenewal guards that a client whose certificate is renewed presents the renewed one
// at its next request, rather than the one replaced on a connection kept alive, which a client in
// constant use keeps for hours, past the expiry of the certificate it presented.
func TestHTTPClientRenewal(t *testing.T) {
	pki := testpki.NewSubCA(t, "t", "")
	serverSide, err := mtls.Load(mtls.ServerDir(pki.Dir))
	if err != nil {
		t.Fatal(err)
	}
	config, err := mtls.ServerConfig(serverSide)
	if err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	var presented []string
	ratd := httptest.NewUnstartedServer(http.HandlerFunc(func(_ http.ResponseWriter, req *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		presented = append(presented, req.TLS.PeerCertificates[0].SerialNumber.String())
	}))
	ratd.TLS = config
	ratd.StartTLS()
	defer ratd.Close()
	watcher, err := mtls.Watch(mtls.ClientDir(pki.Dir, "carol"))
	if err != nil {
		t.Fatal(err)
	}
	client, err := HTTPClient(watcher)
	if err != nil {
		t.Fatal(err)
	}
	get := func() {
		t.Helper()
		resp, err := client.Get(ratd.URL)
		if err != nil {
			t.Fatal(err)
		}
		_, _ = io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()
	}
	first := watcher.Side().Certificate.Leaf.SerialNumber.String()
	get()
	renewed := pki.Renew("carol", false).String()
	// the files are looked at once a second at most
	time.Sleep(1100 * time.Millisecond)
	get()
	mu.Lock()
	defer mu.Unlock()
	if len(presented) != 2 || presented[0] != first || presented[1] != renewed {
		t.Errorf("expected serial %s, then %s, got %v", first, renewed, presented)
	}
}

// TestHTTPClientExpired guards that the client checks the certificate of ratd after every
// response for its validity too: a ratd whose certificate expired since the handshake, on a
// connection kept alive, is refused, the request failing with mtls.ErrValidity.
func TestHTTPClientExpired(t *testing.T) {
	pki := testpki.NewSubCA(t, "t", "")
	// a whole second, as X.509 encodes it (see testpki.Expiring), a second away at least
	expiry := time.Now().Truncate(time.Second).Add(2 * time.Second)
	pki.Expiring("server", expiry)
	serverSide, err := mtls.Load(mtls.ServerDir(pki.Dir))
	if err != nil {
		t.Fatal(err)
	}
	config, err := mtls.ServerConfig(serverSide)
	if err != nil {
		t.Fatal(err)
	}
	ratd := httptest.NewUnstartedServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		// answered once the certificate its handshake accepted expired
		time.Sleep(time.Until(expiry.Add(200 * time.Millisecond)))
	}))
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
	resp, err := client.Get(ratd.URL)
	if resp != nil {
		_ = resp.Body.Close()
	}
	if !errors.Is(err, mtls.ErrValidity) || !strings.Contains(err.Error(), "the certificate of t expired on") {
		t.Errorf("ratd expired since the handshake: expected mtls.ErrValidity, got %v", err)
	}
}
