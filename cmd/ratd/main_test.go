package main

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"github.com/hekmon/rat/internal/version"
)

// TestCommandFlags guards the command line of ratd: a flag given twice is refused before anything
// starts, rather than the last value silently winning, and the version is told.
func TestCommandFlags(t *testing.T) {
	err := command(&bytes.Buffer{}).Run(context.Background(), []string{"ratd", "-b", "one", "-b", "two"})
	if err == nil || !strings.Contains(err.Error(), "can't duplicate this flag") {
		t.Errorf("expected the repeated flag refused, got %v", err)
	}
	cmd := command(&bytes.Buffer{})
	var out bytes.Buffer
	cmd.Writer = &out
	if err = cmd.Run(context.Background(), []string{"ratd", "--version"}); err != nil ||
		out.String() != "ratd version "+version.String()+"\n" {
		t.Errorf("expected the version, got %q, %v", out.String(), err)
	}
}
