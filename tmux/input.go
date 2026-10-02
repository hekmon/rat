package tmux

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"
)

var (
	// ErrInvalidKey is returned by SendKeys for a name that is not a known tmux key name.
	ErrInvalidKey = errors.New("invalid key name")
	// ErrControlCharacter is returned by SendText for a text holding a control character other
	// than tab, new line and carriage return.
	ErrControlCharacter = errors.New("control character")
)

// keyFormat matches the tmux key names SendKeys accepts: a printable ASCII character or a named
// key, optionally with modifiers (C- control, M- meta/alt, S- shift). tmux types an unknown key
// name as text instead of failing, which would hide the mistake: "Ctrl-C" would not interrupt.
var keyFormat = regexp.MustCompile(`^(?:[CMS]-)*(?:[!-~]|Enter|Escape|Tab|BTab|BSpace|Space|Up|Down|Left|Right|Home|End|PageUp|PgUp|PageDown|PgDn|PPage|NPage|Insert|IC|Delete|DC|F[1-9]|F1[0-2])$`)

// SendText pastes text in window of session, as a human pastes, then presses Enter if enter is
// true. The text is pasted as is, never interpreted as key names and with nothing added or
// removed. It holds no control character but tab, new line and carriage return: the error wraps
// ErrControlCharacter otherwise, and nothing is sent (see CheckText). When the text arrives, a program asking for pastes to be marked (bracketed paste:
// bash waiting at its prompt, vim…) receives it as a paste: bash inserts it into its command
// line, new lines included, and runs nothing until Enter. Otherwise, the text is read as typed, a
// new line as Enter: by programs that do not ask, but also by bash reading it later, when it was
// pasted while bash was starting or running a command. It has no size limit, and reaches the
// terminal whole: no other input can interleave with it. A tmux mode a human left the window in
// (copy mode, to scroll back) is left first: in a mode, tmux would paste the text unmarked.
// Sending something clears the prompt recorded until then (see Window.Prompt), and records its time
// (see Window.Input).
// The error wraps ErrSessionNotFound or ErrWindowNotFound if they do not exist.
//
// A paste is all or nothing: a command ended by its context while the text streams to tmux pastes
// none of it, and leaves no buffer behind.
func (c *Controller) SendText(ctx context.Context, session, window, text string, enter bool) error {
	if err := CheckText(text); err != nil {
		return err
	}
	var stdin io.Reader
	if text != "" {
		stdin = strings.NewReader(text)
	}
	return c.sendText(ctx, session, window, stdin, enter)
}

// CheckText returns an error wrapping ErrControlCharacter if text holds a control character
// SendText refuses: an ASCII one below space, or DEL, but tab, new line and carriage return. It
// names the first one, in hexadecimal and caret notation (0x1b (^[) for ESC), and its line. It is
// the rule SendText applies, for callers to find which character is refused.
//
// Refused rather than pasted, or removed: the terminal would not get what the caller sent, and the
// caller would not know. Until tmux 3.6, ESC[201~ ends the bracketed paste early, and the lines
// after it run without Enter; from 3.7, tmux rewrites control characters into visible text. Keys
// are pressed with SendKeys. See "Control characters are refused" in the README.
func CheckText(text string) error {
	line := 1
	// bytes rather than runes: no byte of a multi-byte UTF-8 character is below 0x80, so a control
	// character is found whatever the text holds, invalid UTF-8 included
	for i := 0; i < len(text); i++ {
		switch b := text[i]; {
		case b == '\n':
			line++
		case b == '\t' || b == '\r':
		case b < ' ' || b == 0x7f:
			return fmt.Errorf("%w 0x%02x (^%c) on line %d", ErrControlCharacter, b, b^0x40, line)
		}
	}
	return nil
}

// sendText is SendText, the text read from text, nil when empty: tests stream it, unchecked.
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
	if text != nil || enter {
		args = append(args, ";")
		args = append(args, clearPrompt(tg)...)
		args = append(args, ";")
		args = append(args, recordInput(tg, time.Now())...)
	}
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
// mode, tmux would take the keys for itself, or drop them. Pressing keys clears the prompt
// recorded until then (see Window.Prompt), and records their time (see Window.Input).
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
	args := append(leaveModes(tg), ";")
	args = append(args, clearPrompt(tg)...)
	args = append(args, ";")
	args = append(args, recordInput(tg, time.Now())...)
	args = append(args, ";", "send-keys", "-t", tg, "--")
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

