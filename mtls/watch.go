package mtls

import (
	"crypto"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"time"
)

// Source gives the side of a TLS configuration (see ServerConfig, ClientConfig) at each handshake,
// and tells what happens to its CRL and to its files: a Side, fixed, or a Watcher, reloading it
// when renewed.
type Source interface {
	// Side returns the side in use.
	Side() *Side
	// OnCRL adds events told what happens to the CRL of the side in use, and of the sides after
	// it (see Side.OnCRL).
	OnCRL(events CRLEvents)
	// OnWatch adds events told what happens to the files of the side (see WatchEvents).
	OnWatch(events WatchEvents)
}

// Side returns s: a side loaded once is its own source, never renewed.
func (s *Side) Side() *Side {
	return s
}

// OnWatch tells nothing: a side loaded once is never renewed.
func (s *Side) OnWatch(WatchEvents) {}

var (
	// ErrCAChanged is returned for a directory whose CA changed: the trust of the side changes, which
	// takes a restart, decided by a human, rather than a reload.
	ErrCAChanged = errors.New("CA changed")
	// ErrIdentityChanged is returned for a directory now holding another side: another role or name,
	// which would move a running process to another tenant or session.
	ErrIdentityChanged = errors.New("another side")
)

// watchInterval is how often a Watcher looks at its files, at most: once per handshake and request
// otherwise, each a few stat calls.
const watchInterval = time.Second

// WatchEvents are told what happens to the files of a Watcher, for its holder to log and show.
// They are called outside any lock of the Watcher.
type WatchEvents struct {
	// Reloaded is told a side renewed, now in use, newKey telling its key changed too: a rotation
	// as some PKIs do at each renewal, or a key replaced after it leaked, which a human should know
	// of.
	Reloaded func(side *Side, newKey bool)
	// Refused is told the files changed, and were refused, the side in use kept: the error wraps
	// ErrCAChanged, ErrIdentityChanged, or the error of Load. It is told nil once the files hold the
	// side in use again, nothing being refused anymore.
	Refused func(err error)
}

// Watcher is the side of a directory, loaded again when its files change: a certificate renewed
// by a PKI, with the same CA, role and name, its key renewed or not, is used from the next
// handshake on, without a restart. A restart of ratd stops every terminal, and a rat running for
// weeks would present a certificate expired.
//
// The files are looked at when the side is asked for (Side), at most once per watchInterval: no
// goroutine, nothing on a timer. A new CA, another role or name, or files failing a check of Load
// are refused, told once per change of the files, and the side in use kept: a renewal copied
// halfway never breaks a running side.
type Watcher struct {
	dir string
	// mu guards the fields below.
	mu sync.Mutex
	// side is the side in use.
	side *Side
	// stamp identifies the files last looked at, loaded or refused; checked is when.
	stamp   []fileStamp
	checked time.Time
	// refused tells the files last looked at were refused, for a restore to be told
	refused bool
	// events are told what happens to the files, crlEvents what happens to the CRL of each side.
	events    []WatchEvents
	crlEvents []CRLEvents
}

// Watch loads the side of dir (see Load), and returns its Watcher.
func Watch(dir string) (*Watcher, error) {
	side, err := Load(dir)
	if err != nil {
		return nil, err
	}
	stamp, err := stampFiles(dir, side.Role)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrFiles, err)
	}
	return &Watcher{dir: dir, side: side, stamp: stamp, checked: time.Now()}, nil
}

// OnWatch adds events told what happens to the files of the Watcher.
func (w *Watcher) OnWatch(events WatchEvents) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.events = append(w.events, events)
}

// OnCRL adds events told what happens to the CRL of the side in use, and of each side renewed
// after it: register them here rather than on a side, which a renewal replaces.
func (w *Watcher) OnCRL(events CRLEvents) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.crlEvents = append(w.crlEvents, events)
	w.side.OnCRL(events)
}

