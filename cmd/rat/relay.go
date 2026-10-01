package main

import (
	"bufio"
	"bytes"
	"context"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/hekmon/rat/cmd/ratd/tools"
	"github.com/hekmon/rat/connect"
	"github.com/hekmon/rat/internal/flags"
	"github.com/hekmon/rat/mtls"

	"github.com/google/jsonschema-go/jsonschema"
	"github.com/modelcontextprotocol/go-sdk/jsonrpc"
)

const (
	// maxMessage is the largest message relayed, as the stdio transport of the MCP SDK reads them.
	maxMessage = 16 << 20
	// codeRelayFailed is the JSON-RPC error code of a call rat could not relay to ratd: in the
	// range JSON-RPC leaves to implementations, apart from the codes MCP and its SDKs use.
	codeRelayFailed = -32050
	// headersProtocol is the first protocol version whose requests carry their version in _meta,
	// and the standard headers (Mcp-Method, Mcp-Name, Mcp-Param-*).
	headersProtocol = "2026-07-28"
	// metaProtocolVersion is the _meta key carrying the protocol version of a request.
	metaProtocolVersion = "io.modelcontextprotocol/protocolVersion"
)

// requestTimeout bounds a request to ratd: above the 10 seconds ratd gives a tool call, so that a
// connection hanging is cut rather than waited for. A variable for tests.
var requestTimeout = 30 * time.Second

// relay relays the messages of a harness to ratd, and the answers of ratd back. It reads inside
// a message only to prepare its HTTP request, never changing what it relays.
type relay struct {
	logger   *slog.Logger
	client   *http.Client
	target   flags.Target
	endpoint string
	// schemas are the input schemas of the tools of ratd, for the headers of annotated arguments
	schemas map[string]*jsonschema.Schema
	// outMu writes the answers to out one at a time
	outMu sync.Mutex
	out   io.Writer
	// versionMu guards version, the protocol version negotiated by initialize, for the messages
	// of older protocols, which carry none
	versionMu sync.Mutex
	version   string
	// callsMu guards calls, the calls in flight by identifier, to cancel on notifications/cancelled
	callsMu sync.Mutex
	calls   map[jsonrpc.ID]*call
	// inFlight counts the requests in flight, waited for when stopping
	inFlight sync.WaitGroup
}

// call is a call in flight.
type call struct {
	cancel context.CancelFunc
	// canceled tells the harness canceled the call: nothing is written for it
	canceled atomic.Bool
}

// newRelay returns a relay to the ratd of target, through client, writing the answers to out.
func newRelay(logger *slog.Logger, client *http.Client, target flags.Target, schemas map[string]*jsonschema.Schema,
	out io.Writer) *relay {
	return &relay{logger: logger, client: client, target: target, endpoint: connect.Endpoint(target.Server), schemas: schemas,
		out: out, calls: map[jsonrpc.ID]*call{}}
}

// line is a line read from the harness.
type line struct {
	data []byte
	// tooLong tells the line was over maxMessage: its data is dropped
	tooLong bool
	err     error
}

// run relays the messages read from in until it ends or ctx is done, then cancels the requests in
// flight, and returns why it stopped.
func (r *relay) run(ctx context.Context, in io.Reader) string {
	lines := make(chan line)
	stopped := make(chan struct{})
	defer close(stopped)
	go func() {
		reader := bufio.NewReaderSize(in, 64<<10)
		for {
			l := readLine(reader)
			select {
			case lines <- l:
			case <-stopped:
				return
			}
			if l.err != nil {
				return
			}
		}
	}()
	requests, cancelRequests := context.WithCancel(ctx)
	defer func() {
		cancelRequests()
		r.inFlight.Wait()
	}()
	for {
		select {
		case <-ctx.Done():
			return "signal"
		case l := <-lines:
			if l.tooLong || len(bytes.TrimSpace(l.data)) > 0 {
				r.handle(requests, l)
			}
			switch {
			case errors.Is(l.err, io.EOF):
				return "end of input"
			case l.err != nil:
				return "reading the input failed: " + l.err.Error()
			}
		}
	}
}

// readLine reads a line from reader, without its end of line, up to maxMessage bytes.
func readLine(reader *bufio.Reader) line {
	var l line
	for {
		chunk, err := reader.ReadSlice('\n')
		if !l.tooLong {
			if len(l.data)+len(chunk) > maxMessage {
				l.tooLong, l.data = true, nil
			} else {
				l.data = append(l.data, chunk...)
			}
		}
		if !errors.Is(err, bufio.ErrBufferFull) {
			l.data, l.err = bytes.TrimRight(l.data, "\r\n"), err
			return l
		}
	}
}

