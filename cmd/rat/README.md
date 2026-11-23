# rat: design

This document explains why the bridge is built the way it is: the design decisions and the
alternatives they rejected. What it is is described by its godoc (`doc.go`); why a bridge exists
at all, and which clients are left out, is in the Topology section of the ratd README.

## One mode

```
rat --server HOST:PORT --bundle DIR [--transfers-dir DIR] [--log-level info]
```

- `--server` is required, with no default: which machine to reach is the whole point of the
  configuration. `HOST:PORT` rather than a URL: the scheme is always `https` and the path always
  `/mcp`, less to get wrong. An IPv6 address goes between brackets.
- `--bundle` is a client directory of a bundle (`clients/<name>`, see package `mtls`): its
  certificate names the session.
- `--transfers-dir` offers the file transfers between this directory and ratd (see File
  transfers): not offered without it.
- Nothing else: no configuration file, no environment variable.

These flags are declared once, in package `internal/flags`, shared with `rat-tool check`: a
harness command line is checked by copying it, with the same names and the same checks.

At startup, rat loads and checks its client directory (key matching the certificate, CA, role,
validity, name, see package `mtls`): a failure is written on stderr before it exits, and the
harness shows a server that failed to start. It warns on stderr in the last tenth of the
validity, a year for a bundle (see Expiry in `mtls/README.md`), then every day while it runs, of
the certificate in use: a rat runs for weeks, and would otherwise tell nothing of a certificate
left unrenewed until every request fails. It does not contact ratd before the first message, nor
fetch the CRL of its CA (see Revocation in `mtls/README.md`): a ratd or a PKI briefly down does
not fail the startup of the harness, the first request tells.

## Relay

- **JSON untouched.** rat reads stdin as newline delimited JSON-RPC (up to 16 MiB per message,
  as the stdio transport of the SDK reads it), and posts each line as read, surrounding
  whitespace aside. It writes the body ratd answers on a line of its own, compacted if it spans
  lines, but not decoded and encoded again: not the stdio transport of the SDK, which does. It
  reads inside a message only to prepare its request, never to rewrite it. With a transfers
  directory, three exceptions: rat adds its two tools to the answer of ratd to `tools/list`
  (decoded and encoded again, the last page only), answers their calls itself, never relaying
  them, and names them in the copy hints of the texts of ratd (see File transfers).
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
- **Through the proxy of the environment**, as other HTTPS clients go (`HTTPS_PROXY`, `NO_PROXY`
  to bypass it; never for a loopback address): the proxy is named when starting, and in the
  failures of requests not reaching ratd, as Go reports a proxy refusing ratd (a CONNECT
  answered 403) with a bare `Forbidden`. Its password, if any, is never told. Go reads the
  environment once per process: the MCP SDK has it read when its packages initialize, so that a
  program changing it afterwards changes nothing.
- **No connection state** beyond the negotiated version and the calls in flight: a ratd restart
  only fails the requests sent meanwhile.
- **Stopping**: at the end of stdin (the harness leaving) or on SIGTERM or SIGINT, rat ends the
  calls in flight and exits, rather than waiting for them. Once they ended, it cuts the file
  transfers under way, an upload sent whole included, whose answer a cancel waits for but nobody
  would read then, canceled or not: the context of `connect.Upload` cuts it whatever was sent,
  apart from the cancel of the agent, a context being done once. It waits for them to remove
  their temporary files; an upload ratd already received whole may still be written, and a
  download rat received whole is still put in place, a cancel of the agent alone stopping it then
  (see Cancellation). A file system that stops answering in the transfers directory (a stale NFS
  mount) holds the transfers reading or writing it, and so the stop, until it answers: no call
  waits for it longer than its wait, and the transfers of other paths go on, each job checking
  its local side outside the lock of the jobs.

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

## File transfers

`write_file` and `read_file` move content through the context of a model. A file an agent only
moves (a binary, an archive, a build output) takes a program on each machine to copy by path:
ratd on its own (see File transfers in the README of ratd, for its endpoints), rat on the machine
of the harness. With `--transfers-dir`, rat offers `upload_file` and `download_file`, adds them
to the tool list of ratd, and answers their calls itself, over the transfer endpoints of ratd
(package `connect`). Without it, rat relays alone: upgrading rat never widens what agents can do
on the machine of the harness.

