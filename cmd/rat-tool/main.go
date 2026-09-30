package main

import (
	"context"
	"fmt"
	"os"

	"github.com/hekmon/rat/internal/version"
	"github.com/urfave/cli/v3"
)

func main() {
	if err := command().Run(context.Background(), os.Args); err != nil {
		fmt.Fprintln(os.Stderr, "rat-tool:", err)
		os.Exit(1)
	}
}

// command returns the command line of rat-tool, a new one at each call: a command keeps the state
// of its last run.
func command() *cli.Command {
	return &cli.Command{
		Name:    "rat-tool",
		Usage:   "the utility of rat: certificate bundles, and checking a running ratd",
		Version: version.String(),
		Commands: []*cli.Command{
			bundleCommand(),
			checkCommand(),
		},
	}
}
