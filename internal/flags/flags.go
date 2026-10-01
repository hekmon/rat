// Package flags holds the command line flags of the clients of ratd: the ratd to reach and the
// client directory to present to it. They are declared once, for rat and rat-tool check, so that a
// harness command line is checked by copying it, with the same names and the same checks. The
// server has no default, as which machine to reach is the whole point of the configuration.
//
// Internal: package connect, which programs outside rat import, gets no dependency on the command
// line library.
package flags

import (
	"fmt"
	"net"
	"strconv"

	"github.com/hekmon/rat/connect"

	"github.com/urfave/cli/v3"
)

// Names of the flags.
const (
	ServerFlag = "server"
	BundleFlag = "bundle"
)

// Flags returns the flags naming the ratd to reach and the client directory to present to it,
// both required. A server address that is not HOST:PORT fails parsing.
func Flags() []cli.Flag {
	return []cli.Flag{
		&cli.StringFlag{
			Name:      ServerFlag,
			OnlyOnce:  true,
			Aliases:   []string{"s"},
			Usage:     "the address of ratd, HOST:PORT (an IPv6 address between brackets)",
			Required:  true,
			Validator: checkServer,
		},
		&cli.StringFlag{
			Name:     BundleFlag,
			OnlyOnce: true,
			Aliases:  []string{"b"},
			Usage:    "the client directory of a bundle, which names the session (clients/NAME, from rat-tool bundle generate)",
			Required: true,
		},
	}
}

// checkServer refuses a server address that is not HOST:PORT, with a host and a port number.
func checkServer(server string) error {
	host, port, err := net.SplitHostPort(server)
	if err == nil && host == "" {
		err = fmt.Errorf("missing host")
	}
	if err == nil {
		if number, parseErr := strconv.ParseUint(port, 10, 16); parseErr != nil || number == 0 {
			err = fmt.Errorf("invalid port %q", port)
		}
	}
	if err != nil {
		return fmt.Errorf("invalid server %q, expected HOST:PORT (ratd listens on port %s by default): %w", server,
			connect.DefaultPort, err)
	}
	return nil
}

// Target is a ratd to reach, and the client directory to present to it.
type Target struct {
	// Server is the address of ratd, HOST:PORT.
	Server string
	// Bundle is the client directory of a bundle, which names the session.
	Bundle string
}

// FromCommand returns the target named by the flags of cmd, checked while parsing them.
func FromCommand(cmd *cli.Command) Target {
	return Target{Server: cmd.String(ServerFlag), Bundle: cmd.String(BundleFlag)}
}
