//go:build unix

package main

import (
	"context"
	"path/filepath"
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

// TestRealRatdCancelFindsCutNotTold guards a cancel coming after a cut of an upload portal no call
// has told yet (Q20): it tells the cut and that the portal is not closed on ratd's side, to cancel
// again, without "the cancel came too late", which would contradict it; ratd is not asked, its
// pipe still there. The same cancel again, rat holding no job any more, closes it (Q18).
func TestRealRatdCancelFindsCutNotTold(t *testing.T) {
	tt := startRatdTransfers(t, "test-rat-cut-not-told")
	relay := startRelay(t, tt.addr)
	direct := tt.addr
	tt.addr = relay.listener.Addr().String()
	cut := inProcess(t, tt, tt.local)
	tt.addr = direct
	pipe := filepath.Join(tt.remote, "in.pipe")
	randomFile(t, filepath.Join(tt.local, "in.bin"), 64<<20, 0o644)
	args := map[string]any{"local_path": "in.bin", "remote_path": pipe, "remote_pipe": true}
	if text, isError := callTold(t, cut, context.Background(), uploadTool, args); isError {
		t.Fatalf("expected the portal open, got %q", text)
	}
	j := onlyJob(t, cut)
	relay.cut()
	<-j.done
	args["cancel"] = true
	text, isError := callTold(t, cut, context.Background(), uploadTool, args)
	if !isError || !strings.Contains(text, "was cut") || !strings.Contains(text, "again with cancel to close the portal") ||
		strings.Contains(text, "too late") {
		t.Errorf("expected the cut told, to cancel again, and no cancel come too late, got error %v %q", isError, text)
	}
	expectFIFO(t, pipe)
	text, isError = callTold(t, cut, context.Background(), uploadTool, args)
	if isError || !strings.Contains(text, "Closed the portal at "+pipe) {
		t.Errorf("expected the portal closed by the same cancel again, got error %v %q", isError, text)
	}
	expectNoPipe(t, pipe)
}
