package main

import (
	"fmt"
	"math"
	"strconv"
	"unicode/utf8"

	"github.com/hekmon/rat/tmux"

	"github.com/hekmon/cunits/v3"
)

const (
	// defaultReadBudget is the read budget unless set: about 16k tokens.
	defaultReadBudget = 64 * 1024
	// minReadBudget is the smallest read budget ratd accepts: about 5k tokens. A screen over the
	// budget is refused rather than cut, so the budget holds the largest screen: maxScreenBytes
	// (19200), the line breaks between its rows (23), and the longest header, the one of a cut
	// history, with its line break (126), 19349 bytes, rounded up with about 1 KiB for a header
	// to change (TestMinReadBudget checks it). A screen usually takes about 5 KB.
	minReadBudget = 20 * 1024
	// maxScreenBytes is the largest screen, its cells holding the longest UTF-8 characters, 4 bytes
	// (18.8 KiB); wide characters take 2 cells for as much. Characters stacking combining accents
	// take more a cell (tmux holds 21 to 32 bytes), beyond any budget: such a screen stays refused.
	maxScreenBytes = tmux.ScreenColumns * tmux.ScreenRows * utf8.UTFMax
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
		return 0, fmt.Errorf("read budget %s (%s) under the minimum of %s: a screen alone can take %s", size,
			sizeText(budget), sizeText(minReadBudget), sizeText(maxScreenBytes))
	}
	return budget, nil
}

// sizeText tells a size in bytes as agents and admins read it: in its largest binary unit, with
// a decimal at most (64 KiB, 97.7 KiB, 500 B).
func sizeText(bytes int) string {
	value, unit := (cunits.Bits(bytes) * cunits.Byte).GetHumanSizeAndSuffix()
	return strconv.FormatFloat(math.Round(value*10)/10, 'f', -1, 64) + " " + unit
}
