package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/hekmon/rat/cmd/ratd/tools"
	"github.com/hekmon/rat/connect"
	"github.com/hekmon/rat/tmux"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// TestWaitWindow guards wait_window: it returns as soon as the command sent last finished, telling
// how it exited, or after the seconds asked, telling what still runs and since the last input, with
// a calm hint when nothing was displayed for a while; a command sent while another runs is the one
// waited for, even run by bash, which shows as bash in the foreground; bash in the foreground with
// no prompt since the last input, a bash script running or a text left on the command line, is not
// taken for a command finished, and the hint tells both; the seconds asked are bounded. A command
// run in the background is seen as finished at once, as the instructions and the description tell
// agents. The description tells how to wait longer: several calls at once on a window wait
// together, where an agent wanting to wait longer counted them one after the other.
func TestWaitWindow(t *testing.T) {
	hint := quietHint
	quietHint = 0
	t.Cleanup(func() { quietHint = hint })
	session, _, logs, _ := connectTools(t, "test-ratd-wait")
	list, err := session.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, tool := range list.Tools {
		if tool.Name == "wait_window" && !strings.Contains(tool.Description, "To wait longer, call it again once it "+
			"returns: several calls at once on a window wait together, no longer than one.") {
			t.Errorf("expected wait_window to tell how to wait longer, got %q", tool.Description)
		}
	}
	expectTool(t, session, "wait_window", map[string]any{"window": "main"}, false, "main: bash is at its prompt, waiting for input.")
	expectTool(t, session, "send_text", map[string]any{"window": "main", "text": "sleep 1; false", "enter": true}, false)
	start := time.Now()
	expectTool(t, session, "wait_window", map[string]any{"window": "main", "max_seconds": 10}, false,
		"main: the command finished ", " ago, exit status 1.")
	if waited := time.Since(start); waited > 5*time.Second {
		t.Errorf("expected the wait to end with the command, waited %s", waited)
	}
	// bash runs the second command once back at its prompt, which ends the first one
	expectTool(t, session, "send_text", map[string]any{"window": "main", "text": "sleep 1; false", "enter": true}, false)
	expectTool(t, session, "send_text", map[string]any{"window": "main", "text": "bash -c 'sleep 1; exit 3'", "enter": true}, false)
	expectTool(t, session, "wait_window", map[string]any{"window": "main", "max_seconds": 10}, false,
		"main: the command finished ", " ago, exit status 3.")
	expectTool(t, session, "send_text", map[string]any{"window": "main", "text": "sleep 30", "enter": true}, false)
	// the time since the input, recorded by tmux: a second or two
	expectTool(t, session, "wait_window", map[string]any{"window": "main", "max_seconds": 1}, false,
		"main: still running, ", "s since your last input, sleep in the foreground. No output for ",
		"read_window shows whether it waits for input.")
	expectTool(t, session, "send_keys", map[string]any{"window": "main", "keys": []string{"C-c"}}, false)
	expectTool(t, session, "wait_window", map[string]any{"window": "main", "max_seconds": 5}, false, "exit status 130.")
	// the job outlasts the wait: what finished is starting it
	expectTool(t, session, "send_text", map[string]any{"window": "main", "text": "sleep 30 &", "enter": true}, false)
	expectTool(t, session, "wait_window", map[string]any{"window": "main", "max_seconds": 5}, false,
		"main: the command finished ", " ago, exit status 0.")
	// bash in the foreground, with no prompt since the input: a script running, or a text left on
	// the command line
	bashRunning := []string{"main: nothing finished since your last input, ", "s ago: bash has shown no prompt since. " +
		"A bash script or builtin (read) may still be running, or a text may wait on the command line (sent without " +
		"Enter): read_window shows which."}
	expectTool(t, session, "send_text", map[string]any{"window": "main", "text": "bash -c 'sleep 30; true'", "enter": true}, false)
	expectTool(t, session, "wait_window", map[string]any{"window": "main", "max_seconds": 1}, false, bashRunning...)
	expectTool(t, session, "send_keys", map[string]any{"window": "main", "keys": []string{"C-c"}}, false)
	expectTool(t, session, "wait_window", map[string]any{"window": "main", "max_seconds": 5}, false, "exit status 130.")
	expectTool(t, session, "send_text", map[string]any{"window": "main", "text": "echo pending", "enter": false}, false)
	expectTool(t, session, "wait_window", map[string]any{"window": "main", "max_seconds": 1}, false, bashRunning...)
	expectTool(t, session, "wait_window", map[string]any{"window": "nope", "max_seconds": 1}, true, "No window nope")
	for _, seconds := range []int{0, tools.MaxWaitSeconds + 1} {
		if text, isError := callTool(t, session, "wait_window", map[string]any{"window": "main", "max_seconds": seconds}); !isError {
			t.Errorf("max_seconds %d: expected a refusal, got %s", seconds, text)
		}
	}
	if out := logs.String(); !strings.Contains(out, "tool=wait_window window=main outcome=ok") ||
		!strings.Contains(out, "max_seconds=1") || !strings.Contains(out, "finished=false") {
		t.Errorf("expected the waits to be logged:\n%s", out)
	}
}

