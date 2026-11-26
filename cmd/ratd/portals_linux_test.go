package main

import (
	"errors"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hekmon/rat/connect"
	"golang.org/x/sys/unix"
)

// TestPortalPinFailing guards an opening failing once its pipe is made, ratd having no descriptor
// left to pin it with: it leaves nothing, the pipe removed (P2), and tells the reason of the system,
// not that something else took the path, which a pin finding another file alone tells.
func TestPortalPinFailing(t *testing.T) {
	pt := startPortals(t)
	busy := filepath.Join(pt.home, "busy")
	if err := os.WriteFile(busy, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	// a first exchange, refused: its connection serves the next one, which could open none
	req := newDownload(t, pt.r.addr, busy)
	req.Header.Set(connect.HeaderPipe, "true")
	if resp, body, err := sendTransfer(pt.alice, req); err != nil || resp.StatusCode != http.StatusConflict {
		t.Fatalf("expected a first portal refused, got %v, %v: %q", resp, err, body)
	}
	var limit unix.Rlimit
	if err := unix.Getrlimit(unix.RLIMIT_NOFILE, &limit); err != nil {
		t.Fatal(err)
	}
	low := limit
	low.Cur = 0
	if err := unix.Setrlimit(unix.RLIMIT_NOFILE, &low); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(pt.home, "out.pipe")
	req = newDownload(t, pt.r.addr, path)
	req.Header.Set(connect.HeaderPipe, "true")
	resp, body, err := sendTransfer(pt.alice, req)
	if restoreErr := unix.Setrlimit(unix.RLIMIT_NOFILE, &limit); restoreErr != nil {
		t.Fatal(restoreErr)
	}
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode == http.StatusOK || !strings.Contains(body, "too many open files") ||
		strings.Contains(body, "taken by something else") {
		t.Errorf("expected the opening refused for the lack of descriptors, got %s: %q", resp.Status, body)
	}
	if info, err := os.Lstat(path); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("expected nothing left at %s, got %v, %v", path, info, err)
	}
	if held := pt.portalAt(path); held != nil {
		t.Errorf("expected no portal held at %s", path)
	}
}
