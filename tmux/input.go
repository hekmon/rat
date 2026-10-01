package tmux

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"time"
)

var (
	// ErrInvalidKey is returned by SendKeys for a name that is not a known tmux key name.
	ErrInvalidKey = errors.New("invalid key name")
	// ErrBracketedPasteOverridden is returned by CheckBracketedPaste when bracketed paste is off at
	// the bash prompt of terminals: bash startup files replaced its enforcement, and neither bash
	// nor the inputrc turns it on.
	ErrBracketedPasteOverridden = errors.New("bash startup files override bracketed paste")
)

// keyFormat matches the tmux key names SendKeys accepts: a printable ASCII character or a named
// key, optionally with modifiers (C- control, M- meta/alt, S- shift). tmux types an unknown key
// name as text instead of failing, which would hide the mistake: "Ctrl-C" would not interrupt.
var keyFormat = regexp.MustCompile(`^(?:[CMS]-)*(?:[!-~]|Enter|Escape|Tab|BTab|BSpace|Space|Up|Down|Left|Right|Home|End|PageUp|PgUp|PageDown|PgDn|PPage|NPage|Insert|IC|Delete|DC|F[1-9]|F1[0-2])$`)

// SendText pastes text in window of session, as a human pastes, then presses Enter if enter is
// true. The text is pasted as is, never interpreted as key names and with nothing added or
// removed. When the text arrives, a program asking for pastes to be marked (bracketed paste:
// bash waiting at its prompt, vim…) receives it as a paste: bash inserts it into its command
// line, new lines included, and runs nothing until Enter. Otherwise, the text is read as typed, a
// new line as Enter: by programs that do not ask, but also by bash reading it later, when it was
// pasted while bash was starting or running a command. It has no size limit, and reaches the
// terminal whole: no other input can interleave with it. A tmux mode a human left the window in
// (copy mode, to scroll back) is left first: in a mode, tmux would paste the text unmarked.
// The error wraps ErrSessionNotFound or ErrWindowNotFound if they do not exist.
//
// A paste is all or nothing: a command ended by its context while the text streams to tmux pastes
// none of it, and leaves no buffer behind.
func (c *Controller) SendText(ctx context.Context, session, window, text string, enter bool) error {
	var stdin io.Reader
	if text != "" {
		stdin = strings.NewReader(text)
	}
	return c.sendText(ctx, session, window, stdin, enter)
}