// TestRunningText guards what wait_window tells of a command still running: the time since the
// last input and the time without output, both from the clock of the machine whatever the call
// waited, which an agent adding up its waits miscounts; the time waited without an input recorded.
// bash in the foreground is told as a script or builtin running, or a text on the command line. A
// window whose bash has not run rat's hook is told as such, with the fix of its startup files.
func TestRunningText(t *testing.T) {
	now := time.Unix(1790000000, 0)
	waited := 50 * time.Second
	quiet := " No output for 5m, which is normal for some commands: read_window shows whether it waits for input."
	bashRunning := "A bash script or builtin (read) may still be running, or a text may wait on the command line " +
		"(sent without Enter): read_window shows which."
	running := tmux.Window{Command: "make", Activity: now.Add(-5 * time.Minute), Input: now.Add(-7 * time.Minute), Hooked: true}
	bash := running
	bash.Command = "bash"
	for expected, w := range map[string]tmux.Window{
		"build: still running, 7m since your last input, make in the foreground." + quiet:                               running,
		"build: still running after 50s, make in the foreground." + quiet:                                               {Command: "make", Activity: running.Activity, Hooked: true},
		"build: nothing finished since your last input, 7m ago: bash has shown no prompt since. " + bashRunning + quiet: bash,
		"build: nothing finished after 50s: bash has shown no prompt since your last input. " + bashRunning + quiet:     {Command: "bash", Activity: running.Activity, Hooked: true},
		"build: no prompt recorded in this window yet: its bash has not run rat's prompt hook. Either it is still " +
			"starting (its startup files running: read_window shows it), or its startup files replace PROMPT_COMMAND: " +
			"wait_window then can not tell when its commands finish, nor list_windows how they exited, and multi-line " +
			"text runs line by line. A startup file must add to PROMPT_COMMAND rather than replace it " +
			`(PROMPT_COMMAND="mine;$PROMPT_COMMAND"): windows created afterwards record their prompts again.`: {Command: "bash"},
	} {
		if text := runningText("build", w, waited, now); text != expected {
			t.Errorf("%+v: expected\n%s\ngot\n%s", w, expected, text)
		}
	}
}

// TestWaitWindowMissingSession guards that waiting on a missing session creates it, as the other
// tools do: waiting on main then waits for its first prompt, any other window is missing.
func TestWaitWindowMissingSession(t *testing.T) {
	session, controller, _, _ := connectTools(t, "test-ratd-waitmissing")
	expectTool(t, session, "wait_window", map[string]any{"window": "build", "max_seconds": 5}, true,
		"No window build: your terminals did not exist, main was just created.")
	_ = controller.KillSession(context.Background(), toolsSession)
	expectTool(t, session, "wait_window", map[string]any{"window": "main", "max_seconds": 5}, false,
		"main: bash is at its prompt, waiting for input.")
}