// Side returns the side in use, loading the directory again first when its files changed (see
// Watcher).
func (w *Watcher) Side() *Side {
	w.mu.Lock()
	side, tell := w.look(time.Now())
	events := slices.Clone(w.events)
	w.mu.Unlock()
	if tell != nil {
		for _, e := range events {
			tell(e)
		}
	}
	return side
}

// look loads the directory again when its files changed since the last look, and returns the side
// in use, and what to tell of it, nil for nothing. Its caller holds mu.
func (w *Watcher) look(now time.Time) (*Side, func(WatchEvents)) {
	if now.Sub(w.checked) < watchInterval && !now.Before(w.checked) {
		return w.side, nil
	}
	w.checked = now
	// a file missing stamps the directory as nil: refused once, until the files change again
	stamp, _ := stampFiles(w.dir, w.side.Role)
	if slices.Equal(stamp, w.stamp) {
		return w.side, nil
	}
	// told once per change of the files, refused or not
	w.stamp = stamp
	next, err := Load(w.dir)
	if err == nil {
		err = w.renewal(next)
	}
	if err != nil {
		w.refused = true
		return w.side, func(e WatchEvents) {
			if e.Refused != nil {
				e.Refused(err)
			}
		}
	}
	restored := w.refused
	w.refused = false
	if next.Certificate.Leaf.Equal(w.side.Certificate.Leaf) {
		// the side in use, its files touched (nothing to tell) or restored after a refusal
		if !restored {
			return w.side, nil
		}
		return w.side, func(e WatchEvents) {
			if e.Refused != nil {
				e.Refused(nil)
			}
		}
	}
	newKey := !samePublicKey(next, w.side)
	next.inherit(w.side)
	for _, events := range w.crlEvents {
		next.OnCRL(events)
	}
	w.side = next
	return next, func(e WatchEvents) {
		if e.Reloaded != nil {
			e.Reloaded(next, newKey)
		}
	}
}

// renewal returns an error when next, loaded from the directory, does not renew the side in use:
// another CA, role or name.
func (w *Watcher) renewal(next *Side) error {
	current := w.side
	if !next.CA.Equal(current.CA) {
		return fmt.Errorf("%w: %s holds another CA, which takes a restart", ErrCAChanged, filepath.Join(w.dir, caFile))
	}
	if next.Role != current.Role || next.Name != current.Name {
		return fmt.Errorf("%w: %s now holds the %s %s, not the %s %s", ErrIdentityChanged, w.dir, next.Role, next.Name,
			current.Role, current.Name)
	}
	return nil
}

// inherit takes the CRL held by previous, the side it renews, when its certificate names the same
// distribution point: the CRL is of the same CA, and a CRL older than it stays refused.
func (s *Side) inherit(previous *Side) {
	if s.revocation == nil || previous.revocation == nil || s.revocation.address != previous.revocation.address {
		return
	}
	previous.revocation.mu.Lock()
	crl, attempted, failed := previous.revocation.crl, previous.revocation.attempted, previous.revocation.failed
	previous.revocation.mu.Unlock()
	s.revocation.crl, s.revocation.attempted, s.revocation.failed = crl, attempted, failed
}

// samePublicKey tells whether a and b hold the same key.
func samePublicKey(a, b *Side) bool {
	key, ok := a.Certificate.Leaf.PublicKey.(interface{ Equal(crypto.PublicKey) bool })
	return ok && key.Equal(b.Certificate.Leaf.PublicKey)
}

// fileStamp identifies the content of a file without reading it: its size and modification time,
// which a copy, a rename or a write changes.
type fileStamp struct {
	size    int64
	modTime time.Time
}

// stampFiles returns the stamps of the files of the side of role in dir.
func stampFiles(dir string, role Role) ([]fileStamp, error) {
	files := []string{caFile, certFile(role), keyFile(role)}
	stamps := make([]fileStamp, 0, len(files))
	for _, file := range files {
		info, err := os.Stat(filepath.Join(dir, file))
		if err != nil {
			return nil, err
		}
		stamps = append(stamps, fileStamp{info.Size(), info.ModTime()})
	}
	return stamps, nil
}
