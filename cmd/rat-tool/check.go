package main

import (
	"bufio"
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/hekmon/rat/connect"
	"github.com/hekmon/rat/internal/flags"
	"github.com/hekmon/rat/internal/version"
	"github.com/hekmon/rat/mtls"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/urfave/cli/v3"
)

const (
	// dialTimeout bounds reaching the address of ratd, and the TLS step.
	dialTimeout = 10 * time.Second
	// mcpTimeout bounds the MCP steps, as rat bounds a request.
	mcpTimeout = 30 * time.Second
	// textWidth is the width the instructions and descriptions are wrapped to.
	textWidth = 100
)

// checkCommand returns the command checking a running ratd.
func checkCommand() *cli.Command {
	return &cli.Command{
		Name:  "check",
		Usage: "check a running ratd from a client directory, as a harness reaches it",
		Description: "Takes the flags of rat: copy the command line of a harness to check it. Checks step by step, " +
			"stopping at the first failure: the files of the client directory, the network, TLS, MCP, the tools. " +
			"It calls no tool, leaving nothing behind on the server.",
		Flags:  flags.Flags(),
		Action: checkRatd,
	}
}

// checker is a check of a ratd in progress: what its steps found, for the next ones.
type checker struct {
	out    io.Writer
	target flags.Target
	// side is the client directory, loaded by the files step
	side *mtls.Side
	// conn is the connection to ratd opened by the network step, on which the TLS step runs its
	// handshake: closed without one, it would show in the logs of ratd as a refused handshake
	conn net.Conn
	// session is the MCP session opened by the MCP step
	session *mcp.ClientSession
}

// checkRatd checks the ratd the flags of cmd name, step by step, stopping at the first failure,
// and fails naming the step: scripts rely on the exit code.
func checkRatd(ctx context.Context, cmd *cli.Command) error {
	c := &checker{out: cmd.Root().Writer, target: flags.FromCommand(cmd)}
	defer func() {
		if c.conn != nil {
			_ = c.conn.Close()
		}
		if c.session != nil {
			_ = c.session.Close()
		}
	}()
	for _, step := range []struct {
		name  string
		check func(context.Context) bool
	}{
		{"Files", c.files},
		{"Network", c.network},
		{"TLS", c.tls},
		{"MCP", c.mcp},
		{"Tools", c.tools},
	} {
		if !step.check(ctx) {
			return fmt.Errorf("check failed: %s", step.name)
		}
	}
	fmt.Fprintf(c.out, "ratd at %s serves this client.\n", c.target.Server)
	return nil
}

// ok prints a check passed, fail a check failed.
func (c *checker) ok(format string, args ...any) {
	fmt.Fprintf(c.out, "  ok    "+format+"\n", args...)
}

func (c *checker) fail(format string, args ...any) bool {
	fmt.Fprintf(c.out, "  FAIL  "+format+"\n", args...)
	return false
}

// files checks the client directory as rat does at startup (bundle inspect), a client's.
func (c *checker) files(context.Context) bool {
	fmt.Fprintf(c.out, "Files: %s\n", c.target.Bundle)
	if c.side = inspectSide(c.out, c.target.Bundle); c.side == nil {
		return false
	}
	if c.side.Role != mtls.Client {
		return c.fail("role: the directory of the server, not of a client")
	}
	return true
}

// network checks the address of ratd answers, keeping the connection for the TLS step.
func (c *checker) network(ctx context.Context) bool {
	fmt.Fprintln(c.out, "Network")
	start := time.Now()
	var err error
	if c.conn, err = (&net.Dialer{Timeout: dialTimeout}).DialContext(ctx, "tcp", c.target.Server); err != nil {
		return c.fail("%s does not answer: %s", c.target.Server, innermost(err))
	}
	c.ok("%s answers (TCP, %d ms)", c.target.Server, time.Since(start).Milliseconds())
	return true
}

