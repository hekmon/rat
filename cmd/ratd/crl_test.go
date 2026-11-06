package main

import (
	"context"
	"crypto/tls"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/hekmon/rat/connect"
	"github.com/hekmon/rat/internal/testpki"
	"github.com/hekmon/rat/mtls"
)

// TestRunCRL guards that ratd refuses to start, before its terminals, when its certificate names
// a CRL that can not be had (starting would let in the certificates it revokes), or that revokes
// it, and goes on with a valid CRL not revoking it.
func TestRunCRL(t *testing.T) {
	var body []byte
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write(body)
	}))
	defer server.Close()
	bundle, crl := testpki.SubCABundle(t, "test-ratd-crl", server.URL+"/t.crl")
	dir := mtls.ServerDir(bundle)
	errListen := errors.New("listening")
	notListening := func() (net.Listener, error) { return nil, errListen }

	body = crl(false)
	if err := run(context.Background(), slog.New(slog.DiscardHandler), dir, defaultReadBudget, notListening); !errors.Is(err, errListen) {
		t.Errorf("valid CRL: expected ratd to start up to listening, got %v", err)
	}

	body = crl(false, "server")
	if err := run(context.Background(), slog.New(slog.DiscardHandler), dir, defaultReadBudget, notListening); !errors.Is(err, mtls.ErrRevoked) {
		t.Errorf("revoked: expected mtls.ErrRevoked, got %v", err)
	}
	body = crl(true)
	if err := run(context.Background(), slog.New(slog.DiscardHandler), dir, defaultReadBudget, notListening); !errors.Is(err, mtls.ErrCRL) {
		t.Errorf("stale CRL: expected mtls.ErrCRL, got %v", err)
	}
	server.Close()
	if err := run(context.Background(), slog.New(slog.DiscardHandler), dir, defaultReadBudget, notListening); !errors.Is(err, mtls.ErrCRL) {
		t.Errorf("CRL server down: expected mtls.ErrCRL, got %v", err)
	}
}

// TestServeRevoked guards that ratd checks the CRL of the CA on every request, the body telling
// why it refuses one: 403 for a client the CRL revokes, while serving the others, and 503 for
// every client while ratd can have no valid CRL, logging each refusal. A revoked client whose
// certificate names the CRL learns it on its side, before ratd answers.
func TestServeRevoked(t *testing.T) {
	var body []byte
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write(body)
	}))
	defer server.Close()
	bundle, crl := testpki.SubCABundle(t, "t", server.URL+"/t.crl")
	body = crl(false, "bob", "carol")
	addr, logs := startRatd(t, bundle, slog.LevelInfo)
	// carol presents her certificate through a client checking nothing of the server, as any
	// client may: rat would refuse ratd, whose certificate names a CRL hers does not
	carol := uncheckingClient(t, mtls.ClientDir(bundle, "carol"))
	expect := func(client string, status int, message string) {
		t.Helper()
		c := httpClient(t, bundle, client)
		if client == "carol" {
			c = carol
		}
		resp, err := postBody(t, c, addr)
		if err != nil || resp.status != status || !strings.Contains(resp.body, message) {
			t.Errorf("%s: expected %d telling %q, got %+v, %v", client, status, message, resp, err)
		}
	}
	expect("alice", http.StatusOK, "")
	// carol checks nothing: ratd refuses her
	expect("carol", http.StatusForbidden, "certificate revoked")
	if !strings.Contains(logs.String(), `msg="request refused" session=carol`) {
		t.Errorf("expected the refusal logged with its session:\n%s", logs)
	}
	if _, err := postBody(t, httpClient(t, bundle, "bob"), addr); !errors.Is(err, mtls.ErrRevoked) ||
		!strings.Contains(err.Error(), "revokes the certificate of bob") {
		t.Errorf("bob: expected his client to refuse, telling he is revoked, got %v", err)
	}

	// a new ratd fetching only stale CRLs
	body = crl(true)
	addr, _ = startRatd(t, bundle, slog.LevelInfo)
	expect("carol", http.StatusServiceUnavailable, "no valid CRL of the CA")
}

// TestServeRevokedLater guards that ratd checks revocation on the requests after its startup,
// not only when it starts: the CRL it fetched then goes stale, the next request fetches a new one,
// which revokes the client from then on. The client checks nothing of the server, so only ratd can
// refuse it.
func TestServeRevokedLater(t *testing.T) {
	var mu sync.Mutex
	var body []byte
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		_, _ = w.Write(body)
	}))
	defer server.Close()
	pki := testpki.NewSubCA(t, "test-ratd-revoked-later", server.URL+"/t.crl")
	serve := func(crl []byte) {
		mu.Lock()
		defer mu.Unlock()
		body = crl
	}
	start := time.Now()
	// valid for the first request only: ratd fetches it again once stale
	serve(pki.CRLAt(start.Add(-time.Hour), start.Add(3*time.Second)))
	addr, _ := startRatd(t, pki.Dir, slog.LevelInfo)
	carol := uncheckingClient(t, mtls.ClientDir(pki.Dir, "carol"))
	if resp, err := postBody(t, carol, addr); err != nil || resp.status != http.StatusOK {
		t.Fatalf("before the revocation: expected 200, got %+v, %v", resp, err)
	}
	serve(pki.CRLAt(start.Add(-time.Minute), start.Add(time.Hour), "carol"))
	time.Sleep(time.Until(start.Add(3200 * time.Millisecond)))
	resp, err := postBody(t, carol, addr)
	if err != nil || resp.status != http.StatusForbidden || !strings.Contains(resp.body, "certificate revoked") {
		t.Errorf("after the revocation: expected 403 telling the certificate revoked, got %+v, %v", resp, err)
	}
}

