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
- **A deny list of clients**, in the server directory, reloaded by ratd: revoking one client
  without deploying the others. It is no revocation once the key was used: whoever used it had a
  shell as the user of ratd, which reads the server key, and could copy it to pose as ratd to
  every client. A key leaked but never used is the only case it serves, which a new bundle covers
  too. Its cost: a file of the server that its user must not write, a reload in ratd, a check on
  every request (connections kept alive outlive a check at the handshake), and what to do with
  the terminals of the client revoked.
- **A short validity**: a bundle expiring stops every client at once, the warnings a year ahead
  would start with it, and a key leaked stays valid until then, long enough to be used.

## Names in the certificates

The server certificate names the tenant, and ratd serves the tenant it names: the bundle is the
tenant, and no setting can pair a tenant with the bundle of another. A client certificate names
its client, which is its tmux session in the tenant: ratd keeps each client to its session, and
logs its name with each call. Agents meant to share terminals share a client certificate.

Names are carried in the subject common name, and are plain names, validated as tmux names are
(package `tmux/names`: letters, digits, `_` and `-`), when generating and when loading. They are
required: the tmux controller accepts an empty tenant for its default socket, but a tenant
served by ratd is always named.

The CA names the tenant too, so that a client directory tells which tenant it reaches. Its
subject also carries the organization `rat CA`, which keeps it distinct from the subject of the
server certificate: equal to its issuer, the server certificate would be self-issued (RFC 5280).

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
  it from the address it dials (`transport.go`).
- The server still proves it holds the key of its certificate: `InsecureSkipVerify` only skips
  the verification of the certificate, Go checks the signature of the handshake
  (`CertificateVerify`) regardless (`handshake_client_tls13.go`).
- A server refused fails the handshake with the error of the check of a side it matches (CA,
  validity, role), for the client to tell why.

The server side needs none of this: servers do not check client certificates against a host
name, and with `RequireAndVerifyClientCert` and the CA as `ClientCAs`, Go checks the chain, the
validity and the clientAuth role by itself. Neither end checks that the peer certificate carries
its role only, as loading a side does: no certificate of a closed bundle carries both.

The client configuration is exported: a Go client connecting to ratd directly reuses it, rather
than writing these checks again.

### Where a refused client learns it

In TLS 1.3, a client completes its handshake before the server has checked its certificate: a
refused client learns it on its first read, from the alert of the server, which tells why
(guarded by `TestConfigs`):

| Client certificate | Alert |
|---|---|
| of another bundle of the same tenant | unknown certificate authority |
| of another tenant | certificate required |
| of the wrong role | bad certificate |
| expired | expired certificate |

A client of another tenant presents no certificate at all: the server asks for one signed by the
CA of its bundle, naming it, and Go's client only presents a certificate whose issuer bears that
name. The CAs of two bundles of the same tenant bear the same name.

## One bundle, one ratd

With no address, the server key is the identity of the tenant: whoever holds it answers as the
tenant, wherever it runs. A bundle is meant to be served by a single ratd.

ratd detects a second one on the same machine, for the same Unix user: both would use the same
tmux socket, which the first one holds (see the startup of ratd). It can not detect the others:
another Unix user has its own socket directory, another machine shares nothing. Two servers
then answer as the same tenant, each with its own terminals, and an agent finds different
windows depending on which one it reaches. Keeping a bundle to one machine and one user is left
to the admin.

Rejected: a lock file named after the certificate, in a directory shared by every user, which
any user could create first to block the tenant.

## Algorithms

- **ECDSA P-256**: the widest support among TLS stacks, including those of harnesses connecting
  directly. Ed25519 certificates are not supported everywhere.
- **TLS 1.3 only**: both ends are recent, and nothing older needs negotiating. Certificates only
  need the digital signature key usage: TLS 1.3 key exchange is always ephemeral Diffie-Hellman,
  never encryption with the certificate key.
- **Valid for 10 years, from a day before generation**: rotating is generating a new bundle
  when needed, not a schedule that an expiry would impose. Starting a day early absorbs clock
  skew: a client whose clock runs a few minutes late would otherwise reject a bundle it just
  received, with an error saying little. Rejected: starting at the generation time.

## Expiry

An expired bundle stops every client at once, years after anyone remembers how it was made.
Every side warns once less than a year remains: ratd at startup and every day, rat at startup
(on its stderr, which harnesses log), rat-tool when inspecting a bundle or checking a ratd.
Sysadmins need reminding, often.

## One directory per side

A bundle is written as one directory for the server and one per client, each holding the CA
certificate and that side's certificate and key:

```
<output>/
  server/            ca.crt  server.crt  server.key
  clients/<name>/    ca.crt  client.crt  client.key
```

Each side copies its own directory: mixing the files of two bundles is the likeliest mistake,
and it only shows as an opaque handshake error. Clients have a directory of their own
(`clients/`), so that a client named `server` does not collide with the server. Keys are
readable by their owner only, and so are the directories holding them.

Generating never overwrites: an existing output directory is refused, even empty, as replacing
a bundle silently breaks every client deployed with it. Regenerating is a deliberate act:
removing the directory, or choosing another one. Missing parents are created, as `mkdir -p`
does, and a failure removes the output directory, which did not exist before. Everything is
issued in memory first, so that only writing can fail halfway.

Rejected: writing into a temporary directory, then renaming it to the output directory at the
end, which a failure would leave untouched. A rename replaces an existing empty directory,
silently.

## Loading a side

Each side loads its directory at startup, and checks it, to fail while the admin is still there
rather than at the first connection, with an opaque handshake error. The side is the one whose
certificate the directory holds (`server.crt` or `client.crt`). The checks run in a fixed order,
each with its own error (`Checks` lists them, for callers to name each), so that `rat-tool`
reports one line per check, the ones before a failure having passed:

1. **Files**: the files of one side are present, each a single PEM block. Several certificates
   in `ca.crt` would all be trusted, as a certificate pool built from the file takes each of
   them (and skips the blocks it can not parse).
2. **Key permissions**: the key is readable by its owner only, as ssh requires: a key copied
   without its mode gets the one of the umask, usually readable by every user, and a client key
   grants a shell. Not checked on Windows, where harnesses may run and modes mean nothing.
3. **Key**: the key matches the certificate.
4. **CA**: `ca.crt` is a CA, which signed the certificate.
5. **Role**: the certificate authenticates the side its file names, and only it: a client must
   not run a server with its certificate, nor the other way round. Stricter than the
   verification of Go in TLS handshakes, which accepts a certificate carrying both roles.
6. **Validity**: the certificate and the CA are valid now.
7. **Names**: the certificate and the CA carry plain names.

Credentials may live elsewhere than in a directory: a secret store, an embedded asset, for a Go
program connecting to ratd directly. `Parse` takes the contents of the three files and runs the
same checks, in the same order, but for what only files have: the first checks each content is a
single PEM block of its kind, and the permissions of a key are the concern of whoever holds it.
The side is named by the caller, which a directory tells by the names of its files. Contents
rather than readers: PEM blocks are small, and the standard library takes them so
(`tls.X509KeyPair`), each caller reading them from wherever they are.
