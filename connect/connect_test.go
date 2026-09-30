package connect

import "testing"

// TestEndpoint guards the endpoint of a ratd: https, the path it serves, an IPv6 address kept
// between brackets.
func TestEndpoint(t *testing.T) {
	for server, expected := range map[string]string{
		"host:7281":  "https://host:7281/mcp",
		"[::1]:7281": "https://[::1]:7281/mcp",
		"10.0.0.1:1": "https://10.0.0.1:1/mcp",
	} {
		if endpoint := Endpoint(server); endpoint != expected {
			t.Errorf("%s: expected %s, got %s", server, expected, endpoint)
		}
	}
}
