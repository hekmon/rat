package stamp

import (
	"io/fs"
	"syscall"
	"time"
)

// changeTime returns the change time of the file info describes (st_ctim).
func changeTime(info fs.FileInfo) time.Time {
	if st, ok := info.Sys().(*syscall.Stat_t); ok {
		return time.Unix(st.Ctim.Unix())
	}
	return time.Time{}
}
