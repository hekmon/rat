package main

import (
	"context"
	"crypto/x509"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/hekmon/rat/mtls"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// fakeRatd serves an MCP server as ratd serves it (stateless, JSON responses), over mutual TLS
// with the server directory of bundle, until the test ends, and returns its address and the count
// of tool calls it received. clientCA, when set, replaces the CA it checks clients with, for it to
// refuse them.
func fakeRatd(t *testing.T, bundle string, clientCA *x509.Certificate) (string, *atomic.Int32) {
	t.Helper()
	server := mcp.NewServer(&mcp.Implementation{Name: "ratd", Title: "rat t on host", Version: "v1.2.0"},
		&mcp.ServerOptions{Instructions: "rat gives you persistent terminals on host."})
	mcp.AddTool(server, &mcp.Tool{Name: "list_windows", Description: "List your terminals."},
		func(context.Context, *mcp.CallToolRequest, struct{}) (*mcp.CallToolResult, any, error) {
			return &mcp.CallToolResult{}, nil, nil
		})
	calls := &atomic.Int32{}
	server.AddReceivingMiddleware(func(next mcp.MethodHandler) mcp.MethodHandler {
		return func(ctx context.Context, method string, req mcp.Request) (mcp.Result, error) {
			if method == "tools/call" {
				calls.Add(1)
			}
			return next(ctx, method, req)
		}
	})
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
	httpServer := httptest.NewUnstartedServer(mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return server },
		&mcp.StreamableHTTPOptions{Stateless: true, JSONResponse: true, DisableLocalhostProtection: true}))
	httpServer.TLS = config
	httpServer.StartTLS()
	t.Cleanup(httpServer.Close)
	return httpServer.Listener.Addr().String(), calls
}

// TestCheck guards a check passing: each step, what ratd tells clients (information, protocol,
// instructions, tools), and no tool called.
func TestCheck(t *testing.T) {
	bundle := newBundle(t)
	addr, calls := fakeRatd(t, bundle, nil)
	out, err := run(t, "check", "-s", addr, "-b", mtls.ClientDir(bundle, "alice"))
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	for _, part := range []string{
		"Files: " + mtls.ClientDir(bundle, "alice") + "\n  ok    files of a side\n",
		"client alice of tenant prod, working in session alice",
		"Network\n  ok    " + addr + " answers (TCP, ",
		"TLS\n  ok    ratd serves tenant prod, and accepts this client as session alice\n",
		`MCP` + "\n" + `  ok    ratd v1.2.0, "rat t on host", protocol 2026-07-28`,
		"  instructions:\n    rat gives you persistent terminals on host.\n",
		"Tools\n  ok    1 tool\n    list_windows: List your terminals.\n",
		"ratd at " + addr + " serves this client.\n",
	} {
		if !strings.Contains(out, part) {
			t.Errorf("expected %q in:\n%s", part, out)
		}
	}
	if calls.Load() != 0 {
		t.Errorf("expected no tool called, got %d calls", calls.Load())
	}
}

// TestCheckFailures guards the check stopping at the step failing, telling why, with an error
// naming the step for scripts: a server directory, a key readable by others, ratd unreachable, a
// server of another bundle, ratd refusing the client.
func TestCheckFailures(t *testing.T) {
	bundle := newBundle(t)
	alice := mtls.ClientDir(bundle, "alice")
	addr, _ := fakeRatd(t, bundle, nil)
	otherAddr, _ := fakeRatd(t, newBundle(t), nil)
	otherSide, err := mtls.Load(mtls.ClientDir(newBundle(t), "alice"))
	if err != nil {
		t.Fatal(err)
	}
	refusingAddr, _ := fakeRatd(t, bundle, otherSide.CA)
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	closed := listener.Addr().String()
	_ = listener.Close()
	readable := newBundle(t)
	if err = os.Chmod(filepath.Join(mtls.ClientDir(readable, "alice"), "client.key"), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name, addr, dir, step, expected string
	}{
		{"server directory", addr, mtls.ServerDir(bundle), "Files", "  FAIL  role: the directory of the server, not of a client"},
		{"key readable by others", addr, mtls.ClientDir(readable, "alice"), "Files", "  FAIL  key readable by its owner only: "},
		{"unreachable", closed, alice, "Network", "  FAIL  " + closed + " does not answer: connection refused"},
		{"server of another bundle", otherAddr, alice, "TLS", "  FAIL  this client refused the server: "},
		{"client refused", refusingAddr, alice, "TLS", "  FAIL  ratd refused this client: its certificate is from " +
			"another bundle of the tenant (remote error: tls: unknown certificate authority)"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			out, err := run(t, "check", "-s", tc.addr, "-b", tc.dir)
			if err == nil || err.Error() != "check failed: "+tc.step || !strings.Contains(out, tc.expected) {
				t.Errorf("expected a failure at %s telling %q, got %v:\n%s", tc.step, tc.expected, err, out)
			}
			if strings.Contains(out, "serves this client") {
				t.Errorf("expected the check to stop:\n%s", out)
			}
		})
	}
}