**The transfers directory is the boundary.** The permissions and sandbox of a harness cover its
own tools, not what an MCP server does by itself, and a text on a remote screen could otherwise
mislead an agent into sending `~/.ssh/id_ed25519`, or overwriting `~/.bashrc`. Local paths are
relative to the directory, which the descriptions name, and confined to it (`os.Root`): no
absolute path, no `..`, no link leading out. Remote paths stay free: the agent has a shell there.
`overwrite` guards against mistakes, not against a misled agent, which can set it too: the
directory must be one of its own, as a new `.git/hooks/pre-commit` in a project is code execution.

**Jobs, found by their paths.** A transfer can outlast a call, which several harnesses cut at 60
seconds: it runs in rat, as a job kept in memory. A call waits for it 50 seconds at most, as
`wait_window` does at most; a longer one goes on, the call telling its progress (the bytes of the
size, the rate over the last 30 seconds, a rounded time left, or that nothing moved), for the
agent to wait its own way (a `sleep` in its shell) or to do something else. A job is identified
by its destination, which one transfer at a time writes, and found by its paths, as terminals are
by name: a call with the same paths waits for the rest, or tells how it ended, once, whatever its
overwrite, but for a call setting it to a transfer without it, which may refuse a file existing:
refused while that transfer runs, told how it ended if it wrote its file, starting the copy asked
otherwise. Another source to the same destination is refused while it runs, and once it ended, its
outcome not told yet, the call tells that outcome, as a cancel naming another source does, and
forgets the job: the next call starts the transfer asked, the agent knowing what the first one
wrote (after a context compaction, for one). Paths are cleaned as a call is read, the remote one
lexically when ratd takes it, absolute or starting with `~/` (`path.Clean`, `~/` kept), a relative
one sent as it is for ratd to refuse: its spellings with a doubled slash, a dot or a parent make
one job, but `~/x` and the absolute path it stands for stay two, as do two paths through a link of
ratd's machine, which rat can not resolve. A destination naming a directory by its form (a final
slash, or `.` or `..` as its last part) is refused before any request, as ratd refuses one, rather
than cleaned into the name of a file. Canceling a call (the harness giving
up on it) ends its wait, not the transfer. `cancel` with the same paths stops it, leaving the
destination as it was, decided at one point: before the last chunk of an upload (past it, ratd
may be writing the file, and the call tells its answer, or that it was lost, the file possibly
written), before a download is put in place. An upload with `in_place` writes into the file it
replaces (see In place in the README of ratd): a cut or a cancel may then leave it partly written,
which rat tells, not knowing whether ratd found the file to write into, a missing one being created
all or nothing. Written in place into the very file uploaded, rat and ratd sharing a machine, a
file would be emptied before being read: the description of `in_place` warns against it.
Rejected: detecting it, which rat could only do by the path, wrongly through a tunnel showing a
remote ratd on a loopback address, the same paths on both machines. A call joins the job of its
paths whatever its `in_place`, which changes how the file is written, not what is copied.
Rejected: job identifiers, with tools to wait for, list and cancel jobs, which a context
compaction loses, more tools for small models to juggle; a parameter for the wait, or a size
choosing between waiting and a job, the speed of the link being unknown; MCP tasks (experimental
in protocol 2025-11-25, an extension from 2026-07-28 on, unknown to the SDK), which harnesses
would have to support, and which could only add to this, never replace it.

