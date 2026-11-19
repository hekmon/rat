package tools

import (
	"encoding/json"
	"strings"
	"testing"
)

// TestCopyHints guards the copy hints: every sentence, generic or direct, encodes as itself in
// JSON, for rat to find and replace it in an answer of ratd as it is; the generic sentences differ
// from one another and name no tool, ratd not knowing what its client offers; the direct ones name
// the tools of rat.
func TestCopyHints(t *testing.T) {
	seen := map[string]bool{}
	for _, hint := range CopyHints() {
		for _, sentence := range []string{hint.Generic, hint.Direct} {
			if encoded, err := json.Marshal(sentence); err != nil || string(encoded) != `"`+sentence+`"` {
				t.Errorf("%q: encoded as %s, %v", sentence, encoded, err)
			}
		}
		if seen[hint.Generic] {
			t.Errorf("%q: twice", hint.Generic)
		}
		seen[hint.Generic] = true
		if strings.Contains(hint.Generic, UploadFile) || strings.Contains(hint.Generic, DownloadFile) {
			t.Errorf("%q: names a tool of rat", hint.Generic)
		}
		if !strings.Contains(hint.Direct, UploadFile) && !strings.Contains(hint.Direct, DownloadFile) {
			t.Errorf("%q: names no tool of rat", hint.Direct)
		}
	}
}
