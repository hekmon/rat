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
