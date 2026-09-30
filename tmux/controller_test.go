package tmux

import (
	"context"
	"errors"
	"os/exec"
	"testing"

	"github.com/hekmon/rat/tmux/names"
)

// requireTmux fails the test if tmux is not installed: rat can not work without it,
// and a test run that tested nothing must not pass.
func requireTmux(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("tmux"); err != nil {
		t.Fatal("tmux is required to run the tests:", err)
	}
}

// newTestController returns a controller on its own socket.
// The server is stopped (if still running) when the test ends.
func newTestController(t *testing.T, tenant string) *Controller {
	t.Helper()
	requireTmux(t)
	c, err := New("test-" + tenant)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.StopServer(context.Background()) })
	return c
}

// TestNewTenants guards that a tenant is the default one (empty) or follows the naming rule
// (package names), which keeps its socket in the tmux directory.
func TestNewTenants(t *testing.T) {
	for _, tenant := range []string{"", "alice", "-L"} {
		if _, err := New(tenant); err != nil {
			t.Errorf("tenant %q: %v", tenant, err)
		}
	}
	for _, tenant := range []string{"no/such/dir", "../../etc", ".."} {
		if _, err := New(tenant); !errors.Is(err, names.ErrInvalid) {
			t.Errorf("tenant %q: expected names.ErrInvalid, got %v", tenant, err)
		}
	}
}

// TestTmuxArg guards the protection of arguments ending with ';', which tmux would otherwise read
// as the end of its command.
func TestTmuxArg(t *testing.T) {
	for arg, expected := range map[string]string{
		"plain": "plain", "a;b": "a;b", ";": `\;`, ";;": `;\;`, "echo a;": `echo a\;`, `a\;`: `a\\;`,
	} {
		if got := tmuxArg(arg); got != expected {
			t.Errorf("tmuxArg(%q) = %q, expected %q", arg, got, expected)
		}
	}
}
