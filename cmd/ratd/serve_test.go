package main

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/hekmon/rat/connect"
	"github.com/hekmon/rat/mtls"
	"github.com/hekmon/rat/tmux"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// logBuffer collects logs written concurrently.
type logBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *logBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *logBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// newBundle generates the bundle of tenant, with client alice, and returns its directory. Tests
// starting a tmux server use a tenant of their own, named test-ratd-*: a tenant is a tmux socket.
func newBundle(t *testing.T, tenant string) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "bundle")
	if _, err := mtls.Generate(dir, tenant, []string{"alice"}); err != nil {
		t.Fatal(err)
	}
	return dir
}

// startRatd serves the tenant of bundle on a loopback port until the test ends, logging from
// level, and returns its address and its logs.
func startRatd(t *testing.T, bundle string, level slog.Level) (string, *logBuffer) {
	t.Helper()
	side, err := mtls.Load(mtls.ServerDir(bundle))
	if err != nil {
		t.Fatal(err)
	}
	logs := &logBuffer{}
	// the terminals are not started: these tests call no tool
	controller, err := tmux.New(side.Tenant)
	if err != nil {
		t.Fatal(err)
	}
	d, err := newDaemon(slog.New(slog.NewTextHandler(logs, &slog.HandlerOptions{Level: level})), side, controller, false, defaultReadBudget)
	if err != nil {
		t.Fatal(err)
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	served := make(chan error, 1)
	go func() { served <- d.serve(ctx, listener) }()
	t.Cleanup(func() {
		cancel()
		if err := <-served; err != nil {
			t.Error(err)
		}
	})
	return listener.Addr().String(), logs
}

// httpClient returns an HTTP client presenting the certificate of client in bundle, offering
// HTTP/2 as http.Transport does by default.
func httpClient(t *testing.T, bundle, client string) *http.Client {
	t.Helper()
	side, err := mtls.Load(mtls.ClientDir(bundle, client))
	if err != nil {
		t.Fatal(err)
	}
	config, err := mtls.ClientConfig(side)
	if err != nil {
		t.Fatal(err)
	}
	return &http.Client{Transport: &http.Transport{TLSClientConfig: config, ForceAttemptHTTP2: true}, Timeout: 5 * time.Second}
}

// initialize is the body of an initialize request, sent by hand.
const initialize = `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18",` +
	`"capabilities":{},"clientInfo":{"name":"test","version":"1"}}}`

// post sends an initialize request to ratd at addr, with the host header host, and returns the
// response.
func post(t *testing.T, client *http.Client, addr, host, path string) (*http.Response, error) {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, "https://"+addr+path, strings.NewReader(initialize))
	if err != nil {
		t.Fatal(err)
	}
	req.Host = host
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	resp, err := client.Do(req)
	if err == nil {
		_, _ = io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()
	}
	return resp, err
}

