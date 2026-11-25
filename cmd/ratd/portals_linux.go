package main

import (
	"errors"
	"io/fs"
	"syscall"

	"golang.org/x/sys/unix"
)

// pinPipe keeps the pipe at path, looked at as made, the only file of its device and inode numbers
// for as long as its portal lives, until the function it returns is called. ratd tells the pipe it
// made from what someone else may put at its path by those numbers (os.SameFile), and Linux gives
// the numbers of a file removed to the next one created: a file put at the path right after the
// pipe was removed would pass for it, be sent as the stream of a download, written a byte and
// deleted (measured on ext4 under overlayfs: the same inode number three times out of three, and
// another once something held the pipe). A descriptor naming the pipe without opening it (O_PATH)
// holds it: its numbers are given to no other file meanwhile, and it is no end of the pipe, the
// opens waiting at it not seeing it. Once the pump waits in its open, or opened its end, that holds
// the pipe as well: the pin is for the instant before, and for a pump the portal no longer waits
// for. One gap is left, which nothing closes, no system call making a pipe and returning a
// descriptor of it: the path taken between the pipe being made and this call, which fails then,
// what was opened not being the pipe looked at.
func pinPipe(path string, made fs.FileInfo) (unpin func(), err error) {
	fd, err := unix.Open(path, unix.O_PATH|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, &fs.PathError{Op: "open", Path: path, Err: err}
	}
	var pinned unix.Stat_t
	want, ok := made.Sys().(*syscall.Stat_t)
	if err := unix.Fstat(fd, &pinned); err != nil || !ok || pinned.Dev != want.Dev || pinned.Ino != want.Ino {
		_ = unix.Close(fd)
		return nil, &fs.PathError{Op: "open", Path: path, Err: errors.New("not the pipe made")}
	}
	return func() { _ = unix.Close(fd) }, nil
}
