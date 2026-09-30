package main

import (
	"fmt"
	"math"
	"strconv"

	"github.com/hekmon/cunits/v3"
)

const (
	// defaultReadBudget is the read budget unless set: about 16k tokens.
	defaultReadBudget = 64 * 1024
	// minReadBudget is the smallest read budget ratd accepts: a screen usually takes about 5 KB,
	// 20 KB with multibyte characters, and a screen over the budget is refused rather than cut.
	minReadBudget = 32 * 1024
)

// parseReadBudget returns the read budget in bytes written as size, with its unit (64KiB, 1MiB,
// 65536B). It refuses a budget under minReadBudget, telling the size it read: a unit mistaken
// for another (Kb is kilobits) then shows.
func parseReadBudget(size string) (int, error) {
	bits, err := cunits.Parse(size)
	if err != nil {
		return 0, fmt.Errorf("invalid read budget %q, expected a size with its unit (64KiB): %w", size, err)
	}
	budget := int(min(bits/cunits.Byte, math.MaxInt))
	if budget < minReadBudget {
		return 0, fmt.Errorf("read budget %s (%s) under the minimum of %s: a screen alone can take 20 KB", size,
			sizeText(budget), sizeText(minReadBudget))
	}
	return budget, nil
}

// sizeText tells a size in bytes as agents and admins read it: in its largest binary unit, with
// a decimal at most (64 KiB, 97.7 KiB, 500 B).
func sizeText(bytes int) string {
	value, unit := (cunits.Bits(bytes) * cunits.Byte).GetHumanSizeAndSuffix()
	return strconv.FormatFloat(math.Round(value*10)/10, 'f', -1, 64) + " " + unit
}
