package main

import (
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"slices"
	"strings"
	"testing"
)

// proxyChildEnv is set in the environment of the process TestRelayProxy runs itself in.
const proxyChildEnv = "RAT_TEST_PROXY_CHILD"

// TestRelayProxy guards that rat tells the proxy it reaches ratd through, from the environment:
// when starting, and when a request fails without reaching ratd, the proxy refusing (403, which Go
// reports as a bare "Forbidden") rather than ratd. Its password is never told.
//
// Go reads the proxy of the environment once per process, and the MCP SDK has it read when its
// packages initialize (oauthex, v1.8.0), before any test can set it: the test runs itself in a
// process of its own, started with the proxy in its environment. The proxy answers every CONNECT
// with 403, as one refusing the address of ratd, a name only the proxy sees.
func TestRelayProxy(t *testing.T) {
	if os.Getenv(proxyChildEnv) == "" {
		proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusForbidden)
		}))
		defer proxy.Close()
		address, err := url.Parse(proxy.URL)
		if err != nil {
			t.Fatal(err)
		}
		address.User = url.UserPassword("user", "proxy-secret")
		env := slices.DeleteFunc(os.Environ(), func(variable string) bool {
			name, _, _ := strings.Cut(strings.ToUpper(variable), "=")
			return name == "HTTPS_PROXY" || name == "NO_PROXY"
		})
		child := exec.Command(os.Args[0], "-test.run=^TestRelayProxy$", "-test.count=1", "-test.v")
		child.Env = append(env, proxyChildEnv+"=1", "HTTPS_PROXY="+address.String())
		out, err := child.CombinedOutput()
		if err != nil || !strings.Contains(string(out), "--- PASS: TestRelayProxy") {
			t.Fatalf("in a process with the proxy in its environment: %v\n%s", err, out)
		}
		return
	}
	proxy, err := url.Parse(os.Getenv("HTTPS_PROXY"))
	if err != nil {
		t.Fatal(err)
	}
	p := startRat(t, newBundle(t), "ratd.invalid:7281", slog.LevelInfo)
	code, message, _ := errorOf(t, p.exchange(t, ping))
	if expected := "ratd unreachable at ratd.invalid:7281 through the proxy " + proxy.Redacted() +
		" (from the environment): Forbidden"; code != codeRelayFailed || message != expected {
		t.Errorf("expected error %d %q, got %d %q", codeRelayFailed, expected, code, message)
	}
	if !strings.Contains(p.logs.String(), "proxy="+proxy.Redacted()) {
		t.Errorf("expected the proxy logged when starting:\n%s", p.logs)
	}
	if strings.Contains(message+p.logs.String(), "proxy-secret") {
		t.Errorf("the password of the proxy was told:\n%s\n%s", message, p.logs)
	}
}
