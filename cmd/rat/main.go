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
	// reloaded when renewed: a rat may run for weeks, past the expiry of the certificate it started with
	watcher, err := mtls.Watch(target.Bundle)
	if err != nil {
		return fmt.Errorf("bundle: %w", err)
	}
	side := watcher.Side()
	if side.Role != mtls.Client {
		return fmt.Errorf("bundle: %w: %s is the directory of the server, not of a client", mtls.ErrRole, target.Bundle)
	}
	if side.ExpiresSoon(time.Now()) {
		logger.Warn(side.ExpiryWarning(), "expiry", side.NotAfter(), "days_left", int(time.Until(side.NotAfter()).Hours()/24))
	}
	watcher.OnCRL(logCRL(logger))
	watcher.OnWatch(logWatch(logger))
	client, err := connect.HTTPClient(watcher)
	if err != nil {
		return err
	}
	schemas, err := tools.Schemas()
	if err != nil {
		return err
	}
	crl := side.CRLURL()
	if crl == "" {
		crl = "none"
	}
	logger.Info("relaying", "version", version.String(), "server", target.Server, "tenant", side.Tenant, "session", side.Name,
		"bundle_expiry", side.NotAfter(), "crl", crl)
	r := newRelay(logger, client, target, schemas, stdout)
	reason := r.run(ctx, stdin)
	logger.Info("stopped", "reason", reason)
	return nil
}

// logCRL returns the events logging what happens to the CRL of the CA, fetched at the first
// request and refreshed as rat relays: a CRL newer than the one held, and a fetch failing or
// refused, a warning, with the CRL kept.
func logCRL(logger *slog.Logger) mtls.CRLEvents {
	return mtls.CRLEvents{
		Fetched: func(crl *mtls.CRL) {
			logger.Info("CRL fetched", "address", crl.Address, "revoked", crl.Len(), "this_update", crl.ThisUpdate,
				"next_update", crl.NextUpdate)
		},
		Failed: func(err error, held *mtls.CRL) {
			if held == nil {
				logger.Warn("CRL fetch failed, no valid CRL held: requests are refused", "error", err)
				return
			}
			logger.Warn("CRL fetch failed, the CRL held kept", "error", err, "stale_at", held.NextUpdate)
		},
	}
}

// logWatch returns the events logging what happens to the files of the client directory: a
// renewal, a warning when it changes the key, and files refused, the side in use kept (a new CA
// takes a restart of the harness).
func logWatch(logger *slog.Logger) mtls.WatchEvents {
	return mtls.WatchEvents{
		Reloaded: func(side *mtls.Side, newKey bool) {
			attrs := []any{"serial", side.Certificate.Leaf.SerialNumber, "expiry", side.NotAfter()}
			if newKey {
				logger.Warn("client certificate renewed with a new key", attrs...)
				return
			}
			logger.Info("client certificate renewed", attrs...)
		},
		Refused: func(err error) {
			if err == nil {
				logger.Info("bundle files restored: the side in use is theirs again")
				return
			}
			logger.Warn("bundle files changed and refused, the side in use kept", "error", err)
		},
	}
}
