package main

import (
	"bytes"
	"context"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/hekmon/rat/internal/testpki"
	"github.com/hekmon/rat/internal/version"
)

// TestCommandFlags guards the command line of rat: a flag given twice is refused, and the version
// is told.
func TestCommandFlags(t *testing.T) {
	var out bytes.Buffer
	err := command(strings.NewReader(""), &out, &out).Run(context.Background(),
		[]string{"rat", "-s", "h:1", "-b", "dir", "--log-level", "info", "--log-level", "debug"})
	if err == nil || !strings.Contains(err.Error(), "can't duplicate this flag") {
		t.Errorf("expected the repeated flag refused, got %v", err)
	}
	cmd := command(strings.NewReader(""), &out, &out)
	out.Reset()
	cmd.Writer = &out
	if err = cmd.Run(context.Background(), []string{"rat", "-v"}); err != nil || out.String() != "rat version "+version.String()+"\n" {
		t.Errorf("expected the version, got %q, %v", out.String(), err)
	}
}

// TestRunExpiryWarning guards that rat warns of its certificate expiring soon while it runs, not
// only when starting: before relaying, then at each interval, a rat running for weeks; and that a
// renewal, the certificate in use expiring no longer soon, ends the warnings.
func TestRunExpiryWarning(t *testing.T) {
	interval := expiryCheckInterval
	expiryCheckInterval = 20 * time.Millisecond
	t.Cleanup(func() { expiryCheckInterval = interval })
	pki := testpki.NewSubCA(t, "t", "")
	// valid for an hour and five minutes, five left: in its last tenth
	pki.Expiring("alice", time.Now().Add(5*time.Minute))
	// rat reaches ratd at the first message only: none is sent
	p := startRat(t, pki.Dir, "127.0.0.1:1", slog.LevelInfo)
	warnings := func() int { return strings.Count(p.logs.String(), "expires soon") }
	for deadline := time.Now().Add(5 * time.Second); warnings() < 3; time.Sleep(10 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatalf("expected a warning when starting, then at each interval, got %d:\n%s", warnings(), p.logs)
		}
	}
	pki.Renew("alice", false)
	// the files are looked at once a second at most
	time.Sleep(1200 * time.Millisecond)
	before := warnings()
	time.Sleep(100 * time.Millisecond)
	if after := warnings(); after != before {
		t.Errorf("renewed: expected the warnings to end, got %d more", after-before)
	}
}
