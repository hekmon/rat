//go:build unix

package main

import (
	"io/fs"
	"syscall"
)

// The helpers of the tests that need Unix (see helpers_other_test.go).

// umask returns the umask of the process, which the modes of new files go through.
func umask() fs.FileMode {
	mask := syscall.Umask(0)
	syscall.Umask(mask)
	return fs.FileMode(mask)
}
