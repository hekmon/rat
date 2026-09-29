package tmux

import (
	"context"
	"errors"
	"fmt"
	"regexp"
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
// it is typed as a new line character, which bash (like most programs) reads as Enter.
// The error wraps ErrSessionNotFound or ErrWindowNotFound if they do not exist.
func (c *Controller) SendText(ctx context.Context, session, window, text string, enter bool) error {
	if err := checkNames(session, window); err != nil {
		return err
	}
	// -l: literal text, otherwise words such as "Enter" or "C-c" would be sent as keys.
	// --: text starting with '-' (such as "-y") would otherwise be parsed as flags.
	args := []string{"send-keys", "-t", target(session, window), "-l", "--", tmuxArg(text)}
	if enter {
		args = append(args, ";", "send-keys", "-t", target(session, window), "Enter")
	}
	if _, err := c.run(ctx, args...); err != nil {
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
