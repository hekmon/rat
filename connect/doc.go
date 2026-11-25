// Package connect is how a client reaches ratd: the defaults of rat that ratd and its clients
// share (the path of the MCP endpoint, the port ratd listens on unless told otherwise), the
// contract of the file transfers of rat (their paths, headers and encodings, see [UploadPath]),
// and what a client in Go needs beyond package mtls: the endpoint of a ratd, the HTTP client
// presenting its certificate and checking the CRL of its CA around every request ([HTTPClient]),
// the transport of an MCP client of the Go SDK built on it ([Transport]), and the client side of
// the file transfers ([Upload], [StartDownload]) and of the portals ([StartPipeUpload],
// [ClosePortal]).
//
// The addresses of ratd are HOST:PORT rather than URLs: the scheme is always https and the path
// always [Path], less to get wrong. It holds no command line: the flags of rat and rat-tool are
// theirs (internal/flags), and a program importing this package gets no dependency for them.
package connect
