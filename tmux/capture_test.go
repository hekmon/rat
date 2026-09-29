package tmux

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
)

// screenContains returns a condition true once the screen of window contains text.
func screenContains(c *Controller, session, window, text string) func() (bool, string) {
	return func() (bool, string) {
		snapshot, err := c.Capture(context.Background(), session, window, 0)
		if err != nil {
			return false, err.Error()
		}
		return strings.Contains(snapshot.Content, text), snapshot.Content
	}
}

// typeCommand runs command in window with raw tmux commands, independently of the input methods.
func typeCommand(t *testing.T, c *Controller, session, window, command string) {
	t.Helper()
	tg := target(session, window)
	if _, err := c.run(context.Background(), "send-keys", "-t", tg, "-l", command, ";", "send-keys", "-t", tg, "Enter"); err != nil {
		t.Fatal(err)
	}
}

// TestCapture guards what a capture returns: the screen without trailing blanks by default, and
// the scrollback above it on demand, bounded by what exists and reported in the snapshot.
func TestCapture(t *testing.T) {
	c := startTestServer(t, "capture")
	ctx := context.Background()
	if err := c.NewSession(ctx, "s"); err != nil {
		t.Fatal(err)
	}
	typeCommand(t, c, "s", FirstWindow, "clear; seq 1 100; printf 'trailing   \\n'")
	eventually(t, "output displayed", screenContains(c, "s", FirstWindow, "trailing"))
	snapshot, err := c.Capture(ctx, "s", FirstWindow, 0)
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.ExtraLines != 0 || snapshot.FullScreen {
		t.Errorf("unexpected snapshot properties: %+v", snapshot)
	}
	if strings.Contains(snapshot.Content, "trailing ") || strings.HasSuffix(snapshot.Content, "\n") {
		t.Errorf("trailing blanks not removed: %q", snapshot.Content)
	}
	if strings.Contains(snapshot.Content, "\n50\n") {
		t.Errorf("screen of %s should not show line 50:\n%s", serverDefaultSize, snapshot.Content)
	}
	// as many extra lines as requested
	if snapshot, err = c.Capture(ctx, "s", FirstWindow, 5); err != nil || snapshot.ExtraLines != 5 {
		t.Errorf("expected 5 extra lines, got %d, %v", snapshot.ExtraLines, err)
	}
	// no more than the scrollback holds
	w, err := c.Window(ctx, "s", FirstWindow)
	if err != nil {
		t.Fatal(err)
	}
	if snapshot, err = c.Capture(ctx, "s", FirstWindow, 100000); err != nil {
		t.Fatal(err)
	}
	if snapshot.ExtraLines != w.Scrollback || w.Scrollback >= 100000 {
		t.Errorf("expected the %d rows of scrollback, got %d", w.Scrollback, snapshot.ExtraLines)
	}
	for _, line := range []string{"1", "50", "100"} {
		if !strings.Contains("\n"+snapshot.Content+"\n", fmt.Sprintf("\n%s\n", line)) {
			t.Errorf("extra lines capture misses line %s", line)
		}
	}
	if snapshot, err = c.Capture(ctx, "s", FirstWindow, -1); err != nil || snapshot.ExtraLines != 0 {
		t.Errorf("negative extra lines: expected none, got %d, %v", snapshot.ExtraLines, err)
	}
	if _, err = c.Capture(ctx, "s", "nope", 0); !errors.Is(err, ErrWindowNotFound) {
		t.Errorf("expected ErrWindowNotFound, got %v", err)
	}
}

// TestCaptureFullScreen guards that the scrollback is never included under a full-screen program:
// it belongs to the terminal before the program started, and would be mistaken for its output.
func TestCaptureFullScreen(t *testing.T) {
	c := startTestServer(t, "capturefullscreen")
	ctx := context.Background()
	if err := c.NewSession(ctx, "s"); err != nil {
		t.Fatal(err)
	}
	typeCommand(t, c, "s", FirstWindow, "seq 1 100; seq 500 600 | less")
	eventually(t, "less in full screen", func() (bool, string) {
		w, err := c.Window(ctx, "s", FirstWindow)
		if err != nil {
			return false, err.Error()
		}
		return w.FullScreen, fmt.Sprintf("%+v", w)
	})
	snapshot, err := c.Capture(ctx, "s", FirstWindow, 50)
	if err != nil {
		t.Fatal(err)
	}
	if !snapshot.FullScreen || snapshot.ExtraLines != 0 || !strings.HasPrefix(snapshot.Content, "500\n") {
		t.Errorf("expected the screen of less only, got %+v", snapshot)
	}
}
