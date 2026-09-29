package tmux

import (
	"context"
	"errors"
	"os/exec"
	"strings"
	"testing"
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
	c, err := New(context.Background(), "test-"+tenant)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.StopServer(context.Background()) })
	return c
}

func TestNewValidTenants(t *testing.T) {
	for _, tenant := range []string{"", "alice", "team_1-prod", "-L", strings.Repeat("a", tenantMaxLen)} {
		if _, err := New(context.Background(), tenant); err != nil {
			t.Errorf("tenant %q: %v", tenant, err)
		}
	}
}

func TestNewInvalidTenants(t *testing.T) {
	for _, tenant := range []string{"no/such/dir", "../../etc", "..", "a b", "é", "a\x00b", strings.Repeat("a", tenantMaxLen+1)} {
		if _, err := New(context.Background(), tenant); !errors.Is(err, ErrInvalidTenant) {
			t.Errorf("tenant %q: expected ErrInvalidTenant, got %v", tenant, err)
		}
	}
}
