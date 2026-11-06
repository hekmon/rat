# mtls: design

This document explains why the TLS contract between ratd and its clients is built the way it
is: the design decisions, the alternatives they rejected, and the TLS pitfalls they avoid. What
the package provides is described by its godoc (`doc.go`); why ratd requires mutual TLS on every
address is in the ratd README.

## What each side holds

- **ratd**: the CA certificate (public), its server certificate and key. Terminals run as the
  same user and can read that key: it lets them impersonate ratd towards a client, not get in,
  and a shell is what they already have. Nothing on the server can issue a client certificate:
  the CA key no longer exists, or, for a sub CA of another PKI, is kept by that PKI.
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
- revoking is generating a new bundle: nobody can sign a CRL (a sub CA of another PKI can, see
  Revocation);
- the CA can sign no intermediate CA (`MaxPathLen` 0): a chain is always the CA, then the leaf
  (loading a side checks it, see below).

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
  the terminals of the client revoked. The CRL of a sub CA of another PKI (see Revocation) is
  revocation: rat checks the server certificate too, which that PKI issues again under the same
  CA, a server key copied being revoked with its certificate.
- **A short validity**: a bundle expiring stops every client at once, a short validity makes it
  frequent, and a key leaked stays valid until then, long enough to be used.

## A sub CA of another PKI

A side is its directory and the checks of loading, not the tool that wrote it: a sub CA of
another PKI (an organization's) can take the place of the CA of a bundle, filling the same
directories with certificates following the same rules. That PKI issues the clients of the
tenant, adding one with no new bundle, and keeps the key of the sub CA: whoever can use it can
issue a shell, where nobody can for a closed bundle. `rat-tool` only generates closed bundles:
managing another PKI is out of scope.

The sub CA is dedicated to one tenant, as the CA of a bundle is: ratd lets in every certificate
it signs for clients, and clients, checking no host name, take for ratd every certificate it
signs for servers. Loading checks what certificates can tell of it:

- **It signs no CA** (path length 0, see Loading a side): the certificates of the CAs under it
  would get into ratd.
- **It names the tenant with a plain name**: an issuing CA shared with other uses has a name
  of another kind (`Example Issuing CA 01`), refused, in practice.
- **It is alone in `ca.crt`**: the chain up to the root of the PKI would make ratd trust the root,
  and every certificate under it.

That it signs for rat alone is left to the PKI.

Rejected: **trusting a CA shared with other uses** (the root of the PKI, an issuing CA of the
organization), rat's certificates carrying a marker of their own (a URI naming the tenant and the
session), checked by each side. It would hold, at the price of a field of rat's own in every
certificate, bundles included, and of a template for it in the PKI, while a PKI creates a sub CA
readily, and a sub CA per tenant keeps the tenants apart as bundles do.

### Revocation

A sub CA of another PKI revokes certificates with a CRL. Each side fetches the CRL of its CA from
the distribution point of its own certificate, the first over http or https: every certificate
of the CA names the same CRL, and taking it from the certificate of a peer would let a client
choose an address for ratd to fetch. A certificate naming none revokes nothing: a bundle, or a
PKI issuing short-lived certificates instead. One naming only LDAP distribution points (which AD
CS lists first) is refused: the CA revokes, and rat could not tell what.

A CRL is accepted when it is:

- **of the CA of `ca.crt`**: issued by it and signed by it (Go checks the signature, not the
  issuer named);
- **complete**: no critical extension Go does not read. Go reads a partial CRL (issuing
  distribution point), a delta CRL and an indirect CRL as complete, which would leave revoked
  certificates out;
- **current**: with a next update, which has not passed. Without one, a CRL replayed years later
  would look current;
- **within 8 MiB**, DER or PEM.

Plain http is enough for its content, the CRL being signed: nobody on the network can forge one.
Not for its freshness: a side holding no CRL yet, ratd starting or rat at its first request,
takes any CRL of the CA still valid, and someone on its network can hand it an older one,
un-revoking the certificates revoked since, until its next update. A running side refuses a CRL
older than the one it holds; https, checked against the CAs of the system, closes the rest, and
CRLs valid for a short time bound it. The proxy comes from the environment.

