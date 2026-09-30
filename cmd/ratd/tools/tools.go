package tools

import (
	"fmt"
	"reflect"

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
	WriteFile    = "write_file"
	ReadFile     = "read_file"
)

// NoInput is the input of a tool taking no argument: list_windows.
type NoInput struct{}

// NameInput is the input of a tool acting on a window it names: create_window, close_window.
type NameInput struct {
	Name string `json:"name" jsonschema:"the name of the window: letters, digits, '_' and '-', up to 32 characters"`
}

// SendTextInput is the input of send_text. Enter is required, with no default: the agent decides
// every time whether the text runs, and forgetting it fails validation instead of silently
// leaving a command unrun (false) or running text meant to wait (true).
type SendTextInput struct {
	Window string `json:"window" jsonschema:"the name of the window"`
	Text   string `json:"text" jsonschema:"the text to paste, as is: new lines included, nothing added"`
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
	return schemas, nil
}
