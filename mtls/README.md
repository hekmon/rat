# mtls: design

This document explains why the TLS contract between ratd and its clients is built the way it
is: the design decisions, the alternatives they rejected, and the TLS pitfalls they avoid. What
the package provides is described by its godoc (`doc.go`); why ratd requires mutual TLS on every
address is in the ratd README.

## What each side holds

- **ratd**: the CA certificate (public), its server certificate and key. Terminals run as the
  same user and can read that key: it lets them impersonate ratd towards a client, not get in,
  and a shell is what they already have. Nothing on the server can issue a client certificate:
  the CA key no longer exists.
- **A client**: the CA certificate, its own certificate and key. That key is the credential: it
  grants a shell as the user running ratd, and it never reaches the server.

Nothing is passed on a command line, visible to every user (`ps`): flags carry file paths only.
Keys are written readable by their owner only.

Rejected: a bearer token. ratd would hold the secret to compare it, in a file or in its
environment, both readable by agents running as the same user (`/proc/<pid>/environ` keeps the
startup environment even after an unset), and inherited by terminals unless removed.

## Closed bundles

The CA private key only lives in memory while a bundle is generated, and is never written:

- only the certificates generated together are valid;
- no certificate can be issued later, not even by someone controlling the server;
- revoking is generating a new bundle: there is no CRL nor OCSP (Go does not check revocation of
  client certificates by itself anyway);
- the CA can sign no intermediate CA (`MaxPathLen` 0): a chain is always the CA, then the leaf.

One bundle per tenant: a client of one tenant can not reach the ratd of another, even on the
same machine, which a CA shared between tenants would allow.

Rejected:

- **A CA kept to issue clients later**: a key to protect, and kept on the server, it would let
  an agent issue itself certificates.
- **Pinned self-signed certificates** (the `authorized_keys` model): a client is revoked by
  removing a line, but the server needs verification code of its own, where a closed bundle
  keeps Go's standard client certificate verification, with the same guarantees.
- **One client certificate shared by every client**: clients could not be told apart, neither
  in logs nor by session.

## Names in the certificates

The server certificate names the tenant, and ratd serves the tenant it names: the bundle is the
tenant, and no setting can pair a tenant with the bundle of another. A client certificate names
its client, which is its tmux session in the tenant: ratd keeps each client to its session, and
logs its name with each call. Agents meant to share terminals share a client certificate.

Names are carried in the subject common name, and are plain names, validated as tmux names are
(letters, digits, `_` and `-`), when generating and when loading. They are required: the tmux
controller accepts an empty tenant for its default socket, but a tenant served by ratd is always
named.

Clients are fixed when the bundle is generated: adding one, hence a session, means a new bundle
for everyone. A tenant has a handful of clients, and regenerating is cheap.

## No address in the server certificate

The server certificate carries no DNS name and no IP address (subject alternative names), and
clients do not check the host name they reach against it. On the web, a client with no prior
knowledge relies on a third party vouching that a host name belongs to a server. Here both sides
know each other beforehand: a closed bundle holds a single server certificate, and presenting it
proves being that server, wherever it is reached.

An address would bind the bundle to it. The server certificate can not be reissued (the CA key
is gone), so moving ratd, or reaching it through a tunnel or a NAT, would take a new bundle for
every client. Rejected as well: a server certificate from a public CA (Let's Encrypt), which
needs a public DNS name, while clients still need our certificates.

The consequence: TLS clients check the host name by default, and harnesses do not let that check
be turned off, nor a private CA be trusted. They go through the rat bridge.

### Skipping the host name check, and only it

Go's TLS client has no option to skip the host name check alone. `InsecureSkipVerify` turns off
every check (chain, validity, role): the client would accept any server. The client
configuration therefore turns them all off, then checks everything but the host name again in
`VerifyConnection`: the chain to the CA of the bundle, and the serverAuth role, with no
`DNSName`.

- `VerifyConnection`, not `VerifyPeerCertificate`: it also runs on resumed sessions, and
  `VerifyPeerCertificate` gets no verified chains once `InsecureSkipVerify` is set.
- Leaving `ServerName` empty does not skip the check: Go, `http.Transport` in particular, derives
  it from the address it dials.

The server side needs none of this: servers do not check client certificates against a host
name, and with `RequireAndVerifyClientCert` and the CA as `ClientCAs`, Go checks the chain, the
validity and the clientAuth role by itself.

## Algorithms

- **ECDSA P-256**: the widest support among TLS stacks, including those of harnesses connecting
  directly. Ed25519 certificates are not supported everywhere.
- **TLS 1.3 only**: both ends are recent, and nothing older needs negotiating. Certificates only
  need the digital signature key usage: TLS 1.3 key exchange is always ephemeral Diffie-Hellman,
  never encryption with the certificate key.
- **Long validity** (years): rotating is generating a new bundle when needed, not a schedule
  that an expiry would impose.

## One directory per side

A bundle is written as one directory for the server and one per client, each holding the CA
certificate and that side's certificate and key. Each side copies its own directory: mixing the
files of two bundles is the likeliest mistake, and it only shows as an opaque handshake error.
