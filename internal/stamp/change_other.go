//go:build !linux && !darwin

package stamp

import (
	"io/fs"
	"time"
)

// changeTime returns no time: Go exposes no change time here (Windows has none), and a stamp
// compares the size and the modification time alone.
func changeTime(fs.FileInfo) time.Time {
	return time.Time{}
}
