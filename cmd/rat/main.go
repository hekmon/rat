package main

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/hekmon/rat/cmd/ratd/tools"
	"github.com/hekmon/rat/connect"
	"github.com/hekmon/rat/internal/flags"
	"github.com/hekmon/rat/internal/version"
	"github.com/hekmon/rat/mtls"

	"github.com/urfave/cli/v3"
)

func main() {
	if err := command(os.Stdin, os.Stdout, os.Stderr).Run(context.Background(), os.Args); err != nil {
		fmt.Fprintln(os.Stderr, "rat:", err)
		os.Exit(1)
	}
}

// command returns the command line of rat, relaying stdin to ratd and its answers to stdout, and
// logging to logs. A new one at each call: a command keeps the state of its last run.
func command(stdin io.Reader, stdout, logs io.Writer) *cli.Command {
	return &cli.Command{
		Name:    "rat",
		Usage:   "relay a harness's stdio MCP messages to ratd, over mutual TLS",
		Version: version.String(),
		Flags: append(flags.Flags(), &cli.StringFlag{
			Name:     "log-level",
			OnlyOnce: true,
			Usage:    "the minimum level of the logs: debug, info, warn or error",
			Value:    "info",
		}),
		Action: func(ctx context.Context, cmd *cli.Command) error {
			var level slog.Level
			if err := level.UnmarshalText([]byte(cmd.String("log-level"))); err != nil {
				return fmt.Errorf("invalid log level: %w", err)
			}
			logger := slog.New(slog.NewTextHandler(logs, &slog.HandlerOptions{Level: level}))
			ctx, stop := signal.NotifyContext(ctx, syscall.SIGTERM, os.Interrupt)
			defer stop()
			return run(ctx, logger, flags.FromCommand(cmd), stdin, stdout)
		},
	}
}

// run relays the messages of stdin to the ratd of target, writing its answers to stdout, until
// stdin ends or ctx is done. It loads and checks the client directory first, failing while the
// harness still shows it: ratd itself is only reached by the first message, so that a ratd briefly
// down does not fail the startup of the harness.
func run(ctx context.Context, logger *slog.Logger, target flags.Target, stdin io.Reader, stdout io.Writer) error {
	side, err := mtls.Load(target.Bundle)
	if err != nil {
		return fmt.Errorf("bundle: %w", err)
	}
	if side.Role != mtls.Client {
		return fmt.Errorf("bundle: %w: %s is the directory of the server, not of a client", mtls.ErrRole, target.Bundle)
	}
	if left := time.Until(side.NotAfter()); left < mtls.ExpiryWarning {
		logger.Warn("the bundle expires within a year: an expired bundle stops every client at once, "+
			"generate a new one and deploy it to every side", "expiry", side.NotAfter(), "days_left", int(left.Hours()/24))
	}
	client, err := connect.HTTPClient(side)
	if err != nil {
		return err
	}
	schemas, err := tools.Schemas()
	if err != nil {
		return err
	}
	logger.Info("relaying", "version", version.String(), "server", target.Server, "tenant", side.Tenant, "session", side.Name,
		"bundle_expiry", side.NotAfter())
	r := newRelay(logger, client, target, schemas, stdout)
	reason := r.run(ctx, stdin)
	logger.Info("stopped", "reason", reason)
	return nil
}