ratd fetches it while starting, before its terminals exist, and refuses to start without a valid
one, or with its own certificate revoked: starting would let in the certificates the CRL
revokes, and refusing loses no terminal. A client fetches it at its first handshake: rat
contacts nothing before its first request.

Then each fetches it again as it is used, one fetch at a time, never on a timer: an idle side
fetches nothing.

- **While the CRL held is valid**, a check answers with it at once, and starts a fetch in the
  background 15 minutes after the last one (a certificate revoked is refused within 15 minutes of
  the PKI publishing it), or a minute after a fetch failing, the CRL held kept meanwhile.
- **Without a valid CRL** (none yet, or the one held stale), every check fetches, and accepts no
  certificate while it can not: a side waking from sleep recovers as soon as its network does.
  Checks waiting on a fetch in flight take its result, the CRL or the failure: during an outage
  of the PKI, they would otherwise fetch one after the other, the last waiting for all of them.
- **Delays are told by dates**, the CRL held being judged stale by its next update: the
  monotonic clock of Go, which durations use, stops on some systems while the machine sleeps,
  and a laptop asleep for days would count only the minutes it was awake. A clock set back makes
  a fetch due, the delay being unknown.
- **A CRL older than the one held is refused**, the one held kept: a CRL replayed would un-revoke
  the certificates revoked since. Issued at the same date, it is the same CRL.

What happens to the CRL is told to its holder (`Side.OnCRL`): a newer CRL, which rat logs and on
which it closes its idle connections (see below); every fetch returning a valid CRL, newer or
not, which clears the warning of a failure; and a fetch failing, logged as a warning at once,
every fetch being an early signal, with when the CRL held goes stale. ratd shows the failure in
its status line until a fetch succeeds again.

Each side checks the certificate of its peer, and its own, which a side revoked tells rather
than its peers refusing it:

- **ratd, on every request** (`Side.CheckPeer`): a connection kept alive outlives a handshake. It
  answers 403 a client revoked, misnamed (see Names), or whose certificate expired since the
  handshake (a PKI issuing short-lived certificates relies on their expiry alone), and 503 every
  client while it has no valid CRL, the body telling why: refused during the handshake, a client
  would only learn "bad certificate", the alert Go sends whatever the reason. The terminals of a
  client revoked keep running: revoking closes the way in, stopping the service is the kill
  switch.
- **A client, during the handshake and around every request** (`ClientConfig`,
  `connect.HTTPClient`): it refuses a server revoked, so that a server key copied off the machine
  is revoked with its certificate, the PKI issuing the server a new one under the same CA, while
  clients keep theirs. Its connections are kept alive, for hours by a client calling more often
  than their idle timeout: before each request, it gets the CRL, which refreshes it; on a newer
  CRL, it closes its idle connections, for the next request to check the server during its
  handshake; and after each response, it checks the server of the connection used, still valid and
  not revoked.

A side whose certificate names no CRL refuses a peer whose certificate names one: the CA
revokes, and the side could not check it, its certificate issued from another template than the
peer's (a server template lacking the distribution point would otherwise leave ratd checking no
client, silently). The distribution point of the peer is read as a sign only, never fetched.
Each side tells whether it checks revocation: ratd and rat when starting, `rat-tool bundle
inspect` offline.

Rejected:

- **Fetching the CRL the peer names**, for a side whose certificate names none: a client would
  choose an address for ratd to fetch.
- **Fetching on a timer**: an idle side would fetch for nothing, a laptop asleep would miss its
  turns, and a side in use needs a fetch on use anyway.
- **Conditional requests** (`If-Modified-Since`): state to keep for little, at four fetches an
  hour of a small CRL.
- **Warning in the last tenth of the validity of the CRL**, as of certificates: with a fetch every
  15 minutes, a failure is already the early signal.
- **A CRL file next to `ca.crt`**, delivered by the admin: ratd is one machine, but rat runs on
  every machine of a harness, each needing the file pushed every few days, by tooling every
  organization would have to build.
- **OCSP**: a request per certificate checked, to a responder on the path of every connection,
  where one CRL covers every certificate of the CA.
- **The CRL of the root**, which revokes the sub CA itself: a side holds no certificate above the
  sub CA. Revoking the sub CA is replacing `ca.crt`.
