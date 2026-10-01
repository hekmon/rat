package main

import (
	"context"
	"log/slog"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/hekmon/rat/mtls"
	"github.com/hekmon/rat/tmux"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// toolsSession is the tmux session of the client the tool tests act for.
const toolsSession = "alice"

// connectTools starts a tmux server for tenant, with a fresh home, and returns an MCP client
// connected in memory to the tools of session alice (tools need nothing from HTTP), the controller
// and the logs of ratd. All is stopped when the test ends.
func connectTools(t *testing.T, tenant string) (*mcp.ClientSession, *tmux.Controller, *logBuffer, string) {
	t.Helper()
	requireTmux(t)
	home := testHome(t)
	controller, err := tmux.New(tenant)
	if err != nil {
		t.Fatal(err)
	}
	if err = controller.StartServer(context.Background()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = controller.StopServer(context.Background()) })
	session, logs := connectDaemon(t, controller)
	return session, controller, logs, home
}

// testHome sets HOME to a fresh directory until the test ends, and returns it, symbolic links
// resolved (the temporary directory of macOS is behind one).
func testHome(t *testing.T) string {
	t.Helper()
	home, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", home)
	return home
}

// connectDaemon returns an MCP client connected in memory to the tools of session alice, for
// ratd with the terminals of controller, and the logs of ratd. All is stopped when the test ends.
func connectDaemon(t *testing.T, controller *tmux.Controller) (*mcp.ClientSession, *logBuffer) {
	t.Helper()
	ctx := context.Background()
	logs := &logBuffer{}
	d, err := newDaemon(slog.New(slog.NewTextHandler(logs, nil)), &mtls.Side{Tenant: "t"}, controller, false, defaultReadBudget)
	if err != nil {
		t.Fatal(err)
	}
	serverTransport, clientTransport := mcp.NewInMemoryTransports()
	serverSession, err := d.newServer(toolsSession).Connect(ctx, serverTransport, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = serverSession.Close() })
	session, err := mcp.NewClient(&mcp.Implementation{Name: "test", Version: "1"}, nil).Connect(ctx, clientTransport, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = session.Close() })
	return session, logs
}

// callTool calls tool with args, and returns the text of its result and whether it is an error.
func callTool(t *testing.T, session *mcp.ClientSession, tool string, args map[string]any) (string, bool) {
	t.Helper()
	result, err := session.CallTool(context.Background(), &mcp.CallToolParams{Name: tool, Arguments: args})
	if err != nil {
		t.Fatalf("%s: %v", tool, err)
	}
	var text strings.Builder
	for _, content := range result.Content {
		if c, ok := content.(*mcp.TextContent); ok {
			text.WriteString(c.Text)
		}
	}
	return text.String(), result.IsError
}

// expectTool calls tool with args, and fails the test if its result is not expected, or does not
// contain each of parts.
func expectTool(t *testing.T, session *mcp.ClientSession, tool string, args map[string]any, isError bool, parts ...string) string {
	t.Helper()
	text, gotError := callTool(t, session, tool, args)
	if gotError != isError {
		t.Errorf("%s %v: expected IsError %v, got %v: %s", tool, args, isError, gotError, text)
	}
	for _, part := range parts {
		if !strings.Contains(text, part) {
			t.Errorf("%s %v: expected %q in:\n%s", tool, args, part, text)
		}
	}
	return text
}

// TestListWindows guards the description of the windows: a missing session created with a header
// saying so, then each window with its command, absolute directory, activity, and history or
// full-screen program.
func TestListWindows(t *testing.T) {
	session, controller, _, home := connectTools(t, "test-ratd-list")
	ctx := context.Background()
	expectTool(t, session, "list_windows", nil, false,
		"[your terminals did not exist: main was just created", "\nmain: bash in "+home+", active ")
	if text := expectTool(t, session, "list_windows", nil, false); strings.HasPrefix(text, "[") {
		t.Errorf("expected no header once the session exists, got:\n%s", text)
	}
	if err := controller.SendText(ctx, toolsSession, tmux.FirstWindow, "seq 1 100", true); err != nil {
		t.Fatal(err)
	}
	expectTool(t, session, "create_window", map[string]any{"name": "fs"}, false)
	if err := controller.SendText(ctx, toolsSession, "fs", `printf '\033[?1049h'; sleep 30`, true); err != nil {
		t.Fatal(err)
	}
	eventually(t, "history and full-screen program listed", func() bool {
		text, _ := callTool(t, session, "list_windows", nil)
		return strings.Contains(text, " rows of history") && strings.Contains(text, "fs: sleep in "+home) &&
			strings.Contains(text, ", full-screen program")
	})
}

// TestCreateWindow guards creating windows: main in a missing session is created by creating the
// session, an existing window is not recreated but described, and an invalid name is refused with
// the naming rule.
func TestCreateWindow(t *testing.T) {
	session, _, _, home := connectTools(t, "test-ratd-create")
	expectTool(t, session, "create_window", map[string]any{"name": "main"}, false,
		"Window main created: bash is starting in "+home+".")
	if text := expectTool(t, session, "list_windows", nil, false); strings.Count(text, "\n") != 0 {
		t.Errorf("expected main alone, got:\n%s", text)
	}
	expectTool(t, session, "create_window", map[string]any{"name": "build"}, false, "Window build created")
	expectTool(t, session, "create_window", map[string]any{"name": "build"}, false,
		"Window build already exists, not created: it runs bash in "+home+".")
	expectTool(t, session, "create_window", map[string]any{"name": "a b"}, true, `Invalid window name "a b": `+nameRule)
}

