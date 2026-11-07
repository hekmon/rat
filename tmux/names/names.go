package names

import (
	"errors"
	"fmt"
	"regexp"
	"strings"
)

// ErrInvalid is returned for a name that is not a plain name.
var ErrInvalid = errors.New("invalid name")

// maxLen bounds names. For tenants, it keeps the socket path (/tmp/tmux-<uid>/rat-<tenant>) well
// under the unix socket path limit.
const maxLen = 32

// format only allows plain names: see the package documentation for what other characters would
// do in tmux.
var format = regexp.MustCompile(`^[a-zA-Z0-9_-]+$`)

// Check returns an error wrapping ErrInvalid if name is not a plain name: only letters, digits,
// '_' and '-' are allowed, up to 32 characters. The empty name is invalid.
func Check(name string) error {
	if len(name) > maxLen || !format.MatchString(name) {
		return fmt.Errorf("%w %q: only letters, digits, '_' and '-' are allowed (up to %d characters)",
			ErrInvalid, name, maxLen)
	}
	return nil
}

// CheckWindow returns an error wrapping ErrInvalid if name is not a plain name (see Check), or is
// made of digits only: tmux reads the window part of a target as an index first, even after '=',
// and a window named 1 would be reached as the window at index 1, whatever its name.
func CheckWindow(name string) error {
	if err := Check(name); err != nil {
		return err
	}
	if strings.Trim(name, "0123456789") == "" {
		return fmt.Errorf("%w %q: a window name can not be made of digits only, which tmux reads as a window index",
			ErrInvalid, name)
	}
	return nil
}

// clientName matches the names tmux may give a client: client-<pid> for a client without a
// terminal, and the terminals directly under /dev (tty1, ttyS0, ttys003 on macOS, console), which
// it matches without /dev/. Terminals under a directory of /dev (pts/3) can not be plain names.
var clientName = regexp.MustCompile(`^(?:client-[0-9]+|tty.*|console)$`)

// CheckSession returns an error wrapping ErrInvalid if name is not a plain name (see Check), or may
// be the name of a tmux client: when no session has the name a target asks for, tmux tries it as
// the name of a client, even after '=', and reaches the session of that client. A session missing
// (its client not called yet) would have its commands act on the windows of another session. Not
// every name a terminal can have is refused (hvc0, on some virtual machines), only those of the
// terminals humans attach from, and of clients without one.
func CheckSession(name string) error {
	if err := Check(name); err != nil {
		return err
	}
	if clientName.MatchString(name) {
		return fmt.Errorf("%w %q: a session name can not be the name tmux gives a client (client-<pid>, or a "+
			"terminal: tty…, console), whose session tmux would reach instead", ErrInvalid, name)
	}
	return nil
}
