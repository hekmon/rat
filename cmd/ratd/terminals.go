package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"time"

	"github.com/hekmon/rat/tmux"
)

// terminalsCheckTimeout bounds the check of the terminals, which runs the bash startup files of the
// terminals: one may block. A variable for tests.
var terminalsCheckTimeout = 10 * time.Second

// checkTerminals checks once what the bash prompt of terminals ends up with (bracketed paste, the
// prompts and statuses recorded), logs what bash startup files defeat, and returns it, for ratd to
// tell agents only what holds. Terminals keep working either way (single line input is fine): an
// override is only a warning. A check that can not run, such as a startup file blocking (which
// blocks every terminal as well), is taken as nothing holding. Checked once rather than at each
// tmux restart: clients receive the instructions and the tools once, and must not be told
// something ratd no longer believes.
func checkTerminals(ctx context.Context, logger *slog.Logger, controller *tmux.Controller) tmux.TerminalsCheck {
	ctx, cancel := context.WithTimeout(ctx, terminalsCheckTimeout)
	defer cancel()
	check, err := controller.CheckTerminals(ctx)
	switch {
	case errors.Is(ctx.Err(), context.DeadlineExceeded):
		logger.Warn("bash startup files did not finish: they block every terminal as well, agents are told to "+
			"send one line at a time", "timeout", terminalsCheckTimeout)
		return tmux.TerminalsCheck{}
	case err != nil:
		logger.Warn("the check of the terminals failed: agents are told to send one line at a time", "error", err)
		return tmux.TerminalsCheck{}
	}
	if check.BracketedPaste {
		logger.Info("bracketed paste enforced in terminals")
	} else {
		logger.Warn("bash startup files override bracketed paste: multi-line text may run line by line even at "+
			"a prompt, agents are told to send one line at a time", "prompt_command", check.PromptCommand)
	}
	switch {
	case !check.Prompts:
		logger.Warn("bash startup files replace PROMPT_COMMAND: the prompts of terminals are not recorded, ratd "+
			"can not tell when commands finish", "prompt_command", check.PromptCommand)
	case !check.Statuses:
		logger.Warn("bash startup files run a command changing the exit status before rat's, in PROMPT_COMMAND: "+
			"ratd does not tell how commands exited", "prompt_command", check.PromptCommand)
	}
	return check
}

// restartPolicy is how ratd keeps its tmux server running.
type restartPolicy struct {
	// delay before starting a dead server again, so that a server dying at once does not spin
	delay time.Duration
	// deaths within window make ratd exit: a server that can not stay up is for whoever monitors
	// the service to notice, as a failed service
	deaths int
	window time.Duration
}

// defaultRestartPolicy restarts after a second, and gives up after 5 deaths within 5 minutes. A
// variable for tests.
var defaultRestartPolicy = restartPolicy{delay: time.Second, deaths: 5, window: 5 * time.Minute}

// supervise keeps the tmux server of controller running until ctx is done: when the server exits
// on its own (a crash, the OOM killer, tmux kill-server typed in a terminal), it logs why and
// starts it again with the context terminals, every terminal and its command being lost. It
// returns nil once ctx is done or the server was stopped by ratd, and an error when the server
// died too often (a restart failing counts as a death): ratd then exits. Only the times of recent
// deaths are kept.
func supervise(ctx context.Context, logger *slog.Logger, controller *tmux.Controller, terminals context.Context,
	policy restartPolicy) error {
	var deaths []time.Time
	for {
		err := controller.WaitServer(ctx)
		if err == nil || ctx.Err() != nil {
			return nil
		}
		logger.Error("tmux server exited on its own, every terminal is lost: restarting it", "error", err)
		for {
			now := time.Now()
			deaths = append(slices.DeleteFunc(deaths, func(death time.Time) bool { return now.Sub(death) > policy.window }), now)
			if len(deaths) >= policy.deaths {
				return fmt.Errorf("the tmux server died %d times within %s", len(deaths), policy.window)
			}
			select {
			case <-ctx.Done():
				return nil
			case <-time.After(policy.delay):
			}
			// A stray server may hold the socket (a command racing a crash can start one): the
			// error names it (tmux.ErrServerSocketInUse), and the failure counts as a death.
			if err = controller.StartServer(terminals); err == nil {
				logger.Info("tmux server restarted")
				break
			}
			logger.Error("tmux server failed to restart", "error", err)
		}
	}
}
