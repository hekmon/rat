package main

import (
	"context"
	"io/fs"
	"sync"
	"syscall"
)

// fileLocks serializes the writes in place into a file through ratd, write_file's and those of
// uploads with in_place, by the device and the inode of the file. A write in place can outlive the
// call that made it: the handler of an upload canceled reads and writes the bytes already sent,
// which the buffers of its connection hold, before it sees the cut, while the agent, told the file
// may be partly written, writes it again at once. Without the lock, the two would interleave, the
// file left torn, the late bytes of the first after the content of the second, the second answered
// done. Rejected: refusing the second write while the first ends, which would make the agent call
// again for a wait of moments. A write from a terminal or another program takes no such lock: only
// the writes of one ratd go one at a time.
type fileLocks struct {
	mu sync.Mutex
	// files holds the locks of the files written or waited for
	files map[fileKey]*fileLock
}

// fileKey names a file by its device and inode: two paths to it (a hard link, a symbolic link
// followed by write_file) meet on one lock.
type fileKey struct{ dev, ino uint64 }

// fileLock is the lock of a file: held while held holds a value, and kept in fileLocks while users,
// holding it or waiting for it, remain.
type fileLock struct {
	held  chan struct{}
	users int
}

// lockWaiting is called with the path of a write waiting for the lock of its file, held by
// another: a hook for tests.
var lockWaiting = func(string) {}

// lock waits until no other write in place holds the file at path, described by info, then holds
// it until release is called. The wait ends with ctx, failing with its error: a write_file whose
// call was answered must not write later, replacing what the write it waited for left. It fails
// with errStopping once stopping is closed, ratd stopping: a write waiting never delays the stop. A
// nil stopping, before ratd serves, never closes. Neither is looked at once the lock is held. A
// file whose device and inode the system does not tell is not locked. release is never nil, a call
// to it doing nothing once lock failed; the caller calls it once, a second call blocking.
func (l *fileLocks) lock(ctx context.Context, path string, info fs.FileInfo, stopping <-chan struct{}) (
	release func(), err error) {
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return func() {}, nil
	}
	key := fileKey{dev: uint64(stat.Dev), ino: uint64(stat.Ino)}
	l.mu.Lock()
	if l.files == nil {
		l.files = make(map[fileKey]*fileLock)
	}
	f := l.files[key]
	if f == nil {
		f = &fileLock{held: make(chan struct{}, 1)}
		l.files[key] = f
	}
	f.users++
	l.mu.Unlock()
	// the lock of a file nobody holds nor waits for is dropped: a lock per file ever written would
	// grow for as long as ratd runs
	leave := func() {
		l.mu.Lock()
		defer l.mu.Unlock()
		if f.users--; f.users == 0 {
			delete(l.files, key)
		}
	}
	select {
	case f.held <- struct{}{}:
	default:
		lockWaiting(path)
		select {
		case f.held <- struct{}{}:
		case <-ctx.Done():
			leave()
			return func() {}, ctx.Err()
		case <-stopping:
			leave()
			return func() {}, errStopping
		}
	}
	return func() {
		<-f.held
		leave()
	}, nil
}