// message is what rat reads of a message, to prepare its request.
type message struct {
	ID     json.RawMessage `json:"id"`
	Method string          `json:"method"`
	Params json.RawMessage `json:"params"`
}

// params is what rat reads of the parameters of a request.
type params struct {
	Meta      map[string]any  `json:"_meta"`
	Name      string          `json:"name"`
	URI       string          `json:"uri"`
	Arguments json.RawMessage `json:"arguments"`
	RequestID any             `json:"requestId"`
}

// isCall tells whether m is a request expecting a response.
func (m message) isCall() bool {
	return m.Method != "" && len(m.ID) > 0 && string(m.ID) != "null"
}

// handle relays a line of the harness: a call, answered by ratd or by rat when it could not be
// relayed; anything else (a notification, a response, a batch), only logged when it could not.
func (r *relay) handle(ctx context.Context, l line) {
	if l.tooLong {
		r.logger.Warn("message over 16 MiB, not relayed")
		r.writeError(nil, codeRelayFailed, "message over 16 MiB, not relayed")
		return
	}
	data := bytes.TrimSpace(l.data)
	if data[0] == '[' {
		// a batch (protocol 2025-03-26 only): relayed as is, with the negotiated version
		r.relayOther(ctx, data, r.header(message{}, params{}), "batch")
		return
	}
	var msg message
	if err := json.Unmarshal(data, &msg); err != nil {
		r.logger.Warn("invalid message from the harness", "error", err)
		r.writeError(nil, jsonrpc.CodeParseError, "invalid JSON-RPC message: "+err.Error())
		return
	}
	var p params
	// best effort: a field of another type is left empty, the others read
	_ = json.Unmarshal(msg.Params, &p)
	header := r.header(msg, p)
	if !msg.isCall() {
		if msg.Method == "notifications/cancelled" {
			r.cancelCall(p.RequestID)
		}
		r.relayOther(ctx, data, header, msg.Method)
		return
	}
	id, err := makeID(msg.ID)
	if err != nil {
		r.writeError(nil, jsonrpc.CodeInvalidRequest, "invalid JSON-RPC message: "+err.Error())
		return
	}
	// registered before relaying, for a cancellation read next to find it
	callCtx, cancel := context.WithTimeout(ctx, requestTimeout)
	c := &call{cancel: cancel}
	r.callsMu.Lock()
	r.calls[id] = c
	r.callsMu.Unlock()
	r.inFlight.Add(1)
	go func() {
		defer r.inFlight.Done()
		defer func() {
			cancel()
			r.callsMu.Lock()
			if r.calls[id] == c {
				delete(r.calls, id)
			}
			r.callsMu.Unlock()
		}()
		r.relayCall(callCtx, c, msg, data, header)
	}()
}

// makeID returns the identifier of a request, as JSON-RPC compares them: 1 and "1" differ.
func makeID(raw json.RawMessage) (jsonrpc.ID, error) {
	var value any
	if err := json.Unmarshal(raw, &value); err != nil {
		return jsonrpc.ID{}, err
	}
	return jsonrpc.MakeID(value)
}

// cancelCall cancels the call requestID names, if still in flight: its request to ratd ends,
// which cancels it in ratd from protocol 2026-07-28 on, and nothing is written for it.
func (r *relay) cancelCall(requestID any) {
	id, err := jsonrpc.MakeID(requestID)
	if err != nil {
		return
	}
	r.callsMu.Lock()
	c := r.calls[id]
	r.callsMu.Unlock()
	if c != nil {
		c.canceled.Store(true)
		c.cancel()
		r.logger.Debug("call canceled", "id", id.Raw())
	}
}

// header returns the headers of the request relaying msg, of parameters p: its protocol version
// (carried by the request, or else negotiated by initialize), and from protocol 2026-07-28 on,
// the standard headers derived from it.
func (r *relay) header(msg message, p params) http.Header {
	header := http.Header{}
	header.Set("Content-Type", "application/json")
	// both, as the specification requires, ratd answering in JSON
	header.Set("Accept", "application/json, text/event-stream")
	version, _ := p.Meta[metaProtocolVersion].(string)
	if version == "" {
		r.versionMu.Lock()
		version = r.version
		r.versionMu.Unlock()
	}
	if version == "" {
		return header
	}
	header.Set("Mcp-Protocol-Version", version)
	if version < headersProtocol || msg.Method == "" {
		return header
	}
	header.Set("Mcp-Method", msg.Method)
	switch msg.Method {
	case "tools/call", "prompts/get":
		if p.Name != "" {
			header.Set("Mcp-Name", p.Name)
		}
	case "resources/read":
		if p.URI != "" {
			header.Set("Mcp-Name", p.URI)
		}
	}
	if msg.Method == "tools/call" {
		for name, value := range tools.ParamHeaders(r.schemas[p.Name], p.Arguments) {
			header.Set(name, value)
		}
	}
	return header
}

