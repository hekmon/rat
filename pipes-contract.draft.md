# What a portal promises (draft)

Draft of 2026-11-24, from the design settled that day, every line of it agreed. Nothing of it is
implemented yet: it is written before the code, which will be checked against it. Its final place
is `cmd/ratd/README.md`, next to "What a transfer promises", once the portals exist.

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
  touches nothing.
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
  every option it has today.
- **P8. A close packs up.** `cancel` closes the portal of its paths: each side closes its end and
  deletes its pipe. The paths alone name it: it works after a cut, from a rat started again, from
  another rat of the session. A portal broken is known by its paths until closed, and opening
  another there is refused meanwhile.
- **P9. The names go first.** At a cut and at a close, each side deletes its pipe at once,
  whatever becomes of a command still at it: a command arriving later is told there is no such
  file, rather than left waiting at a pipe nobody serves.
- **P10. Nothing left.** Once a portal closed, by itself or asked, and after a clean stop of rat
  or of ratd: no pipe, no temporary file, no end left open.
- **P11. Each command is accounted for.** An answer about a portal that ended tells, for each
  pipe, what its command saw: the end of its input once all of it came; the end of its input
  after N bytes of a stream that was not whole; a broken pipe; or that it still waits.
- **P12. Nothing waits on a portal.** A tool call waits 50 seconds at most, the same paths again
  following the portal, and stopping rat or ratd is never delayed by one (G8). An outcome is kept
  until told. No byte reaches the model (G9).

**Outcomes.** Every answer about a portal conveys one of these states.

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
- **Q6. A portal nobody closes** stays: one ready and unused, or one broken, until rat or ratd
  stops. No time limit.
- **Q7. Other commands at a pipe.** Two commands opening one pipe share its stream between them,
  and a command that is not the agent's may open it. Not prevented.
- **Q8. No size, no check.** A source pipe has no size: progress tells bytes, no total and no time
  left, and the change check of a source file (G1) has no meaning for it.
- **Q9. Killed.** A rat or a ratd killed leaves its pipes behind (L3).
- **Q10. Windows** (rat only): no local pipe. `local_pipe` is refused there, `remote_pipe` works.
- **Q11. File systems without pipes** (FAT, some network ones) refuse a portal, as the system
  tells it.
- **Q12. The limits of a transfer hold** for a file end, and for the link: L1 to L16.

**For the descriptions of the tools**, not promises: a command at a local pipe runs in the
background of the agent's shell, which otherwise waits on it; to stop a stream midway, stop the
command reading first, then cancel.

## Not decided yet

Mechanisms, to settle with measurements before any code:

- how a side learns that a command opened its pipe, and tells "still waits for the command at P"
  from "N bytes through";
- how a side gives up waiting for a command at a close, without handing it an end;
- whether ratd sees its client leave while a portal is ready and idle;
- how rat makes a pipe within its transfers directory (`os.Root` has no call for it).

Measured so far, on macOS and Linux unless said: a pipe deleted under a command reading it
changes nothing for that command, which ends with status 0 on what it got once the other end
closes (macOS, 2026-11-24); a command started after the deletion fails with "No such file or
directory"; a pipe removed once both ends are open keeps working.
