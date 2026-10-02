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
