package connect

import "net/url"

const (
	// Path is the path of the MCP endpoint of ratd: the convention of the MCP specification and of
	// the SDK. ratd also serves the file transfers of rat (see [UploadPath]).
	Path = "/mcp"
	// DefaultPort is the port ratd listens on unless told otherwise: 7281 reads "RAT!" on a phone
	// keypad.
	DefaultPort = "7281"
)

// Endpoint returns the URL of the MCP endpoint of the ratd at server, HOST:PORT.
func Endpoint(server string) string {
	return (&url.URL{Scheme: "https", Host: server, Path: Path}).String()
}