// TestWaitWindowWithoutPrompts guards that where the terminals record no prompt (bash startup
// files replacing PROMPT_COMMAND), wait_window is offered all the same, its description, its answer
// and the instructions telling why it can not wait. The texts telling to wait for a prompt point to
// read_window instead, and send_text does not count wait_window among the tools following a command.
func TestWaitWindowWithoutPrompts(t *testing.T) {
	session, _, _, _ := connectToolsWith(t, "test-ratd-nowait", tmux.TerminalsCheck{BracketedPaste: true})
	list, err := session.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	listed := false
	for _, tool := range list.Tools {
		if tool.Name == "wait_window" {
			listed = strings.Contains(tool.Description, "Unavailable: "+noPromptsText)
		}
	}
	if !listed {
		t.Errorf("expected wait_window to tell why it is unavailable:\n%+v", list.Tools)
	}
	expectTool(t, session, "wait_window", map[string]any{"window": "main"}, true, noPromptsText)
	if instructions := session.InitializeResult().Instructions; !strings.Contains(instructions,
		"rat can not tell when a command finishes on this machine (wait_window tells why)") {
		t.Errorf("expected the instructions to tell rat can not tell when commands finish, got %q", instructions)
	}
	for _, tool := range list.Tools {
		if tool.Name == "create_window" && !strings.Contains(tool.Description, "(read_window)") {
			t.Errorf("expected create_window to point to read_window, got %q", tool.Description)
		}
		if tool.Name == "send_text" && (!strings.Contains(tool.Description, "(&, nohup): list_windows tells whether") ||
			!strings.Contains(tool.Description, "wait for the prompt (read_window)")) {
			t.Errorf("expected send_text not to point to wait_window, got %q", tool.Description)
		}
	}
	expectTool(t, session, "send_text", map[string]any{"window": "main", "text": "echo x", "enter": true}, true,
		"Wait for its prompt (read_window)")
}

