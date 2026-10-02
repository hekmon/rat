package tools

import (
	"encoding/json"
	"fmt"
	"reflect"
	"strconv"
	"time"

	"github.com/google/jsonschema-go/jsonschema"
)

// Names of the tools.
const (
	ListWindows  = "list_windows"
	CreateWindow = "create_window"
	CloseWindow  = "close_window"
	SendText     = "send_text"
	SendKeys     = "send_keys"
	ReadWindow   = "read_window"
	WaitWindow   = "wait_window"
	WriteFile    = "write_file"
	ReadFile     = "read_file"
)

// NoInput is the input of a tool taking no argument: list_windows.
type NoInput struct{}

// NameInput is the input of a tool acting on a window it names: create_window, close_window.
type NameInput struct {
	Name string `json:"name" jsonschema:"the name of the window: letters, digits, '_' and '-', up to 32 characters, not digits only"`
}

// SendTextInput is the input of send_text. Enter is required, with no default: the agent decides
// every time whether the text runs, and forgetting it fails validation instead of silently
// leaving a command unrun (false) or running text meant to wait (true).
type SendTextInput struct {
	Window string `json:"window" jsonschema:"the name of the window"`
	Text   string `json:"text" jsonschema:"the text to paste, as is: new lines included, nothing added; no control character but tab and new lines (keys: send_keys)"`
	Enter  bool   `json:"enter" jsonschema:"true runs the text at a prompt; false leaves it on the command line, or answers a program without Enter"`
}

// SendKeysInput is the input of send_keys.
type SendKeysInput struct {
	Window string   `json:"window" jsonschema:"the name of the window"`
	Keys   []string `json:"keys" jsonschema:"the key names, pressed in order"`
}

// ReadWindowInput is the input of read_window. The rows are unsigned: the schema then refuses a
// negative number.
type ReadWindowInput struct {
	Window         string `json:"window" jsonschema:"the name of the window"`
	ScrollbackRows uint   `json:"scrollback_rows,omitempty" jsonschema:"rows of history to include above the screen"`
}

// WaitWindowInput is the input of wait_window. max_seconds 0 is DefaultWaitSeconds: the schema can
// not tell 0 from a missing number, and bounds the others (see Schemas).
type WaitWindowInput struct {
	Window     string `json:"window" jsonschema:"the name of the window"`
	MaxSeconds uint   `json:"max_seconds,omitempty" jsonschema:"the most seconds to wait"`
}

const (
	// DefaultWaitSeconds is how long wait_window waits at most, unless told.
	DefaultWaitSeconds = 20
	// MaxWaitSeconds is the longest wait_window waits. A harness cuts a tool call at a limit of its
	// own, which ratd can not see, nor push back: it answers without a stream, with no progress to
	// send meanwhile. Several harnesses cut at 60 seconds by default (Cline, Zed, OpenCode, Continue,
	// the Cursor CLI, and Claude Code reaching ratd over HTTP, checked in their code in 2026-10): a
	// wait of 60 seconds would race them, its answer coming after the cut, and the agent getting an
	// error from its harness instead. 50 leaves room for the network and for ratd to answer.
	MaxWaitSeconds = 50
)

// Wait returns how long a call of wait_window with arguments waits at most: max_seconds, or
// DefaultWaitSeconds when missing or not a number, MaxWaitSeconds at most. ratd refuses a value
// out of bounds, but rat, which bounds its request with it, reads it before.
func Wait(arguments json.RawMessage) time.Duration {
	var args struct {
		MaxSeconds *float64 `json:"max_seconds"`
	}
	seconds := float64(DefaultWaitSeconds)
	if json.Unmarshal(arguments, &args) == nil && args.MaxSeconds != nil {
		seconds = min(max(*args.MaxSeconds, 0), MaxWaitSeconds)
	}
	return time.Duration(seconds * float64(time.Second))
}

// WriteFileInput is the input of write_file. The content is required, and may be empty.
type WriteFileInput struct {
	Path    string `json:"path" jsonschema:"absolute, or starting with ~/ for the home directory"`
	Content string `json:"content" jsonschema:"the whole content of the file, written as is"`
}

// ReadFileInput is the input of read_file. start_line 0 is no start: lines count from 1, and the
// schema can not tell 0 from a missing number. max_lines is unsigned: the schema then refuses a
// negative number.
type ReadFileInput struct {
	Path        string `json:"path" jsonschema:"absolute, or starting with ~/ for the home directory"`
	StartLine   int    `json:"start_line,omitempty" jsonschema:"the first line to read, from 1; negative counts from the end (-1 is the last line)"`
	MaxLines    uint   `json:"max_lines,omitempty" jsonschema:"the most lines to read, all by default"`
	LineNumbers bool   `json:"line_numbers,omitempty" jsonschema:"prefix each line with its number and a tab, as cat -n does: to refer to lines, not to write the content back"`
}

// inputs are the input types of the tools, by name.
var inputs = map[string]reflect.Type{
	ListWindows:  reflect.TypeFor[NoInput](),
	CreateWindow: reflect.TypeFor[NameInput](),
	CloseWindow:  reflect.TypeFor[NameInput](),
	SendText:     reflect.TypeFor[SendTextInput](),
	SendKeys:     reflect.TypeFor[SendKeysInput](),
	ReadWindow:   reflect.TypeFor[ReadWindowInput](),
	WaitWindow:   reflect.TypeFor[WaitWindowInput](),
	WriteFile:    reflect.TypeFor[WriteFileInput](),
	ReadFile:     reflect.TypeFor[ReadFileInput](),
}

// Schemas returns the input schema of each tool, by name: inferred from its type as the MCP SDK
// infers it, then annotated. New schemas at each call: ratd builds them once and serves the same
// ones to every request, as the schema cache of the SDK keys them by pointer.
func Schemas() (map[string]*jsonschema.Schema, error) {
	schemas := make(map[string]*jsonschema.Schema, len(inputs))
	for name, input := range inputs {
		schema, err := jsonschema.ForType(input, &jsonschema.ForOptions{})
		if err != nil {
			return nil, fmt.Errorf("input schema of %s: %w", name, err)
		}
		schemas[name] = schema
	}
	// max_seconds of wait_window is bounded, and defaults: the schema tells agents, and the SDK
	// refuses a value out of bounds before the call
	maxSeconds := schemas[WaitWindow].Properties["max_seconds"]
	minimum, maximum := 1.0, float64(MaxWaitSeconds)
	maxSeconds.Minimum, maxSeconds.Maximum = &minimum, &maximum
	maxSeconds.Default = json.RawMessage(strconv.Itoa(DefaultWaitSeconds))
	return schemas, nil
}
