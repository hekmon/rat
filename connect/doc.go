// Package connect is how a client reaches ratd: the defaults of rat that ratd and its clients
// share (the path ratd serves, the port it listens on unless told otherwise), and what a client
// in Go needs beyond package mtls: the endpoint of a ratd, the HTTP client presenting its
// certificate, and the transport of an MCP client of the Go SDK built on it ([Transport]).
//
// The addresses of ratd are HOST:PORT rather than URLs: the scheme is always https and the path
// always [Path], less to get wrong. It holds no command line: the flags of rat and rat-tool are
// theirs (internal/flags), and a program importing this package gets no dependency for them.
package connect
