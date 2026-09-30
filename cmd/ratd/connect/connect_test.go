package connect

import (
	"context"
	"strings"
	"testing"

	"github.com/urfave/cli/v3"
)

// parse runs a command with the flags on args, and returns the target it read.
func parse(args ...string) (Target, error) {
	var target Target
	cmd := &cli.Command{
		Name:  "test",
		Flags: Flags(),
		Action: func(_ context.Context, cmd *cli.Command) error {
			target = FromCommand(cmd)
			return nil
		},
	}
	err := cmd.Run(context.Background(), append([]string{"test"}, args...))
	return target, err
}

// TestFlags guards the flags naming a ratd, long and short, and the endpoint they lead to: https,
// the path of ratd, an IPv6 address between brackets.
func TestFlags(t *testing.T) {
	for _, tc := range []struct {
		args     []string
		expected Target
		endpoint string
	}{
		{[]string{"--server", "host:7281", "--bundle", "dir"}, Target{"host:7281", "dir"}, "https://host:7281/mcp"},
		{[]string{"-s", "[::1]:7281", "-b", "dir"}, Target{"[::1]:7281", "dir"}, "https://[::1]:7281/mcp"},
		{[]string{"-s", "10.0.0.1:1", "-b", "dir"}, Target{"10.0.0.1:1", "dir"}, "https://10.0.0.1:1/mcp"},
	} {
		target, err := parse(tc.args...)
		if err != nil || target != tc.expected || target.Endpoint() != tc.endpoint {
			t.Errorf("%v: expected %+v and %s, got %+v and %s, %v", tc.args, tc.expected, tc.endpoint, target,
				target.Endpoint(), err)
		}
	}
}

// TestFlagsRefused guards the addresses refused, with the expected form in the error: no port, no
// host, a port out of range or not a number, a URL, an IPv6 address without brackets. Both flags
// are required.
func TestFlagsRefused(t *testing.T) {
	for _, server := range []string{"host", ":7281", "host:0", "host:65536", "host:https", "https://host:7281", "::1:7281"} {
		if _, err := parse("-s", server, "-b", "dir"); err == nil || !strings.Contains(err.Error(), "expected HOST:PORT") {
			t.Errorf("%s: expected an error telling HOST:PORT, got %v", server, err)
		}
	}
	for _, args := range [][]string{{"-s", "host:7281"}, {"-b", "dir"}} {
		if _, err := parse(args...); err == nil {
			t.Errorf("%v: expected a missing flag error", args)
		}
	}
}
