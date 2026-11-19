// Package fifo streams through named pipes (FIFOs), for the transfers of rat whose end is a pipe
// rather than a file: a command writes into a pipe the transfer reads (tar -cf - . > ~/data.pipe),
// or reads from a pipe the transfer writes into, with no file in between, as a command piped into
// nc would. The agent makes the pipes (mkfifo) and runs the commands, in its terminals or its own
// shell: a transfer only opens its end of a pipe, never creating nor removing one.
//
// Opening an end waits for a command to open the other end, as a command opening a pipe waits
// (see [Open]): a transfer may start before its commands, and its commands before it. The wait
// ends when the command opens its end, or when the transfer gives up: Open then opens the pipe
// itself, read and write, which ends its own open, and closes both, the pipe left as it was.
// Rejected: opening without blocking, after which nothing tells a command not there yet from a
// command gone (a read gives the end of the data at once, on Linux and macOS alike).
//
// Reads and writes go through a loop of their own, never blocking: it waits for the pipe with
// select, a quarter of a second at most, and checks in between whether its transfer gave up. Go
// keeps named pipes out of its poller on macOS (golang/go#24164: kqueue does not tell a reader
// that the last writer left), so that a read or a write blocked on a pipe takes no deadline there,
// and Close leaves it blocked until the command at the other end acts. select rather than poll: on
// macOS, poll and kqueue miss what a command blocked in write(2) writes (dd, tar), and a command
// leaving the pipe, where select tells both at once, as Linux does with either. Reading 2 MB that
// dd wrote 10240 bytes at a time took 40 s with poll, 159 waits out of 351 running their whole
// quarter of a second, 33 s with kqueue, and 3 ms with select. Beyond the 1024 descriptors select
// can watch, poll is used, and a wait on macOS may then run its whole quarter of a second. One
// loop on both systems, rather than Go's own reads and deadlines on Linux: what is tested on one is
// what runs on the other.
//
// Giving up a read or a write leaves the pipe open, held: the command at the other end waits,
// rather than taking the end of the pipe for the end of its data (a psql reading a dump would run
// half of it). Closing it is the call of the caller ([Pipe.Close]), the transfers of rat holding
// a pipe after a cut until the agent cancels. Nothing watches a held pipe for its command
// leaving: the agent closes what it opened, explicitly.
//
// A write once the command reading the pipe left fails with EPIPE: Go ignores SIGPIPE for files
// other than the standard output and error, the process going on.
//
// Measured with Go 1.27.1 on macOS (Darwin 27.2) and Linux 7.0, both arm64: a pipe holds 8 KiB on
// macOS and 64 KiB on Linux, and carries 1.25 GB/s on macOS and 1.9 GB/s on Linux between dd and
// cat (a plain pipe: 6 and 2 GB/s); between the loops of this package, writing 32 KiB at a time
// as io.Copy does, 890 MB/s on macOS and 8 GB/s on Linux (BenchmarkStream): far beyond what a
// transfer gets from the network.
//
// Named pipes are Unix's: elsewhere (Windows), Open fails with errors.ErrUnsupported, rat keeping
// the files of its transfers directory to regular files there.
package fifo
