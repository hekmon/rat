package main

import (
	"bufio"
	"bytes"
	"context"
	"crypto/x509"
	"encoding/json"
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

	"github.com/hekmon/rat/internal/flags"
	"github.com/hekmon/rat/mtls"
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

// newBundle generates a bundle of tenant t, with client alice, and returns its directory.
func newBundle(t *testing.T) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "bundle")
	if _, err := mtls.Generate(dir, "t", []string{"alice"}); err != nil {
		t.Fatal(err)
	}
	return dir
}

// recorder serves handler, keeping each request it receives.
type recorder struct {
	handler  http.Handler
	mu       sync.Mutex
	requests []recorded
}

// recorded is a request received by the fake ratd.
type recorded struct {
	header http.Header
	body   []byte
}

func (r *recorder) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	body, _ := io.ReadAll(req.Body)
	r.mu.Lock()
	r.requests = append(r.requests, recorded{header: req.Header.Clone(), body: body})
	r.mu.Unlock()
	req.Body = io.NopCloser(bytes.NewReader(body))
	r.handler.ServeHTTP(w, req)
}

// last returns the last request of method received.
func (r *recorder) last(method string) recorded {
	r.mu.Lock()
	defer r.mu.Unlock()
	for i := len(r.requests) - 1; i >= 0; i-- {
		if bytes.Contains(r.requests[i].body, []byte(`"method":"`+method+`"`)) {
			return r.requests[i]
		}
	}
	return recorded{}
}

// fakeRatd serves handler over mutual TLS with the server directory of bundle, as ratd does, until
// the test ends, and returns its address. clientCA, when set, replaces the CA ratd checks clients
// with, for ratd to refuse them.
func fakeRatd(t *testing.T, bundle string, handler http.Handler, clientCA *x509.Certificate) string {
	t.Helper()
	side, err := mtls.Load(mtls.ServerDir(bundle))
	if err != nil {
		t.Fatal(err)
	}
	config, err := mtls.ServerConfig(side)
	if err != nil {
		t.Fatal(err)
	}
	if clientCA != nil {
		config.ClientCAs = x509.NewCertPool()
		config.ClientCAs.AddCert(clientCA)
	}
	config.NextProtos = []string{"http/1.1"}
	server := httptest.NewUnstartedServer(handler)
	server.TLS = config
	server.StartTLS()
	t.Cleanup(server.Close)
	return server.Listener.Addr().String()
}

// mcpServer is an MCP server as ratd serves it, canceled being closed once its blocking tool sees
// its call canceled.
type mcpServer struct {
	handler  http.Handler
	canceled chan struct{}
}

// newMCPServer returns an MCP server as ratd serves it (stateless, JSON responses), with a tool
// echo returning its text argument, and a tool block returning once its call is canceled.
func newMCPServer() *mcpServer {
	s := &mcpServer{canceled: make(chan struct{})}
	var once sync.Once
	server := mcp.NewServer(&mcp.Implementation{Name: "fake", Version: "1"}, nil)
	type echoInput struct {
		Text string `json:"text"`
	}
	mcp.AddTool(server, &mcp.Tool{Name: "echo"}, func(_ context.Context, _ *mcp.CallToolRequest, in echoInput) (*mcp.CallToolResult, any, error) {
		return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: in.Text}}}, nil, nil
	})
	mcp.AddTool(server, &mcp.Tool{Name: "block"}, func(ctx context.Context, _ *mcp.CallToolRequest, _ struct{}) (*mcp.CallToolResult, any, error) {
		<-ctx.Done()
		once.Do(func() { close(s.canceled) })
		return nil, nil, ctx.Err()
	})
	s.handler = mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return server }, &mcp.StreamableHTTPOptions{
		Stateless: true, JSONResponse: true, DisableLocalhostProtection: true, PropagateRequestCancellation: true,
	})
	return s
}

// ratProcess is rat running for a test, fed and read through pipes.
type ratProcess struct {
	stdin  *io.PipeWriter
	stdout *bufio.Reader
	// stdoutReader is the end of the pipe rat writes to, for the harness of the SDK
	stdoutReader *io.PipeReader
	logs         *logBuffer
	done         chan error
}

