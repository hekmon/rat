package main

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/hekmon/rat/tmux"
)

// TestReadWindow guards what read_window tells: a missing session created, main then read with a
// header saying so, the rows of history included, a full-screen program with its cursor (the
// history only mentioned when asked), missing windows, and the rows read in the log line.
func TestReadWindow(t *testing.T) {
	session, controller, logs, _ := connectTools(t, "test-ratd-read")
	ctx := context.Background()
	expectTool(t, session, "read_window", map[string]any{"window": "build"}, true,
		"No window build: your terminals did not exist, main was just created. Create build with create_window.")
	if _, err := controller.Window(ctx, toolsSession, tmux.FirstWindow); err != nil {
		t.Errorf("expected main to be created, got %v", err)
	}
	_ = controller.KillSession(ctx, toolsSession)
	if text := expectTool(t, session, "read_window", map[string]any{"window": "main"}, false); !strings.HasPrefix(text, freshMainHeader) {
		t.Errorf("expected the fresh main header, got:\n%s", text)
	}
	expectTool(t, session, "read_window", map[string]any{"window": "nope"}, true, "No window nope: list_windows shows your windows.")

	waitPrompt(t, session, controller, tmux.FirstWindow)
	expectTool(t, session, "send_text", map[string]any{"window": "main", "text": "seq 1 100", "enter": true}, false)
	eventually(t, "output displayed", screenOf(controller, tmux.FirstWindow, "\n100\n"))
	if text := expectTool(t, session, "read_window", map[string]any{"window": "main"}, false, "\n100\n"); strings.HasPrefix(text, "[") ||
		strings.Contains(text, "\n50\n") {
		t.Errorf("expected the screen alone, without header, got:\n%s", text)
	}
	expectTool(t, session, "read_window", map[string]any{"window": "main", "scrollback_rows": 30}, false,
		"[history: 30 rows above the screen]\n")
	w, err := controller.Window(ctx, toolsSession, tmux.FirstWindow)
	if err != nil {
		t.Fatal(err)
	}
	expectTool(t, session, "read_window", map[string]any{"window": "main", "scrollback_rows": uint64(1) << 63}, false,
		fmt.Sprintf("[history: %d rows above the screen]\n", w.Scrollback), "\n1\n")
	expectTool(t, session, "read_window", map[string]any{"window": "main", "scrollback_rows": -1}, true, "scrollback_rows")
	if !strings.Contains(logs.String(), "tool=read_window window=main outcome=ok") ||
		!strings.Contains(logs.String(), "scrollback_rows=30 history_rows=30 bytes=") {
		t.Errorf("expected the rows read to be logged:\n%s", logs)
	}

	expectTool(t, session, "create_window", map[string]any{"name": "fs"}, false)
	waitPrompt(t, session, controller, "fs")
	expectTool(t, session, "send_text", map[string]any{"window": "fs", "text": `printf '\033[?1049h\033[3;1Hdrawn\033[6;10H'; sleep 30`,
		"enter": true}, false)
	eventually(t, "full-screen program drawn", func() bool {
		snapshot, err := controller.Capture(ctx, toolsSession, "fs", 0)
		return err == nil && snapshot.FullScreen && snapshot.Cursor.Row == 6
	})
	expectTool(t, session, "read_window", map[string]any{"window": "fs"}, false,
		"[full-screen program: cursor at row 6, column 10]\n\n\ndrawn")
	expectTool(t, session, "read_window", map[string]any{"window": "fs", "scrollback_rows": 10}, false,
		"[full-screen program: no history, it belongs to the terminal before the program started; cursor at row 6, column 10]\n")
	expectTool(t, session, "create_window", map[string]any{"name": "hidden"}, false)
	waitPrompt(t, session, controller, "hidden")
	expectTool(t, session, "send_text", map[string]any{"window": "hidden", "text": `printf '\033[?1049h\033[?25l\033[Hhidden'; sleep 30`,
		"enter": true}, false)
	eventually(t, "cursor hidden", func() bool {
		text, _ := callTool(t, session, "read_window", map[string]any{"window": "hidden"})
		return strings.HasPrefix(text, "[full-screen program: cursor hidden]\nhidden")
	})
}