// response is what ratd answered a request.
type response struct {
	status      int
	contentType string
	body        []byte
}

// post sends data to ratd with header, and returns its answer.
func (r *relay) post(ctx context.Context, data []byte, header http.Header) (response, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, r.endpoint, bytes.NewReader(data))
	if err != nil {
		return response{}, err
	}
	req.Header = header
	resp, err := r.client.Do(req)
	if err != nil {
		return response{}, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxMessage+1))
	if err == nil && len(body) > maxMessage {
		err = errors.New("response over 16 MiB")
	}
	return response{status: resp.StatusCode, contentType: resp.Header.Get("Content-Type"), body: body}, err
}

// relayCall relays the call msg, data as read, and writes the answer: the response of ratd, or an
// error telling why there is none. Nothing is written for a call the harness canceled, nor while
// rat stops.
func (r *relay) relayCall(ctx context.Context, c *call, msg message, data []byte, header http.Header) {
	start := time.Now()
	resp, err := r.post(ctx, data, header)
	if c.canceled.Load() || errors.Is(context.Cause(ctx), context.Canceled) {
		return
	}
	r.logger.Debug("message", "method", msg.Method, "id", string(msg.ID), "status", resp.status,
		"duration", time.Since(start))
	if err != nil {
		r.logger.Warn("relay failed", "method", msg.Method, "id", string(msg.ID), "error", err)
		r.writeError(msg.ID, codeRelayFailed, r.failureText(ctx, err))
		return
	}
	isResponse := isResponseTo(resp.body, msg.ID)
	switch {
	case resp.status == http.StatusOK && isResponse:
		if msg.Method == "initialize" {
			r.keepVersion(resp.body)
		}
	case resp.status >= http.StatusBadRequest && isResponse:
		// ratd, or its SDK, refused the call itself: its error is the answer
	default:
		text := r.invalidResponseText(resp)
		r.logger.Warn("relay failed", "method", msg.Method, "id", string(msg.ID), "error", text)
		r.writeError(msg.ID, codeRelayFailed, text)
		return
	}
	r.write(resp.body)
}

// relayOther relays data, a notification, a response or a batch, which gets no answer but for a
// batch of calls; what fails is only logged, having no identifier to answer.
func (r *relay) relayOther(ctx context.Context, data []byte, header http.Header, what string) {
	r.inFlight.Add(1)
	go func() {
		defer r.inFlight.Done()
		ctx, cancel := context.WithTimeout(ctx, requestTimeout)
		defer cancel()
		start := time.Now()
		resp, err := r.post(ctx, data, header)
		if errors.Is(context.Cause(ctx), context.Canceled) {
			return
		}
		r.logger.Debug("message", "method", what, "status", resp.status, "duration", time.Since(start))
		switch {
		case err != nil:
			r.logger.Warn("relay failed", "method", what, "error", err)
		case resp.status == http.StatusAccepted:
		case resp.status == http.StatusOK && json.Valid(resp.body):
			r.write(resp.body)
		default:
			r.logger.Warn("relay failed", "method", what, "error", r.invalidResponseText(resp))
		}
	}()
}

// keepVersion keeps the protocol version negotiated by initialize, from its result, for the
// messages of older protocols, which carry none.
func (r *relay) keepVersion(body []byte) {
	var result struct {
		Result struct {
			ProtocolVersion string `json:"protocolVersion"`
		} `json:"result"`
	}
	if json.Unmarshal(body, &result) == nil && result.Result.ProtocolVersion != "" {
		r.versionMu.Lock()
		r.version = result.Result.ProtocolVersion
		r.versionMu.Unlock()
	}
}

