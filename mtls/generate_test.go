package mtls

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/hekmon/rat/tmux/names"
)

// newBundle generates a bundle for tenant t, with clients alice and bob, as if at the time
// generatedAt, and returns its directory.
func newBundle(t *testing.T, generatedAt time.Time) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "bundle")
	if _, err := generate(dir, "t", []string{"alice", "bob"}, generatedAt); err != nil {
		t.Fatal(err)
	}
	return dir
}

// TestGenerate guards the bundle written: missing parents created, one directory per side, keys
// and their directories readable by their owner only, each side loading with its role and names,
// one CA, no address in the certificates, and a validity of 10 years from a day before.
func TestGenerate(t *testing.T) {
	now := time.Now()
	dir := filepath.Join(t.TempDir(), "missing", "parents", "bundle")
	// a client named "server" does not collide with the server
	bundle, err := generate(dir, "prod", []string{"alice", "server"}, now)
	if err != nil {
		t.Fatal(err)
	}
	notBefore := now.Add(-clockSkew).Truncate(time.Second)
	if bundle.Tenant != "prod" || len(bundle.Clients) != 2 || !bundle.NotAfter.Equal(notBefore.AddDate(10, 0, 0)) {
		t.Errorf("unexpected bundle description: %+v", bundle)
	}
	for _, side := range []struct {
		dir  string
		role Role
		name string
	}{
		{"server", Server, "prod"},
		{filepath.Join("clients", "alice"), Client, "alice"},
		{filepath.Join("clients", "server"), Client, "server"},
	} {
		loaded, err := Load(filepath.Join(dir, side.dir))
		if err != nil {
			t.Errorf("%s: %v", side.dir, err)
			continue
		}
		if loaded.Role != side.role || loaded.Name != side.name || loaded.Tenant != "prod" {
			t.Errorf("%s: expected a %s named %s of tenant prod, got a %s named %s of tenant %s",
				side.dir, side.role, side.name, loaded.Role, loaded.Name, loaded.Tenant)
		}
		if Fingerprint(loaded.CA) != bundle.CAFingerprint || !loaded.NotAfter().Equal(bundle.NotAfter) {
			t.Errorf("%s: CA %s expiring %s, expected the CA of the bundle", side.dir, Fingerprint(loaded.CA), loaded.NotAfter())
		}
		leaf := loaded.Certificate.Leaf
		if !leaf.NotBefore.Equal(notBefore) || len(leaf.DNSNames) > 0 || len(leaf.IPAddresses) > 0 {
			t.Errorf("%s: expected a certificate valid from %s without address, got %s, %v, %v",
				side.dir, notBefore, leaf.NotBefore, leaf.DNSNames, leaf.IPAddresses)
		}
	}
	server, _ := Load(filepath.Join(dir, "server"))
	if ca := server.CA; !ca.IsCA || ca.MaxPathLen != 0 || !ca.MaxPathLenZero {
		t.Errorf("expected a CA signing no intermediate CA, got IsCA %v, MaxPathLen %d, MaxPathLenZero %v",
			ca.IsCA, ca.MaxPathLen, ca.MaxPathLenZero)
	}
	if runtime.GOOS == "windows" {
		return
	}
	for _, private := range []string{"", "server", "clients", "clients/alice", "server/server.key", "clients/alice/client.key"} {
		info, err := os.Stat(filepath.Join(dir, private))
		if err != nil {
			t.Fatal(err)
		}
		if info.Mode().Perm()&0o077 != 0 {
			t.Errorf("%s: readable by others (%04o)", private, info.Mode().Perm())
		}
	}
}

// TestGenerateRefuses guards that generating never replaces anything: an existing directory, even
// empty, is refused and left as is, and invalid names or clients create nothing.
func TestGenerateRefuses(t *testing.T) {
	parent := t.TempDir()
	existing := filepath.Join(parent, "existing")
	if err := os.MkdirAll(filepath.Join(existing, "server"), 0o700); err != nil {
		t.Fatal(err)
	}
	empty := filepath.Join(parent, "empty")
	if err := os.Mkdir(empty, 0o700); err != nil {
		t.Fatal(err)
	}
	for _, dir := range []string{existing, empty} {
		if _, err := Generate(dir, "t", []string{"a"}); !errors.Is(err, fs.ErrExist) {
			t.Errorf("%s: expected fs.ErrExist, got %v", dir, err)
		}
	}
	if entries, err := os.ReadDir(filepath.Join(existing, "server")); err != nil || len(entries) > 0 {
		t.Errorf("existing directory changed: %v, %v", entries, err)
	}
	if entries, err := os.ReadDir(empty); err != nil || len(entries) > 0 {
		t.Errorf("empty directory changed: %v, %v", entries, err)
	}
	for _, input := range []struct {
		tenant   string
		clients  []string
		expected error
	}{
		{"a:b", []string{"a"}, names.ErrInvalid},
		{"", []string{"a"}, names.ErrInvalid},
		{"t", []string{"a b"}, names.ErrInvalid},
		{"t", nil, nil},
		{"t", []string{"a", "b", "a"}, nil},
	} {
		dir := filepath.Join(parent, "new")
		_, err := Generate(dir, input.tenant, input.clients)
		if err == nil || (input.expected != nil && !errors.Is(err, input.expected)) {
			t.Errorf("tenant %q, clients %q: expected an error wrapping %v, got %v", input.tenant, input.clients, input.expected, err)
		}
		if _, statErr := os.Stat(dir); !errors.Is(statErr, fs.ErrNotExist) {
			t.Errorf("tenant %q, clients %q: the bundle directory was created", input.tenant, input.clients)
		}
	}
}
