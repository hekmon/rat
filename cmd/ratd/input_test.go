package main

import (
	"context"
	"slices"
	"strings"
	"testing"

	"github.com/hekmon/rat/tmux"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// screenOf returns a condition true once the screen of window of the tool tests shows text.
func screenOf(controller *tmux.Controller, window, text string) func() bool {
	return func() bool {
		snapshot, err := controller.Capture(context.Background(), toolsSession, window, 0)
		return err == nil && strings.Contains(snapshot.Content, text)
	}
}

// waitPrompt waits for bash to show its prompt in window, after the output of a command: bash then
// waits for input, and asked for pasted text to be marked.
func waitPrompt(t *testing.T, session *mcp.ClientSession, controller *tmux.Controller, window string) {
	t.Helper()
	expectTool(t, session, "send_text", map[string]any{"window": window, "text": "echo ready", "enter": true}, false)
	eventually(t, "prompt after the command", screenOf(controller, window, "\nready\n"))
}

// TestSendText guards send_text: a multi-line text pasted at a prompt waits for Enter, Enter alone
// runs it, enter is required, and the log line tells the size of the text, never the text.
func TestSendText(t *testing.T) {
	session, controller, logs, _ := connectTools(t, "test-ratd-sendtext")
	expectTool(t, session, "list_windows", nil, false)
	waitPrompt(t, session, controller, tmux.FirstWindow)
	expectTool(t, session, "send_text", map[string]any{"window": "main", "text": "echo one\necho secret-two", "enter": false},
		false, "Pasted in main, Enter not pressed.")
	eventually(t, "text pasted", screenOf(controller, tmux.FirstWindow, "echo secret-two"))
	if snapshot, err := controller.Capture(context.Background(), toolsSession, tmux.FirstWindow, 0); err != nil ||
		slices.Contains(strings.Split(snapshot.Content, "\n"), "one") {
		t.Fatalf("pasted text ran without Enter: %v\n%s", err, snapshot.Content)
	}
	expectTool(t, session, "send_text", map[string]any{"window": "main", "text": "", "enter": true}, false, "Enter pressed in main.")
	eventually(t, "every line run", screenOf(controller, tmux.FirstWindow, "\none\nsecret-two"))
	expectTool(t, session, "send_text", map[string]any{"window": "main", "text": ""}, true, "enter")
	expectTool(t, session, "send_text", map[string]any{"window": "nope", "text": "x", "enter": true}, true,
		"No window nope: list_windows shows your windows.")
	out := logs.String()
	if !strings.Contains(out, "tool=send_text window=main outcome=ok") || !strings.Contains(out, "text_bytes=24 enter=false") {
		t.Errorf("expected the size of the text to be logged:\n%s", out)
	}
	if strings.Contains(out, "secret") {
		t.Errorf("the text was logged:\n%s", out)
	}
}

// TestSendKeys guards send_keys: keys act as keys (C-c interrupts), an unknown name is refused
// naming it, with nothing sent, and the log line tells the number of keys, never the keys.
func TestSendKeys(t *testing.T) {
	session, controller, logs, _ := connectTools(t, "test-ratd-sendkeys")
	expectTool(t, session, "list_windows", nil, false)
	expectTool(t, session, "send_text", map[string]any{"window": "main", "text": "sleep 30", "enter": true}, false)
	foreground := func(command string) func() bool {
		return func() bool {
			w, err := controller.Window(context.Background(), toolsSession, tmux.FirstWindow)
			return err == nil && w.Command == command
		}
	}
	eventually(t, "sleep in the foreground", foreground("sleep"))
	expectTool(t, session, "send_keys", map[string]any{"window": "main", "keys": []string{"C-c", "Ctrl-C"}}, true,
		`Unknown key "Ctrl-C": key names are`, "To type text, use send_text.")
	if w, err := controller.Window(context.Background(), toolsSession, tmux.FirstWindow); err != nil || w.Command != "sleep" {
		t.Errorf("expected nothing sent, sleep running, got %+v, %v", w, err)
	}
	expectTool(t, session, "send_keys", map[string]any{"window": "main", "keys": []string{}}, true, "No key to press")
	expectTool(t, session, "send_keys", map[string]any{"window": "main", "keys": []string{"C-c"}}, false, "Pressed in main: C-c.")
	eventually(t, "sleep interrupted", foreground("bash"))
	if out := logs.String(); !strings.Contains(out, "tool=send_keys window=main outcome=ok") || !strings.Contains(out, "keys=1") ||
		strings.Contains(out, "C-c") {
		t.Errorf("expected the number of keys to be logged, never the keys:\n%s", out)
	}
}

// TestSendToMissingSession guards that sending to a missing session creates it, but sends nothing:
// bash is still starting in the new main, and a pasted text would run line by line.
func TestSendToMissingSession(t *testing.T) {
	session, controller, _, _ := connectTools(t, "test-ratd-sendmissing")
	expectTool(t, session, "send_text", map[string]any{"window": "main", "text": "echo x", "enter": true}, true,
		"Nothing was sent: your terminals did not exist, main was just created", "Wait for its prompt")
	if _, err := controller.Window(context.Background(), toolsSession, tmux.FirstWindow); err != nil {
		t.Errorf("expected main to be created, got %v", err)
	}
	_ = controller.KillSession(context.Background(), toolsSession)
	expectTool(t, session, "send_keys", map[string]any{"window": "build", "keys": []string{"C-c"}}, true,
		"Nothing was sent", "Window build does not exist: create it with create_window.")
}