- **LDAP**: a client of its own, for an AD CS that can publish over http as well.

## Names in the certificates

The server certificate names the tenant, as its CA does, and ratd serves the tenant they name
(loading the server side requires them to agree): the bundle is the tenant, and no setting can
pair a tenant with the bundle of another. A client certificate names its client, which is its
tmux session in the tenant: ratd keeps each client to its session, and logs its name with each
call. Agents meant to share terminals share a client certificate.

Names are carried in the subject common name, and are plain names, validated as tmux names are
(package `tmux/names`: letters, digits, `_` and `-`), when generating and when loading. They are
required: the tmux controller accepts an empty tenant for its default socket, but a tenant
served by ratd is always named. The name of a peer is checked too, on each request
(`Side.CheckPeer`), before ratd takes it for a session: a sub CA of another PKI names its
certificates as its templates do, and a new line in a name, shown in the status line of ratd,
would start other fields for systemd.

The CA names the tenant too, so that a client directory tells which tenant it reaches. Its
subject also carries the organization `rat CA`, which keeps it distinct from the subject of the
server certificate: equal to its issuer, the server certificate would be self-issued (RFC 5280).

Clients are fixed when the bundle is generated: adding one, hence a session, means a new bundle
for everyone. A tenant has a handful of clients, and regenerating is cheap. A sub CA of another
PKI issues them as they come (see below).

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
validity and the clientAuth role by itself. Each end also checks that the certificate of its
peer carries its role only, as loading a side does of its own: Go accepts a certificate carrying
both roles, or none, for either, which a template of another PKI may issue, letting a client
stand for ratd (guarded by `TestConfigsRoles`). ratd refuses such a client with "bad
certificate", as one of the wrong role.

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
| of the wrong role, of both, or of none | bad certificate |
| expired | expired certificate |

A client revoked, or expired since its handshake, is not refused by an alert: ratd answers its
next request 403 (see Revocation).

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
  directly. Ed25519 certificates are not supported everywhere. Loading a side takes any key Go
  signs with, in PKCS#8: a sub CA of another PKI may issue RSA keys, which work as well.
- **TLS 1.3 only**: both ends are recent, and nothing older needs negotiating. Certificates only
  need the digital signature key usage: TLS 1.3 key exchange is always ephemeral Diffie-Hellman,
  never encryption with the certificate key.
- **Valid for 10 years, from a day before generation**: rotating is generating a new bundle
  when needed, not a schedule that an expiry would impose. Starting a day early absorbs clock
  skew: a client whose clock runs a few minutes late would otherwise reject a bundle it just
  received, with an error saying little. Rejected: starting at the generation time.

## Expiry

An expired bundle stops every client at once, years after anyone remembers how it was made.
Every side warns in the last tenth of the validity of whichever of its certificate and CA
expires first: ratd and rat at startup and every day (rat on its stderr, which harnesses log),
rat-tool when inspecting a bundle or checking a ratd. Sysadmins need reminding, often.

A tenth is a year for a bundle. A sub CA of another PKI issues certificates valid for a year or
less, which a fixed year would warn of from their first day, until nobody reads the warning: a
tenth still leaves weeks to act. The warning tells what expires first and what to do: a bundle
is generated again, a certificate of another PKI issued again by it.

Rejected: a fixed delay, a year or a month, too early for one kind of certificate or too late for
the other.

## Renewal

A sub CA of another PKI renews certificates a year at a time, or less. A side renewed is used
without a restart: a restart of ratd stops every terminal, and a rat running for weeks would
present its certificate past its expiry. A Watcher holds the side of a directory, and loads it
again when its files change: ratd and rat use one, `Load` and `Parse` stay for callers that do
not.

- **Looked at on use**: at each handshake and each request, at most once a second, a stat of
  each file (its size, mode and modification time; a chmod, fixing a key refused for being
  readable by others, changes neither its size nor its time). Nothing on a timer, nor a goroutine;
  any renewal tool works (a copy, a rename), with no hook to write.