// sendText is SendText, the text read from text, nil when empty: tests stream it.
func (c *Controller) sendText(ctx context.Context, session, window string, text io.Reader, enter bool) error {
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
	var args []string
	if text != nil {
		// load-buffer creates no buffer from an empty input, and paste-buffer would then fail
		buffer = "rat-input-" + strconv.FormatUint(c.inputBuffers.Add(1), 10)
		// copy stdin ("-") into a buffer of its own
		args = []string{"load-buffer", "-b", buffer, "-", ";"}
	}
	// Leave any mode, which also reports a missing window when there is nothing to send. After
	// load-buffer, which waits while reading stdin and lets other clients run meanwhile: a mode
	// entered then would still be on for the paste.
	args = append(args, leaveModes(tg)...)
	if text != nil {
		// paste the buffer into the window, where:
		//  -r keeps new lines as is (tmux would replace them with carriage returns)
		//  -d deletes the buffer once pasted
		//  -p marks the paste (bracketed paste) if the program asked for it
		args = append(args, ";", "paste-buffer", "-t", tg, "-b", buffer, "-r", "-d", "-p")
	}
	if enter {
		args = append(args, ";", "send-keys", "-t", tg, "Enter")
	}
	if _, err := c.runWithStdin(ctx, text, args...); err != nil {
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

// CheckKey returns an error wrapping ErrInvalidKey if key is not a key name SendKeys accepts: a
// printable ASCII character or a named key (Enter, Escape, Up, F5…), with optional modifiers (C-,
// M-, S-). It is the rule SendKeys applies, for callers to find which key is refused.
func CheckKey(key string) error {
	if !keyFormat.MatchString(key) {
		return fmt.Errorf("%w: %q", ErrInvalidKey, key)
	}
	return nil
}

// SendKeys presses keys in window of session, in order. Keys are tmux key names, such as
// "C-c", "Escape", "Up" or "F5"; the error wraps ErrInvalidKey for an unknown one, and nothing
// is sent. A tmux mode a human left the window in (copy mode, to scroll back) is left first: in a
// mode, tmux would take the keys for itself, or drop them.
// The error wraps ErrSessionNotFound or ErrWindowNotFound if they do not exist.
func (c *Controller) SendKeys(ctx context.Context, session, window string, keys ...string) error {
	if err := checkNames(session, window); err != nil {
		return err
	}
	for _, key := range keys {
		if err := CheckKey(key); err != nil {
			return err
		}
	}
	tg := target(session, window)
	args := append(leaveModes(tg), ";", "send-keys", "-t", tg, "--")
	for _, key := range keys {
		args = append(args, tmuxArg(key))
	}
	if _, err := c.run(ctx, args...); err != nil {
		return c.windowError(ctx, fmt.Errorf("failed to send keys to %s:%s: %w", session, window, err), session, window)
	}
	return nil
}

// leaveModes returns the tmux command leaving every mode of the window target (copy mode, clock
// mode…), to chain in an input invocation right before the input. In a mode, tmux pastes a text
// without marking it (bracketed paste), even if the program asked for it, and takes keys for the
// mode: copy mode binds them to its own commands, the other modes drop them. A human peeking at
// the terminals leaves a mode behind when detaching: agents come first, and never see modes.
// It does nothing on a window in no mode, and fails on a missing one.
func leaveModes(target string) []string {
	return []string{"copy-mode", "-q", "-t", target}
}

// checkMarker starts the line the bash run by CheckBracketedPaste answers on, telling it apart from
// what startup files print.
const checkMarker = "__rat_check|"

// checkScript is what CheckBracketedPaste types into bash, a command per line: bash shows its
// prompt before each, running PROMPT_COMMAND as in a terminal. The first keeps the history of the
// user as it is: an interactive bash saves the commands it read when it exits. The second answers
// after the marker: bracketed paste at the prompt, as readline reports it, and what PROMPT_COMMAND
// became, quoted on a single line.
const checkScript = "unset HISTFILE\n" +
	`printf '\n%s%s|%q\n' '` + checkMarker + `' "$(bind -v 2>/dev/null | grep -F enable-bracketed-paste)" "${PROMPT_COMMAND[*]-}"` + "\n" +
	"exit 0\n"

// CheckBracketedPaste checks that bracketed paste is on at the bash prompt of terminals, which
// pasted text relies on (see SendText). rat turns it on through PROMPT_COMMAND, which a startup
// file assigning it (rather than adding to it) replaces: bracketed paste then depends on bash
// defaults (on from 5.1) and the inputrc, and pasted text may run line by line. The error then
// wraps ErrBracketedPasteOverridden, with what PROMPT_COMMAND became. It is a warning for the
// caller to relay, terminals keep working.
// It runs bash once as terminals start it (login, interactive, their environment), typing commands
// into it: bash shows its prompt and runs PROMPT_COMMAND as in a terminal, so that a startup file
// running rat's command from a function of its own (as starship does) is not taken for an
// override. Startup files may block, so ctx should bound it. It fails with ErrServerNotRunning if
// the server is not started.
func (c *Controller) CheckBracketedPaste(ctx context.Context) error {
	// What tmux adds for terminals, on top of their environment: the shell, TERM, and TMUX, and
	// what StartServer set apart from terminalEnvironment, PROMPT_COMMAND. TMUX matters: a common
	// startup file runs tmux when it is empty.
	out, err := c.run(ctx, "show-options", "-gv", "default-shell", ";",
		"show-options", "-gv", "default-terminal", ";",
		"display-message", "-p", "#{socket_path},#{pid},0", ";",
		"show-environment", "-g", "PROMPT_COMMAND")
	if err != nil {
		return fmt.Errorf("failed to read the terminals settings: %w", err)
	}
	settings := strings.Split(strings.TrimSuffix(out, "\n"), "\n")
	if len(settings) != 4 || !strings.HasPrefix(settings[3], "PROMPT_COMMAND=") {
		return fmt.Errorf("unexpected terminals settings %q", out)
	}
	shell, term, tmux, promptCommand := settings[0], settings[1], settings[2], settings[3]
	// Not through tmux (run-shell): tmux 3.3 does not return its output to the client.
	// The commands are typed on stdin rather than given with -c: bash then shows a prompt before
	// each, as in a terminal. Reading PROMPT_COMMAND instead took a startup file running rat's
	// command from a function of its own, as starship does, for an override. Stderr is the null
	// device: bash shows its prompts there, and complains it has no terminal for job control.
	cmd := exec.CommandContext(ctx, shell, "-l", "-i")
	cmd.Stdin = strings.NewReader(checkScript)
	cmd.Env = os.Environ() // the server environment, which it copied from rat's when started
	for _, variable := range terminalEnvironment {
		cmd.Env = append(cmd.Env, variable[0]+"="+variable[1])
	}
	cmd.Env = append(cmd.Env, "TERM="+term, "TMUX="+tmux, promptCommand)
	if cmd.Dir, err = os.UserHomeDir(); err != nil {
		return fmt.Errorf("failed to run bash as terminals do: %w", err)
	}
	// a startup file starting a daemon would keep the output open after bash exits
	cmd.WaitDelay = time.Second
	output, err := cmd.Output()
	if err != nil {
		return fmt.Errorf("failed to run bash as terminals do: %w", err)
	}
	answer, found := "", false
	for line := range strings.Lines(string(output)) {
		if answer, found = strings.CutPrefix(strings.TrimSuffix(line, "\n"), checkMarker); found {
			break
		}
	}
	if !found {
		return errors.New("failed to run bash as terminals do: no answer at its prompt")
	}
	paste, promptCommand, _ := strings.Cut(answer, "|")
	switch paste {
	case "set enable-bracketed-paste on":
		return nil
	case "set enable-bracketed-paste off":
		return fmt.Errorf("%w: PROMPT_COMMAND is %s, pasted text may run line by line", ErrBracketedPasteOverridden,
			promptCommand)
	default:
		return fmt.Errorf("failed to read bracketed paste at the bash prompt: %q", paste)
	}
}
