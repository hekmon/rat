package connect

import (
	"errors"
	"net/http"

	"github.com/hekmon/rat/mtls"
)

// HTTPClient returns an HTTP client presenting the certificate of side, a client directory of a
// bundle, and checking ratd as package mtls does. Its transport is its own, cloned from the
// default one: setting the configuration on http.DefaultTransport would change a global of the
// process, and a bare http.Transport loses the defaults (proxy from the environment, dial and
// handshake timeouts). It has no timeout: callers bound each request.
func HTTPClient(side *mtls.Side) (*http.Client, error) {
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
	return &http.Client{Transport: transport}, nil
}