- **A renewal is the same CA, role and name**, the files passing every check of loading. Its key
  may be new: some PKIs issue one at each renewal, and a key copied off the machine stays valid
  only until the certificate it goes with is replaced, where renewing the same key keeps it
  valid with every certificate after it. A new key is logged as a warning, and shown a day in
  the status of ratd: a rotation, or a key replaced after it leaked, which a human should know
  of.
- **A new CA takes a restart**: the trust of the side changes, which a human decides, and the
  clients need the new CA too. Another role or name is refused: a running rat would move its
  agent to the terminals of another session.
- **Files refused keep the side in use**, told once per change, a warning: a renewal copied
  halfway never breaks a running side, and the expiry warning keeps telling of the certificate in
  use. Files holding the side in use again, after a refusal, are told restored. A certificate
  refused only for being not valid yet, from a PKI not backdating its certificates or a clock
  behind, is tried again every minute, the files unchanged, told nothing more until taken once
  valid: waiting for the files to change would leave the certificate replaced in use until its
  expiry.
- **From the next handshake on**: ratd picks the certificate of each handshake from the side in
  use, connections open keeping theirs until they close. A client closes its idle connections on a
  renewal of its certificate (`connect.HTTPClient`), as on a newer CRL: in constant use, it would
  otherwise present the certificate replaced for hours, until its expiry, from which ratd refuses
  its requests (see Revocation). The CRL held, and what is told of it, carry over to the side
  renewed when its certificate names the same distribution point.

Two pitfalls of Go:

- The server configuration gives the certificate of each handshake with `GetConfigForClient`,
  not `GetCertificate`: Go skips `GetCertificate` for `Certificates` when the client names no
  host (an address), and `httptest` fills `Certificates`. The configuration of the handshake is
  a copy made then, keeping what the caller set after `ServerConfig` (ALPN).
- The client configuration gives its certificate with `GetClientCertificate`, which presents it
  whatever the server asks: it sends none to a server asking for another CA, as Go does with
  `Certificates`, for a ratd of another tenant to tell "certificate required".

Rejected:

- **Reloading on SIGHUP** (`systemctl reload`): every renewal job would have to send it, and
  nothing sends one to rat.
- **Restarting the HTTP server** on a change: it would cut the calls in flight, or wait for them,
  and free the port a moment, for another process to take.
- **Accepting a new CA while running**: the trust of a side changing unnoticed.
- **Refusing a new key while running**: a restart for each rotation, which a PKI issuing a key
  at each renewal makes routine, would push to renewals keeping the same key, valid forever once
  copied.

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
4. **CA**: `ca.crt` is a CA, which signed the certificate, and signs no CA (path length limited
   to 0). The TLS server of Go builds the chain of a client with the intermediate CAs the client
   sends: a CA allowed to sign CAs would let into ratd the certificates of every CA under it,
   rather than only those it signs. The CA of a bundle never is one, a directory filled by other
   means may hold one.
5. **Role**: the certificate authenticates the side its file names, and only it: a client must
   not run a server with its certificate, nor the other way round. Stricter than the
   verification of Go in TLS handshakes, which accepts a certificate carrying both roles, or
   none: each end of a handshake checks its peer so too.
6. **Validity**: the certificate and the CA are valid now.
7. **Names**: the certificate and the CA carry plain names, and the server certificate names the
   tenant its CA names: ratd serves the tenant of its CA, while its clients are told the one of
   its certificate. A bundle always names the same, a sub CA of another PKI may not.
8. **CRL**: the CRL distribution points of the certificate, if any, include one over http or
   https (see Revocation), and the CA may sign CRLs (`cRLSign`), which Go only checks when
   checking a CRL, a CA without it making ratd refuse to start. The CRL itself is fetched when
   first needed, not when loading: `rat-tool bundle inspect` stays offline, and rat contacts
   nothing before its first request.

Credentials may live elsewhere than in a directory: a secret store, an embedded asset, for a Go
program connecting to ratd directly. `Parse` takes the contents of the three files and runs the
same checks, in the same order, but for what only files have: the first checks each content is a
single PEM block of its kind, and the permissions of a key are the concern of whoever holds it.
The side is named by the caller, which a directory tells by the names of its files. Contents
rather than readers: PEM blocks are small, and the standard library takes them so
(`tls.X509KeyPair`), each caller reading them from wherever they are.
