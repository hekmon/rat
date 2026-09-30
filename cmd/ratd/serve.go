package main

import (
	"context"
	"crypto/tls"
	"fmt"
	"log"
	"log/slog"
	"net"
	"net/http"
	"os"
	"runtime/debug"
	"strings"
	"sync"
	"time"

	"github.com/hekmon/rat/mtls"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

const (
	// endpoint is the only path served: the convention of the MCP specification and of the SDK.
	endpoint = "/mcp"
	// readHeaderTimeout bounds reading the headers of a request, and the TLS handshake with it
	// (net/http bounds the handshake by the smallest of its timeouts). There is no timeout on the
	// body nor on the response: a 4 MiB request on a slow link must not be cut.
	readHeaderTimeout = 10 * time.Second
	// idleTimeout closes the connections of clients gone quiet.
	idleTimeout = 2 * time.Minute
	// shutdownGrace is how long the calls in flight have to end once ratd stops.
	shutdownGrace = 10 * time.Second
	// handshakeLogInterval is how often a refused TLS handshake is logged at most: a public port
	// gets scanned.
	handshakeLogInterval = 10 * time.Second
)

// instructions are sent to each client when it initializes (or discovers the server, from
// protocol 2026-07-28 on), host being the machine ratd runs on: an agent with a local shell too
// must know where commands run.
const instructionsFormat = `rat gives you persistent terminals on %s: commands run there, not where you run. ` +
	`It is asynchronous by design: sending a command returns at once, without waiting for it to finish. ` +
	`Start long running commands (builds, tests, deployments), carry on with other work, and come back to ` +
	`check them: rat does not know when a command finishes, list_windows shows the foreground command ` +
	`(bash means the terminal waits for input), read_window shows the screen. Run several commands in ` +
	`parallel in several windows. Each terminal is a window running bash, found by name: windows persist ` +
	`across your restarts and context compactions, so call list_windows first. A window named main exists ` +
	`when you start. To move a whole file, or to read a long output (redirect it to a file), use write_file ` +
	`and read_file rather than the terminal. If the terminals restart after a failure, every window ` +
	`disappears and you find a fresh main.`

// pasteWarning ends the instructions when bash startup files defeat bracketed paste, or when that
// could not be checked (see checkBracketedPaste).
const pasteWarning = ` Multi-line text may run line by line even at a prompt: send one line at a time.`

// daemon is ratd serving a tenant: what every request needs, fixed at startup. Terminals are not
// held here: tmux is their only source of truth.
type daemon struct {
	logger *slog.Logger
	// sdkLogger is the logger of the MCP SDK (see newDaemon)
	sdkLogger *slog.Logger
	// side is the server directory of the bundle, which names the tenant
	side *mtls.Side
	// host is the name of the machine, told to agents
	host string
	// instructions are sent to every client
	instructions string
	// schemas spares building the schemas of the tools again for each request, which builds its
	// own MCP server
	schemas *mcp.SchemaCache
}

// newDaemon returns ratd serving the tenant of side, the server directory of its bundle, warning
// agents that pasted text may run line by line if warnPaste.
func newDaemon(logger *slog.Logger, side *mtls.Side, warnPaste bool) (*daemon, error) {
	host, err := os.Hostname()
	if err != nil {
		return nil, fmt.Errorf("failed to read the host name: %w", err)
	}
	instructions := fmt.Sprintf(instructionsFormat, host)
	if warnPaste {
		instructions += pasteWarning
	}
	// The SDK logs every stateless request at info level (a session connecting, then
	// disconnecting): only its warnings and errors are kept, unless ratd logs at debug level.
	sdkLogger := logger
	if !logger.Enabled(context.Background(), slog.LevelDebug) {
		sdkLogger = slog.New(minLevel{Handler: logger.Handler(), min: slog.LevelWarn})
	}
	return &daemon{logger: logger, sdkLogger: sdkLogger, side: side, host: host, instructions: instructions,
		schemas: mcp.NewSchemaCache()}, nil
}

// minLevel passes on the records of its handler from a minimum level only.
type minLevel struct {
	slog.Handler
	min slog.Level
}

func (h minLevel) Enabled(ctx context.Context, level slog.Level) bool {
	return level >= h.min && h.Handler.Enabled(ctx, level)
}

func (h minLevel) WithAttrs(attrs []slog.Attr) slog.Handler {
	return minLevel{Handler: h.Handler.WithAttrs(attrs), min: h.min}
}

func (h minLevel) WithGroup(name string) slog.Handler {
	return minLevel{Handler: h.Handler.WithGroup(name), min: h.min}
}

// serve serves the MCP endpoint on listener, over mutual TLS, until ctx is done. It then closes
// the door: no connection is accepted anymore, and the calls in flight get shutdownGrace to end
// before being cut. It returns early if serving fails.
func (d *daemon) serve(ctx context.Context, listener net.Listener) error {
	tlsConfig, err := mtls.ServerConfig(d.side)
	if err != nil {
		return err
	}
	// HTTP/1.1 only: requests are short JSON exchanges, which HTTP/2 would not speed up
	tlsConfig.NextProtos = []string{"http/1.1"}
	mux := http.NewServeMux()
	mux.Handle(endpoint, mcp.NewStreamableHTTPHandler(d.getServer, &mcp.StreamableHTTPOptions{
		// Each request names its session by its client certificate, and tmux is the only state:
		// nothing is kept per connection (no Mcp-Session-Id).
		Stateless: true,
		// A single JSON answer per request rather than an event stream: calls are short, and
		// no long lived connection is left for a proxy to cut.
		JSONResponse: true,
		// The protection against DNS rebinding refuses a request reaching a loopback address
		// with another host name than localhost: a client through an ssh tunnel under an alias,
		// or on the machine itself under its name (Debian maps it to 127.0.1.1). It protects
		// nothing here: a browser tricked into calling ratd can not present a client
		// certificate, and is refused by the TLS handshake before any request exists.
		DisableLocalhostProtection: true,
		// A call abandoned by its client (protocol 2026-07-28 and later) is canceled.
		PropagateRequestCancellation: true,
		Logger:                       d.sdkLogger,
	}))
	server := &http.Server{
		Handler:           mux,
		ReadHeaderTimeout: readHeaderTimeout,
		IdleTimeout:       idleTimeout,
		// net/http would otherwise print its errors, refused handshakes mostly, through the log
		// package, outside ratd's logs
		ErrorLog: log.New(&errorLog{logger: d.logger}, "", 0),
	}
	served := make(chan error, 1)
	go func() {
		// Through a TLS listener: http.Server.TLSConfig alone is ignored by Serve, which would
		// then serve plain HTTP.
		served <- server.Serve(tls.NewListener(listener, tlsConfig))
	}()
	select {
	case err = <-served:
		return fmt.Errorf("failed to serve: %w", err)
	case <-ctx.Done():
	}
	shutdownCtx, cancel := context.WithTimeout(context.Background(), shutdownGrace)
	defer cancel()
	if err = server.Shutdown(shutdownCtx); err != nil {
		d.logger.Warn("calls in flight cut", "grace", shutdownGrace, "error", err)
		_ = server.Close()
	}
	return nil
}

// getServer returns the MCP server of the request, for the session its client certificate names,
// or nil, answered 400, without one. The certificate was checked during the handshake, by the TLS
// configuration of the bundle: its name follows the naming rule.
func (d *daemon) getServer(req *http.Request) *mcp.Server {
	if req.TLS == nil {
		return nil
	}
	session := mtls.PeerName(*req.TLS)
	if session == "" {
		return nil
	}
	return d.newServer(session)
}

// newServer returns the MCP server of session, one per request: stateless, and cheap enough
// (about 0.1 ms with the schemas cached, measured with 8 tools, next to tool calls taking
// milliseconds of tmux).
func (d *daemon) newServer(session string) *mcp.Server {
	server := mcp.NewServer(&mcp.Implementation{
		Name:    "ratd",
		Title:   fmt.Sprintf("rat %s on %s", d.side.Tenant, d.host),
		Version: version(),
	}, &mcp.ServerOptions{
		Instructions: d.instructions,
		SchemaCache:  d.schemas,
		Logger:       d.sdkLogger,
	})
	server.AddReceivingMiddleware(d.logRequests(session))
	return server
}

// logRequests logs each MCP request of session at debug level, whatever the protocol version
// (initialize, or server/discover from 2026-07-28 on, then the calls).
func (d *daemon) logRequests(session string) mcp.Middleware {
	return func(next mcp.MethodHandler) mcp.MethodHandler {
		return func(ctx context.Context, method string, req mcp.Request) (mcp.Result, error) {
			start := time.Now()
			result, err := next(ctx, method, req)
			attrs := []any{"session", session, "method", method, "duration", time.Since(start)}
			if err != nil {
				attrs = append(attrs, "error", err)
			}
			d.logger.Debug("request", attrs...)
			return result, err
		}
	}
}

// version returns the version of ratd, from its build.
func version() string {
	if info, ok := debug.ReadBuildInfo(); ok {
		return info.Main.Version
	}
	return "unknown"
}

// errorLog receives what net/http logs (http.Server.ErrorLog), and logs it through slog. Refused
// TLS handshakes, which a scanned public port produces in bulk, are logged at most once per
// handshakeLogInterval, with the count of the others: the first refusal of a misconfigured
// client is always logged, unless a scan hides it.
type errorLog struct {
	logger *slog.Logger
	// mu guards the fields below
	mu            sync.Mutex
	lastHandshake time.Time
	suppressed    int
}

// handshakePrefix starts the lines net/http logs for a refused TLS handshake, followed by the
// address of the client and the reason.
const handshakePrefix = "http: TLS handshake error from "

func (l *errorLog) Write(p []byte) (int, error) {
	message := strings.TrimSpace(string(p))
	refused, isHandshake := strings.CutPrefix(message, handshakePrefix)
	if !isHandshake {
		l.logger.Error("http server", "error", message)
		return len(p), nil
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	now := time.Now()
	if now.Sub(l.lastHandshake) < handshakeLogInterval {
		l.suppressed++
		return len(p), nil
	}
	// IPv6 addresses are between brackets: the first ": " ends the address
	remote, reason, _ := strings.Cut(refused, ": ")
	l.logger.Warn("TLS handshake refused", "remote", remote, "reason", reason, "suppressed", l.suppressed)
	l.lastHandshake, l.suppressed = now, 0
	return len(p), nil
}