// isResponseTo tells whether body is a JSON-RPC response to the request of identifier rawID.
func isResponseTo(body []byte, rawID json.RawMessage) bool {
	var resp struct {
		JSONRPC string          `json:"jsonrpc"`
		ID      json.RawMessage `json:"id"`
		Result  json.RawMessage `json:"result"`
		Error   json.RawMessage `json:"error"`
	}
	if json.Unmarshal(body, &resp) != nil || resp.JSONRPC != "2.0" || (resp.Result == nil && resp.Error == nil) {
		return false
	}
	id, err := makeID(resp.ID)
	want, wantErr := makeID(rawID)
	return err == nil && wantErr == nil && id == want
}

// failureText tells the harness why a request did not reach ratd, or got no answer: ratd
// unreachable, refusing the handshake or refused by rat, or not answering in time.
func (r *relay) failureText(ctx context.Context, err error) string {
	server := r.target.Server
	var opErr *net.OpError
	var urlErr interface{ Unwrap() error }
	switch {
	case errors.Is(context.Cause(ctx), context.DeadlineExceeded):
		return fmt.Sprintf("ratd at %s did not answer within %s", server, requestTimeout)
	case errors.As(err, &opErr) && opErr.Op == "remote error":
		return fmt.Sprintf("ratd at %s refused the TLS handshake: %s. Check the bundle with rat-tool check.", server, opErr)
	case refusedByRat(err) && errors.As(err, &urlErr):
		return fmt.Sprintf("rat refused the certificate of ratd at %s: %s. Check the bundle with rat-tool check.", server,
			urlErr.Unwrap())
	default:
		return fmt.Sprintf("ratd unreachable at %s: %s", server, innermost(err))
	}
}

// refusedByRat tells whether err is rat refusing the certificate of the server (package mtls).
func refusedByRat(err error) bool {
	var unknownAuthority x509.UnknownAuthorityError
	var invalid x509.CertificateInvalidError
	return errors.Is(err, mtls.ErrNotSignedByCA) || errors.Is(err, mtls.ErrValidity) || errors.Is(err, mtls.ErrRole) ||
		errors.As(err, &unknownAuthority) || errors.As(err, &invalid)
}

// innermost returns the message of the innermost error of err: the reason the system gave.
func innermost(err error) string {
	for {
		next := errors.Unwrap(err)
		if next == nil {
			return err.Error()
		}
		err = next
	}
}

// invalidResponseText tells the harness why an answer of ratd is not a response to relay.
func (r *relay) invalidResponseText(resp response) string {
	server := r.target.Server
	switch {
	case resp.status >= http.StatusBadRequest:
		text := fmt.Sprintf("ratd at %s answered HTTP %d", server, resp.status)
		if first, _, _ := strings.Cut(strings.TrimSpace(string(resp.body)), "\n"); first != "" {
			if len(first) > 200 {
				first = first[:200]
			}
			text += ": " + first
		}
		return text
	case strings.HasPrefix(resp.contentType, "text/event-stream"):
		return fmt.Sprintf("ratd at %s sent an invalid response: an event stream rather than JSON", server)
	case resp.status == http.StatusAccepted:
		return fmt.Sprintf("ratd at %s sent an invalid response: none to the call (HTTP 202)", server)
	default:
		return fmt.Sprintf("ratd at %s sent an invalid response: not a JSON-RPC response to the call (HTTP %d)",
			server, resp.status)
	}
}

// writeError writes a JSON-RPC error to the harness, answering the request of identifier rawID
// (null when unknown).
func (r *relay) writeError(rawID json.RawMessage, code int, text string) {
	if len(rawID) == 0 {
		rawID = json.RawMessage("null")
	}
	data, err := json.Marshal(struct {
		JSONRPC string          `json:"jsonrpc"`
		ID      json.RawMessage `json:"id"`
		Error   *jsonrpc.Error  `json:"error"`
	}{"2.0", rawID, &jsonrpc.Error{Code: int64(code), Message: text}})
	if err != nil {
		r.logger.Error("failed to encode an error for the harness", "error", err)
		return
	}
	r.write(data)
}

// write writes a message to the harness, on a line of its own: a message spread over lines (JSON
// with new lines between its values) is compacted, which only removes that space.
func (r *relay) write(data []byte) {
	data = bytes.TrimSpace(data)
	if bytes.ContainsAny(data, "\r\n") {
		var compact bytes.Buffer
		if err := json.Compact(&compact, data); err == nil {
			data = compact.Bytes()
		}
	}
	r.outMu.Lock()
	defer r.outMu.Unlock()
	if _, err := r.out.Write(append(data, '\n')); err != nil {
		r.logger.Warn("failed to write to the harness", "error", err)
	}
}
