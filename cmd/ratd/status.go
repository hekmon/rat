package main

import (
	"cmp"
	"context"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"sync"
	"time"
)

// statusInterval is how often ratd renders its status for systemd, which it sends only when it
// changed: the times it shows are coarse (see seenText), most renders send nothing. A variable for
// tests.
var statusInterval = 10 * time.Second

// statusSessions is how many sessions the status names at most, the most recently seen.
const statusSessions = 5

// degradation is a known issue of a ratd that serves all the same, logged once when found, and
// shown in the status for as long as it holds: the startup ones until ratd restarts.
type degradation int

const (
	// runningAsRoot: every agent is root (see warnDeployment)
	runningAsRoot degradation = iota
	// bundleWritable: an agent can replace the bundle (see warnDeployment)
	bundleWritable
	// checkUnfinished: bash startup files block the terminals (see checkTerminals)
	checkUnfinished
	// checkFailed: the check of the terminals could not run
	checkFailed
	// pasteOverridden: multi-line text may run line by line
	pasteOverridden
	// promptsReplaced: ratd can not tell when commands finish
	promptsReplaced
	// statusesLost: ratd does not tell how commands exited
	statusesLost
	// pipelinesLost: ratd does not tell the status of each command of a pipeline
	pipelinesLost
	degradations
)

// degradationLabels are short: the status line is one line, and the log tells the details.
var degradationLabels = [degradations]string{
	runningAsRoot:   "running as root",
	bundleWritable:  "bundle writable by its user",
	checkUnfinished: "terminals check did not finish",
	checkFailed:     "terminals check failed",
	pasteOverridden: "bracketed paste overridden",
	promptsReplaced: "PROMPT_COMMAND replaced",
	statusesLost:    "exit statuses lost",
	pipelinesLost:   "pipeline statuses lost",
}

func (d degradation) String() string {
	return degradationLabels[d]
}

// status is what ratd tells a human operator in systemctl status (STATUS=): the one line shown
// whatever the logs did since, where one line per tool call buries what was logged earlier. First
// what degrades ratd while it serves, then whether stopping ratd now cuts anyone: who called
// lately, and the waits in flight, the only calls lasting long enough to be seen at a render (the
// others take milliseconds). Of its calls, it is not tmux's state, which tmux keeps (see
// AGENTS.md): what ratd alone knows, lost when it stops. Any local user can read the status of a
// system unit, where the journal takes a group: session names are shown, never the addresses of
// clients. Nothing is told before ratd serves, systemd waiting for READY=1.
type status struct {
	logger *slog.Logger
	// notifier tells systemd the line (see takeNotifySocket)
	notifier notifier
	// mu guards the fields below
	mu sync.Mutex
	// serving is the start of the line, once ratd serves
	serving string
	// degraded holds the degradations found
	degraded [degradations]bool
	// expiry is when the bundle expires, once it does within a year (see warnExpiry)
	expiry time.Time
	// deaths are the times the tmux server died on its own, shown for deathWindow
	deaths      []time.Time
	deathWindow time.Duration
	// seen is when each session sent its last request: as many as the client certificates of the
	// bundle, which is closed
	seen map[string]time.Time
	// waits counts the calls of wait_window in flight
	waits int
	// stopping is the step of the stop, which replaces the line once set
	stopping string
	// sent is the line systemd was last told
	sent string
	// failing tells that telling systemd failed, logged once until it succeeds again
	failing bool
}

// newStatus returns the status of ratd, logging to logger its failures to tell systemd.
func newStatus(logger *slog.Logger) *status {
	return &status{logger: logger, seen: map[string]time.Time{}}
}

// see records a request of session, now.
func (s *status) see(session string, now time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.seen[session] = now
}

// waitStarted records a call of wait_window starting; waitEnded, the same call ending.
func (s *status) waitStarted() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.waits++
}

func (s *status) waitEnded() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.waits--
}

// degrade records degradations, and tells systemd at once: an operator must not wait for a render.
func (s *status) degrade(found ...degradation) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, d := range found {
		s.degraded[d] = true
	}
	s.tell(time.Now())
}

// expires records that the bundle expires at expiry, within a year, and tells systemd at once.
func (s *status) expires(expiry time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.expiry = expiry
	s.tell(time.Now())
}

