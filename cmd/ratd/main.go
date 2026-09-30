package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/hekmon/rat/connect"
	"github.com/hekmon/rat/internal/version"
	"github.com/hekmon/rat/mtls"
	"github.com/hekmon/rat/tmux"
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
// fails on machines with IPv6 disabled.
const defaultListen = ":" + connect.DefaultPort

// expiryCheckInterval is how often ratd checks whether its bundle expires within a year.
const expiryCheckInterval = 24 * time.Hour

// command returns the command line of ratd, logging to logs. A new one at each call: a command
// keeps the state of its last run.
func command(logs io.Writer) *cli.Command {
	return &cli.Command{
		Name:    "ratd",
		Usage:   "serve persistent terminals to agents over MCP, with mutual TLS",
		Version: version.String(),
		Flags: []cli.Flag{
			&cli.StringFlag{
				Name:     "bundle",
				OnlyOnce: true,
				Aliases:  []string{"b"},
				Usage:    "the server directory of the bundle of the tenant (rat-tool bundle generate)",
				Required: true,
			},
			&cli.StringFlag{
				Name:     "listen",
				OnlyOnce: true,
				Aliases:  []string{"l"},
				Usage:    "the address to listen on",
				Value:    defaultListen,
			},
			&cli.StringFlag{
				Name:     "read-budget",
				OnlyOnce: true,
				Aliases:  []string{"r"},
				Usage:    "the most a read of a window or a file sends back, with its unit, 32KiB at least",
				Value:    "64KiB",
			},
			&cli.StringFlag{
				Name:     "log-level",
				OnlyOnce: true,
				Usage:    "the minimum level of the logs: debug, info, warn or error",
				Value:    "info",
			},
		},
		Action: func(ctx context.Context, cmd *cli.Command) error {
			var level slog.Level
			if err := level.UnmarshalText([]byte(cmd.String("log-level"))); err != nil {
				return fmt.Errorf("invalid log level: %w", err)
			}
			readBudget, err := parseReadBudget(cmd.String("read-budget"))
			if err != nil {
				return err
			}
			logger := slog.New(slog.NewTextHandler(logs, &slog.HandlerOptions{Level: level}))
			ctx, stop := signal.NotifyContext(ctx, syscall.SIGTERM, os.Interrupt)
			defer stop()
			listen := cmd.String("listen")
			return run(ctx, logger, cmd.String("bundle"), readBudget, func() (net.Listener, error) {
				return net.Listen("tcp", listen)
			})
		},
	}
}

// run starts ratd with the server directory of a bundle, its reads bounded by readBudget bytes,
// then listens with listen, and serves until ctx is done. The steps of the startup run in order, a failure making ratd exit while the
// admin is still there: loading the bundle, starting the tmux server of its tenant, checking
// bracketed paste, listening. A second ratd for the same tenant thus fails before listening.
// Stopping closes the door first (no call gets in anymore), then stops the terminals.
// It also returns an error when the tmux server died too often: ratd then shows as failed.
func run(ctx context.Context, logger *slog.Logger, bundle string, readBudget int, listen func() (net.Listener, error)) error {
	side, err := mtls.Load(bundle)
	if err != nil {
		return fmt.Errorf("bundle: %w", err)
	}
	if side.Role != mtls.Server {
		return fmt.Errorf("bundle: %w: %s is the directory of a client, not of the server", mtls.ErrRole, bundle)
	}
	controller, err := tmux.New(side.Tenant)
	if err != nil {
		return err
	}
	// The terminals outlive ctx, which SIGTERM cancels: they stop once the door is closed, not with
	// it. Their context only terminates a server ratd would leave running.
	terminals, stopTerminals := context.WithCancel(context.WithoutCancel(ctx))
	defer stopTerminals()
	if err = controller.StartServer(terminals); err != nil {
		// tmux.ErrServerSocketInUse: another ratd serves the tenant on this machine
		return fmt.Errorf("failed to start the tmux server of tenant %s: %w", side.Tenant, err)
	}
	defer func() {
		// with the controller's own escalation: SIGTERM, then SIGKILL
		if err := controller.StopServer(context.Background()); err != nil && !errors.Is(err, tmux.ErrServerNotRunning) {
			logger.Warn("tmux server stopped forcibly", "error", err)
		}
		logger.Info("terminals stopped")
	}()
	warnPaste := checkBracketedPaste(ctx, logger, controller)
	d, err := newDaemon(logger, side, controller, warnPaste, readBudget)
	if err != nil {
		return err
	}
	listener, err := listen()
	if err != nil {
		return err
	}
	logger.Info("serving", "version", version.String(), "tenant", side.Tenant, "address", listener.Addr().String(), "endpoint", connect.Path,
		"read_budget", sizeText(readBudget), "bundle_expiry", side.NotAfter())
	serving, stopServing := context.WithCancel(ctx)
	defer stopServing()
	supervised := make(chan struct{})
	var supervisorErr error
	go func() {
		defer close(supervised)
		if supervisorErr = supervise(serving, logger, controller, terminals, defaultRestartPolicy); supervisorErr != nil {
			stopServing()
		}
	}()
	go warnExpiry(serving, logger, side)
	err = d.serve(serving, listener)
	logger.Info("door closed")
	// no restart once stopping: the terminals are stopped next
	stopServing()
	<-supervised
	if supervisorErr != nil {
		return supervisorErr
	}
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
