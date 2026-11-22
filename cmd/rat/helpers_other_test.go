//go:build !unix

package main

import "io/fs"

// The helpers of helpers_unix_test.go on the other systems rat is released for (Windows), where the
// tests build, CI vetting them there, but do not run: they run on Unix alone.

// umask returns 0: the modes of new files go through no umask outside Unix.
func umask() fs.FileMode {
	return 0
}
