package main

import (
	"context"
	"io"
	"strings"
	"testing"

	"github.com/urfave/cli/v3"
)

// TestParseReadBudget guards the sizes the read budget accepts: with their unit, binary or not,
// and 32 KiB at least, the refusal telling the size read.
func TestParseReadBudget(t *testing.T) {
	for size, expected := range map[string]int{
		"64KiB":  defaultReadBudget,
		"64 KiB": defaultReadBudget,
		"32KiB":  minReadBudget,
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
		"16KiB": "(16 KiB) under the minimum of 32 KiB",
		"64Kib": "(8 KiB) under the minimum of 32 KiB",
	} {
		if _, err := parseReadBudget(size); err == nil || !strings.Contains(err.Error(), part) {
			t.Errorf("%s: expected an error containing %q, got %v", size, part, err)
		}
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
