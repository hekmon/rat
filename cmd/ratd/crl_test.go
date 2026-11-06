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
