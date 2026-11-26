package main

import (
	"path/filepath"
	"testing"

	"github.com/hekmon/rat/connect"
)

// TestPortalUploadStopNotDone guards what the answer of an upload portal tells when ratd stops,
// waiting for a command or copying to one: never a portal done. An answer ending cleanly with no
// Rat-Error is one done; the stop cuts the answer most often (see cutOnStop), and tells the stop in
// the trailer otherwise, so that a client reading it to its end takes neither for a file delivered.
func TestPortalUploadStopNotDone(t *testing.T) {
	for _, phase := range []string{"waiting", "copying"} {
		t.Run(phase, func(t *testing.T) {
			pt := startPortals(t)
			path := filepath.Join(pt.home, "in.pipe")
			resp, send := pt.openUpload(t, pt.alice, path)
			expectPipe(t, path)
			if phase == "copying" {
				r := openReader(t, path)
				pt.waitOpened(t, path)
				if _, err := send.Write([]byte("begin")); err != nil {
					t.Fatal(err)
				}
				r.readUntil(5)
				eventually(t, "the pump waiting in a read of the body", func() bool {
					return pumpAt("net/http.(*body).Read(")
				})
			}
			expectQuickStop(t, pt.r)
			_, err := readBody(t, resp.Body)
			told := connect.DecodeError(resp.Trailer.Get(connect.HeaderError))
			switch {
			case err != nil:
				// cut by the stop
			case told != "ratd is stopping: the portal at "+path+" is closed, its pipe deleted.":
				t.Errorf("expected the answer cut, or ended telling the stop, got it ended with Rat-Error %q", told)
			}
			pt.r.d.portals.release()
		})
	}
}
