package main

import (
	"errors"
	"syscall"
)

// errorInvalidFunction is ERROR_INVALID_FUNCTION, which a link gives on a file system without hard
// links (FAT, exFAT): Go turns the status Windows answers (STATUS_INVALID_DEVICE_REQUEST) into it.
const errorInvalidFunction = syscall.Errno(1)

// linkUnsupported tells whether err, of a link, is that of a file system without hard links, a
// download then renamed into place (see place). Windows gives none of the errors of Unix (Go's
// syscall.EPERM is a value of its own there): ERROR_INVALID_FUNCTION, or ERROR_NOT_SUPPORTED and
// ERROR_CALL_NOT_IMPLEMENTED, told by errors.ErrUnsupported.
func linkUnsupported(err error) bool {
	return errors.Is(err, errorInvalidFunction) || errors.Is(err, errors.ErrUnsupported)
}