// startRat runs rat with the client directory of alice in bundle, relaying to the ratd at addr,
// logging from level, until the test ends.
func startRat(t *testing.T, bundle, addr string, level slog.Level) *ratProcess {
	t.Helper()
	stdinReader, stdinWriter := io.Pipe()
	stdoutReader, stdoutWriter := io.Pipe()
	p := &ratProcess{stdin: stdinWriter, stdout: bufio.NewReader(stdoutReader), stdoutReader: stdoutReader,
		logs: &logBuffer{}, done: make(chan error, 1)}
	logger := slog.New(slog.NewTextHandler(p.logs, &slog.HandlerOptions{Level: level}))
	target := flags.Target{Server: addr, Bundle: mtls.ClientDir(bundle, "alice")}
	go func() {
		p.done <- run(context.Background(), logger, target, stdinReader, stdoutWriter)
		_ = stdoutWriter.Close()
	}()
	t.Cleanup(func() {
		_ = stdinWriter.Close()
		select {
		case <-p.done:
		case <-time.After(5 * time.Second):
			t.Error("rat did not stop at the end of its input")
		}
	})
	return p
}

// exchange writes line to rat, and returns the next line it writes.
func (p *ratProcess) exchange(t *testing.T, line string) string {
	t.Helper()
	if _, err := io.WriteString(p.stdin, line+"\n"); err != nil {
		t.Fatal(err)
	}
	answer, err := p.stdout.ReadString('\n')
	if err != nil {
		t.Fatalf("no answer to %s: %v\n%s", line, err, p.logs)
	}
	return strings.TrimSuffix(answer, "\n")
}

// connectHarness connects an MCP client of the SDK to rat, speaking protocol version (its default
// when empty), and returns its session.
func connectHarness(t *testing.T, p *ratProcess, version string) *mcp.ClientSession {
	t.Helper()
	client := mcp.NewClient(&mcp.Implementation{Name: "harness", Version: "1"}, nil)
	session, err := client.Connect(context.Background(), &mcp.IOTransport{Reader: p.stdoutReader, Writer: p.stdin},
		&mcp.ClientSessionOptions{ProtocolVersion: version})
	if err != nil {
		t.Fatalf("%v\n%s", err, p.logs)
	}
	t.Cleanup(func() { _ = session.Close() })
	return session
}

// TestRelay guards that a harness reaches ratd through rat, whatever the protocol version: from
// 2026-07-28 on, each request carries its version, which rat sends with the standard headers;
// before, the version negotiated by initialize is sent with each later message.
func TestRelay(t *testing.T) {
	for _, tc := range []struct {
		version, header, method string
	}{
		{"", "2026-07-28", "tools/call"},
		{"2025-11-25", "2025-11-25", ""},
	} {
		t.Run("protocol "+tc.header, func(t *testing.T) {
			bundle := newBundle(t)
			rec := &recorder{handler: newMCPServer().handler}
			p := startRat(t, bundle, fakeRatd(t, bundle, rec, nil), slog.LevelInfo)
			session := connectHarness(t, p, tc.version)
			result, err := session.CallTool(context.Background(), &mcp.CallToolParams{Name: "echo",
				Arguments: map[string]any{"text": "relayed"}})
			if err != nil || result.IsError || result.Content[0].(*mcp.TextContent).Text != "relayed" {
				t.Fatalf("expected the call relayed, got %+v, %v\n%s", result, err, p.logs)
			}
			header := rec.last("tools/call").header
			if header.Get("Mcp-Protocol-Version") != tc.header || header.Get("Mcp-Method") != tc.method {
				t.Errorf("expected version %s and method %q, got %v", tc.header, tc.method, header)
			}
			if tc.method != "" && header.Get("Mcp-Name") != "echo" {
				t.Errorf("expected the tool named in Mcp-Name, got %v", header)
			}
			if !strings.Contains(p.logs.String(), `msg=relaying version=`) || !strings.Contains(p.logs.String(), "tenant=t session=alice") {
				t.Errorf("expected the startup logged:\n%s", p.logs)
			}
		})
	}
}

