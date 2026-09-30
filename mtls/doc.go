// Package mtls is the TLS contract between ratd and its clients (the rat bridge, or a harness
// targeting ratd directly): how bundles are made, and how each side checks the other. The design
// decisions, and the alternatives they rejected, are described in the README of this package.
//
// # Bundles
//
// A bundle is generated at once, for one tenant ([Generate]): a CA, a server certificate for
// ratd, and one certificate per named client. The CA private key only exists in memory while
// generating: once written, the bundle is closed, and no certificate can be added to it. Adding a
// client, or replacing a compromised key, means generating a new bundle. Each side gets a
// directory holding what it needs, the CA certificate included, and nothing else. Certificates
// are valid for 10 years, and every side warns once less than a year remains ([ExpiryWarning]).
//
// # Roles and names
//
// Certificates carry their role (extended key usage): serverAuth for ratd, clientAuth for
// clients, so that neither can stand for the other. They also carry a name: the server
// certificate names the tenant, which ratd serves, and so does the CA, so that a client directory
// tells which tenant it reaches. A client certificate names its client, which is its tmux
// session. Both are required, and are plain names, validated as tmux names are.
//
// # Checks
//
// ratd checks client certificates: signed by the CA of the bundle, valid, clientAuth. Clients
// check the server certificate the same way with serverAuth, but not the host name they reach:
// it carries no address. Each side also checks its own directory when loading it ([Load]: files,
// key permissions, key matching the certificate, CA, role, validity, names), to fail at startup
// rather than at the first connection; rat-tool runs the same checks for humans.
//
// # TLS configurations
//
// [ServerConfig] and [ClientConfig] build the TLS configuration of each side from its loaded
// directory, TLS 1.3 only. The client configuration is exported, for Go clients connecting to
// ratd directly. [PeerName] tells the name of the peer of a connection: its session for ratd, the
// tenant for a client.
package mtls