// tls checks ratd and this client accept each other. In TLS 1.3, a client completes its handshake
// before the server has checked its certificate: a refusal only shows on the first read. An HTTP
// request follows the handshake, for that read to come without waiting on a timer.
func (c *checker) tls(ctx context.Context) bool {
	fmt.Fprintln(c.out, "TLS")
	config, err := mtls.ClientConfig(c.side)
	if err != nil {
		return c.fail("%v", err)
	}
	config.NextProtos = []string{"http/1.1"}
	ctx, cancel := context.WithTimeout(ctx, dialTimeout)
	defer cancel()
	tlsConn := tls.Client(c.conn, config)
	if err = tlsConn.HandshakeContext(ctx); err != nil {
		return c.fail("%s", handshakeFailure(err))
	}
	_ = tlsConn.SetDeadline(time.Now().Add(dialTimeout))
	_, err = fmt.Fprintf(tlsConn, "GET %s HTTP/1.1\r\nHost: %s\r\nConnection: close\r\n\r\n", connect.Path, c.target.Server)
	if err == nil {
		var resp *http.Response
		if resp, err = http.ReadResponse(bufio.NewReader(tlsConn), nil); err == nil {
			_ = resp.Body.Close()
		}
	}
	if err != nil {
		return c.fail("%s", handshakeFailure(err))
	}
	c.ok("ratd serves tenant %s, and accepts this client as session %s", mtls.PeerName(tlsConn.ConnectionState()),
		c.side.Name)
	return true
}

// clientRefusals explain the TLS alerts of ratd refusing a client, as package mtls guards them.
var clientRefusals = map[string]string{
	"tls: unknown certificate authority": "its certificate is from another bundle of the tenant",
	"tls: certificate required":          "it is from another tenant",
	"tls: bad certificate":               "its certificate is not a client certificate",
	"tls: expired certificate":           "its certificate expired",
}

// handshakeFailure tells why ratd and this client did not accept each other: ratd refusing the
// client (a TLS alert, explained), this client refusing the server (package mtls tells why), or
// the handshake failing otherwise.
func handshakeFailure(err error) string {
	var opErr *net.OpError
	if errors.As(err, &opErr) && opErr.Op == "remote error" {
		alert := opErr.Err.Error()
		if why, ok := clientRefusals[alert]; ok {
			return fmt.Sprintf("ratd refused this client: %s (%v)", why, opErr)
		}
		return fmt.Sprintf("ratd refused this client (%v)", opErr)
	}
	var unknownAuthority x509.UnknownAuthorityError
	var invalid x509.CertificateInvalidError
	if errors.Is(err, mtls.ErrNotSignedByCA) || errors.Is(err, mtls.ErrValidity) || errors.Is(err, mtls.ErrRole) ||
		errors.As(err, &unknownAuthority) || errors.As(err, &invalid) {
		return fmt.Sprintf("this client refused the server: %v", err)
	}
	return fmt.Sprintf("TLS handshake failed: %v", err)
}

// mcp checks ratd answers as an MCP server, with the Go SDK a harness author would use, and shows
// what it tells clients: its information, the protocol version, the instructions (with the
// bracketed paste warning, when ratd has one).
func (c *checker) mcp(ctx context.Context) bool {
	fmt.Fprintln(c.out, "MCP")
	transport, err := connect.Transport(c.side, c.target.Server)
	if err != nil {
		return c.fail("%v", err)
	}
	ctx, cancel := context.WithTimeout(ctx, mcpTimeout)
	defer cancel()
	client := mcp.NewClient(&mcp.Implementation{Name: "rat-tool", Version: version.Module()}, nil)
	if c.session, err = client.Connect(ctx, transport, nil); err != nil {
		return c.fail("%v", err)
	}
	result := c.session.InitializeResult()
	info := result.ServerInfo
	c.ok("%s %s, %q, protocol %s", info.Name, info.Version, info.Title, result.ProtocolVersion)
	fmt.Fprintln(c.out, "  instructions:")
	fmt.Fprint(c.out, wrap(result.Instructions, "    ", "    "))
	return true
}

// tools checks ratd lists its tools, and shows them. It calls none: listing windows, for instance,
// would create the session.
func (c *checker) tools(ctx context.Context) bool {
	fmt.Fprintln(c.out, "Tools")
	ctx, cancel := context.WithTimeout(ctx, mcpTimeout)
	defer cancel()
	result, err := c.session.ListTools(ctx, nil)
	if err != nil {
		return c.fail("%v", err)
	}
	if len(result.Tools) == 1 {
		c.ok("1 tool")
	} else {
		c.ok("%d tools", len(result.Tools))
	}
	for _, tool := range result.Tools {
		fmt.Fprint(c.out, wrap(tool.Name+": "+tool.Description, "    ", "      "))
	}
	return true
}

// wrap returns text wrapped to textWidth, its first line indented by first, the others by rest.
func wrap(text, first, rest string) string {
	var out strings.Builder
	line := first
	for _, word := range strings.Fields(text) {
		if len(line)+1+len(word) > textWidth && strings.TrimSpace(line) != "" {
			out.WriteString(line + "\n")
			line = rest
		}
		if strings.TrimSpace(line) != "" {
			line += " "
		}
		line += word
	}
	return out.String() + line + "\n"
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