// TestServe guards that a standard MCP client, the one of the Go SDK, presenting a client
// certificate of the bundle, connects: it gets the instructions naming the host, and ratd serves
// its requests in the session its certificate names, whatever the protocol version (the SDK
// discovers the server, 2026-07-28; older clients initialize). The client opens its optional
// standalone stream, which a stateless ratd refuses (405): the client carries on.
func TestServe(t *testing.T) {
	bundle := newBundle(t, "t")
	addr, logs := startRatd(t, bundle, slog.LevelDebug)
	client := mcp.NewClient(&mcp.Implementation{Name: "test", Version: "1"}, nil)
	session, err := client.Connect(context.Background(), &mcp.StreamableClientTransport{
		Endpoint:   "https://" + addr + connect.Path,
		HTTPClient: httpClient(t, bundle, "alice"),
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer session.Close()
	host, err := os.Hostname()
	if err != nil {
		t.Fatal(err)
	}
	result := session.InitializeResult()
	if result.ServerInfo.Title != "rat t on "+host || !strings.Contains(result.Instructions, "terminals on "+host+":") {
		t.Errorf("expected the tenant and the host in the title, the host in the instructions, got %q and %q",
			result.ServerInfo.Title, result.Instructions)
	}
	if resp, err := post(t, httpClient(t, bundle, "alice"), addr, addr, connect.Path); err != nil || resp.StatusCode != http.StatusOK {
		t.Fatalf("initialize: expected 200, got %v, %v", resp, err)
	}
	for _, method := range []string{"server/discover", "initialize"} {
		if !strings.Contains(logs.String(), "msg=request session=alice method="+method) {
			t.Errorf("expected %s to be served in session alice:\n%s", method, logs)
		}
	}
}

// TestServeLogs guards that the logs of the SDK, a few lines at info level for each stateless
// request, are only kept from warnings on, unless ratd logs at debug level.
func TestServeLogs(t *testing.T) {
	bundle := newBundle(t, "t")
	for _, level := range []slog.Level{slog.LevelInfo, slog.LevelDebug} {
		addr, logs := startRatd(t, bundle, level)
		if resp, err := post(t, httpClient(t, bundle, "alice"), addr, addr, connect.Path); err != nil || resp.StatusCode != http.StatusOK {
			t.Fatalf("initialize: expected 200, got %v, %v", resp, err)
		}
		if logged := strings.Contains(logs.String(), "server session connected"); logged != (level == slog.LevelDebug) {
			t.Errorf("at level %s, the SDK logs: expected %v, got:\n%s", level, level == slog.LevelDebug, logs)
		}
	}
}

// TestServeHosts guards that any host name reaches ratd, the DNS rebinding protection of the SDK
// being off: on a loopback address, it refuses any other host name than localhost, such as the
// alias of an ssh tunnel.
func TestServeHosts(t *testing.T) {
	bundle := newBundle(t, "t")
	addr, _ := startRatd(t, bundle, slog.LevelInfo)
	client := httpClient(t, bundle, "alice")
	for _, host := range []string{addr, "localhost", "tunnel-alias:7281"} {
		if resp, err := post(t, client, addr, host, connect.Path); err != nil || resp.StatusCode != http.StatusOK {
			t.Errorf("host %s: expected 200, got %v, %v", host, resp, err)
		}
	}
}

// TestServeRefuses guards the door: plain HTTP, TLS without a client certificate or with one of
// another bundle, and other paths are refused, refused handshakes are logged, and HTTP/2 is not
// negotiated.
func TestServeRefuses(t *testing.T) {
	bundle := newBundle(t, "t")
	addr, logs := startRatd(t, bundle, slog.LevelInfo)

	// http.Server.TLSConfig alone would be ignored by Serve, which would then serve plain HTTP: the
	// refusal must come from the TLS listener, not from getServer finding no certificate
	resp, err := http.Post("http://"+addr+connect.Path, "application/json", strings.NewReader(initialize))
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest || !strings.Contains(string(body), "HTTP request to an HTTPS server") {
		t.Errorf("plain HTTP: expected the TLS listener to refuse it, got %s: %s", resp.Status, body)
	}

	noCertificate := &http.Client{Transport: &http.Transport{TLSClientConfig: &tls.Config{InsecureSkipVerify: true}}}
	if resp, err := post(t, noCertificate, addr, addr, connect.Path); err == nil {
		t.Errorf("no client certificate: expected an error, got %s", resp.Status)
	}
	if resp, err := post(t, httpClient(t, newBundle(t, "t"), "alice"), addr, addr, connect.Path); err == nil {
		t.Errorf("client of another bundle: expected an error, got %s", resp.Status)
	}
	if !strings.Contains(logs.String(), `msg="TLS handshake refused"`) {
		t.Errorf("expected the refused handshakes to be logged:\n%s", logs)
	}

	client := httpClient(t, bundle, "alice")
	if resp, err := post(t, client, addr, addr, "/other"); err != nil || resp.StatusCode != http.StatusNotFound {
		t.Errorf("another path: expected 404, got %v, %v", resp, err)
	}
	if resp, err := post(t, client, addr, addr, connect.Path); err != nil || resp.ProtoMajor != 1 {
		t.Errorf("expected HTTP/1.1, got %v, %v", resp, err)
	}
}

// TestRunClientDirectory guards that ratd refuses to start with the directory of a client: a
// client can not run a server with its certificate.
func TestRunClientDirectory(t *testing.T) {
	bundle := newBundle(t, "t")
	err := run(context.Background(), slog.New(slog.DiscardHandler), mtls.ClientDir(bundle, "alice"), defaultReadBudget,
		func() (net.Listener, error) { return nil, errors.New("not expected to listen") })
	if !errors.Is(err, mtls.ErrRole) {
		t.Errorf("expected mtls.ErrRole, got %v", err)
	}
}

// TestErrorLog guards the logs of net/http: refused handshakes at most once per interval, with the
// count of the others, and other errors always.
func TestErrorLog(t *testing.T) {
	logs := &logBuffer{}
	errors := &errorLog{logger: slog.New(slog.NewTextHandler(logs, nil))}
	for range 3 {
		_, _ = errors.Write([]byte(handshakePrefix + "[::1]:1234: remote error: tls: bad certificate\n"))
	}
	_, _ = errors.Write([]byte("http: panic serving\n"))
	out := logs.String()
	if strings.Count(out, "TLS handshake refused") != 1 || !strings.Contains(out, `remote=[::1]:1234 reason="remote error: tls: bad certificate"`) {
		t.Errorf("expected one refused handshake logged, with its address and reason:\n%s", out)
	}
	if !strings.Contains(out, `msg="http server" error="http: panic serving"`) {
		t.Errorf("expected the other error to be logged:\n%s", out)
	}
	// the interval elapsed: the next refusal is logged, with the count of the suppressed ones
	errors.lastHandshake = time.Now().Add(-handshakeLogInterval)
	_, _ = errors.Write([]byte(handshakePrefix + "[::1]:1234: EOF\n"))
	if !strings.Contains(logs.String(), "suppressed=2") {
		t.Errorf("expected the suppressed refusals to be counted:\n%s", logs)
	}
}

// TestWarnExpiry guards that ratd warns of a bundle expiring within a year, and only then.
func TestWarnExpiry(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // a single check
	for _, tc := range []struct {
		left time.Duration
		warn bool
	}{{100 * 24 * time.Hour, true}, {2 * mtls.ExpiryWarning, false}} {
		expiry := &x509.Certificate{NotAfter: time.Now().Add(tc.left)}
		side := &mtls.Side{CA: expiry, Certificate: tls.Certificate{Leaf: expiry}}
		logs := &logBuffer{}
		warnExpiry(ctx, slog.New(slog.NewTextHandler(logs, nil)), side)
		if warned := strings.Contains(logs.String(), "expires within a year"); warned != tc.warn {
			t.Errorf("%s left: expected a warning %v, got:\n%s", tc.left, tc.warn, logs)
		}
	}
}
