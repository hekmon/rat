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
		printExitError(os.Stderr, err)
		os.Exit(1)
	}
}

// printExitError prints to logs the error ratd exits with: outside the logs, the command line
// failing before they exist, but with the priority of an error in the journal, where it would
// otherwise be taken for information.
func printExitError(logs io.Writer, err error) {
	prefix := ""
	if isJournalStream(logs) {
		prefix = journalPriority(slog.LevelError)
	}
	fmt.Fprintln(logs, prefix+"ratd:", err)
}

// defaultListen is the default listen address: every interface, which mutual TLS makes safe. An
// empty host rather than [::]: Go then listens on IPv6 and IPv4 at once, where a literal [::]
// fails on machines with IPv6 disabled.
const defaultListen = ":" + connect.DefaultPort

// expiryCheckInterval is how often ratd checks whether its bundle expires soon.
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
				Usage:    "the most a read of a window or a file sends back, with its unit, 20KiB at least",
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
			logger := slog.New(newLogHandler(logs, level))
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
// then listens with listen, and serves until ctx is done. The steps of the startup run in order,
// a failure making ratd exit while the admin is still there: loading the bundle, fetching the CRL
// its certificate names (refusing to start without a valid one, or revoked), warning of a
// deployment handing agents more than ratd's user (root, a bundle that user can replace),
// starting the tmux server of its tenant, checking its terminals, listening. A second ratd for
// the same tenant thus fails before listening.
// Stopping closes the door first (no call gets in anymore), then stops the terminals.
// It also returns an error when the tmux server died too often: ratd then shows as failed.
func run(ctx context.Context, logger *slog.Logger, bundle string, readBudget int, listen func() (net.Listener, error)) error {
	watcher, err := mtls.Watch(bundle)
	if err != nil {
		return fmt.Errorf("bundle: %w", err)
	}
	side := watcher.Side()
	if side.Role != mtls.Server {
		return fmt.Errorf("bundle: %w: %s is the directory of a client, not of the server", mtls.ErrRole, bundle)
	}
	// Starting without the CRL the certificate names would let in the certificates it revokes,
	// and before tmux starts, a failure stops nothing.
	watcher.OnCRL(logCRL(logger))
	watcher.OnWatch(logWatch(logger))
	crl, err := side.CRL(ctx)
	if err != nil {
		return fmt.Errorf("bundle: %w", err)
	}
	if crl == nil {
		// expected of a bundle, telling of a sub CA whose server template lacks a distribution point
		logger.Info("revocation not checked: the server certificate names no CRL distribution point")
	}
	// before anything inherits NOTIFY_SOCKET, the tmux server first, its terminals with it
	notifier := takeNotifySocket()
	deployment := warnDeployment(logger, bundle, os.Geteuid())
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
	checked, degraded := checkTerminals(ctx, logger, controller)
	d, err := newDaemon(logger, watcher, controller, checked, readBudget)
	if err != nil {
		return err
	}
	d.notifier, d.status.notifier = notifier, notifier
	d.status.degrade(append(deployment, degraded...)...)
	watchStatus(watcher, d.status)
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
		if supervisorErr = supervise(serving, logger, controller, terminals, defaultRestartPolicy, d.status); supervisorErr != nil {
			stopServing()
		}
	}()
	go warnExpiry(serving, logger, watcher, d.status)
	err = d.serve(serving, listener)
	logger.Info("door closed")
	d.status.stop("stopping the terminals")
	// no restart once stopping: the terminals are stopped next
	stopServing()
	<-supervised
	if supervisorErr != nil {
		return supervisorErr
	}
	return err
}

// logCRL returns the events logging what happens to the CRL of the CA, for ratd and rat alike: a
// CRL newer than the one held, and a fetch failing or refused, a warning, with the CRL kept.
func logCRL(logger *slog.Logger) mtls.CRLEvents {
	return mtls.CRLEvents{
		Fetched: func(crl *mtls.CRL) {
			logger.Info("CRL fetched", "address", crl.Address, "revoked", crl.Len(), "this_update", crl.ThisUpdate,
				"next_update", crl.NextUpdate)
		},
		Failed: func(err error, held *mtls.CRL) {
			if held == nil {
				logger.Warn("CRL fetch failed, no valid CRL held: every client is refused", "error", err)
				return
			}
			logger.Warn("CRL fetch failed, the CRL held kept", "error", err, "stale_at", held.NextUpdate)
		},
	}
}

// logWatch returns the events logging what happens to the files of the server directory: a
// renewal, a warning when it changes the key, which a human should know of (a rotation, or a key
// replaced after it leaked), and files refused, the side in use kept.
func logWatch(logger *slog.Logger) mtls.WatchEvents {
	return mtls.WatchEvents{
		Reloaded: func(side *mtls.Side, newKey bool) {
			attrs := []any{"serial", side.Certificate.Leaf.SerialNumber, "expiry", side.NotAfter()}
			if newKey {
				logger.Warn("server certificate renewed with a new key", attrs...)
				return
			}
			logger.Info("server certificate renewed", attrs...)
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

// eventSource is what watchStatus registers on: the Watcher of ratd, and a recorder in tests.
type eventSource interface {
	OnCRL(mtls.CRLEvents)
	OnWatch(mtls.WatchEvents)
}

// watchStatus tells status what happens to the CRL of the CA and to the files of source: a fetch
// failing or succeeding again, a certificate renewed, the files refused or restored. Succeeded
// rather than Fetched: a fetch returning the CRL held clears a failure as well.
func watchStatus(source eventSource, status *status) {
	source.OnCRL(mtls.CRLEvents{
		Succeeded: func(*mtls.CRL) { status.crlSucceeded() },
		Failed:    func(_ error, held *mtls.CRL) { status.crlFailed(held) },
	})
	source.OnWatch(mtls.WatchEvents{
		Reloaded: func(_ *mtls.Side, newKey bool) { status.renewed(time.Now(), newKey) },
		Refused:  func(err error) { status.refused(err) },
	})
}

// warnExpiry warns now, then every day until ctx is done, if the bundle of side expires soon
// (mtls.Side.ExpiresSoon): sysadmins need reminding, often. The expiry is told to status as well.
func warnExpiry(ctx context.Context, logger *slog.Logger, source mtls.Source, status *status) {
	ticker := time.NewTicker(expiryCheckInterval)
	defer ticker.Stop()
	for {
		// the side in use, renewed or not: a renewal refused still gets reminded
		if side, now := source.Side(), time.Now(); side.ExpiresSoon(now) {
			logger.Warn(side.ExpiryWarning(now), "expiry", side.NotAfter(), "days_left", int(side.NotAfter().Sub(now).Hours()/24))
			status.expires(side.NotAfter())
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}
