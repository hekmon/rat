//go:build !linux

package main

import "io/fs"

// pinPipe does nothing but on Linux (see portals_linux.go): the file systems of the others (APFS
// on macOS) give the inode number of a file removed to no other file, and the pipe ratd made is
// told from what takes its path by its numbers alone.
func pinPipe(string, fs.FileInfo) (unpin func(), err error) {
	return func() {}, nil
}
