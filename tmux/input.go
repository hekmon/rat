package tmux

import (
	"context"
	"errors"
	"fmt"
	"io"
	"regexp"
	"strconv"
	"strings"
)

var (
	// ErrInvalidKey is returned by SendKeys for a name that is not a known tmux key name.
	ErrInvalidKey = errors.New("invalid key name")
)

// keyFormat matches the tmux key names SendKeys accepts: a printable ASCII character or a named
// key, optionally with modifiers (C- control, M- meta/alt, S- shift). tmux types an unknown key
// name as text instead of failing, which would hide the mistake: "Ctrl-C" would not interrupt.
var keyFormat = regexp.MustCompile(`^(?:[CMS]-)*(?:[!-~]|Enter|Escape|Tab|BTab|BSpace|Space|Up|Down|Left|Right|Home|End|PageUp|PgUp|PageDown|PgDn|PPage|NPage|Insert|IC|Delete|DC|F[1-9]|F1[0-2])$`)

// SendText types text in window of session, then presses Enter if enter is true. The text is
// typed as is, never interpreted as key names and with nothing added or removed: a new line in
// it is typed as a new line character, which bash (like most programs) reads as Enter. It has no
// size limit, and reaches the terminal whole: no other input can interleave with it.
// The error wraps ErrSessionNotFound or ErrWindowNotFound if they do not exist.
func (c *Controller) SendText(ctx context.Context, session, window, text string, enter bool) error {
	if err := checkNames(session, window); err != nil {
		return err
	}
	// Not send-keys -l: tmux refuses an invocation whose arguments exceed 16 KiB, and text would
	// go through its argument parsing (a trailing ';' is dropped, a leading '-' read as a flag).
	// The text is copied into a tmux buffer (its clipboard, kept in the server memory) from
	// stdin instead, which has neither issue, then pasted from it.
	// Everything is a single tmux invocation, which tmux runs whole: that is what keeps other
	// inputs from interleaving, and why a text and its Enter are sent together.
	tg := target(session, window)
	var buffer string
	var stdin io.Reader
	var args []string
	if text != "" {
		// load-buffer creates no buffer from an empty input, and paste-buffer would then fail
		buffer = "rat-input-" + strconv.FormatUint(c.inputBuffers.Add(1), 10)
		stdin = strings.NewReader(text)
		args = []string{
			// copy stdin ("-") into a buffer of its own
			"load-buffer", "-b", buffer, "-", ";",
			// paste it into the window, where:
			//  -r keeps new lines as is (tmux would replace them with carriage returns)
			//  -d deletes the buffer once pasted
			//  no -p: no bracketed paste markers, programs read the text as if typed
			"paste-buffer", "-t", tg, "-b", buffer, "-r", "-d",
		}
	}
	if enter {
		if len(args) > 0 {
			args = append(args, ";")
		}
		args = append(args, "send-keys", "-t", tg, "Enter")
	}
	if len(args) == 0 {
		// nothing to send: send-keys without keys still reports a missing window
		args = []string{"send-keys", "-t", tg}
	}
	if _, err := c.runWithStdin(ctx, stdin, args...); err != nil {
		if buffer != "" {
			// -d only deletes the buffer once pasted: a failed paste leaves the text in tmux.
			// Not ctx: when it is canceled (which may be why the paste failed), the cleanup would
			// fail too. The server context lives as long as the server holding the buffer.
			if serverCtx := c.serverContext(); serverCtx != nil {
				_, _ = c.run(serverCtx, "delete-buffer", "-b", buffer)
			}
		}
		return c.windowError(ctx, fmt.Errorf("failed to send text to %s:%s: %w", session, window, err), session, window)
	}
	return nil
}

// SendKeys presses keys in window of session, in order. Keys are tmux key names, such as
// "C-c", "Escape", "Up" or "F5"; the error wraps ErrInvalidKey for an unknown one, and nothing
// is sent. The error wraps ErrSessionNotFound or ErrWindowNotFound if they do not exist.
func (c *Controller) SendKeys(ctx context.Context, session, window string, keys ...string) error {
	if err := checkNames(session, window); err != nil {
		return err
	}
	for _, key := range keys {
		if !keyFormat.MatchString(key) {
			return fmt.Errorf("%w: %q", ErrInvalidKey, key)
		}
	}
	args := []string{"send-keys", "-t", target(session, window), "--"}
	for _, key := range keys {
		args = append(args, tmuxArg(key))
	}
	if _, err := c.run(ctx, args...); err != nil {
		return c.windowError(ctx, fmt.Errorf("failed to send keys to %s:%s: %w", session, window, err), session, window)
	}
	return nil
}
