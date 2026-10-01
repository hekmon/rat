package tmux

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

// TestSendText guards that text is sent literally (key names and a leading '-' included), and
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

// TestSendTextPasted guards that text is pasted as a human pastes: bash, which asks for bracketed
// paste, inserts a multi-line text into its command line and runs nothing until Enter, which then
// runs every line.
func TestSendTextPasted(t *testing.T) {
	c := startTestServer(t, "sendpasted")
	checkPasted(t, c)
}

// TestSendTextPastedInputrcOff guards that bracketed paste is enforced even when an inputrc turns
// it off (as bash 4.4 and 5.0 do by default): terminals inherit INPUTRC from rat's environment.
func TestSendTextPastedInputrcOff(t *testing.T) {
	inputrc := filepath.Join(t.TempDir(), "inputrc")
	if err := os.WriteFile(inputrc, []byte("set enable-bracketed-paste off\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("INPUTRC", inputrc)
	c := startTestServer(t, "sendpastedoff")
	checkPasted(t, c)
}

// checkPasted checks that a multi-line text pasted at a bash prompt runs nothing until Enter,
// which then runs every line.
func checkPasted(t *testing.T, c *Controller) {
	t.Helper()
	ctx := context.Background()
	if err := c.NewSession(ctx, "s"); err != nil {
		t.Fatal(err)
	}
	// bash asks for pastes to be marked whenever it waits at a prompt, before displaying it: a
	// text pasted earlier, while bash starts, is marked for nobody and read as typed. A prompt
	// displayed after the output of a command means bash is waiting and asked for it.
	typeCommand(t, c, "s", FirstWindow, "echo ready")
	eventually(t, "prompt after the command", screenContains(c, "s", FirstWindow, "\nready\n"))
	if err := c.SendText(ctx, "s", FirstWindow, "echo one\necho two\n", false); err != nil {
		t.Fatal(err)
	}
	// typed rather than pasted, bash would have run the first line before displaying the second
	eventually(t, "text pasted", screenContains(c, "s", FirstWindow, "echo two"))
	snapshot, err := c.Capture(ctx, "s", FirstWindow, 0)
	if err != nil {
		t.Fatal(err)
	}
	if slices.Contains(strings.Split(snapshot.Content, "\n"), "one") {
		// rat enforces bracketed paste with PROMPT_COMMAND, which a startup file of the machine
		// running the tests can replace: terminals read the real ones (~/.bash_profile…)
		t.Fatalf("pasted text ran without Enter: bash did not ask for bracketed paste. Does a bash "+
			"startup file assign PROMPT_COMMAND (rather than adding to it)? Current screen:\n%s", snapshot.Content)
	}
	if err := c.SendText(ctx, "s", FirstWindow, "", true); err != nil {
		t.Fatal(err)
	}
	eventually(t, "every line run", screenContains(c, "s", FirstWindow, "\none\ntwo"))
}

// TestInputLeavesModes guards that input reaches the program whatever tmux mode a human peeking at
// the terminal left the window in: copy mode (scrolling back), where tmux would paste text without
// marking it and take keys for itself, and clock mode, where it would drop keys, reporting success.
func TestInputLeavesModes(t *testing.T) {
	c := startTestServer(t, "inputmodes")
	ctx := context.Background()
	if err := c.NewSession(ctx, "s"); err != nil {
		t.Fatal(err)
	}
	// a prompt displayed after the output of a command: bash waits, and asked for bracketed paste
	typeCommand(t, c, "s", FirstWindow, "echo ready")
	eventually(t, "prompt after the command", screenContains(c, "s", FirstWindow, "\nready\n"))
	enterMode := func(mode string) {
		t.Helper()
		if _, err := c.run(ctx, mode, "-t", target("s", FirstWindow)); err != nil {
			t.Fatal(err)
		}
	}
	for _, mode := range []string{"copy-mode", "clock-mode"} {
		// a text is pasted as a paste: nothing runs until Enter
		enterMode(mode)
		if err := c.SendText(ctx, "s", FirstWindow, "echo "+mode+"-one\necho "+mode+"-two", false); err != nil {
			t.Fatal(err)
		}
		// typed rather than pasted, bash would have run the first line before displaying the second
		eventually(t, mode+": text pasted", screenContains(c, "s", FirstWindow, "echo "+mode+"-two"))
		if snapshot, err := c.Capture(ctx, "s", FirstWindow, 0); err != nil ||
			slices.Contains(strings.Split(snapshot.Content, "\n"), mode+"-one") {
			t.Fatalf("%s: pasted text ran without Enter: %v\n%s", mode, err, snapshot.Content)
		}
		// keys reach bash
		enterMode(mode)
		if err := c.SendKeys(ctx, "s", FirstWindow, "Enter"); err != nil {
			t.Fatal(err)
		}
		eventually(t, mode+": keys pressed", screenContains(c, "s", FirstWindow, "\n"+mode+"-one\n"+mode+"-two"))
		// and so does the Enter of a text
		enterMode(mode)
		if err := c.SendText(ctx, "s", FirstWindow, "echo "+mode+"-enter", true); err != nil {
			t.Fatal(err)
		}
		eventually(t, mode+": text run", screenContains(c, "s", FirstWindow, "\n"+mode+"-enter"))
	}
}

// TestSendKeys guards that keys act as keys (C-c interrupts the foreground command), that an
// unknown key name is refused rather than typed as text, and that missing targets are reported.
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
	if err := c.SendKeys(ctx, "s", "nope", "C-c"); !errors.Is(err, ErrWindowNotFound) {
		t.Errorf("expected ErrWindowNotFound, got %v", err)
	}
	if err := c.SendKeys(ctx, "nope", FirstWindow, "C-c"); !errors.Is(err, ErrSessionNotFound) {
		t.Errorf("expected ErrSessionNotFound, got %v", err)
	}
	if err := c.SendKeys(ctx, "s", FirstWindow, "C-c"); err != nil {
		t.Fatal(err)
	}
	eventually(t, "sleep interrupted", foregroundIs(c, "s", FirstWindow, "bash"))
	for _, key := range []string{"Enter", "Escape", "Up", "F12", "M-x", "C-S-Left", "y", "^"} {
		if err := CheckKey(key); err != nil {
			t.Errorf("key %q refused: %v", key, err)
		}
	}
	for _, key := range []string{"", "Ctrl-C", "ab", "F13", "C-", "enter"} {
		if err := CheckKey(key); !errors.Is(err, ErrInvalidKey) {
			t.Errorf("key %q: expected ErrInvalidKey, got %v", key, err)
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

// TestSendSemicolon guards that a text or key ending with ';' is sent as is: tmux would read that
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

// TestSendLargeText guards that a text larger than what a tmux invocation accepts as arguments
// (16 KiB, "command too long") reaches the terminal whole and byte for byte, key names, leading
// '-', trailing ';', tabs and UTF-8 included.
func TestSendLargeText(t *testing.T) {
	c := startTestServer(t, "largetext")
	ctx := context.Background()
	if err := c.NewSession(ctx, "s"); err != nil {
		t.Fatal(err)
	}
	// cat writes what the terminal receives to a file; echo is off so the screen stays short.
	// Lines stay short: in canonical mode, the terminal truncates lines longer than its line
	// buffer (1024 bytes on macOS), whatever tmux sends.
	out := filepath.Join(t.TempDir(), "out")
	typeCommand(t, c, "s", FirstWindow, "stty -echo; cat > '"+out+"'")
	eventually(t, "cat in the foreground", foregroundIs(c, "s", FirstWindow, "cat"))
	var text strings.Builder
	for i := 0; text.Len() < 64*1024; i++ {
		fmt.Fprintf(&text, "-line %d\tEnter C-c café ✓ echo a;\n", i)
	}
	if err := c.SendText(ctx, "s", FirstWindow, text.String(), false); err != nil {
		t.Fatal(err)
	}
	if err := c.SendKeys(ctx, "s", FirstWindow, "C-d"); err != nil {
		t.Fatal(err)
	}
	eventually(t, "cat done", foregroundIs(c, "s", FirstWindow, "bash"))
	received, err := os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}
	if string(received) != text.String() {
		t.Fatalf("sent %d bytes, received %d bytes differing from them", text.Len(), len(received))
	}
}

// TestSendTextMissingTarget guards that input to a missing window or session reports it, with
// or without text and Enter, and that a failed paste leaves no buffer holding the text in tmux.
func TestSendTextMissingTarget(t *testing.T) {
	c := startTestServer(t, "sendmissing")
	ctx := context.Background()
	if err := c.NewSession(ctx, "s"); err != nil {
		t.Fatal(err)
	}
	for _, input := range []struct {
		text  string
		enter bool
	}{{"secret", true}, {"secret", false}, {"", true}, {"", false}} {
		if err := c.SendText(ctx, "s", "nope", input.text, input.enter); !errors.Is(err, ErrWindowNotFound) {
			t.Errorf("text %q, enter %v: expected ErrWindowNotFound, got %v", input.text, input.enter, err)
		}
		if err := c.SendText(ctx, "nope", FirstWindow, input.text, input.enter); !errors.Is(err, ErrSessionNotFound) {
			t.Errorf("text %q, enter %v: expected ErrSessionNotFound, got %v", input.text, input.enter, err)
		}
	}
	if buffers, err := c.run(ctx, "list-buffers", "-F", "#{buffer_name}"); err != nil || buffers != "" {
		t.Errorf("buffers left: %q (%v)", buffers, err)
	}
	// an empty input to an existing window is not an error
	if err := c.SendText(ctx, "s", FirstWindow, "", false); err != nil {
		t.Error(err)
	}
}

// TestCheckTerminals guards what the check finds at the bash prompt, whatever startup files do with
// PROMPT_COMMAND: replaced, rat's command does not run (no bracketed paste, no prompt recorded);
// added to, it runs, and gets the status unless a command run before changes it; run from a
// function of its own, as starship does, it runs as well. An inputrc turns bracketed paste off, so
// that only rat's command turns it on, whatever the default of the bash running the tests (on from
// 5.1).
func TestCheckTerminals(t *testing.T) {
	inputrc := filepath.Join(t.TempDir(), "inputrc")
	if err := os.WriteFile(inputrc, []byte("set enable-bracketed-paste off\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("INPUTRC", inputrc)
	c := newTestController(t, "checkterminals")
	if _, err := c.CheckTerminals(context.Background()); !errors.Is(err, ErrServerNotRunning) {
		t.Fatalf("expected ErrServerNotRunning, got %v", err)
	}
	if err := c.StartServer(context.Background()); err != nil {
		t.Fatal(err)
	}
	all := TerminalsCheck{BracketedPaste: true, Prompts: true, Statuses: true}
	for profile, expected := range map[string]TerminalsCheck{
		"true":                        all,
		`PROMPT_COMMAND="history -a"`: {},
		// history -a fails once HISTFILE is unset, the check unsetting it: it changes the status
		`PROMPT_COMMAND="history -a;$PROMPT_COMMAND"`: {BracketedPaste: true, Prompts: true},
		// as direnv does: a command added in front, keeping the status
		`__keep() { local s=$?; true; return $s; }; PROMPT_COMMAND="__keep;$PROMPT_COMMAND"`: all,
		// as terminals, bash runs within tmux: startup files often run tmux when TMUX is empty
		`[ -z "$TMUX" ] && PROMPT_COMMAND="outside tmux"`: all,
		// as starship does: PROMPT_COMMAND kept, and run by a function of its own, which runs
		// commands before (changing the status), or not
		`__saved=$PROMPT_COMMAND; PROMPT_COMMAND=__precmd; __precmd() { eval "$__saved"; }`:             all,
		`__saved=$PROMPT_COMMAND; PROMPT_COMMAND=__precmd; __precmd() { local s=$?; eval "$__saved"; }`: {BracketedPaste: true, Prompts: true},
	} {
		home := t.TempDir()
		if err := os.WriteFile(filepath.Join(home, ".bash_profile"), []byte(profile+"\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		t.Setenv("HOME", home)
		check, err := c.CheckTerminals(context.Background())
		check.PromptCommand = ""
		if err != nil || check != expected {
			t.Errorf("profile %s: expected %+v, got %+v, %v", profile, expected, check, err)
		}
	}
}

// TestCheckTerminalsNoPane guards that the check records no prompt in a window: a rat started in a
// terminal of another tmux inherits a TMUX_PANE, which would name a pane of rat's server too.
func TestCheckTerminalsNoPane(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	c := startTestServer(t, "checknopane")
	ctx := context.Background()
	if err := c.NewSession(ctx, "s"); err != nil {
		t.Fatal(err)
	}
	eventually(t, "first prompt", promptIs(c, "s", FirstWindow, NoStatus))
	pane, err := c.run(ctx, "display-message", "-p", "-t", target("s", FirstWindow), "#{pane_id}")
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("TMUX_PANE", strings.TrimSpace(pane))
	if check, err := c.CheckTerminals(ctx); err != nil || !check.Prompts {
		t.Fatalf("expected the prompt hook to run, got %+v, %v", check, err)
	}
	// the hook records in the background: its tmux client may reach the server after the check
	for deadline := time.Now().Add(500 * time.Millisecond); time.Now().Before(deadline); time.Sleep(50 * time.Millisecond) {
		if w, err := c.Window(ctx, "s", FirstWindow); err != nil || w.Prompt.Status != NoStatus {
			t.Fatalf("expected the first prompt of the window, got %+v, %v", w.Prompt, err)
		}
	}
}

// stallingReader serves data, then signals stalled and blocks until release is closed: a text
// still streaming to tmux. Released before the tmux client is killed, it would end the text
// early, and tmux would paste what it received: a text held in memory never ends early.
type stallingReader struct {
	data             *strings.Reader
	stalled, release chan struct{}
}

func (r *stallingReader) Read(p []byte) (int, error) {
	if r.data.Len() > 0 {
		return r.data.Read(p)
	}
	close(r.stalled)
	<-r.release
	return 0, io.EOF
}

// TestSendTextAllOrNothing guards that a paste is all or nothing: a command ended by its context
// while its text streams to tmux (killing the tmux client in load-buffer) pastes none of it, and
// leaves no buffer. The window runs cat on a raw terminal, which writes all it receives to a file:
// the line discipline of a cooked one would keep a few KiB of a line without a new line. A paste
// afterwards shows the file receives what is pasted. The text is released once the client is
// gone: until then, the command waits for the goroutine copying it (os/exec), which WaitDelay
// only frees from a pipe, not from a reader blocking by itself.
func TestSendTextAllOrNothing(t *testing.T) {
	c := startTestServer(t, "sendtextcut")
	ctx := context.Background()
	if err := c.NewSession(ctx, "s"); err != nil {
		t.Fatal(err)
	}
	out := filepath.Join(t.TempDir(), "pasted")
	typeCommand(t, c, "s", FirstWindow, "stty raw -echo; cat > "+out)
	eventually(t, "cat in the foreground", foregroundIs(c, "s", FirstWindow, "cat"))

	text := &stallingReader{data: strings.NewReader(strings.Repeat("a", 1<<20)), stalled: make(chan struct{}),
		release: make(chan struct{})}
	sendCtx, cancel := context.WithCancel(ctx)
	sent := make(chan error, 1)
	go func() { sent <- c.sendText(sendCtx, "s", FirstWindow, text, false) }()
	<-text.stalled
	cancel()
	eventually(t, "tmux client killed", func() (bool, string) {
		err := exec.Command("pgrep", "-f", c.socketName()+" .*load-buffer").Run()
		return err != nil, fmt.Sprintf("pgrep: %v", err)
	})
	close(text.release)
	if err := <-sent; err == nil {
		t.Fatal("expected the canceled paste to fail")
	}

	if err := c.SendText(ctx, "s", FirstWindow, "after", false); err != nil {
		t.Fatal(err)
	}
	eventually(t, "control paste received", func() (bool, string) {
		data, err := os.ReadFile(out)
		return err == nil && len(data) > 0, fmt.Sprintf("%d bytes, %v", len(data), err)
	})
	if data, _ := os.ReadFile(out); string(data) != "after" {
		t.Errorf("expected nothing of the canceled paste, got %d bytes before the control one", len(data)-len("after"))
	}
	if buffers, err := c.run(ctx, "list-buffers", "-F", "#{buffer_name}"); err != nil || buffers != "" {
		t.Errorf("expected no buffer left, got %q, %v", buffers, err)
	}
}

// TestInputClearsPrompt guards that an input clears the prompt recorded until then, in its own
// invocation: no prompt shows while the command sent runs, nor while text or keys wait on the
// command line, and the prompt recorded afterwards follows the input, with its status.
func TestInputClearsPrompt(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	c := startTestServer(t, "inputprompt")
	ctx := context.Background()
	if err := c.NewSession(ctx, "s"); err != nil {
		t.Fatal(err)
	}
	eventually(t, "first prompt", promptIs(c, "s", FirstWindow, NoStatus))
	noPrompt := func(what string) {
		t.Helper()
		if w, err := c.Window(ctx, "s", FirstWindow); err != nil || w.Prompt != (Prompt{}) {
			t.Errorf("%s: expected no prompt, got %+v, %v", what, w.Prompt, err)
		}
	}
	if err := c.SendText(ctx, "s", FirstWindow, "sleep 1; false", true); err != nil {
		t.Fatal(err)
	}
	noPrompt("command running")
	eventually(t, "prompt after the command", promptIs(c, "s", FirstWindow, 1))
	if err := c.SendText(ctx, "s", FirstWindow, "echo pending", false); err != nil {
		t.Fatal(err)
	}
	eventually(t, "text typed", promptEndsWith(c, "s", FirstWindow, "echo pending"))
	noPrompt("text on the command line")
	// C-c discards the command line, and bash shows a new prompt
	if err := c.SendKeys(ctx, "s", FirstWindow, "C-c"); err != nil {
		t.Fatal(err)
	}
	eventually(t, "prompt after C-c", promptIs(c, "s", FirstWindow, 130))
	if err := c.SendKeys(ctx, "s", FirstWindow, "x"); err != nil {
		t.Fatal(err)
	}
	eventually(t, "key typed", promptEndsWith(c, "s", FirstWindow, "x"))
	noPrompt("key on the command line")
}