// TestCloseWindow guards closing windows: nothing to close is a message that creates nothing, and
// closing the last window tells a fresh main comes next.
func TestCloseWindow(t *testing.T) {
	session, controller, _, _ := connectTools(t, "test-ratd-close")
	expectTool(t, session, "close_window", map[string]any{"name": "build"}, false, "No window build: nothing to close.")
	if sessions, err := controller.ListSessions(context.Background()); err != nil || slices.Contains(sessions, toolsSession) {
		t.Errorf("expected no session created, got %v, %v", sessions, err)
	}
	expectTool(t, session, "create_window", map[string]any{"name": "build"}, false)
	if text := expectTool(t, session, "close_window", map[string]any{"name": "build"}, false, "Window build closed."); strings.Contains(text, "last") {
		t.Errorf("main remains: expected no last window note, got %q", text)
	}
	expectTool(t, session, "close_window", map[string]any{"name": "build"}, false, "No window build: nothing to close.")
	expectTool(t, session, "close_window", map[string]any{"name": "main"}, false,
		"Window main closed. It was your last window: a fresh main will be there on your next call.")
}

// TestToolFailures guards the failures told to agents, and the log line of each call: tmux
// restarting, then tmux not answering in time (its server stopped by a signal), logged as a
// warning.
func TestToolFailures(t *testing.T) {
	const tenant = "test-ratd-failures"
	session, controller, logs, _ := connectTools(t, tenant)
	expectTool(t, session, "create_window", map[string]any{"name": "build"}, false)
	if !strings.Contains(logs.String(), "msg=tool session=alice tool=create_window window=build outcome=ok") {
		t.Errorf("expected the call to be logged:\n%s", logs)
	}

	timeout := toolTimeout
	toolTimeout = 500 * time.Millisecond
	t.Cleanup(func() { toolTimeout = timeout })
	out, err := exec.Command("tmux", "-L", "rat-"+tenant, "display-message", "-p", "#{pid}").Output()
	if err != nil {
		t.Fatal(err)
	}
	pid := strings.TrimSpace(string(out))
	if err = exec.Command("kill", "-STOP", pid).Run(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = exec.Command("kill", "-CONT", pid).Run() })
	expectTool(t, session, "list_windows", nil, true, "The terminals did not answer in time: retrying may work.")
	if !strings.Contains(logs.String(), "level=WARN msg=tool session=alice tool=list_windows outcome=error") {
		t.Errorf("expected the timeout to be logged as a warning:\n%s", logs)
	}
	_ = exec.Command("kill", "-CONT", pid).Run()

	if err = controller.StopServer(context.Background()); err != nil {
		t.Fatal(err)
	}
	expectTool(t, session, "close_window", map[string]any{"name": "build"}, true,
		"The terminals are restarting after a failure, and every window was lost.")
}

// TestToolsAnnotations guards the hints of the tools, set explicitly: the specification reads a
// missing destructive or open world hint as true. Only the inputs reach an open world.
func TestToolsAnnotations(t *testing.T) {
	session, _, _, _ := connectTools(t, "test-ratd-hints")
	tools, err := session.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	expected := map[string]mcp.ToolAnnotations{
		"list_windows":  {ReadOnlyHint: true, OpenWorldHint: ptr(false)},
		"create_window": {DestructiveHint: ptr(false), IdempotentHint: true, OpenWorldHint: ptr(false)},
		"close_window":  {DestructiveHint: ptr(true), IdempotentHint: true, OpenWorldHint: ptr(false)},
		"send_text":     {DestructiveHint: ptr(true), OpenWorldHint: ptr(true)},
		"send_keys":     {DestructiveHint: ptr(true), OpenWorldHint: ptr(true)},
		"read_window":   {ReadOnlyHint: true, OpenWorldHint: ptr(false)},
		"write_file":    {DestructiveHint: ptr(true), IdempotentHint: true, OpenWorldHint: ptr(false)},
		"read_file":     {ReadOnlyHint: true, OpenWorldHint: ptr(false)},
	}
	for _, tool := range tools.Tools {
		want, ok := expected[tool.Name]
		if !ok {
			continue
		}
		delete(expected, tool.Name)
		got := tool.Annotations
		if got == nil || got.ReadOnlyHint != want.ReadOnlyHint || got.IdempotentHint != want.IdempotentHint ||
			!equalHint(got.DestructiveHint, want.DestructiveHint) || !equalHint(got.OpenWorldHint, want.OpenWorldHint) {
			t.Errorf("%s: unexpected annotations %+v", tool.Name, got)
		}
	}
	if len(expected) > 0 {
		t.Errorf("tools missing: %v", expected)
	}
}

// equalHint tells whether two optional hints are equal, unset ones included.
func equalHint(a, b *bool) bool {
	return (a == nil) == (b == nil) && (a == nil || *a == *b)
}