**The local side mirrors ratd's.** A download goes to a temporary file next to its destination,
put in place once complete: linked under its name without overwrite, which fails if a file
appeared meanwhile, renamed over the file it replaces with overwrite, the file replaced keeping
its mode; missing directories are created, and named, and the destination looked at again once
they are there, whoever created them, before any byte is written: under a directory missing, the
look made before the request could not tell a name too long. A download failing once some were
created, creating them included, names them too, leaving them; an upload cut or canceled can not
name those ratd created, its answer never read. A symbolic link at the end of a local path is
refused, telling where it leads; `os.Root` ignores `O_NOFOLLOW`, so rat looks at the file with
`Root.Lstat`, then checks the file it opened is the one it looked at. An upload is stamped when
opened and once read (`connect.Upload`): a file changing meanwhile fails the transfer, ratd
writing nothing, or part of it in place. A replaced file keeping permissions other than those of
its source is told, as an agent setting overwrite may expect a chmod in the same call, but on
Windows, whose files have no permission bits (Go shows 0666, or 0444 for a read-only file): almost
every replace would tell it. A read-only file there is not replaced, `os.Root.Rename` refusing to,
as Windows does: the agent is told so, rather than access denied.

**The texts of ratd name the tools.** The descriptions of `write_file` and `read_file`, and the
instructions, point a file the agent only moves to "a tool copying files by path if you have
one": ratd can not know whether its client offers one. rat, offering its transfers, puts in place
of each such sentence one naming its tools ("use upload_file"), in the answers carrying them
(`initialize`, `server/discover` from protocol 2026-07-28 on, `tools/list`): found as they are,
none of their characters being one JSON escapes, the rest of the answer left untouched. The
sentences are declared in package `cmd/ratd/tools` with both sides; a ratd of another version,
whose sentences differ, keeps its generic ones, still true. Rejected: ratd naming the tools of rat
conditionally, which a harness connected directly never has; rat rewriting the texts of ratd
freely, which a sentence changed between versions would silently miss; a header telling ratd
that rat offers its transfers, for ratd to write its texts accordingly, a header of rat's own on
`/mcp`.

## Errors

- **Tool and protocol errors** come from ratd, relayed untouched, whatever the HTTP status: an
  answer that is a JSON-RPC response to the call (ratd or its SDK refusing it, with a 4xx) is
  the answer.
- **The file transfers** refused or failing are tool results marked as errors, with a sentence
  for the agent: ratd's, relayed, or rat's (the local side, a ratd older than the transfers,
  ratd unreachable, told as for a call; or the transfer cut once ratd answered, told so with the
  reason the system gave, which `connect.ErrCut` tells apart; or an upload whose answer was lost
  once all of it was sent, told possibly written, or partly when written in place, which
  `connect.ErrAnswerLost` tells apart).
- **Transport failures**: no JSON-RPC response exists, while the harness waits for one matching
  its request. rat answers with a JSON-RPC error of its own, code -32050 (in the range JSON-RPC
  leaves to implementations, apart from the codes of MCP and its SDKs), telling what failed:
  - `ratd unreachable at HOST:PORT: connection refused` (the reason of the system), `ratd
    unreachable at HOST:PORT through the proxy http://proxy:3128 (from the environment):
    Forbidden` when a proxy is on the way;
  - `ratd at HOST:PORT refused the TLS handshake: remote error: tls: unknown certificate
    authority. Check the bundle with rat-tool check.`, or `rat refused the certificate of ratd
    at HOST:PORT: …` when the server is not of the bundle;
  - `ratd at HOST:PORT did not answer within 30s`;
  - `rat refused to reach ratd at HOST:PORT: certificate revoked: the CRL of the CA, fetched
    from …, revokes the certificate of …` (of ratd, or of rat itself), or `…: no valid CRL of the
    CA: …`;
  - `ratd at HOST:PORT answered HTTP 502: …` (the first line of the body), as ratd tells a client
    revoked (403) or that it has no valid CRL (503);
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
session, bundle expiry, the address of the CRL or none, the proxy or none), each newer CRL, a fetch of the CRL
failing (a warning, with when the CRL held goes stale), a renewal of its certificate (a warning
when its key changed) and files refused (see Renewal in `mtls/README.md`), relay failures (the
method and identifier, the error), expiry warnings, the stop and its reason, and one line per
file transfer once ended (the direction, the local and remote paths, the outcome, the bytes moved,
the duration). At debug level, one line per message (method, identifier, HTTP status, duration),
and each call canceled. Never the content of a message, nor of a file.
