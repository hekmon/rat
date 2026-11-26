//go:build unix

package main

import (
	"strings"
	"testing"
)

// TestRealRatdCancelPathRefused guards a cancel finding no job at a remote path ratd refuses, a
// relative one: ratd holds no portal at a path it does not take, so the cancel tells nothing is
// under way, with ratd's sentence, not that a portal may still be open and to cancel again.
func TestRealRatdCancelPathRefused(t *testing.T) {
	tt := startRatdTransfers(t, "test-rat-cancelpath")
	args := map[string]any{"local_path": "in.bin", "remote_path": "relative/in.bin", "cancel": true}
	text := tt.expect(t, uploadTool, args, false, "No transfer to relative/in.bin is under way: nothing to cancel. ")
	if strings.Contains(text, "may still be open") {
		t.Errorf("expected no portal told possibly open at a path ratd refuses, got %q", text)
	}
}