// TestRelayUntouched guards that rat relays the bytes of a message as read, whatever their form:
// it reads a message to prepare its request, never to rewrite it.
func TestRelayUntouched(t *testing.T) {
	bundle := newBundle(t)
	rec := &recorder{handler: newMCPServer().handler}
	p := startRat(t, bundle, fakeRatd(t, bundle, rec, nil), slog.LevelInfo)
	line := `{ "method" : "ping",  "id" : 7 , "jsonrpc":"2.0", "extra": {"kept":  true} }`
	if answer := p.exchange(t, line); !strings.Contains(answer, `"id":7`) {
		t.Errorf("expected the answer to ping 7, got %s", answer)
	}
	rec.mu.Lock()
	defer rec.mu.Unlock()
	if len(rec.requests) != 1 || string(rec.requests[0].body) != line {
		t.Errorf("expected the message relayed as read, got %q", rec.requests)
	}
}

// TestRelayCancel guards that a call the harness cancels is canceled in ratd: rat ends the
// request carrying it (the only cancellation a stateless server sees), and writes nothing for it.
func TestRelayCancel(t *testing.T) {
	bundle := newBundle(t)
	server := newMCPServer()
	p := startRat(t, bundle, fakeRatd(t, bundle, server.handler, nil), slog.LevelDebug)
	session := connectHarness(t, p, "")
	ctx, cancel := context.WithCancel(context.Background())
	called := make(chan error, 1)
	go func() {
		_, err := session.CallTool(ctx, &mcp.CallToolParams{Name: "block", Arguments: map[string]any{}})
		called <- err
	}()
	// the call reached ratd
	time.Sleep(200 * time.Millisecond)
	cancel()
	select {
	case <-server.canceled:
	case <-time.After(5 * time.Second):
		t.Fatalf("the call was not canceled in ratd\n%s", p.logs)
	}
	<-called
	// the next call gets its own answer, nothing having been written for the canceled one
	if result, err := session.CallTool(context.Background(), &mcp.CallToolParams{Name: "echo",
		Arguments: map[string]any{"text": "next"}}); err != nil || result.Content[0].(*mcp.TextContent).Text != "next" {
		t.Errorf("expected the next call answered, got %+v, %v", result, err)
	}
	if out := p.logs.String(); !strings.Contains(out, `msg="call canceled"`) || strings.Contains(out, "relay failed") {
		t.Errorf("expected the cancellation logged, and no failure:\n%s", out)
	}
}