// died records that the tmux server died on its own at now, shown for window (the window of the
// restart policy, within which deaths make ratd exit), and tells systemd at once.
func (s *status) died(now time.Time, window time.Duration) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.deaths, s.deathWindow = append(s.deaths, now), window
	s.tell(now)
}

// stop records the step of the stop, which the line tells from then on, and tells systemd at once.
func (s *status) stop(step string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.stopping = step
	s.tell(time.Now())
}

// render returns the status line at now, forgetting the deaths it no longer shows. Its caller holds
// mu.
func (s *status) render(now time.Time) string {
	if s.stopping != "" {
		return "stopping: " + s.stopping
	}
	parts := []string{s.serving}
	var issues []string
	for d, found := range s.degraded {
		if found {
			issues = append(issues, degradation(d).String())
		}
	}
	if !s.expiry.IsZero() {
		if left := s.expiry.Sub(now); left > 0 {
			issues = append(issues, fmt.Sprintf("bundle expires in %d days", int(left.Hours()/24)))
		} else {
			issues = append(issues, "bundle expired")
		}
	}
	s.deaths = slices.DeleteFunc(s.deaths, func(death time.Time) bool { return now.Sub(death) > s.deathWindow })
	switch window := int(s.deathWindow.Minutes()); {
	case len(s.deaths) == 1:
		issues = append(issues, fmt.Sprintf("tmux server died once in %dm", window))
	case len(s.deaths) > 1:
		issues = append(issues, fmt.Sprintf("tmux server died %d times in %dm", len(s.deaths), window))
	}
	if len(issues) > 0 {
		parts = append(parts, strings.Join(issues, ", "))
	}
	if len(s.seen) > 0 {
		sessions := make([]string, 0, len(s.seen))
		for session := range s.seen {
			sessions = append(sessions, session)
		}
		// most recent first, by name for the same time
		slices.SortFunc(sessions, func(a, b string) int {
			return cmp.Or(s.seen[b].Compare(s.seen[a]), strings.Compare(a, b))
		})
		shown := make([]string, 0, statusSessions+1)
		for _, session := range sessions[:min(len(sessions), statusSessions)] {
			shown = append(shown, session+" "+seenText(now.Sub(s.seen[session])))
		}
		if more := len(sessions) - statusSessions; more > 0 {
			shown = append(shown, fmt.Sprintf("+%d more", more))
		}
		parts = append(parts, "last seen: "+strings.Join(shown, ", "))
	}
	switch {
	case s.waits == 1:
		parts = append(parts, "1 wait in flight")
	case s.waits > 1:
		parts = append(parts, fmt.Sprintf("%d waits in flight", s.waits))
	}
	return strings.Join(parts, " · ")
}

// seenText tells how long ago a session was last seen, in the coarse unit a human needs: whether
// it calls right now, or since when it does not. A coarse text changes seldom, and so does the line.
func seenText(ago time.Duration) string {
	switch {
	case ago < time.Minute:
		return "now"
	case ago < time.Hour:
		return fmt.Sprintf("%dm ago", int(ago.Minutes()))
	case ago < 24*time.Hour:
		return fmt.Sprintf("%dh ago", int(ago.Hours()))
	default:
		return fmt.Sprintf("%dd ago", int(ago.Hours()/24))
	}
}

// tell sends the status line at now to systemd, once ratd serves, if it changed since it was last
// told. A failure is logged once, until telling succeeds again: the line is retried at each render.
// Its caller holds mu.
func (s *status) tell(now time.Time) {
	if s.serving == "" {
		return
	}
	line := s.render(now)
	if line == s.sent {
		return
	}
	if err := s.notifier.notify("STATUS=" + line); err != nil {
		if !s.failing {
			s.logger.Warn("failed to tell systemd the status of ratd", "error", err)
			s.failing = true
		}
		return
	}
	s.sent, s.failing = line, false
}

// run tells systemd the status of ratd serving the tenant at address, now then every
// statusInterval, until ctx is done.
func (s *status) run(ctx context.Context, tenant, address string) {
	s.mu.Lock()
	s.serving = fmt.Sprintf("serving tenant %s on %s", tenant, address)
	s.mu.Unlock()
	ticker := time.NewTicker(statusInterval)
	defer ticker.Stop()
	for {
		s.mu.Lock()
		s.tell(time.Now())
		s.mu.Unlock()
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}
