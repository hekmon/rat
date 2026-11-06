package mtls

import (
	"crypto/tls"
	"crypto/x509"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
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

// checkOf returns the index, in Checks, of the one check whose sentinel err wraps, or -1 if it
// wraps none or several: a caller could not tell which check failed.
func checkOf(err error) int {
	index := -1
	for i, check := range Checks() {
		if errors.Is(err, check.Err) {
			if index >= 0 {
				return -1
			}
			index = i
		}
	}
	return index
}

// TestLoadChecks guards each check of Load, and their order: a broken directory is refused with
// the sentinel of the first check it fails, and of that check only (see Checks). Each case breaks
// the directory of client alice of a bundle, generated age ago; other is another bundle, for files
// that do not belong.
func TestLoadChecks(t *testing.T) {
	now := time.Now()
	other := newBundle(t, now)
	otherAlice := filepath.Join(other, "clients", "alice")
	// alice issued by a CA allowed to sign pathLen levels of CAs, as many as it wants for -1
	caSigningCAs := func(pathLen int) func(t *testing.T, bundle, alice string) {
		return func(t *testing.T, bundle, alice string) {
			ca := caWithPathLen(t, "t", pathLen, nil)
			client, err := issue(ca, "alice", Client, now.Add(-time.Hour), now.Add(time.Hour))
			if err != nil {
				t.Fatal(err)
			}
			for _, file := range []string{"ca.crt", "client.crt", "client.key"} {
				_ = os.Remove(filepath.Join(alice, file))
			}
			if err = writeSide(alice, ca.cert, client, Client); err != nil {
				t.Fatal(err)
			}
		}
	}
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
		// ratd would let in the certificates of the CAs under it (TestConfigsCAUnder)
		{"CA signing CAs", 0, caSigningCAs(-1), ErrNotSignedByCA},
		{"CA signing a level of CAs", 0, caSigningCAs(1), ErrNotSignedByCA},
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
			_, err := load(alice, now)
			if !errors.Is(err, tc.expected) {
				t.Errorf("expected an error wrapping %v, got %v", tc.expected, err)
			}
			if checkOf(err) < 0 {
				t.Errorf("the error matches no check of Checks, or several: %v", err)
			}
		})
	}
}

// TestLoadNames guards the names check: a certificate or a CA carrying a name that is not a plain
// name is refused, and so is a server certificate naming another tenant than its CA, as ratd
// serves the tenant of its CA while its clients are told the one of its certificate. Generate
// never writes either: the side is written by hand.
func TestLoadNames(t *testing.T) {
	now := time.Now()
	for _, tc := range []struct {
		tenant, name string
		role         Role
	}{
		{"t", "a:b", Client},
		{"a b", "c", Client},
		{"t", "u", Server},
	} {
		ca, err := newCA(tc.tenant, now.Add(-time.Hour), now.Add(time.Hour))
		if err != nil {
			t.Fatal(err)
		}
		side, err := issue(ca, tc.name, tc.role, now.Add(-time.Hour), now.Add(time.Hour))
		if err != nil {
			t.Fatal(err)
		}
		dir := filepath.Join(t.TempDir(), "side")
		if err = writeSide(dir, ca.cert, side, tc.role); err != nil {
			t.Fatal(err)
		}
		if _, err = load(dir, now); !errors.Is(err, names.ErrInvalid) || checkOf(err) < 0 {
			t.Errorf("tenant %q, %s %q: expected names.ErrInvalid, and its check only, got %v", tc.tenant, tc.role, tc.name, err)
		}
	}
}

// TestExpiresSoon guards when the holders of a side warn of its expiry: in the last tenth of the
// validity of whichever of its certificate and CA expires first, a year for a bundle, and not
// from the first day of a certificate valid for a year, as another PKI issues them. The warning
// names what expires first, and what to do, and tells a side expired as such: a side running
// keeps being reminded past its expiry.
func TestExpiresSoon(t *testing.T) {
	now := time.Now()
	day, year := 24*time.Hour, 365*24*time.Hour
	// a certificate valid from since ago, for validity, its issuer named issuer
	cert := func(subject, issuer string, since, validity time.Duration) *x509.Certificate {
		return &x509.Certificate{RawSubject: []byte(subject), RawIssuer: []byte(issuer),
			NotBefore: now.Add(-since), NotAfter: now.Add(-since + validity)}
	}
	side := func(role Role, ca, leaf *x509.Certificate) *Side {
		return &Side{Role: role, CA: ca, Certificate: tls.Certificate{Leaf: leaf}}
	}
	for _, tc := range []struct {
		name    string
		side    *Side
		soon    bool
		warning string
	}{
		{"bundle, new", side(Client, cert("ca", "ca", day, 10*year), cert("alice", "ca", day, 10*year)), false, ""},
		{"bundle, 2 years left", side(Client, cert("ca", "ca", 8*year, 10*year), cert("alice", "ca", 8*year, 10*year)), false, ""},
		{"bundle, half a year left", side(Server, cert("ca", "ca", 9*year+year/2, 10*year), cert("t", "ca", 9*year+year/2, 10*year)),
			true, "the bundle expires soon"},
		// warned of from its first day with a fixed year
		{"client of a year, new", side(Client, cert("sub", "root", year, 5*year), cert("alice", "sub", day, year)), false, ""},
		{"client of a year, 25 days left", side(Client, cert("sub", "root", year, 5*year), cert("alice", "sub", 340*day, year)),
			true, "the client certificate expires soon"},
		{"server of a year, 25 days left", side(Server, cert("sub", "root", year, 5*year), cert("t", "sub", 340*day, year)),
			true, "the server certificate expires soon"},
		{"sub CA expiring first", side(Client, cert("sub", "root", 2*year-30*day, 2*year), cert("alice", "sub", day, year)),
			true, "the sub CA expires soon"},
		{"bundle, expired", side(Server, cert("ca", "ca", 10*year+day, 10*year), cert("t", "ca", 10*year+day, 10*year)),
			true, "the bundle has expired"},
		{"server of a year, expired", side(Server, cert("sub", "root", year, 5*year), cert("t", "sub", year+day, year)),
			true, "the server certificate has expired: every client refuses ratd"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if soon := tc.side.ExpiresSoon(now); soon != tc.soon {
				t.Errorf("expected expiring soon %v, got %v", tc.soon, soon)
			}
			if warning := tc.side.ExpiryWarning(now); tc.soon && !strings.HasPrefix(warning, tc.warning) {
				t.Errorf("expected a warning starting with %q, got %q", tc.warning, warning)
			}
		})
	}
}
