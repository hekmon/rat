package main

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/hekmon/rat/cmd/ratd/tools"
	"github.com/hekmon/rat/tmux"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// keyNames lists the key names the controller accepts (tmux.CheckKey), one per key, for agents:
// aliases are accepted, not shown.
const keyNames = "a printable character, Space, Enter, Tab, BTab, BSpace, Escape, Up, Down, Left, Right, Home, " +
	"End, PageUp, PageDown, Insert, Delete, F1 to F12, with modifiers C- (control), M- (alt), S- (shift), " +
	"combinable (C-c, M-x, C-S-Left)"

// addInputTools adds the tools sending input to the windows of session. What runs in a terminal can
// reach anything: an open world.
func (d *daemon) addInputTools(server *mcp.Server, session string) {
	mcp.AddTool(server, &mcp.Tool{
		Name:        tools.SendText,
		InputSchema: d.inputSchemas[tools.SendText],
		Description: "Paste text in a window, as a human pastes, then press Enter if enter is true. At a bash prompt, " +
			"the text waits on the command line, new lines included, until Enter runs it. While a command runs, or " +
			"before a new window shows its prompt, the text is read as typed: each line runs. Returns at once, " +
			"without waiting for the command: check it with list_windows or read_window.",
		Annotations: &mcp.ToolAnnotations{DestructiveHint: ptr(true), OpenWorldHint: ptr(true)},
	}, tool(d, session, tools.SendText, func(in tools.SendTextInput) string { return in.Window },
		func(ctx context.Context, in tools.SendTextInput) result { return d.sendText(ctx, session, in) }))
	mcp.AddTool(server, &mcp.Tool{
		Name:        tools.SendKeys,
		InputSchema: d.inputSchemas[tools.SendKeys],
		Description: "Press keys in a window, in order: to interrupt (C-c), drive a full-screen program (q, Escape, " +
			"arrows), complete (Tab). Key names: " + keyNames + ". To type text, use send_text.",
		Annotations: &mcp.ToolAnnotations{DestructiveHint: ptr(true), OpenWorldHint: ptr(true)},
	}, tool(d, session, tools.SendKeys, func(in tools.SendKeysInput) string { return in.Window },
		func(ctx context.Context, in tools.SendKeysInput) result { return d.sendKeys(ctx, session, in) }))
}

// sendText pastes the text of in, then presses Enter if asked. The log line tells the size of the
// text, never the text.
func (d *daemon) sendText(ctx context.Context, session string, in tools.SendTextInput) result {
	attrs := []any{"text_bytes", len(in.Text), "enter", in.Enter}
	if in.Text == "" && !in.Enter {
		return result{outcome: message, text: fmt.Sprintf("Nothing sent to %s: the text is empty, and enter false.", in.Window),
			attrs: attrs}
	}
	err := d.controller.SendText(ctx, session, in.Window, in.Text, in.Enter)
	r := d.sent(ctx, session, in.Window, err)
	if r.outcome == done {
		switch {
		case in.Text == "":
			r.text = fmt.Sprintf("Enter pressed in %s.", in.Window)
		case in.Enter:
			r.text = fmt.Sprintf("Pasted in %s, then Enter pressed.", in.Window)
		default:
			r.text = fmt.Sprintf("Pasted in %s, Enter not pressed.", in.Window)
		}
	}
	r.attrs = attrs
	return r
}

// sendKeys presses the keys of in. An unknown key name is refused, naming it, and nothing is sent:
// tmux would type it as text. The log line tells the number of keys, never the keys: they can
// spell a password one key at a time.
func (d *daemon) sendKeys(ctx context.Context, session string, in tools.SendKeysInput) result {
	attrs := []any{"keys", len(in.Keys)}
	if len(in.Keys) == 0 {
		return result{outcome: failed, text: "No key to press: give at least one key name.", attrs: attrs}
	}
	for _, key := range in.Keys {
		if err := tmux.CheckKey(key); err != nil {
			return result{outcome: failed, err: err, attrs: attrs,
				text: fmt.Sprintf("Unknown key %q: key names are %s. To type text, use send_text.", key, keyNames)}
		}
	}
	r := d.sent(ctx, session, in.Window, d.controller.SendKeys(ctx, session, in.Window, in.Keys...))
	if r.outcome == done {
		r.text = fmt.Sprintf("Pressed in %s: %s.", in.Window, strings.Join(in.Keys, " "))
	}
	r.attrs = attrs
	return r
}

// sent returns the result of sending input to window of session, which failed with err, if not
// nil. A missing session is created, but nothing is sent: bash is still starting in the new main,
// and a pasted text would run line by line. This is the usual case after a tmux restart, when the
// agent retries its command.
func (d *daemon) sent(ctx context.Context, session, window string, err error) result {
	if err == nil {
		return result{outcome: done}
	}
	if !errors.Is(err, tmux.ErrSessionNotFound) {
		return failure(ctx, err, window)
	}
	if _, err = d.createSession(ctx, session); err != nil {
		return failure(ctx, err, window)
	}
	text := "Nothing was sent: your terminals did not exist, main was just created and bash is starting."
	if window == tmux.FirstWindow {
		text += " Wait for its prompt (" + d.promptTool() + "), then send again."
	} else {
		text += fmt.Sprintf(" Window %s does not exist: create it with create_window.", window)
	}
	return result{outcome: failed, text: text, err: tmux.ErrSessionNotFound}
}
