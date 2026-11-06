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
// are valid for 10 years, and every side warns in the last tenth of its validity, a year
// ([Side.ExpiresSoon]).
//
// A sub CA of another PKI can take the place of the CA of a bundle, dedicated to one tenant: it
// fills the same directories, with certificates following the same rules, and issues clients as
// they come. Loading checks what a certificate can tell of it: it signs no CA, and names the
// tenant.
//
// # Revocation
//
// A sub CA of another PKI revokes certificates with a CRL, which each side fetches from the
// distribution point of its own certificate when first needed, and again as it is used
// ([Side.CRL]), telling its holder what happens to it ([Side.OnCRL]), and checks ([FetchCRL]): of
// the CA, complete, current. A certificate naming none revokes nothing, as in a bundle.
//
// # Renewal
//
// A [Watcher] holds the side of a directory, loading it again when its files change: a
// certificate renewed by a PKI, with the same CA, role and name, its key new or not, is used from
// the next handshake on, without a restart. A new CA takes a restart. The TLS configurations take
// a [Source], a Side or a Watcher, and use the side in use at each handshake.
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
// ratd checks client certificates: signed by the CA of the bundle, valid, clientAuth, and not
// revoked, on every request ([Side.CheckRevoked]). Clients check the server certificate the same
// way with serverAuth, but not the host name they reach: it carries no address. Each side also
// checks its own directory when loading it ([Load]: files, key permissions, key matching the
// certificate, CA, role, validity, names, CRL distribution point), to fail at startup rather than
// at the first connection; rat-tool runs the same checks for humans. [Parse] runs them on
// credentials that are not in a directory (a secret store, an embedded asset), but for what only
// files have.
//
// # TLS configurations
//
// [ServerConfig] and [ClientConfig] build the TLS configuration of each side from its checked
// credentials, TLS 1.3 only. The client configuration is exported, for Go clients connecting to
// ratd directly. [PeerName] tells the name of the peer of a connection: its session for ratd, the
// tenant for a client.
package mtls
