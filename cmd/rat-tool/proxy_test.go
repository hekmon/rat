package main

import (
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"slices"
	"strings"
	"testing"

	"github.com/hekmon/rat/mtls"
)

// proxyChildEnv is set in the environment of the process TestCheckProxy runs itself in, to the
// bundle of the ratd its proxy tunnels to.
const proxyChildEnv = "RAT_TEST_CHECK_PROXY_BUNDLE"

// tunnel serves a proxy tunneling every CONNECT to the ratd at addr, until the test ends, but for
// the host refused.invalid, answered 403, as a proxy refusing the address of ratd. Both are names
// only the proxy sees.
func tunnel(t *testing.T, addr string) *httptest.Server {
	t.Helper()
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if req.Method != http.MethodConnect || strings.HasPrefix(req.Host, "refused.invalid:") {
			w.WriteHeader(http.StatusForbidden)
			return
		}
		upstream, err := net.Dial("tcp", addr)
		if err != nil {
			w.WriteHeader(http.StatusBadGateway)
			return
		}
		defer upstream.Close()
		conn, buffered, err := w.(http.Hijacker).Hijack()
		if err != nil {
			return
		}
		defer conn.Close()
		if _, err = io.WriteString(conn, "HTTP/1.1 200 Connection established\r\n\r\n"); err != nil {
			return
		}
		go func() { _, _ = io.Copy(upstream, buffered) }()
		_, _ = io.Copy(conn, upstream)
	}))
	t.Cleanup(proxy.Close)
	return proxy
}

// TestCheckProxy guards that the check reaches ratd through the proxy of the environment, as rat
// does, every step included: ratd reached through it passes, the network step telling the proxy;
// a proxy refusing ratd (403, which Go reports as a bare "Forbidden") fails the network step,
// naming the proxy. Its password is never told.
//
// Go reads the proxy of the environment once per process, and the MCP SDK has it read when its
// packages initialize (oauthex, v1.8.0), before any test can set it: the test runs itself in a
// process of its own, started with the proxy in its environment.
func TestCheckProxy(t *testing.T) {
	bundle := os.Getenv(proxyChildEnv)
	if bundle == "" {
		bundle = newBundle(t)
		addr, _, _ := fakeRatd(t, bundle, nil)
		address, err := url.Parse(tunnel(t, addr).URL)
		if err != nil {
			t.Fatal(err)
		}
		address.User = url.UserPassword("user", "proxy-secret")
		env := slices.DeleteFunc(os.Environ(), func(variable string) bool {
			name, _, _ := strings.Cut(strings.ToUpper(variable), "=")
			return name == "HTTPS_PROXY" || name == "NO_PROXY"
		})
		child := exec.Command(os.Args[0], "-test.run=^TestCheckProxy$", "-test.count=1", "-test.v")
		child.Env = append(env, proxyChildEnv+"="+bundle, "HTTPS_PROXY="+address.String())
		out, err := child.CombinedOutput()
		if err != nil || !strings.Contains(string(out), "--- PASS: TestCheckProxy") {
			t.Fatalf("in a process with the proxy in its environment: %v\n%s", err, out)
		}
		return
	}
	proxy, err := url.Parse(os.Getenv("HTTPS_PROXY"))
	if err != nil {
		t.Fatal(err)
	}
	alice := mtls.ClientDir(bundle, "alice")
	out, err := run(t, "check", "-s", "ratd.invalid:7281", "-b", alice)
	if err != nil || !strings.Contains(out, "Network\n  ok    ratd.invalid:7281 answers through the proxy "+proxy.Redacted()+" (") ||
		!strings.Contains(out, "ratd at ratd.invalid:7281 serves this client.") {
		t.Errorf("expected ratd reached through the proxy, got %v:\n%s", err, out)
	}
	refused, err := run(t, "check", "-s", "refused.invalid:7281", "-b", alice)
	if expected := "Network\n  FAIL  refused.invalid:7281 does not answer through the proxy " + proxy.Redacted() + ": Forbidden\n"; err == nil ||
		err.Error() != "check failed: Network" || !strings.Contains(refused, expected) {
		t.Errorf("expected a failure at Network telling %q, got %v:\n%s", expected, err, refused)
	}
	if strings.Contains(out+refused, "proxy-secret") {
		t.Errorf("the password of the proxy was told:\n%s\n%s", out, refused)
	}
}
