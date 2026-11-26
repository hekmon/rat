# What a portal promises (draft)

Draft of 2026-11-24, from the design settled that day, every line of it agreed, written before
the code, which is checked against it. The portals with a remote pipe exist since 2026-11-25
(`remote_pipe`); those with a local pipe (`local_pipe`) do not yet, and what is said of a local
pipe below is still ahead of the code. Its final place is `cmd/ratd/README.md`, next to "What a
transfer promises", once they all exist.

A portal is a transfer with a pipe at one end, or at both, where a file transfer has files. It is
asked with `local_pipe` and `remote_pipe` on `upload_file` and `download_file`, each end being a
file or a pipe on its own: a local file into a remote pipe, a remote pipe into a local file, a
pipe into a pipe. *The command* at a pipe is the agent's: one writing, at a source pipe; one
reading, at a destination pipe. rat and ratd never start one, and never stop one.

The model is a portal: it opens, both sides making their pipe; the tool answers ready; commands
use it; it closes, both sides packing up. The code sticks to it, and to nothing else.

As for a transfer, the list below is what a review checks the code against. A finding is a bug
when it breaks a guarantee, reproduced on a local file system on Linux or macOS; otherwise it is a
limit, a line below and no code, or nothing. A fix adds no promise.

**Guarantees.**

- **P1. The pipes are the portal's.** A portal makes its pipes, at the paths named: rat the local
  one, within its transfers directory, ratd the remote one. Where anything is, it refuses, and
  touches nothing. The directory of a pipe must exist, none being made for one, and the options of
  a file (`overwrite`, `in_place`) are refused where they would apply to a pipe.
- **P2. Ready is told from the fact.** "Ready" is answered once every pipe exists, every file end
  was checked as a transfer checks it before its first byte (L9), and ratd answered that its side
  is ready. Nothing has flowed, and the call returns then, without waiting for a command. An
  opening that fails leaves nothing on either side.
- **P3. One stream, as given.** A portal carries one stream. What reaches its destination is what
  its source gave, in order, nothing added: all of it when told done, its beginning otherwise.
- **P4. It closes by itself when its source ended.** A source file sent whole, or a source pipe
  whose command closed its end: the rest is delivered, a command reading sees the end of its
  input, a destination file is put in place, and both sides delete their pipes.
- **P5. A cut is never an end for a command reading.** When the link is lost, or the source fails
  (a file that changed, a read failing), the side of a destination pipe keeps its end open: its
  command sees no end, and waits, until the portal is closed.
- **P6. A command writing is told by the system.** At a cut and at a close, the side of a source
  pipe closes its end: the command gets a broken pipe.
- **P7. A file end is a file.** A destination file is whole or untouched (G3), written in place
  when asked (G7), replaced only with `overwrite` (G4), with its nine bits (G6): the file end keeps
  every option it has today. A new file made from a pipe takes the bits of that source as from any
  other, those of the pipe, 0600, minus the umask.
- **P8. A close packs up.** `cancel` closes the portal of its paths: each side closes its end and
  deletes its pipe. The paths alone name it: it works after a cut, from a rat started again, from
  another rat of the session (a cut rat has not told yet: Q20). A portal broken is known by its paths until closed, and opening
  another there is refused meanwhile.
- **P9. The names go first.** At a close, and at a cut as it sees it (Q15), each side deletes its
  pipe at once, whatever becomes of a command still at it: a command arriving later is told there is no such
  file, rather than left waiting at a pipe nobody serves.
- **P10. Nothing left.** Once a portal closed, by itself or asked, and after a clean stop of rat
  or of ratd: no pipe, no temporary file, no end left open.
- **P11. Each command is accounted for.** An answer about a portal that ended tells, for each
  pipe, what its command saw: the end of its input once all of it came; the end of its input
  after N bytes of a stream that was not whole; a broken pipe; or that it still waits.
- **P12. Nothing waits on a portal.** A tool call waits 50 seconds at most, the same paths again
  following the portal, and stopping rat or ratd is never delayed by one (G8). The outcome of a
  transfer rat runs is kept until told (Q18 for a close it runs none for). No byte reaches the
  model (G9).

**Outcomes.** Every answer about a portal conveys one of these states, the opening cut of Q17
aside, which tells that ratd's side is not known.