// errorOf returns the code and message of the JSON-RPC error answer, and its identifier.
func errorOf(t *testing.T, answer string) (code int, message, id string) {
	t.Helper()
	var resp struct {
		ID    json.RawMessage `json:"id"`
		Error struct {
			Code    int    `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal([]byte(answer), &resp); err != nil {
		t.Fatalf("invalid answer %q: %v", answer, err)
	}
	return resp.Error.Code, resp.Error.Message, string(resp.ID)
}

// ping is a call relayed as is, answered by any MCP server.
const ping = `{"jsonrpc":"2.0","id":1,"method":"ping"}`

// TestRelayFailures guards the answers of rat to a call it could not relay, each telling why:
// ratd unreachable, ratd refusing the client, rat refusing ratd, ratd not answering in time, an
// HTTP error, an event stream; and ratd refusing the call itself, whose error is relayed as is.
func TestRelayFailures(t *testing.T) {
	bundle := newBundle(t)
	timeout := requestTimeout
	requestTimeout = 300 * time.Millisecond
	t.Cleanup(func() { requestTimeout = timeout })

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	closed := listener.Addr().String()
	_ = listener.Close()

	otherSide, err := mtls.Load(mtls.ClientDir(newBundle(t), "alice"))
	if err != nil {
		t.Fatal(err)
	}
	fixed := func(status int, contentType, body string) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", contentType)
			w.WriteHeader(status)
			_, _ = io.WriteString(w, body)
		})
	}
	// net/http only sees a client leaving once the body is read, as ratd reads it
	stuck := http.HandlerFunc(func(_ http.ResponseWriter, req *http.Request) {
		_, _ = io.Copy(io.Discard, req.Body)
		<-req.Context().Done()
	})
	refusal := `{"jsonrpc":"2.0","id":1,"error":{"code":-32020,"message":"header mismatch"}}`
	for _, tc := range []struct {
		name, addr, expected string
	}{
		{"unreachable", closed, "ratd unreachable at " + closed + ": connection refused"},
		{"client refused", fakeRatd(t, bundle, newMCPServer().handler, otherSide.CA),
			"refused the TLS handshake: remote error: tls: unknown certificate authority. Check the bundle with rat-tool check."},
		{"ratd refused", fakeRatd(t, newBundle(t), newMCPServer().handler, nil),
			"rat refused the certificate of ratd at "},
		{"timeout", fakeRatd(t, bundle, stuck, nil), "did not answer within 300ms"},
		{"HTTP error", fakeRatd(t, bundle, fixed(http.StatusBadGateway, "text/plain", "bad gateway\nmore"), nil),
			"answered HTTP 502: bad gateway"},
		{"event stream", fakeRatd(t, bundle, fixed(http.StatusOK, "text/event-stream", "data: x\n\n"), nil),
			"sent an invalid response: an event stream rather than JSON"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := startRat(t, bundle, tc.addr, slog.LevelInfo)
			code, message, id := errorOf(t, p.exchange(t, ping))
			if code != codeRelayFailed || id != "1" || !strings.Contains(message, tc.expected) {
				t.Errorf("expected error %d for id 1 containing %q, got %d for %s: %q", codeRelayFailed, tc.expected, code, id, message)
			}
			if !strings.Contains(p.logs.String(), `msg="relay failed" method=ping id=1`) {
				t.Errorf("expected the failure logged:\n%s", p.logs)
			}
		})
	}
	t.Run("call refused by ratd", func(t *testing.T) {
		p := startRat(t, bundle, fakeRatd(t, bundle, fixed(http.StatusBadRequest, "application/json", refusal), nil), slog.LevelInfo)
		if answer := p.exchange(t, ping); answer != refusal {
			t.Errorf("expected the refusal of ratd relayed, got %s", answer)
		}
	})
}

// TestRelayInvalidInput guards the answers to lines rat can not relay: not JSON, and over 16 MiB,
// after which rat reads on.
func TestRelayInvalidInput(t *testing.T) {
	bundle := newBundle(t)
	p := startRat(t, bundle, fakeRatd(t, bundle, newMCPServer().handler, nil), slog.LevelInfo)
	if code, message, id := errorOf(t, p.exchange(t, "not json")); code != -32700 || id != "null" ||
		!strings.HasPrefix(message, "invalid JSON-RPC message: ") {
		t.Errorf("not JSON: unexpected error %d for %s: %q", code, id, message)
	}
	if code, message, id := errorOf(t, p.exchange(t, strings.Repeat("x", maxMessage+1))); code != codeRelayFailed ||
		id != "null" || message != "message over 16 MiB, not relayed" {
		t.Errorf("too long: unexpected error %d for %s: %q", code, id, message)
	}
	if answer := p.exchange(t, ping); !strings.Contains(answer, `"result"`) {
		t.Errorf("expected rat to read on, got %s", answer)
	}
}

// TestRelayEndOfInput guards that rat stops at the end of its input, the harness leaving, ending
// the calls in flight rather than waiting for them.
func TestRelayEndOfInput(t *testing.T) {
	bundle := newBundle(t)
	server := newMCPServer()
	p := startRat(t, bundle, fakeRatd(t, bundle, server.handler, nil), slog.LevelInfo)
	session := connectHarness(t, p, "")
	go func() {
		_, _ = session.CallTool(context.Background(), &mcp.CallToolParams{Name: "block", Arguments: map[string]any{}})
	}()
	time.Sleep(200 * time.Millisecond)
	_ = p.stdin.Close()
	select {
	case err := <-p.done:
		if err != nil {
			t.Errorf("expected rat to stop without error, got %v", err)
		}
		p.done <- nil
	case <-time.After(5 * time.Second):
		t.Fatalf("rat did not stop\n%s", p.logs)
	}
	select {
	case <-server.canceled:
	case <-time.After(5 * time.Second):
		t.Error("the call in flight was not ended")
	}
	if !strings.Contains(p.logs.String(), `msg=stopped reason="end of input"`) {
		t.Errorf("expected the stop logged:\n%s", p.logs)
	}
}

// TestRunServerDirectory guards that rat refuses the directory of the server: a client presents a
// client certificate.
func TestRunServerDirectory(t *testing.T) {
	bundle := newBundle(t)
	err := run(context.Background(), slog.New(slog.DiscardHandler), flags.Target{Server: "h:1", Bundle: mtls.ServerDir(bundle)},
		strings.NewReader(""), io.Discard)
	if !errors.Is(err, mtls.ErrRole) {
		t.Errorf("expected mtls.ErrRole, got %v", err)
	}
}
