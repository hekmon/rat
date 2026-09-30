# rat-tool: design

This document explains why the utility is built the way it is: its commands, the decisions
behind them and the alternatives they rejected. What it is is described by its godoc (`doc.go`).

rat-tool holds every secondary feature, so that ratd and rat keep a single mode each: a service
and a bridge are configured once and started by a supervisor or a harness, where a utility is run
by a human, command after command.

```
rat-tool bundle generate --tenant NAME --client NAME [--client NAME…] --output DIR
rat-tool bundle inspect DIR [DIR…]
rat-tool check --server HOST:PORT --bundle DIR
```

## bundle generate

Generates the bundle of a tenant (see package `mtls`): the tenant and at least one client are
required, validated as tmux names, the client names unique. It runs wherever the admin trusts:
nothing in a bundle is tied to the server nor to a client. The output directory must not exist,
as replacing a bundle silently breaks every client deployed with it: the error says so, and how
to regenerate (remove it, or choose another one). It prints the tenant, the directory of each
side and what it is for, the CA fingerprint and the expiry, and reminds to copy each directory
to its side only.

## bundle inspect

Offline, for each directory: the checks ratd and rat run at startup, one line per check until
one fails (`mtls.Checks`: files of a side, key readable by its owner only, key matching the
certificate, certificate signed by the CA, of its role only, certificates valid now, plain
names), then which side it is (from its certificate), its name (the tenant, or the client, which
is its session), the CA fingerprint and the remaining validity, with a warning under a year.

Given several directories, it tells whether they belong to the same bundle (the same CA): mixing
the files of two bundles is the likeliest mistake, and a handshake only reports it as an opaque
error.

Its exit code is not zero when a directory fails a check, or when the directories belong to
several bundles: checking that directories go together is what inspecting several is for, and
scripts rely on the exit code.

## check

Checks a running ratd from a client directory, with the flags of rat, declared once in package
`cmd/ratd/connect`: a harness command line is checked by copying it. Step by step, stopping
at the first failure with a message proper to that step:

1. **Files**: the checks of `bundle inspect`.
2. **Network**: the address answers.
3. **TLS**: the handshake, and when it fails, why (a server from another bundle, a wrong role, an
   expired certificate). It reports the tenant, from the server certificate, and the session,
   from the client certificate. In TLS 1.3, a client completes its handshake before the server
   has checked its certificate: a refusal only shows on the first read, which this step does
   before concluding.
4. **MCP**: initialize, then the server information, the protocol version, and the instructions,
   which carry the bracketed paste warning when ratd has one: a human sees it too.
5. **Tools**: their names and descriptions.

It calls no tool: listing windows, for instance, would create the session. Its exit code is not
zero on failure, for scripts. It warns once less than a year of validity remains.

It is written with the `Client` of the Go SDK, as a harness author would: it checks that ratd
stays a standard MCP server, where rat talks to ratd through a relay of its own (see the README
of `cmd/rat`).

Rejected: a `--check` mode in rat, which would test the configuration actually used by the
harness, but give the bridge a second mode. Sharing the flags gets the same result.

## Later

- **config**: prints the configuration snippet of a harness for a client directory (the
  `mcpServers` entry starting rat with its flags), and the full URL for harnesses connecting
  directly: the harness would run exactly what the tool wrote.
