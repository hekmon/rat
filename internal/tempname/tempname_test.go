package tempname

import (
	"strings"
	"testing"
	"unicode/utf8"
)

// TestFor guards the temporary names: .NAME.rat- then 8 hex digits, NAME the name whole up to 200
// bytes, cut beyond at the start of a character, a character of 2, 3 or 4 bytes across byte 200
// left out, a valid name keeping valid, as APFS takes only those; a name that is not UTF-8 cut
// three bytes back at most; and the random part drawn at each call.
func TestFor(t *testing.T) {
	for _, c := range []struct{ what, name, kept string }{
		{"short", "data.pipe", "data.pipe"},
		{"200 bytes", strings.Repeat("a", 200), strings.Repeat("a", 200)},
		{"longer", strings.Repeat("a", 250), strings.Repeat("a", 200)},
		{"2 bytes across", strings.Repeat("a", 199) + "é.bin", strings.Repeat("a", 199)},
		{"3 bytes across", strings.Repeat("a", 198) + "€.bin", strings.Repeat("a", 198)},
		{"4 bytes across", strings.Repeat("a", 197) + "😀.bin", strings.Repeat("a", 197)},
		{"ending at 200", strings.Repeat("a", 198) + "é.bin", strings.Repeat("a", 198) + "é"},
		{"not UTF-8", strings.Repeat("\x80", 250), strings.Repeat("\x80", 197)},
	} {
		got := For(c.name)
		suffix, found := strings.CutPrefix(got, "."+c.kept+".rat-")
		if !found || len(suffix) != 8 || strings.Trim(suffix, "0123456789abcdef") != "" {
			t.Errorf("%s: expected .%q.rat- then 8 hex digits, got %q", c.what, c.kept, got)
		}
		if utf8.ValidString(c.name) && !utf8.ValidString(got) {
			t.Errorf("%s: a valid name gave %q, not UTF-8", c.what, got)
		}
	}
	if For("data.pipe") == For("data.pipe") {
		t.Error("expected a random part drawn at each call")
	}
}
