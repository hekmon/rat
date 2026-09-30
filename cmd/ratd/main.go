package main

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/hekmon/rat/mtls"
	"github.com/urfave/cli/v3"
)

func main() {
	if err := command(os.Stderr).Run(context.Background(), os.Args); err != nil {
		fmt.Fprintln(os.Stderr, "ratd:", err)
		os.Exit(1)
	}
}

// defaultListen is the default listen address: every interface, which mutual TLS makes safe. An
// empty host rather than [::]: Go then listens on IPv6 and IPv4 at once, where a literal [::]
// fails on machines with IPv6 disabled. 7281 reads "RAT!" on a phone keypad.
const defaultListen = ":7281"

// expiryCheckInterval is how often ratd checks whether its bundle expires within a year.
const expiryCheckInterval = 24 * time.Hour

// command returns the command line of ratd, logging to logs. A new one at each call: a command
// keeps the state of its last run.
func command(logs io.Writer) *cli.Command {
	return &cli.Command{
		Name:  "ratd",
		Usage: "serve persistent terminals to agents over MCP, with mutual TLS",
		Flags: []cli.Flag{
			&cli.StringFlag{
				Name:     "bundle",
				Aliases:  []string{"b"},
				Usage:    "the server directory of the bundle of the tenant (rat-tool bundle generate)",
				Required: true,
			},
			&cli.StringFlag{
				Name:    "listen",
				Aliases: []string{"l"},
				Usage:   "the address to listen on",
				Value:   defaultListen,
			},
			&cli.StringFlag{
				Name:  "log-level",
				Usage: "the minimum level of the logs: debug, info, warn or error",
				Value: "info",
			},
		},
		Action: func(ctx context.Context, cmd *cli.Command) error {
			var level slog.Level
			if err := level.UnmarshalText([]byte(cmd.String("log-level"))); err != nil {
				return fmt.Errorf("invalid log level: %w", err)
			}
			logger := slog.New(slog.NewTextHandler(logs, &slog.HandlerOptions{Level: level}))
			ctx, stop := signal.NotifyContext(ctx, syscall.SIGTERM, os.Interrupt)
			defer stop()
			return run(ctx, logger, cmd.String("bundle"), cmd.String("listen"))
		},
	}
}

// run starts ratd with the server directory of a bundle, listening on listen, and serves until
// ctx is done. A failure to start is returned while the admin is still there.
func run(ctx context.Context, logger *slog.Logger, bundle, listen string) error {
	side, err := mtls.Load(bundle)
	if err != nil {
		return fmt.Errorf("bundle: %w", err)
	}
	if side.Role != mtls.Server {
		return fmt.Errorf("bundle: %w: %s is the directory of a client, not of the server", mtls.ErrRole, bundle)
	}
	d, err := newDaemon(logger, side)
	if err != nil {
		return err
	}
	listener, err := net.Listen("tcp", listen)
	if err != nil {
		return err
	}
	logger.Info("serving", "tenant", side.Tenant, "address", listener.Addr().String(), "endpoint", endpoint,
		"bundle_expiry", side.NotAfter())
	go warnExpiry(ctx, logger, side)
	err = d.serve(ctx, listener)
	logger.Info("stopped")
	return err
}

// warnExpiry warns now, then every day until ctx is done, if the bundle of side expires within a
// year: an expired bundle stops every client at once, years after anyone remembers how it was
// made. Sysadmins need reminding, often.
func warnExpiry(ctx context.Context, logger *slog.Logger, side *mtls.Side) {
	ticker := time.NewTicker(expiryCheckInterval)
	defer ticker.Stop()
	for {
		if left := time.Until(side.NotAfter()); left < mtls.ExpiryWarning {
			logger.Warn("the bundle expires within a year: an expired bundle stops every client at once, "+
				"generate a new one and deploy it to every side", "expiry", side.NotAfter(), "days_left", int(left.Hours()/24))
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}
