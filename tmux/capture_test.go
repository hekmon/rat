package tmux

import (
	"context"
	"errors"
	"fmt"
	"math"
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

// TestCapture guards what a capture returns on the normal screen: the screen without trailing
// blanks nor cursor by default, and the scrollback above it on demand, bounded by what exists and
// reported in the snapshot.
func TestCapture(t *testing.T) {
	c := startTestServer(t, "capture")
	ctx := context.Background()
	if err := c.NewSession(ctx, "s"); err != nil {
		t.Fatal(err)
	}
	typeCommand(t, c, "s", FirstWindow, "clear; seq 1 100; printf 'trailing   \\n'")
	// "\ntrailing": the output, at the start of a line, not the command line typed, which also
	// contains "trailing"
	eventually(t, "output displayed", screenContains(c, "s", FirstWindow, "\ntrailing"))
	snapshot, err := c.Capture(ctx, "s", FirstWindow, 0)
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.ExtraLines != 0 || snapshot.FullScreen || snapshot.Cursor != (Position{}) {
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
	// no more than the scrollback holds, even beyond what tmux accepts (a C int), for which it
	// would capture no scrollback at all
	w, err := c.Window(ctx, "s", FirstWindow)
	if err != nil {
		t.Fatal(err)
	}
	if snapshot, err = c.Capture(ctx, "s", FirstWindow, math.MaxInt); err != nil {
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
	if _, err = c.Capture(ctx, "nope", FirstWindow, 0); !errors.Is(err, ErrSessionNotFound) {
		t.Errorf("expected ErrSessionNotFound, got %v", err)
	}
}

// TestCaptureFullScreen guards that the scrollback is never included under a full-screen program:
// it belongs to the terminal before the program started, and would be mistaken for its output.
// It needs the real less: BusyBox's does not use the alternate screen (TestMinimumTmux installs it).
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

// TestCaptureFullScreenAsDisplayed guards that a full-screen program is captured as displayed,
// with its cursor: rows the terminal wrapped are not joined and empty rows at the top are kept, so
// that row N of the content is row N of the screen, where the cursor is told. A hidden cursor is
// not told, as a human sees none. The program is drawn with escape sequences, to control the
// screen exactly: the alternate screen, rows 1 and 2 left empty, a line of 250 characters wrapped
// by the 200 columns terminal, trailing spaces, and the cursor moved below the text.
func TestCaptureFullScreenAsDisplayed(t *testing.T) {
	c := startTestServer(t, "capturedisplayed")
	ctx := context.Background()
	if err := c.NewSession(ctx, "s"); err != nil {
		t.Fatal(err)
	}
	typeCommand(t, c, "s", FirstWindow,
		`printf '\033[?1049h\033[3;1H%s  \033[6;10H' "$(printf '%250s' '' | tr ' ' a)"; sleep 30`)
	cursorMoved := func() (bool, string) {
		snapshot, err := c.Capture(ctx, "s", FirstWindow, 50)
		if err != nil {
			return false, err.Error()
		}
		return snapshot.Cursor == Position{Row: 6, Column: 10}, fmt.Sprintf("%+v", snapshot)
	}
	eventually(t, "cursor moved", cursorMoved)
	snapshot, err := c.Capture(ctx, "s", FirstWindow, 50)
	if err != nil {
		t.Fatal(err)
	}
	// the cursor on row 6 is below the last line: the empty rows at the end are removed
	content := "\n\n" + strings.Repeat("a", 200) + "\n" + strings.Repeat("a", 50)
	expected := Snapshot{
		Content:     content,
		FullScreen:  true,
		ScreenBytes: len(content),
		Cursor:      Position{Row: 6, Column: 10},
	}
	if snapshot != expected {
		t.Errorf("expected the screen as displayed:\n%+v\ngot:\n%+v", expected, snapshot)
	}

	if err = c.NewWindow(ctx, "s", "hidden"); err != nil {
		t.Fatal(err)
	}
	typeCommand(t, c, "s", "hidden", `printf '\033[?1049h\033[?25l\033[3;4Hhidden'; sleep 30`)
	eventually(t, "full-screen program drawn", screenContains(c, "s", "hidden", "hidden"))
	if snapshot, err = c.Capture(ctx, "s", "hidden", 0); err != nil || !snapshot.FullScreen || snapshot.Cursor != (Position{}) {
		t.Errorf("expected a full-screen program with no cursor, got %+v, %v", snapshot, err)
	}
}

// TestCaptureScreenBytes guards the size of the screen alone, on the normal screen, where a
// capture with scrollback can not tell it: a line of 100000 characters, wrapped over the screen
// and the scrollback, is a single line of the content. The screen alone is the same whatever the
// scrollback included, and its size counts its rows as displayed: the joined screen differs by the
// new lines of the rows joined.
func TestCaptureScreenBytes(t *testing.T) {
	c := startTestServer(t, "capturescreenbytes")
	ctx := context.Background()
	if err := c.NewSession(ctx, "s"); err != nil {
		t.Fatal(err)
	}
	typeCommand(t, c, "s", FirstWindow, `head -c 100000 /dev/zero | tr '\0' x; echo; printf 'done\n'`)
	eventually(t, "output displayed", screenContains(c, "s", FirstWindow, "\ndone\n"))
	screen, err := c.Capture(ctx, "s", FirstWindow, 0)
	if err != nil {
		t.Fatal(err)
	}
	withScrollback, err := c.Capture(ctx, "s", FirstWindow, 1000)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(withScrollback.Content, strings.Repeat("x", 100000)+"\n") || withScrollback.ScreenBytes != screen.ScreenBytes {
		t.Errorf("expected the long line whole, and the size of the screen alone (%d), got %d:\n%s", screen.ScreenBytes,
			withScrollback.ScreenBytes, withScrollback.Content)
	}
	// the screen: the end of the long line on 22 rows, done, and the prompt
	firstLine, _, _ := strings.Cut(screen.Content, "\n")
	if firstLine != strings.Repeat("x", 22*200) || screen.ScreenBytes != len(screen.Content)+21 {
		t.Errorf("expected 22 rows of the long line joined, and 21 more new lines in the screen alone, got %d bytes for:\n%s",
			screen.ScreenBytes, screen.Content)
	}
}

// TestParseCaptureState guards the reading of the pane state captured with the screen: the
// scrollback bounds the extra lines on the normal screen only, the cursor, counted from 1, is
// only told under a full-screen program showing it, and the height of the screen is told apart.
func TestParseCaptureState(t *testing.T) {
	for _, tc := range []struct {
		state      string
		extraLines int
		expected   Snapshot
	}{
		{"0 42 1 5 23 24", 10, Snapshot{ExtraLines: 10}},
		{"0 42 1 5 23 24", 100, Snapshot{ExtraLines: 42}},
		{"1 42 1 9 4 24", 10, Snapshot{FullScreen: true, Cursor: Position{Row: 5, Column: 10}}},
		{"1 42 0 9 4 24", 10, Snapshot{FullScreen: true}},
	} {
		if snapshot, height, err := parseCaptureState(tc.state, tc.extraLines); err != nil || snapshot != tc.expected || height != 24 {
			t.Errorf("%q, %d extra lines: expected %+v and 24 rows, got %+v and %d, %v", tc.state, tc.extraLines, tc.expected,
				snapshot, height, err)
		}
	}
	for _, state := range []string{"", "0 42 1 5 23", "0 42 1 5 23 24 7", "0 42 1 x 23 24", "0  42 1 5 23 24"} {
		if _, _, err := parseCaptureState(state, 0); err == nil {
			t.Errorf("%q: expected an error", state)
		}
	}
}
