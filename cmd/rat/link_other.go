//go:build !windows

package main

import (
	"errors"
	"syscall"
)

// linkUnsupported tells whether err, of a link, is that of a file system without hard links, a
// download then renamed into place (see place): FAT refuses with EPERM, FUSE without link (sshfs)
// with ENOSYS, others with EMLINK, ENOTSUP or EOPNOTSUPP, ENOSYS and the last two told by
// errors.ErrUnsupported.
func linkUnsupported(err error) bool {
	return errors.Is(err, syscall.EPERM) || errors.Is(err, syscall.EMLINK) || errors.Is(err, errors.ErrUnsupported)
}
