package mtls

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/hekmon/rat/tmux/names"
)

// copyFile copies src over dst, keeping the mode of dst if it exists.
func copyFile(t *testing.T, src, dst string) {
	t.Helper()
	data, err := os.ReadFile(src)
	if err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(dst, data, 0o600); err != nil {
		t.Fatal(err)
	}
}

// appendFile appends the content of src to dst.
func appendFile(t *testing.T, src, dst string) {
	t.Helper()
	data, err := os.ReadFile(src)
	if err != nil {
		t.Fatal(err)
	}
	f, err := os.OpenFile(dst, os.O_WRONLY|os.O_APPEND, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if _, err = f.Write(data); err != nil {
		t.Fatal(err)
	}
}

// TestLoadChecks guards each check of Load, and their order: a broken directory is refused with
// the sentinel of the first check it fails. Each case breaks the directory of client alice of a
// bundle, generated age ago; other is another bundle, for files that do not belong.
func TestLoadChecks(t *testing.T) {
	now := time.Now()
	other := newBundle(t, now)
	otherAlice := filepath.Join(other, "clients", "alice")
	for _, tc := range []struct {
		name     string
		age      time.Duration
		breakIt  func(t *testing.T, bundle, alice string)
		expected error
	}{
		{"no CA", 0, func(t *testing.T, bundle, alice string) {
			_ = os.Remove(filepath.Join(alice, "ca.crt"))
		}, ErrFiles},
		{"no certificate", 0, func(t *testing.T, bundle, alice string) {
			_ = os.Remove(filepath.Join(alice, "client.crt"))
		}, ErrFiles},
		{"no key", 0, func(t *testing.T, bundle, alice string) {
			_ = os.Remove(filepath.Join(alice, "client.key"))
		}, ErrFiles},
		{"both sides", 0, func(t *testing.T, bundle, alice string) {
			copyFile(t, filepath.Join(bundle, "server", "server.crt"), filepath.Join(alice, "server.crt"))
		}, ErrFiles},
		// all the certificates of ca.crt would be trusted
		{"two CAs", 0, func(t *testing.T, bundle, alice string) {
			appendFile(t, filepath.Join(otherAlice, "ca.crt"), filepath.Join(alice, "ca.crt"))
		}, ErrFiles},
		{"not a key", 0, func(t *testing.T, bundle, alice string) {
			copyFile(t, filepath.Join(alice, "client.crt"), filepath.Join(alice, "client.key"))
		}, ErrFiles},
		{"key readable by others", 0, func(t *testing.T, bundle, alice string) {
			if runtime.GOOS == "windows" {
				t.Skip("modes mean nothing on Windows")
			}
			_ = os.Chmod(filepath.Join(alice, "client.key"), 0o644)
		}, ErrKeyPermissions},
		{"key of another client", 0, func(t *testing.T, bundle, alice string) {
			copyFile(t, filepath.Join(bundle, "clients", "bob", "client.key"), filepath.Join(alice, "client.key"))
		}, ErrKeyMismatch},
		{"CA of another bundle", 0, func(t *testing.T, bundle, alice string) {
			copyFile(t, filepath.Join(otherAlice, "ca.crt"), filepath.Join(alice, "ca.crt"))
		}, ErrNotSignedByCA},
		{"certificate of another bundle", 0, func(t *testing.T, bundle, alice string) {
			copyFile(t, filepath.Join(otherAlice, "client.crt"), filepath.Join(alice, "client.crt"))
			copyFile(t, filepath.Join(otherAlice, "client.key"), filepath.Join(alice, "client.key"))
		}, ErrNotSignedByCA},
		// the server certificate, and its key, standing for a client
		{"server certificate", 0, func(t *testing.T, bundle, alice string) {
			copyFile(t, filepath.Join(bundle, "server", "server.crt"), filepath.Join(alice, "client.crt"))
			copyFile(t, filepath.Join(bundle, "server", "server.key"), filepath.Join(alice, "client.key"))
		}, ErrRole},
		{"expired", 11 * 365 * 24 * time.Hour, func(t *testing.T, bundle, alice string) {}, ErrValidity},
		{"not valid yet", -2 * clockSkew, func(t *testing.T, bundle, alice string) {}, ErrValidity},
		// order: the key is checked before the CA
		{"key and CA of another bundle", 0, func(t *testing.T, bundle, alice string) {
			copyFile(t, filepath.Join(otherAlice, "ca.crt"), filepath.Join(alice, "ca.crt"))
			copyFile(t, filepath.Join(otherAlice, "client.key"), filepath.Join(alice, "client.key"))
		}, ErrKeyMismatch},
	} {
		t.Run(tc.name, func(t *testing.T) {
			bundle := newBundle(t, now.Add(-tc.age))
			alice := filepath.Join(bundle, "clients", "alice")
			tc.breakIt(t, bundle, alice)
			if _, err := load(alice, now); !errors.Is(err, tc.expected) {
				t.Errorf("expected an error wrapping %v, got %v", tc.expected, err)
			}
		})
	}
}

// TestLoadNames guards the last check: a certificate or a CA carrying a name that is not a plain
// name is refused. Generate never writes one: the side is written by hand.
func TestLoadNames(t *testing.T) {
	now := time.Now()
	for _, tc := range []struct{ tenant, client string }{{"t", "a:b"}, {"a b", "c"}} {
		ca, err := newCA(tc.tenant, now.Add(-time.Hour), now.Add(time.Hour))
		if err != nil {
			t.Fatal(err)
		}
		client, err := issue(ca, tc.client, Client, now.Add(-time.Hour), now.Add(time.Hour))
		if err != nil {
			t.Fatal(err)
		}
		dir := filepath.Join(t.TempDir(), "side")
		if err = writeSide(dir, ca.cert, client, Client); err != nil {
			t.Fatal(err)
		}
		if _, err = load(dir, now); !errors.Is(err, names.ErrInvalid) {
			t.Errorf("tenant %q, client %q: expected names.ErrInvalid, got %v", tc.tenant, tc.client, err)
		}
	}
}
