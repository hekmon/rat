// Package tempname names the temporary files of the transfers of rat, each next to the file it
// stands for: the file an upload or a download goes to before it is put in its place, which ratd
// and rat make. Each is named after that file, .NAME.rat-RANDOM: hidden from ls and globs, and
// telling what it is, for a human finding one left behind (a process killed during a transfer) to
// remove it.
package tempname

import (
	"fmt"
	"math/rand/v2"
	"unicode/utf8"
)

// nameMax bounds NAME in a temporary name, which its dot and suffix (.rat- and 8 hex digits) must
// fit with within 255 bytes: what a name takes on Linux, which counts bytes (APFS counts
// characters, 255 of them).
const nameMax = 200

// For returns a temporary name for the file named name, its random part drawn at each call:
// .NAME.rat-RANDOM, NAME being name cut to 200 bytes at most, at the start of a character rather
// than within it: APFS refuses a name that is not UTF-8 (EILSEQ), which a valid name cut within a
// character would be. A name that is not UTF-8, which Linux takes, is cut three bytes back at
// most.
func For(name string) string {
	return fmt.Sprintf(".%s.rat-%08x", cut(name), rand.Uint32())
}

// cut returns name cut to nameMax bytes at most, at the start of a character (see For).
func cut(name string) string {
	if len(name) <= nameMax {
		return name
	}
	n := nameMax
	for back := 0; back < utf8.UTFMax-1 && !utf8.RuneStart(name[n]); back++ {
		n--
	}
	return name[:n]
}
