package names

import (
	"errors"
	"strings"
	"testing"
)

// TestCheck guards the naming rule: plain names only, as '/' and ".." would move a tenant socket
// out of the tmux directory, and ':', '.' and '=' change what a tmux target reaches. A name is
// required, and bounded for the socket path.
func TestCheck(t *testing.T) {
	for _, name := range []string{"main", "build_1", "team_1-prod", "-L", strings.Repeat("a", maxLen)} {
		if err := Check(name); err != nil {
			t.Errorf("name %q: %v", name, err)
		}
	}
	for _, name := range []string{"", "no/such/dir", "../../etc", "..", "a:b", "s.1", "=s", "a b", "é", "a\x00b",
		strings.Repeat("a", maxLen+1)} {
		if err := Check(name); !errors.Is(err, ErrInvalid) {
			t.Errorf("name %q: expected ErrInvalid, got %v", name, err)
		}
	}
}

// TestCheckWindow guards the rule of window names: plain names, but not made of digits only,
// which tmux reads as a window index in a target, even after '='. Digits with anything else, a
// leading '-' included, read as a name.
func TestCheckWindow(t *testing.T) {
	for _, name := range []string{"main", "1a", "a1", "-1", "build_2"} {
		if err := CheckWindow(name); err != nil {
			t.Errorf("name %q: %v", name, err)
		}
	}
	for _, name := range []string{"0", "1", "07", "123", "", "a:b"} {
		if err := CheckWindow(name); !errors.Is(err, ErrInvalid) {
			t.Errorf("name %q: expected ErrInvalid, got %v", name, err)
		}
	}
}

// TestCheckSession guards the rule of session names: plain names, but not the name of a tmux client
// (client-<pid> for a client without a terminal, a terminal under /dev: tty1, ttyS0, ttys003 on
// macOS, console), which tmux reaches the session of when no session has the name asked, even after
// '='. Names only looking alike (client-a, mytty, TTY1, consoles) are names.
func TestCheckSession(t *testing.T) {
	for _, name := range []string{"alice", "client", "client-", "client-a", "client-1a", "Client-1", "mytty", "TTY1",
		"consoles", "con"} {
		if err := CheckSession(name); err != nil {
			t.Errorf("name %q: %v", name, err)
		}
	}
	for _, name := range []string{"client-1", "client-4242", "tty", "tty1", "ttyS0", "ttyUSB0", "ttys003", "console", "",
		"a:b"} {
		if err := CheckSession(name); !errors.Is(err, ErrInvalid) {
			t.Errorf("name %q: expected ErrInvalid, got %v", name, err)
		}
	}
}