// clearPrompt returns the tmux command clearing the prompt the window target recorded (see
// Window.Prompt), to chain in an input invocation right before the input: the prompt recorded
// until then precedes the input, and one recorded afterwards follows it. It clears the mark of a
// prompt being recorded as well, which drops that record (see promptCommand): bash checked no line
// waits before the input arrived. Clearing an option that is not set succeeds: it can not fail the
// input.
func clearPrompt(target string) []string {
	return []string{"set-option", "-pu", "-t", target, promptOption}
}

// recordInput returns the tmux command recording now as the time of an input on the window target
// (see Window.Input), to chain in an input invocation. tmux formats have no clock: the time is
// rat's, which runs on the machine of tmux.
func recordInput(target string, now time.Time) []string {
	return []string{"set-option", "-p", "-t", target, inputOption, strconv.FormatInt(now.Unix(), 10)}
}

// TerminalsCheck is what the bash of terminals ends up with at its prompt, once its startup files
// ran: what rat relies on, which startup files can defeat (see CheckTerminals).
type TerminalsCheck struct {
	// BracketedPaste tells pasted text waits on the command line until Enter (see SendText).
	BracketedPaste bool
	// Prompts tells the prompt hook runs: windows record their prompts (see Window.Prompt).
	Prompts bool
	// Statuses tells the prompt hook gets the exit status of commands, which a command a startup
	// file adds in front of rat's, in PROMPT_COMMAND, may change.
	Statuses bool
	// Pipelines tells the prompt hook gets the status of each command of a pipeline (see
	// Prompt.Pipeline), which a command a startup file adds in front of rat's loses, even when it
	// keeps the status (direnv does).
	Pipelines bool
	// PromptCommand is what PROMPT_COMMAND became, quoted for bash, for the caller to tell.
	PromptCommand string
}

// checkMarker starts the line the bash run by CheckTerminals answers on, telling it apart from what
// startup files print.
const checkMarker = "__rat_check|"

// checkStatus is the exit status of the command CheckTerminals runs before reading what the prompt
// hook got: unlike 0 and 1, a status the commands of a PROMPT_COMMAND are unlikely to leave.
const checkStatus = "7"

// checkPipeline is the pipeline of that command, ending with checkStatus, as the prompt hook
// records it (see promptCommand).
const checkPipeline = " 6 " + checkStatus

// checkScript is what CheckTerminals types into bash, a command per line: bash shows its prompt
// before each, running PROMPT_COMMAND as in a terminal. The first keeps the history of the user as
// it is: an interactive bash saves the commands it read when it exits. The second is a pipeline
// exiting with checkStatus, for the prompt after it. The third answers after the marker: bracketed
// paste at the prompt, as readline reports it, the status and the pipeline the prompt hook got
// (empty if it did not run, see promptCommand), and what PROMPT_COMMAND became, quoted on a single
// line.
const checkScript = "unset HISTFILE\n" +
	"(exit 6) | (exit " + checkStatus + ")\n" +
	`printf '\n%s%s|%s|%s|%q\n' '` + checkMarker + `' "$(bind -v 2>/dev/null | grep -F enable-bracketed-paste)" ` +
	`"${__rat_status-}" "${__rat_pipeline-}" "${PROMPT_COMMAND[*]-}"` + "\n" +
	"exit 0\n"