// response is the status and the body of a response.
type response struct {
	status int
	body   string
}

// postBody sends an initialize request to ratd at addr with client, and returns the response.
func postBody(t *testing.T, client *http.Client, addr string) (response, error) {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, "https://"+addr+connect.Path, strings.NewReader(initialize))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	resp, err := client.Do(req)
	if err != nil {
		return response{}, err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(resp.Body)
	return response{resp.StatusCode, string(data)}, err
}

// uncheckingClient returns an HTTP client presenting the client certificate of dir, and checking
// nothing of the server: to see what ratd answers a client that does not check, as rat does.
func uncheckingClient(t *testing.T, dir string) *http.Client {
	t.Helper()
	certificate, err := tls.LoadX509KeyPair(filepath.Join(dir, "client.crt"), filepath.Join(dir, "client.key"))
	if err != nil {
		t.Fatal(err)
	}
	config := &tls.Config{Certificates: []tls.Certificate{certificate}, InsecureSkipVerify: true, MinVersion: tls.VersionTLS13}
	return &http.Client{Transport: &http.Transport{TLSClientConfig: config}, Timeout: 5 * time.Second}
}

// TestRunRenewal guards that ratd, as run starts it, presents its certificate renewed while it
// serves at the next handshake, without a restart, a new key logged as a warning.
func TestRunRenewal(t *testing.T) {
	pki := testpki.NewSubCA(t, "test-ratd-renew", "")
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	logs := &logBuffer{}
	ctx, cancel := context.WithCancel(context.Background())
	ran := make(chan error, 1)
	go func() {
		ran <- run(ctx, slog.New(slog.NewTextHandler(logs, nil)), mtls.ServerDir(pki.Dir), defaultReadBudget,
			func() (net.Listener, error) { return listener, nil })
	}()
	defer func() {
		cancel()
		if err := <-ran; err != nil {
			t.Error(err)
		}
	}()
	carol, err := tls.LoadX509KeyPair(filepath.Join(mtls.ClientDir(pki.Dir, "carol"), "client.crt"),
		filepath.Join(mtls.ClientDir(pki.Dir, "carol"), "client.key"))
	if err != nil {
		t.Fatal(err)
	}
	// the serial of the certificate ratd presents, once it serves
	presented := func() string {
		t.Helper()
		conn, err := tls.Dial("tcp", listener.Addr().String(), &tls.Config{InsecureSkipVerify: true,
			Certificates: []tls.Certificate{carol}, MinVersion: tls.VersionTLS13})
		if err != nil {
			t.Fatal(err)
		}
		defer conn.Close()
		return conn.ConnectionState().PeerCertificates[0].SerialNumber.String()
	}
	for deadline := time.Now().Add(15 * time.Second); !strings.Contains(logs.String(), "msg=serving"); time.Sleep(10 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatalf("ratd did not serve:\n%s", logs)
		}
	}
	first := presented()
	renewed := pki.Renew("server", true).String()
	// the files are looked at once a second at most
	time.Sleep(1100 * time.Millisecond)
	if second := presented(); second == first || second != renewed {
		t.Errorf("expected the renewed certificate %s presented, after %s, got %s", renewed, first, second)
	}
	if !strings.Contains(logs.String(), `msg="server certificate renewed with a new key"`) {
		t.Errorf("expected the renewal with a new key logged as a warning:\n%s", logs)
	}
}

// TestServeExpired guards that ratd refuses a client whose certificate expired since the
// handshake of its connection, kept alive: 403 at its next request, telling why. A new handshake
// would be refused by TLS, the 403 telling the check of the request did it.
func TestServeExpired(t *testing.T) {
	pki := testpki.NewSubCA(t, "t", "")
	expiry := time.Now().Add(time.Second)
	pki.Expiring("carol", expiry)
	addr, logs := startRatd(t, pki.Dir, slog.LevelInfo)
	client := httpClient(t, pki.Dir, "carol")
	if resp, err := postBody(t, client, addr); err != nil || resp.status != http.StatusOK {
		t.Fatalf("before the expiry: expected 200, got %+v, %v", resp, err)
	}
	time.Sleep(time.Until(expiry.Add(200 * time.Millisecond)))
	resp, err := postBody(t, client, addr)
	if err != nil || resp.status != http.StatusForbidden || !strings.Contains(resp.body, "the certificate of carol expired on") {
		t.Errorf("after the expiry: expected 403 telling it expired, got %+v, %v", resp, err)
	}
	if !strings.Contains(logs.String(), `msg="request refused" session=carol`) {
		t.Errorf("expected the refusal logged:\n%s", logs)
	}
}
