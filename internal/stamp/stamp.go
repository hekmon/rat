// Package stamp tells whether a file changed between two moments, from what its file system
// records of it: its size, its modification time and its change time, as GNU tar checks each
// file it archives ("file changed as we read it"). A file transfer of rat stamps the file it
// reads when opening it and once read: a file still being written (a log a command appends to, a
// build output) is refused rather than copied torn.
//
// The change time is set by the system on any write or change of metadata, and no program can set
// it back, unlike the modification time (touch, cp -p, rsync -t): it is compared where Go exposes
// it, on Linux and macOS, and left out elsewhere (Windows). What a stamp can not see: a rewrite
// keeping the size within one tick of the clock the system stamps times with, and writes through
// a memory mapping, whose times the system updates late. Comparing the content instead would
// take reading the file twice.
package stamp

import (
	"io/fs"
	"time"
)

// Stamp is what tells a file from itself changed: its size and times at a moment.
type Stamp struct {
	size     int64
	modified time.Time
	changed  time.Time
}

// Of returns the stamp of the file info describes: from Stat on the file opened, rather than on
// its path, which may name another file by the time it is stamped again.
func Of(info fs.FileInfo) Stamp {
	return Stamp{size: info.Size(), modified: info.ModTime(), changed: changeTime(info)}
}

// Equal tells whether s and other stamp the same file unchanged.
func (s Stamp) Equal(other Stamp) bool {
	return s.size == other.size && s.modified.Equal(other.modified) && s.changed.Equal(other.changed)
}