// TestRunStopsWaits guards that stopping ratd does not wait for a call waiting: it returns at once,
// telling the terminals stop, and ratd stops well within the grace of the calls in flight. The kill
// switch does not depend on what agents wait for.
func TestRunStopsWaits(t *testing.T) {
	requireTmux(t)
	testHome(t)
	bundle := newBundle(t, "test-ratd-stopwait")
	addr, logs, stop := runRatd(t, bundle)
	client := mcp.NewClient(&mcp.Implementation{Name: "test", Version: "1"}, nil)
	httpClient := httpClient(t, bundle, "alice")
	httpClient.Timeout = 0
	session, err := client.Connect(context.Background(), &mcp.StreamableClientTransport{
		Endpoint:   "https://" + addr + connect.Path,
		HTTPClient: httpClient,
	}, nil)
	if err != nil {
		t.Fatalf("%v\n%s", err, logs)
	}
	defer session.Close()
	expectTool(t, session, "wait_window", map[string]any{"window": "main", "max_seconds": 10}, false, "waiting for input")
	expectTool(t, session, "send_text", map[string]any{"window": "main", "text": "sleep 100", "enter": true}, false)
	waited := make(chan string, 1)
	go func() {
		result, err := session.CallTool(context.Background(), &mcp.CallToolParams{Name: "wait_window",
			Arguments: map[string]any{"window": "main", "max_seconds": tools.MaxWaitSeconds}})
		switch {
		case err != nil:
			waited <- err.Error()
		case !result.IsError || len(result.Content) != 1:
			waited <- "unexpected result"
		default:
			text, _ := result.Content[0].(*mcp.TextContent)
			waited <- text.Text
		}
	}()
	// the wait reaches ratd, which asks tmux about the window until it stops
	time.Sleep(time.Second)
	start := time.Now()
	if err = stop(); err != nil {
		t.Fatal(err)
	}
	if stopped := time.Since(start); stopped > shutdownGrace/2 {
		t.Errorf("expected ratd to stop without waiting for the wait, took %s", stopped)
	}
	select {
	case text := <-waited:
		if !strings.Contains(text, "ratd is stopping: the terminals stop with it.") {
			t.Errorf("expected the wait to tell ratd stops, got %q", text)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the wait did not return")
	}
}

// TestWaitWindowNotHooked guards what agents are told of a window whose startup files changed
// after ratd checked them, and replace PROMPT_COMMAND: list_windows tells no prompt is recorded,
// and wait_window why, with the fix, while the windows created before keep recording prompts.
func TestWaitWindowNotHooked(t *testing.T) {
	session, controller, _, home := connectTools(t, "test-ratd-nothooked")
	expectTool(t, session, "list_windows", nil, false)
	waitPrompt(t, session, controller, tmux.FirstWindow)
	if err := os.WriteFile(filepath.Join(home, ".bash_profile"), []byte(`PROMPT_COMMAND="history -a"`+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	expectTool(t, session, "create_window", map[string]any{"name": "w"}, false)
	// bash started in the new window, and showed its prompt
	expectTool(t, session, "send_text", map[string]any{"window": "w", "text": "echo ready", "enter": true}, false)
	eventually(t, "prompt after the command", screenOf(controller, "w", "\nready\n"))
	text := expectTool(t, session, "list_windows", nil, false,
		"w: bash in "+home+", active ", ", no prompt recorded yet: bash still starting, or its startup files replace PROMPT_COMMAND")
	if strings.Contains(strings.Split(text, "\n")[0], "no prompt recorded") {
		t.Errorf("expected main to record its prompts, got:\n%s", text)
	}
	expectTool(t, session, "wait_window", map[string]any{"window": "w", "max_seconds": 1}, false,
		"w: no prompt recorded in this window yet: its bash has not run rat's prompt hook.",
		`(PROMPT_COMMAND="mine;$PROMPT_COMMAND")`)
	expectTool(t, session, "wait_window", map[string]any{"window": "main", "max_seconds": 1}, false,
		"main: the command finished ")
}

// TestWaitWindowPipeline guards, through the tools, that a command failing behind | tee, the
// pattern ratd recommends for long outputs, is not told as a success: wait_window and list_windows
// tell the command that failed, and their descriptions tell it before the agent meets it. Where the
// terminals do not record pipelines, the descriptions do not tell it.
func TestWaitWindowPipeline(t *testing.T) {
	session, _, _, _ := connectTools(t, "test-ratd-pipeline")
	list, err := session.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, tool := range list.Tools {
		if (tool.Name == "wait_window" || tool.Name == "list_windows") && !strings.Contains(tool.Description, pipelineSentence) {
			t.Errorf("expected %s to tell how pipelines exit, got %q", tool.Name, tool.Description)
		}
	}
	expectTool(t, session, "wait_window", map[string]any{"window": "main"}, false, "main: bash is at its prompt, waiting for input.")
	expectTool(t, session, "send_text", map[string]any{"window": "main", "text": "(echo build; exit 2) 2>&1 | tee /dev/null",
		"enter": true}, false)
	expectTool(t, session, "wait_window", map[string]any{"window": "main", "max_seconds": 10}, false,
		"main: the command finished ", " ago: command 1 of its pipeline of 2 exited with status 2. The pipeline exits "+
			"with the status of its last command, 0, as bash reports it (statuses, left to right: 2 0).")
	expectTool(t, session, "list_windows", nil, false,
		", last command exited with status 0, command 1 of its pipeline of 2 with 2 (statuses: 2 0) (")

	withoutPipelines, _, _, _ := connectToolsWith(t, "test-ratd-nopipeline", tmux.TerminalsCheck{BracketedPaste: true,
		Prompts: true, Statuses: true})
	if list, err = withoutPipelines.ListTools(context.Background(), nil); err != nil {
		t.Fatal(err)
	}
	for _, tool := range list.Tools {
		if strings.Contains(tool.Description, pipelineSentence) {
			t.Errorf("expected %s not to tell how pipelines exit where they are not recorded, got %q", tool.Name,
				tool.Description)
		}
	}
}