| State | When | What the agent does |
|---|---|---|
| Ready | it opened | starts its commands, calls again with the same paths |
| Done | its source ended and all of it was delivered | reads the exit statuses of its commands |
| Stopped | the command reading left before the end of the source; the command writing got a broken pipe | reads why in its terminal |
| Cut, nothing held | the link lost or the source failing, the destination a file: the file untouched, a command writing got a broken pipe | fixes the cause, opens again |
| Broken | the same, the destination a pipe: its command waits | stops that command in its terminal, then calls with `cancel` |
| Closed | `cancel` was asked | stops a command still reading, and does not trust what it made of a stream that was not whole |
| Refused | it could not open | fixes the cause; nothing was left |

**Limits.** Each is a line a finding is closed with.

- **Q1. The line in the sand.** The system gives a command reading a pipe two things: more data,
  or the end. There is no error to hand it, and deleting its pipe tells it nothing. So rat and
  ratd say what each command saw, and the agent acts in its terminal: they do not handle what
  they can not.
- **Q2. A close asked ends a command reading.** Closing its end is what tells it the data ended,
  on a stream that was not whole. The answer says so, and the description tells to stop that
  command first, then close.
- **Q3. A source pipe ends when its command closes it.** A command that failed closes it too:
  Done then delivered, and put in place, what it wrote. Its exit status tells whether that was
  all, and the answer says so.
- **Q4. Done is delivered, not used.** The stream reached the pipe and its end was closed: what
  the command made of it is its own, told by its exit status.
- **Q5. rat stopping closes its side.** A local command reading then takes what it got for the
  whole, and nobody tells it otherwise. ratd stops its terminals before its pipes, so its side
  has no such case.
- **Q6. A portal nobody closes** stays: one ready and unused, or one broken, until ratd stops. rat
  stopping ends one case only, a download ready and unused, which ratd sees leave: an upload no
  command reads is not seen (Q15), and a broken portal stays whoever leaves. No time limit.
- **Q7. Other commands at a pipe.** Two commands opening one pipe share its stream between them,
  and a command that is not the agent's may open it. Not prevented.
- **Q8. No size, no check.** A source pipe has no size: progress tells bytes, no total and no time
  left, and the change check of a source file (G1) has no meaning for it.
- **Q9. Killed.** A rat or a ratd killed leaves its pipes behind (L3).
- **Q10. Windows** (rat only): no local pipe. `local_pipe` is refused there, `remote_pipe` works.
- **Q11. File systems without pipes** (FAT, some network ones) refuse a portal, as the system
  tells it.
- **Q12. The limits of a transfer hold** for a file end, and for the link: L1 to L16.
- **Q13. Nothing between rat and ratd.** A portal's upload is answered before its body is sent
  (see The link). A proxy between the two may hold that answer until the body ended, and "ready"
  with it: a portal is promised on a direct connection, which mutual TLS makes the usual one.
- **Q14. A local pipe is where its path says.** rat makes it at the path given, under its
  transfers directory, and tells the agent its full path. Every path rat writes begins with that
  directory, and that is all it promises there: a symbolic link among the directories of the path
  is followed as the system follows it, and is the business of whoever put it there.
- **Q15. An upload's cut is seen once a command reads.** ratd reads the body of an upload only
  when a command opened its pipe, and the system tells it nothing of a client gone behind data it
  has not read. Until then the pipe stays, and a command arriving is given what ratd received: all
  of a file received whole, with its end; otherwise its beginning, then nothing, the portal
  broken. `cancel` closes it, and opening another there is refused meanwhile.
- **Q16. A pipe someone else removed.** A portal waits in the open of its pipe, which only the
  name of the pipe lets it leave. That name removed by someone else (`rm` in a terminal), `cancel`
  frees the path and answers all the same, and ratd is left one thread waiting in that open until
  it stops. Removed once a command opened the pipe, ratd is left waiting for that command instead:
  one writing is told at its second write, not its next, and one that writes no more holds a
  thread of ratd as long as it lives.
- **Q17. An opening that is not answered.** `cancel` waits for a portal still opening, ratd knowing
  it by its path only once it made it. Not answered within 50 seconds of the cancel, the opening
  is cut on rat's side and told so: whether it reached ratd is not known, and the same `cancel`
  again asks ratd.