// CheckTerminals checks what the bash of terminals ends up with at its prompt, once its startup
// files ran: bracketed paste, which pasted text relies on (see SendText), and the prompt hook,
// which records prompts and the status of commands (see Window.Prompt). rat sets both through
// PROMPT_COMMAND, which a startup file assigning it (rather than adding to it) replaces: bracketed
// paste then depends on bash defaults (on from 5.1) and the inputrc, and no prompt is recorded. A
// command a startup file adds in front of rat's may change the status rat's gets, or lose the
// statuses of a pipeline. What it finds
// are warnings for the caller to relay, terminals keep working: the error tells the check could
// not run.
// It runs bash once as terminals start it (login, interactive, their environment), typing commands
// into it: bash shows its prompt and runs PROMPT_COMMAND as in a terminal, so that a startup file
// running rat's command from a function of its own (as starship does) is not taken for an
// override. Startup files may block, so ctx should bound it. It fails with ErrServerNotRunning if
// the server is not started.
func (c *Controller) CheckTerminals(ctx context.Context) (TerminalsCheck, error) {
	// What tmux adds for terminals, on top of their environment: the shell, TERM, and TMUX. TMUX
	// matters: a common startup file runs tmux when it is empty.
	out, err := c.run(ctx, "show-options", "-gv", "default-shell", ";",
		"show-options", "-gv", "default-terminal", ";",
		"display-message", "-p", "#{socket_path},#{pid},0")
	if err != nil {
		return TerminalsCheck{}, fmt.Errorf("failed to read the terminals settings: %w", err)
	}
	settings := strings.Split(strings.TrimSuffix(out, "\n"), "\n")
	if len(settings) != 3 {
		return TerminalsCheck{}, fmt.Errorf("unexpected terminals settings %q", out)
	}
	shell, term, tmux := settings[0], settings[1], settings[2]
	// And what StartServer set apart from terminalEnvironment, PROMPT_COMMAND, as it set it rather
	// than read back: tmux 3.4 escapes $ in what show-environment prints (\${…}), a copy bash would
	// run garbled.
	c.serverAction.RLock()
	server := c.server
	c.serverAction.RUnlock()
	if server == nil {
		return TerminalsCheck{}, ErrServerNotRunning
	}
	// Not through tmux (run-shell): tmux 3.3 does not return its output to the client.
	// The commands are typed on stdin rather than given with -c: bash then shows a prompt before
	// each, as in a terminal. Reading PROMPT_COMMAND instead took a startup file running rat's
	// command from a function of its own, as starship does, for an override. Stderr is the null
	// device: bash shows its prompts there, and complains it has no terminal for job control.
	cmd := exec.CommandContext(ctx, shell, "-l", "-i")
	cmd.Stdin = strings.NewReader(checkScript)
	// The server environment, which it copied from rat's when started, but TMUX_PANE: the prompt
	// hook would record the prompts of the check in the pane it names, on the server TMUX names,
	// rat's (a rat started in a terminal of another tmux inherits a TMUX_PANE).
	cmd.Env = slices.DeleteFunc(os.Environ(), func(variable string) bool { return strings.HasPrefix(variable, "TMUX_PANE=") })
	for _, variable := range terminalEnvironment {
		cmd.Env = append(cmd.Env, variable[0]+"="+variable[1])
	}
	cmd.Env = append(cmd.Env, "TERM="+term, "TMUX="+tmux, "PROMPT_COMMAND="+server.promptCommand)
	if cmd.Dir, err = os.UserHomeDir(); err != nil {
		return TerminalsCheck{}, fmt.Errorf("failed to run bash as terminals do: %w", err)
	}
	// a startup file starting a daemon would keep the output open after bash exits
	cmd.WaitDelay = time.Second
	output, err := cmd.Output()
	if err != nil {
		return TerminalsCheck{}, fmt.Errorf("failed to run bash as terminals do: %w", err)
	}
	var answer []string
	for line := range strings.Lines(string(output)) {
		if fields, found := strings.CutPrefix(strings.TrimSuffix(line, "\n"), checkMarker); found {
			answer = strings.SplitN(fields, "|", 4)
			break
		}
	}
	if len(answer) != 4 {
		return TerminalsCheck{}, errors.New("failed to run bash as terminals do: no answer at its prompt")
	}
	paste, status, pipeline := answer[0], answer[1], answer[2]
	check := TerminalsCheck{Prompts: status != "", Statuses: status == checkStatus, Pipelines: pipeline == checkPipeline,
		PromptCommand: answer[3]}
	switch paste {
	case "set enable-bracketed-paste on":
		check.BracketedPaste = true
	case "set enable-bracketed-paste off":
	default:
		return TerminalsCheck{}, fmt.Errorf("failed to read bracketed paste at the bash prompt: %q", paste)
	}
	return check, nil
}
