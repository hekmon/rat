package connect

import (
	"errors"
	"net/http"

	"github.com/hekmon/rat/mtls"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// HTTPClient returns an HTTP client presenting the certificate of side, a client directory of a
// bundle, and checking ratd as package mtls does, the CRL of its CA on every request (see
// revocationTransport). A Watcher as side has its renewed certificate presented from the next
// request on. Its transport is its own, cloned from the default one: setting the
// configuration on http.DefaultTransport would change a global of the process, and a bare
// http.Transport loses the defaults (proxy from the environment, dial and handshake timeouts). It
// has no timeout: callers bound each request.
func HTTPClient(side mtls.Source) (*http.Client, error) {
	config, err := mtls.ClientConfig(side)
	if err != nil {
		return nil, err
	}
	defaultTransport, ok := http.DefaultTransport.(*http.Transport)
	if !ok {
		return nil, errors.New("the default HTTP transport was replaced")
	}
	transport := defaultTransport.Clone()
	transport.TLSClientConfig = config
	// The connections kept idle were checked against the CRL held before: the next request
	// checks ratd against the new one during its handshake, rather than after its response.
	side.OnCRL(mtls.CRLEvents{Fetched: func(*mtls.CRL) { transport.CloseIdleConnections() }})
	// The connections kept idle present the certificate replaced: the next request presents the
	// renewed one during its handshake, rather than the old one until it expires, a client in
	// constant use keeping a connection for hours.
	side.OnWatch(mtls.WatchEvents{Reloaded: func(*mtls.Side, bool) { transport.CloseIdleConnections() }})
	return &http.Client{Transport: &revocationTransport{side: side, next: transport}}, nil
}

// revocationTransport checks the CRL of the CA of side around every request, as ratd does: a
// connection kept alive outlives the check of its handshake, and a client calling more often
// than the idle timeout keeps it for hours. Before the request, it gets the CRL (mtls.Side.CRL),
// which refreshes it, refusing the request without a valid one, or with the client revoked. After
// the response, it checks the certificate of ratd on the connection used, dropping the response
// of a ratd revoked since its handshake.
type revocationTransport struct {
	side mtls.Source
	next *http.Transport
}

func (t *revocationTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if _, err := t.side.Side().CRL(req.Context()); err != nil {
		// a RoundTripper closes the body of the request, even when failing
		if req.Body != nil {
			_ = req.Body.Close()
		}
		return nil, err
	}
	resp, err := t.next.RoundTrip(req)
	if err != nil {
		return nil, err
	}
	if resp.TLS != nil && len(resp.TLS.PeerCertificates) > 0 {
		if err = t.side.Side().CheckRevoked(req.Context(), resp.TLS.PeerCertificates[0]); err != nil {
			_ = resp.Body.Close()
			t.next.CloseIdleConnections()
			return nil, err
		}
	}
	return resp, nil
}

// CloseIdleConnections closes the connections kept idle, for http.Client.CloseIdleConnections.
func (t *revocationTransport) CloseIdleConnections() {
	t.next.CloseIdleConnections()
}

// Transport returns the transport of an MCP client of the Go SDK reaching the ratd at server
// (HOST:PORT), presenting the certificate of side, a client side of a bundle (mtls.Load or
// mtls.Parse): the only addition ratd asks of a standard MCP client.
//
//	side, err := mtls.Load("clients/alice")
//	transport, err := connect.Transport(side, "host:7281")
//	session, err := mcp.NewClient(implementation, nil).Connect(ctx, transport, nil)
func Transport(side mtls.Source, server string) (*mcp.StreamableClientTransport, error) {
	client, err := HTTPClient(side)
	if err != nil {
		return nil, err
	}
	return &mcp.StreamableClientTransport{Endpoint: Endpoint(server), HTTPClient: client}, nil
}