- **Q18. A close rat has no transfer for.** A `cancel` finding no transfer in rat (rat restarted,
  a cut already told) is one request to ratd, and its answer is its telling: rat has nothing to
  keep it in. The answer lost, the harness giving up on the call, what the command at the pipe
  saw is told to nobody, and the same `cancel` again tells nothing is open there, which is true.
- **Q19. Ready and a cancel crossing.** A call made once a `cancel` was asked is not told ready.
  A call already waiting for the opening when the `cancel` comes may be: the portal did open,
  and the `cancel` then tells it closed. Both answers are true, in that order; the agent, which
  asked the `cancel`, knows which stands.
- **Q20. A cancel finding a cut not told yet.** rat tells an outcome before anything else: a
  `cancel` coming after a cut no call has told yet tells the cut, and that the portal is not
  closed on ratd's side; the same `cancel` again closes it (Q18).

**For the descriptions of the tools**, not promises: a command at a local pipe runs in the
background of the agent's shell, which otherwise waits on it; to stop a stream midway, stop the
command reading first, then cancel.

## How a portal waits

Opening one end of a pipe waits until a command opens the other: a portal sits in that open, and
needs to know nothing more. It moves what comes through; until then it waits, to be closed or
used. So no byte is ever written into a pipe nobody reads, and Done is never told of a stream
nobody took. Rejected: taking our end at once and then looking for a command at the other, which
writes into an empty pipe and no longer sees a stream end. The price: a progress answer says
that nothing came through yet, not whether a command is there.

A close takes a side out of that open by opening the other end itself for an instant, then
closing both, the name deleted first. A command arriving at that very instant gets what a close
asked gives any command (P6, Q2).

## The link

One HTTP request is the portal: opened as the portal opens, alive for as long as it is, ended as
it closes. Its headers say ready, and only its body waits. A download does so already: ratd
answers its headers, then the body comes as the command writes. An upload does the same the
other way: ratd answers its headers before it reads any byte of the body, which then waits for
the command reading. The status being sent by then, how the portal ended is told after the
stream, in a trailer, as a download tells a failure today.

Measured on 2026-11-25 with Go's server and client, HTTP/1.1, without TLS: the headers of the
answer reach the client at once, 3 MiB of body sent after them arrive whole, and the trailer
tells the outcome. Two conditions: the server allows both directions at once
(`EnableFullDuplex`), and the request does not ask for a go-ahead (`Expect: 100-continue`), which
a final answer cancels, the client then sending no body at all.

For an upload this means an answer that starts before its request was sent whole. HTTP/1.1
allows it, and few programs expect it: it is the one unusual thing a portal does on the wire,
and the reason it is written here at length. Rejected: an interim answer (`100 Continue`, as the
go-ahead of a file upload today, or `102`) saying ready, the real answer coming at the end. It is
the common path, but it gives an upload and a download two different ways to say ready, where
"the headers say ready, the body waits" is one rule for both. It stays the way back if this one
meets an obstacle. Confirmed on 2026-11-25 over mutual TLS, on the real pair: ready comes back
before ratd read any byte of the body, and 8 MiB sent after it arrive whole.

## To confirm

No design question is left open. One fact is still to check on the real pair, rat and ratd over
TLS (the other, an upload answered before its body is sent, is confirmed: see The link):

- that a side touching a connection whose other end is gone gets an error in a bounded time. No
  timeout and no heartbeat of ours: a portal waits to be used or closed, and a side needs to know
  the other is gone only when it has something to do with it, which is when the connection tells.

Measured on 2026-11-25, on macOS and Linux, over TLS, for Q15: the context of a request ends
when its client leaves only if something reads its body; asked about the socket without reading
it, the system tells a client that reset the connection, and one that closed it once all it sent
was received, but not one that closed it while sending a file larger than the connection holds,
the notice of the close travelling behind that data. Rejected with it: a buffer read ahead of the
command, which sees the cut of a file smaller than itself alone; rat asking ratd to close at its
own cut, one more message between the two.

Measured so far, on macOS and Linux unless said: a pipe deleted under a command reading it
changes nothing for that command, which ends with status 0 on what it got once the other end
closes (macOS, 2026-11-24); a command started after the deletion fails with "No such file or
directory"; a pipe removed once both ends are open keeps working.
