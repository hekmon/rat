package tools

import (
	"context"
	"encoding/json"
	"maps"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/google/jsonschema-go/jsonschema"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// annotated returns a schema of type typ, bound to header.
func annotated(typ, header string) *jsonschema.Schema {
	return &jsonschema.Schema{Type: typ, Extra: map[string]any{HeaderAnnotation: header}}
}

// probeSchema is the input schema of a tool annotating arguments of each kind, one of them nested:
// no tool of ratd annotates one yet.
func probeSchema() *jsonschema.Schema {
	return &jsonschema.Schema{Type: "object", Properties: map[string]*jsonschema.Schema{
		"text":  annotated("string", "Text"),
		"count": annotated("integer", "Count"),
		"flag":  annotated("boolean", "Flag"),
		"nested": {Type: "object", Properties: map[string]*jsonschema.Schema{
			"inner": annotated("string", "Inner"),
		}},
		"plain": {Type: "string"},
	}}
}

// headerRecorder serves handler, keeping the headers of the last tools/call it received.
type headerRecorder struct {
	handler http.Handler
	mu      sync.Mutex
	last    http.Header
}

func (r *headerRecorder) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	if req.Header.Get("Mcp-Method") == "tools/call" {
		r.mu.Lock()
		r.last = req.Header.Clone()
		r.mu.Unlock()
	}
	r.handler.ServeHTTP(w, req)
}

// paramHeaders returns the Mcp-Param headers of the last tools/call.
func (r *headerRecorder) paramHeaders() map[string]string {
	r.mu.Lock()
	defer r.mu.Unlock()
	headers := map[string]string{}
	for name := range r.last {
		if strings.HasPrefix(name, ParamHeaderPrefix) {
			headers[name] = r.last.Get(name)
		}
	}
	return headers
}

// TestParamHeadersMatchSDK guards that ParamHeaders computes the headers of annotated arguments
// exactly as the MCP SDK does: its client calls a tool of its server with each set of arguments,
// the server checks the headers against the arguments (refusing the call on a mismatch), and the
// headers the client sent are those ParamHeaders returns. The protocol version is checked to be
// one sending them, lest empty sets be compared.
func TestParamHeadersMatchSDK(t *testing.T) {
	schema := probeSchema()
	server := mcp.NewServer(&mcp.Implementation{Name: "probe", Version: "1"}, nil)
	mcp.AddTool(server, &mcp.Tool{Name: "probe", InputSchema: schema},
		func(context.Context, *mcp.CallToolRequest, map[string]any) (*mcp.CallToolResult, any, error) {
			return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: "ok"}}}, nil, nil
		})
	recorder := &headerRecorder{handler: mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return server },
		&mcp.StreamableHTTPOptions{Stateless: true, JSONResponse: true})}
	httpServer := httptest.NewServer(recorder)
	defer httpServer.Close()
	ctx := context.Background()
	session, err := mcp.NewClient(&mcp.Implementation{Name: "test", Version: "1"}, nil).Connect(ctx,
		&mcp.StreamableClientTransport{Endpoint: httpServer.URL}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer session.Close()
	// the client sends the headers of the tools it listed
	if _, err = session.ListTools(ctx, nil); err != nil {
		t.Fatal(err)
	}
	for _, args := range []map[string]any{
		{"text": "plain", "count": 42, "flag": true, "nested": map[string]any{"inner": "deep"}, "plain": "not bound"},
		{"text": "café", "count": -7, "flag": false},
		{"text": " space around "},
		{"text": "tab\t"},
		{"text": "line\nbreak"},
		{"text": "=?base64?looks encoded?="},
		{"text": "del\x7f"},
		{"plain": "nothing bound"},
	} {
		result, err := session.CallTool(ctx, &mcp.CallToolParams{Name: "probe", Arguments: args})
		if err != nil || result.IsError {
			t.Errorf("%v: expected the server to accept the call, got %v, %+v", args, err, result)
			continue
		}
		if version := recorder.last.Get("Mcp-Protocol-Version"); version < "2026-07-28" {
			t.Fatalf("protocol %q sends no parameter header: nothing compared", version)
		}
		arguments, err := json.Marshal(args)
		if err != nil {
			t.Fatal(err)
		}
		sent, computed := recorder.paramHeaders(), ParamHeaders(schema, arguments)
		if computed == nil {
			computed = map[string]string{}
		}
		if !maps.Equal(sent, computed) {
			t.Errorf("%v: the SDK sent %v, ParamHeaders computed %v", args, sent, computed)
		}
	}
}

// TestParamHeadersNone guards the arguments carried by no header: missing, null, of another type
// than a string, a boolean or an integer JavaScript represents exactly, and without a schema.
func TestParamHeadersNone(t *testing.T) {
	for _, arguments := range []string{
		`{}`, `{"text": null}`, `{"count": 1.5}`, `{"count": 9007199254740993}`, `{"text": ["a"]}`, `{"nested": "flat"}`,
		`not json`,
	} {
		if headers := ParamHeaders(probeSchema(), json.RawMessage(arguments)); headers != nil {
			t.Errorf("%s: expected no header, got %v", arguments, headers)
		}
	}
	if headers := ParamHeaders(nil, json.RawMessage(`{"text": "a"}`)); headers != nil {
		t.Errorf("no schema: expected no header, got %v", headers)
	}
}

// TestSchemas guards that every tool has a schema, of an object: the MCP SDK requires it.
func TestSchemas(t *testing.T) {
	schemas, err := Schemas()
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{ListWindows, CreateWindow, CloseWindow, SendText, SendKeys, ReadWindow, WriteFile, ReadFile} {
		if schema := schemas[name]; schema == nil || schema.Type != "object" {
			t.Errorf("%s: expected an object schema, got %+v", name, schema)
		}
	}
}
