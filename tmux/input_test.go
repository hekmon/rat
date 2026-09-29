package tmux

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestSendText guards that text is typed literally (key names and a leading '-' included), and
// that Enter is only pressed when asked.
func TestSendText(t *testing.T) {
	c := startTestServer(t, "sendtext")
	ctx := context.Background()
	if err := c.NewSession(ctx, "s"); err != nil {
		t.Fatal(err)
	}
	// without Enter: typed on the prompt, not run
	if err := c.SendText(ctx, "s", FirstWindow, "echo -n Enter C-c; echo ran", false); err != nil {
		t.Fatal(err)
	}
	eventually(t, "text typed", screenContains(c, "s", FirstWindow, "echo -n Enter C-c; echo ran"))
	if snapshot, _ := c.Capture(ctx, "s", FirstWindow, 0); strings.Contains(snapshot.Content, "\nran") {
		t.Fatalf("text ran without Enter:\n%s", snapshot.Content)
	}
	if err := c.SendText(ctx, "s", FirstWindow, "", true); err != nil {
		t.Fatal(err)
	}
	eventually(t, "text run", screenContains(c, "s", FirstWindow, "Enter C-cran"))
	// leading '-', and text with Enter in one call
	if err := c.SendText(ctx, "s", FirstWindow, "-dash-", false); err != nil {
		t.Fatal(err)
	}
	eventually(t, "text starting with '-' typed", screenContains(c, "s", FirstWindow, "-dash-"))
	if err := c.SendKeys(ctx, "s", FirstWindow, "C-u"); err != nil {
		t.Fatal(err)
	}
	if err := c.SendText(ctx, "s", FirstWindow, "echo one-call", true); err != nil {
		t.Fatal(err)
	}
	eventually(t, "text and Enter in one call", screenContains(c, "s", FirstWindow, "\none-call"))
}

// TestSendKeys guards that keys act as keys (C-c interrupts the foreground command), and that
// an unknown key name is refused rather than typed as text.
func TestSendKeys(t *testing.T) {
	c := startTestServer(t, "sendkeys")
	ctx := context.Background()
	if err := c.NewSession(ctx, "s"); err != nil {
		t.Fatal(err)
	}
	if err := c.SendText(ctx, "s", FirstWindow, "sleep 30", true); err != nil {
		t.Fatal(err)
	}
	eventually(t, "sleep in the foreground", foregroundIs(c, "s", FirstWindow, "sleep"))
	if err := c.SendKeys(ctx, "s", FirstWindow, "Ctrl-C"); !errors.Is(err, ErrInvalidKey) {
		t.Errorf("expected ErrInvalidKey, got %v", err)
	}
	if err := c.SendKeys(ctx, "s", FirstWindow, "C-c"); err != nil {
		t.Fatal(err)
	}
	eventually(t, "sleep interrupted", foregroundIs(c, "s", FirstWindow, "bash"))
	for _, key := range []string{"Enter", "Escape", "Up", "F12", "M-x", "C-S-Left", "y", "^"} {
		if !keyFormat.MatchString(key) {
			t.Errorf("key %q refused", key)
		}
	}
	for _, key := range []string{"", "Ctrl-C", "ab", "F13", "C-", "enter"} {
		if keyFormat.MatchString(key) {
			t.Errorf("key %q accepted", key)
		}
	}
}

// TestUTF8 guards that non ASCII text survives the whole chain (typed, run, captured) and in
// paths, even when rat runs without a UTF-8 locale, as services often do.
func TestUTF8(t *testing.T) {
	t.Setenv("LANG", "")
	t.Setenv("LC_CTYPE", "")
	t.Setenv("LC_ALL", "C")
	c := startTestServer(t, "utf8")
	ctx := context.Background()
	dir := filepath.Join(t.TempDir(), "café ✓")
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	resolved, err := filepath.EvalSymlinks(dir)
	if err != nil {
		t.Fatal(err)
	}
	if err = c.NewSession(ctx, "s"); err != nil {
		t.Fatal(err)
	}
	if err = c.SendText(ctx, "s", FirstWindow, "cd '"+dir+"' && echo 'ok → é ✓'", true); err != nil {
		t.Fatal(err)
	}
	eventually(t, "UTF-8 output", screenContains(c, "s", FirstWindow, "\nok → é ✓"))
	eventually(t, "UTF-8 path", func() (bool, string) {
		w, err := c.Window(ctx, "s", FirstWindow)
		if err != nil {
			return false, err.Error()
		}
		return w.Path == resolved, w.Path
	})
}

// TestSendSemicolon guards that a text or key ending with ';' is typed as is: tmux would read that
// ';' as the end of its command and drop it.
func TestSendSemicolon(t *testing.T) {
	c := startTestServer(t, "semicolon")
	ctx := context.Background()
	if err := c.NewSession(ctx, "s"); err != nil {
		t.Fatal(err)
	}
	// each text starts with '#' so that what is typed is easy to find on the prompt line
	for _, text := range []string{"#echo a;", "#;", "#;;", `#a\;`, "#a;b"} {
		if err := c.SendKeys(ctx, "s", FirstWindow, "C-u"); err != nil {
			t.Fatal(err)
		}
		if err := c.SendText(ctx, "s", FirstWindow, text, false); err != nil {
			t.Fatal(err)
		}
		eventually(t, "text "+text+" typed", promptEndsWith(c, "s", FirstWindow, text))
	}
	if err := c.SendKeys(ctx, "s", FirstWindow, "C-u", "#", ";"); err != nil {
		t.Fatal(err)
	}
	eventually(t, "key ; pressed", promptEndsWith(c, "s", FirstWindow, "#;"))
}

// promptEndsWith returns a condition true once the last line of window ends with text.
func promptEndsWith(c *Controller, session, window, text string) func() (bool, string) {
	return func() (bool, string) {
		snapshot, err := c.Capture(context.Background(), session, window, 0)
		if err != nil {
			return false, err.Error()
		}
		lines := strings.Split(snapshot.Content, "\n")
		last := lines[len(lines)-1]
		return strings.HasSuffix(last, text), last
	}
}
