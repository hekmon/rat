# rat: design

This document explains why the bridge is built the way it is: the design decisions and the
alternatives they rejected. What it is is described by its godoc (`doc.go`); why a bridge exists
at all, and which clients are left out, is in the Topology section of the ratd README.

## One mode

```
rat --server HOST:PORT --bundle DIR [--log-level info]
```

- `--server` is required, with no default: which machine to reach is the whole point of the
  configuration. `HOST:PORT` rather than a URL: the scheme is always `https` and the path always
  `/mcp`, less to get wrong. An IPv6 address goes between brackets.
- `--bundle` is a client directory of a bundle (`clients/<name>`, see package `mtls`): its
  certificate names the session.
- Nothing else: no configuration file, no environment variable.

These flags are declared once, in a package internal to the module, shared with
`rat-tool check`: a harness command line is checked by copying it, with the same names.

At startup, rat loads and checks its client directory (key matching the certificate, CA, role,
validity, name, see package `mtls`): a failure is written on stderr before it exits, and the
harness shows a server that failed to start. It warns on stderr once less than a year of
validity remains. It does not contact ratd before the first message: a ratd briefly down does
not fail the startup of the harness, the first request tells.

## Relay

- **stdio side**: the stdio transport of the SDK (newline delimited JSON-RPC, up to 16 MiB per
  message).
- **HTTP side**: a client of its own for the Streamable HTTP transport, reduced to what a
  stateless ratd answering in JSON needs: one POST to `https://HOST:PORT/mcp` per message,
  answered by a JSON body (a response) or by 202 (a notification). Headers: `Content-Type`,
  `Accept` (both media types, as the specification requires), `Mcp-Protocol-Version` from the
  result of initialize, and from protocol 2026-07-28 on, the standard headers derived from the
  message (`Mcp-Method`, `Mcp-Name`).
- **Only initialize is read**, for the protocol version: every other message passes as is, so
  rat never needs to change when the tools of ratd do.
- **Requests are relayed concurrently**, each response written to stdout as it comes (JSON-RPC
  identifiers match them), one message at a time.
- **An HTTP transport of its own**, with the client configuration of package `mtls`: setting it
  on `http.DefaultTransport` would change a global of the process, and a bare `http.Transport`
  loses the defaults (proxy from the environment, dial and handshake timeouts).
- **No connection state** beyond the protocol version: a ratd restart only fails the requests
  sent meanwhile.

Rejected:

- **Relaying through the connection of the SDK client**: it learns the negotiated protocol
  version only through a hook that the SDK's own `Client` calls after initialize (unexported,
  `sessionUpdated` in go-sdk v1.8.0). Before protocol 2026-07-28, which carries the version in
  each message, requests after initialize would lack the version header, and ratd would assume
  2025-03-26, silently downgrading the protocol. The connection also gives up after its retries
  when ratd goes down, and a new one loses the negotiated state.
- **An SDK client and server repeating the tools and instructions of ratd**: the bridge would
  interpret everything, and change with ratd.

The cost: rat implements the client headers of the transport, and must follow their evolution.
`rat-tool check` uses the SDK `Client`, which keeps ratd verified as a standard server.

## Errors

- **Tool and protocol errors** come from ratd, relayed untouched.
- **Transport failures** (ratd unreachable, TLS refused, an HTTP error status): no JSON-RPC
  response exists, while the harness waits for one matching its request. rat answers with a
  JSON-RPC error of its own (a code of the implementation defined range, a message telling what
  failed: "ratd unreachable: connection refused"), and logs the details on stderr. A
  notification has no identifier to answer: it is only logged.
- **Its own failures at startup**: stderr, then exit.

Nothing is retried: the harness, or the agent, decides. A request gets 30 seconds, above the 10
ratd gives a tool call, so that a connection hanging is cut rather than waited for.

## Logs

`log/slog`, text handler, on stderr, which harnesses keep: startup, relay failures, expiry
warnings. Never the content of a message.