// TestReadWindowBudget guards the read budget: a long history is cut to the end that fits, with a
// header saying so, even when a single line wraps over the history and the screen, while a screen
// over the budget alone is refused rather than cut.
func TestReadWindowBudget(t *testing.T) {
	session, controller, logs, _ := connectTools(t, "test-ratd-budget")
	expectTool(t, session, "list_windows", nil, false)
	waitPrompt(t, session, controller, tmux.FirstWindow)
	cutHeader := "[history cut to fit the read budget (64 KiB): redirect long output to a file and use read_file]\n"

	expectTool(t, session, "send_text", map[string]any{"window": "main", "enter": true,
		"text": `seq 1 20000 | sed 's/$/ padding padding padding/'`}, false)
	eventually(t, "long output displayed", screenOf(controller, tmux.FirstWindow, "\n20000 padding"))
	text := expectTool(t, session, "read_window", map[string]any{"window": "main", "scrollback_rows": 10000}, false,
		"\n20000 padding padding padding\n")
	if !strings.HasPrefix(text, cutHeader) || len(text) != defaultReadBudget {
		t.Errorf("expected the history cut to the budget, got %d bytes:\n%.300s", len(text), text)
	}
	if !strings.Contains(logs.String(), "cut=true") {
		t.Errorf("expected the cut to be logged:\n%s", logs)
	}

	expectTool(t, session, "send_text", map[string]any{"window": "main", "enter": true,
		"text": `clear; head -c 100000 /dev/zero | tr '\0' x; echo`}, false)
	eventually(t, "long line displayed", screenOf(controller, tmux.FirstWindow, strings.Repeat("x", 200)+"\n"))
	// the end of the line, on the screen, comes whole: 22 rows, then the prompt
	text = expectTool(t, session, "read_window", map[string]any{"window": "main", "scrollback_rows": 1000}, false,
		strings.Repeat("x", 22*200)+"\n")
	if !strings.HasPrefix(text, cutHeader) || len(text) > defaultReadBudget {
		t.Errorf("expected the long line cut to the budget, got %d bytes:\n%.300s", len(text), text)
	}

	// 8 combining accents on each character: 17 bytes a cell, 78 KB for 23 rows
	expectTool(t, session, "send_text", map[string]any{"window": "main", "enter": true,
		"text": `clear; c=$(printf 'e\u0301\u0302\u0303\u0304\u0306\u0307\u0308\u030a'); l=$(printf "$c%.0s" {1..200}); ` +
			`for i in {1..23}; do echo "$l"; done`}, false)
	eventually(t, "accents displayed", func() bool {
		snapshot, err := controller.Capture(context.Background(), toolsSession, tmux.FirstWindow, 0)
		return err == nil && snapshot.ScreenBytes > 70000
	})
	for _, rows := range []int{0, 100} {
		expectTool(t, session, "read_window", map[string]any{"window": "main", "scrollback_rows": rows}, true,
			"The screen of main holds ", "KiB, more than the read budget (64 KiB): it was not sent. Close the window "+
				"(close_window) and start over in a new one.")
	}
	if !strings.Contains(logs.String(), "outcome=error") || !strings.Contains(logs.String(), "screen_bytes=") {
		t.Errorf("expected the refusal to be logged with the size of the screen:\n%s", logs)
	}
}

// TestTail guards the cut of a long history: the end that fits, from a character boundary.
func TestTail(t *testing.T) {
	for _, tc := range []struct {
		content  string
		size     int
		expected string
	}{
		{"one\ntwo\nthree", 20, "one\ntwo\nthree"},
		{"one\ntwo\nthree", 9, "two\nthree"},
		{"one\ntwo\nthree", 7, "o\nthree"},
		{"é\nab", 4, "\nab"},
		{"é\nab", 5, "é\nab"},
		{"aé", 1, ""},
	} {
		if cut := tail(tc.content, tc.size); cut != tc.expected {
			t.Errorf("%q in %d bytes: expected %q, got %q", tc.content, tc.size, tc.expected, cut)
		}
	}
}
