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

These flags are declared once, in package `internal/flags`, shared with `rat-tool check`: a
harness command line is checked by copying it, with the same names and the same checks.

At startup, rat loads and checks its client directory (key matching the certificate, CA, role,
validity, name, see package `mtls`): a failure is written on stderr before it exits, and the
harness shows a server that failed to start. It warns on stderr once less than a year of
validity remains. It does not contact ratd before the first message: a ratd briefly down does
not fail the startup of the harness, the first request tells.

## Relay

- **Bytes untouched.** rat reads stdin as newline delimited JSON-RPC (up to 16 MiB per message,
  as the stdio transport of the SDK reads it), and posts each line exactly as read. It writes the
  body ratd answers exactly as received, on a line of its own. It reads inside a message only to
  prepare its request, never to rewrite it: not the stdio transport of the SDK, which decodes and
  encodes messages again.
- **HTTP side**: a client of its own for the Streamable HTTP transport, reduced to what a
  stateless ratd answering in JSON needs: one POST to `https://HOST:PORT/mcp` per message,
  answered by a JSON body (a response) or by 202 (a notification).
- **What is read, for the headers.** `Content-Type` and `Accept` (both media types, as the
  specification requires) are fixed. The rest comes from the message:
  - `Mcp-Protocol-Version`: from protocol 2026-07-28 on, each request carries its version in
    `_meta` (`io.modelcontextprotocol/protocolVersion`), which ratd requires in the header too;
    older protocols negotiate it once, and rat keeps the version of the result of `initialize`
    for the messages after it.
  - From 2026-07-28 on, the standard headers derived from the request: `Mcp-Method`, `Mcp-Name`
    (the tool or prompt name, the resource URI), and `Mcp-Param-*` for the arguments of a tool
    annotated `x-mcp-header`, which rat computes from the input schemas of package
    `cmd/ratd/tools`, built with it.
  - `notifications/cancelled`: see Cancellation.
- **Requests are relayed concurrently**, each response written to stdout as it comes (JSON-RPC
  identifiers match them), one message at a time. Batches (protocol 2025-03-26) are relayed as
  they are, with the negotiated version only.
- **An HTTP transport of its own** (`connect.HTTPClient`), with the client configuration of
  package `mtls`: setting it on `http.DefaultTransport` would change a global of the process,
  and a bare `http.Transport` loses the defaults (proxy from the environment, dial and handshake
  timeouts).
- **No connection state** beyond the negotiated version and the calls in flight: a ratd restart
  only fails the requests sent meanwhile.
- **Stopping**: at the end of stdin (the harness leaving) or on SIGTERM or SIGINT, rat ends the
  calls in flight and exits, rather than waiting for them.

### Cancellation

Before protocol 2026-07-28, a client and a server share a session, and a client cancels a call
with `notifications/cancelled`, naming it. From 2026-07-28 on, there is no session: each POST is
the whole life of its request, and a cancellation posted on its own reaches nothing (a stateless
server keeps no state, and behind a load balancer may not be the same instance). Cancelling is
ending the HTTP request, which ratd propagates to the call (`PropagateRequestCancellation`).

The harness still cancels with the notification, on stdio. rat keeps the calls in flight by
identifier (as JSON-RPC compares them: 1 and "1" differ), registered before relaying them and
dropped once done, and ends the request of a call canceled. Nothing is written for it, as
JSON-RPC expects no response to a canceled request. The notification is relayed all the same.
Whatever the version: before 2026-07-28, ratd lets the call finish, within its 10 seconds.

Canceling a call ending in tmux kills its tmux client, which pastes all of a text or none of it
(see `tmux/README.md`).

Rejected:

- **Relaying through the connection of the SDK client**: it takes the version of a request from
  its `_meta` (go-sdk v1.8.0), but learns the version negotiated by older protocols only through a
  hook that the SDK's own `Client` calls after initialize (unexported, `sessionUpdated`): their
  requests after initialize would lack the version header, and ratd would assume 2025-03-26,
  silently downgrading the protocol. It decodes and encodes the messages again, and gives up
  after its retries when ratd goes down, a new connection losing the negotiated state.
- **An SDK client and server repeating the tools and instructions of ratd**: the bridge would
  interpret everything, and change with ratd.

The cost: rat implements the client headers of the transport, and must follow their evolution.
`rat-tool check` uses the SDK `Client`, which keeps ratd verified as a standard server.

## Errors

- **Tool and protocol errors** come from ratd, relayed untouched, whatever the HTTP status: an
  answer that is a JSON-RPC response to the call (ratd or its SDK refusing it, with a 4xx) is
  the answer.
- **Transport failures**: no JSON-RPC response exists, while the harness waits for one matching
  its request. rat answers with a JSON-RPC error of its own, code -32050 (in the range JSON-RPC
  leaves to implementations, apart from the codes of MCP and its SDKs), telling what failed:
  - `ratd unreachable at HOST:PORT: connection refused` (the reason of the system);
  - `ratd at HOST:PORT refused the TLS handshake: remote error: tls: unknown certificate
    authority. Check the bundle with rat-tool check.`, or `rat refused the certificate of ratd
    at HOST:PORT: …` when the server is not of the bundle;
  - `ratd at HOST:PORT did not answer within 30s`;
  - `ratd at HOST:PORT answered HTTP 502: …` (the first line of the body);
  - `ratd at HOST:PORT sent an invalid response: …`.

  A notification has no identifier to answer: its failure is only logged.
- **Lines it can not relay**: not JSON (-32700, `invalid JSON-RPC message: …`), over 16 MiB
  (-32050, `message over 16 MiB, not relayed`), answered with a null identifier. rat reads on.
- **Its own failures at startup**: stderr, then exit.

Nothing is retried: the harness, or the agent, decides. A request gets 30 seconds, above the 10
ratd gives a tool call, so that a connection hanging is cut rather than waited for. A call of
`wait_window` gets what it waits on top of it, read from its arguments (`tools.Wait`).

## Logs

`log/slog`, text handler, on stderr, which harnesses keep: startup (version, server, tenant,
session, bundle expiry), relay failures (the method and identifier, the error), expiry warnings, the stop
and its reason. At debug level, one line per message (method, identifier, HTTP status,
duration), and each call canceled. Never the content of a message.
