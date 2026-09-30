package main

import (
	"bytes"
	"context"
	"strings"
	"testing"

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
