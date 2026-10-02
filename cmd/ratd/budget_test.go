package main

import (
	"context"
	"io"
	"strings"
	"testing"

	"github.com/hekmon/rat/tmux"

	"github.com/urfave/cli/v3"
)

// TestParseReadBudget guards the sizes the read budget accepts: with their unit, binary or not,
// and 20 KiB at least, the refusal telling the size read.
func TestParseReadBudget(t *testing.T) {
	for size, expected := range map[string]int{
		"64KiB":  defaultReadBudget,
		"64 KiB": defaultReadBudget,
		"20KiB":  minReadBudget,
		"1MiB":   1 << 20,
		"100KB":  100000,
		"65536B": 65536,
	} {
		if budget, err := parseReadBudget(size); err != nil || budget != expected {
			t.Errorf("%s: expected %d, got %d, %v", size, expected, budget, err)
		}
	}
	for size, part := range map[string]string{
		"65536": "expected a size with its unit",
		"64k":   "expected a size with its unit",
		"16KiB": "(16 KiB) under the minimum of 20 KiB: a screen alone can take 18.8 KiB",
		"64Kib": "(8 KiB) under the minimum of 20 KiB",
	} {
		if _, err := parseReadBudget(size); err == nil || !strings.Contains(err.Error(), part) {
			t.Errorf("%s: expected an error containing %q, got %v", size, part, err)
		}
	}
}

// TestMinReadBudget guards that the smallest budget holds the largest screen, with its header, the
// longest being the one of a cut history: a screen over the budget is refused, its window unusable.
// It fails if the size of the terminals grows past it.
func TestMinReadBudget(t *testing.T) {
	row := strings.Repeat("\U0001D400", tmux.ScreenColumns) // 4 bytes, one cell
	screen := strings.Repeat(row+"\n", tmux.ScreenRows-1) + row
	if len(screen) != maxScreenBytes+tmux.ScreenRows-1 {
		t.Fatalf("expected a screen of %d bytes, got %d", maxScreenBytes+tmux.ScreenRows-1, len(screen))
	}
	header := cutHeader(minReadBudget)
	if room := minReadBudget - len(header) - len("\n"); len(screen) > room {
		t.Errorf("the largest screen takes %d bytes, more than the %d left by the smallest budget", len(screen), room)
	}
}

// TestSizeText guards how sizes are told: in their largest binary unit, with a decimal at most.
func TestSizeText(t *testing.T) {
	for bytes, expected := range map[int]string{
		500:               "500 B",
		defaultReadBudget: "64 KiB",
		100000:            "97.7 KiB",
		1 << 20:           "1 MiB",
	} {
		if text := sizeText(bytes); text != expected {
			t.Errorf("%d: expected %q, got %q", bytes, expected, text)
		}
	}
}

// TestCommandReadBudget guards that ratd refuses a read budget under the minimum before starting
// anything, and that its default is a valid budget.
func TestCommandReadBudget(t *testing.T) {
	err := command(io.Discard).Run(context.Background(), []string{"ratd", "--bundle", "/nonexistent", "-r", "16KiB"})
	if err == nil || !strings.Contains(err.Error(), "under the minimum") {
		t.Errorf("expected the budget to be refused, got %v", err)
	}
	for _, flag := range command(io.Discard).Flags {
		if f, ok := flag.(*cli.StringFlag); ok && f.Name == "read-budget" {
			if budget, err := parseReadBudget(f.Value); err != nil || budget != defaultReadBudget {
				t.Errorf("default: expected %d, got %d, %v", defaultReadBudget, budget, err)
			}
		}
	}
}
