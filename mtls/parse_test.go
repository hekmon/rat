package mtls

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// readPart returns the content of the file name of the side at dir.
func readPart(t *testing.T, dir, name string) []byte {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(dir, name))
	if err != nil {
		t.Fatal(err)
	}
	return data
}

// TestParse guards that credentials given as contents make the same side as the directory they
// come from, whatever the permissions of the key file they were read from: its holder's concern.
func TestParse(t *testing.T) {
	now := time.Now()
	alice := filepath.Join(newBundle(t, now), "clients", "alice")
	if err := os.Chmod(filepath.Join(alice, "client.key"), 0o644); err != nil {
		t.Fatal(err)
	}
	side, err := parse(Client, readPart(t, alice, "ca.crt"), readPart(t, alice, "client.crt"),
		readPart(t, alice, "client.key"), now)
	if err != nil {
		t.Fatal(err)
	}
	if err = os.Chmod(filepath.Join(alice, "client.key"), 0o600); err != nil {
		t.Fatal(err)
	}
	loaded, err := load(alice, now)
	if err != nil {
		t.Fatal(err)
	}
	if side.Role != loaded.Role || side.Name != loaded.Name || side.Tenant != loaded.Tenant ||
		!side.CA.Equal(loaded.CA) || !side.Certificate.Leaf.Equal(loaded.Certificate.Leaf) {
		t.Errorf("expected the side loaded from the directory, got %+v", side)
	}
}

// TestParseChecks guards the checks of Parse, those of Load but for files: contents that are not
// a single PEM block of their kind, a key not matching, a CA of another bundle, another role, an
// expired certificate, errors naming the parts by their kind.
func TestParseChecks(t *testing.T) {
	now := time.Now()
	bundle := newBundle(t, now)
	alice := filepath.Join(bundle, "clients", "alice")
	otherAlice := filepath.Join(newBundle(t, now), "clients", "alice")
	ca, cert, key := readPart(t, alice, "ca.crt"), readPart(t, alice, "client.crt"), readPart(t, alice, "client.key")
	serverCert := readPart(t, filepath.Join(bundle, "server"), "server.crt")
	serverKey := readPart(t, filepath.Join(bundle, "server"), "server.key")
	for _, tc := range []struct {
		name          string
		role          Role
		ca, cert, key []byte
		age           time.Duration
		expected      error
		message       string
	}{
		{"no CA", Client, nil, cert, key, 0, ErrFiles, "the CA certificate does not hold a PEM CERTIFICATE"},
		{"two CAs", Client, append(bytes.Clone(ca), readPart(t, otherAlice, "ca.crt")...), cert, key, 0, ErrFiles,
			"the CA certificate holds more than a PEM CERTIFICATE"},
		{"not a key", Client, ca, cert, cert, 0, ErrFiles, "the key does not hold a PEM PRIVATE KEY"},
		{"key of another client", Client, ca, cert, readPart(t, filepath.Join(bundle, "clients", "bob"), "client.key"), 0,
			ErrKeyMismatch, "the key does not match the certificate"},
		{"CA of another bundle", Client, readPart(t, otherAlice, "ca.crt"), cert, key, 0, ErrNotSignedByCA,
			"the certificate is not signed by the CA certificate"},
		{"server certificate as a client", Client, ca, serverCert, serverKey, 0, ErrRole,
			"the certificate is not a client certificate only"},
		{"client certificate as a server", Server, ca, cert, key, 0, ErrRole, "the certificate is not a server certificate only"},
		{"unknown role", Role(0), ca, cert, key, 0, ErrRole, "unknown role 0"},
		{"expired", Client, ca, cert, key, -11 * 365 * 24 * time.Hour, ErrValidity, "the certificate expired on"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := parse(tc.role, tc.ca, tc.cert, tc.key, now.Add(-tc.age))
			if !errors.Is(err, tc.expected) || !strings.Contains(err.Error(), tc.message) {
				t.Errorf("expected an error wrapping %v, telling %q, got %v", tc.expected, tc.message, err)
			}
		})
	}
}
