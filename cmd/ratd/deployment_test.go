package main

import (
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/hekmon/rat/mtls"
)

// TestWarnDeploymentRoot guards that ratd run as root warns of it, and checks nothing else: root
// can write any bundle, which the status tells as well.
func TestWarnDeploymentRoot(t *testing.T) {
	logs := &logBuffer{}
	found := warnDeployment(slog.New(slog.NewTextHandler(logs, nil)), writableServerDir(t), 0)
	if !slices.Equal(found, []degradation{runningAsRoot}) {
		t.Errorf("expected running as root for the status, got %v", found)
	}
	out := logs.String()
	if !strings.Contains(out, "level=WARN") || !strings.Contains(out, "ratd runs as root") ||
		strings.Contains(out, "replace the bundle") {
		t.Errorf("expected the root warning alone:\n%s", out)
	}
}

// TestWarnDeploymentWritable guards that ratd warns of a bundle its user can write, naming the
// paths, and that a bundle out of its reach is not warned of, nor told in the status.
func TestWarnDeploymentWritable(t *testing.T) {
	requireNotRoot(t)
	dir := writableServerDir(t)
	logs := &logBuffer{}
	if found := warnDeployment(slog.New(slog.NewTextHandler(logs, nil)), dir, os.Geteuid()); !slices.Equal(found, []degradation{bundleWritable}) {
		t.Errorf("expected bundle writable for the status, got %v", found)
	}
	if out := logs.String(); !strings.Contains(out, "level=WARN") || !strings.Contains(out, "replace the bundle") ||
		!strings.Contains(out, filepath.Join(dir, "ca.crt")) {
		t.Errorf("expected a warning naming the writable paths:\n%s", out)
	}
	logs = &logBuffer{}
	if found := warnDeployment(slog.New(slog.NewTextHandler(logs, nil)), "/", os.Geteuid()); found != nil {
		t.Errorf("expected nothing for the status, got %v", found)
	}
	if out := logs.String(); out != "" {
		t.Errorf("expected no warning for a bundle out of reach:\n%s", out)
	}
}

// TestWritableBundle guards the paths through which the user can replace a bundle: the
// certificates, their directory and the directories above it, the target of a symbolic link and
// the directories above it as well, but never the key, which ratd owns.
func TestWritableBundle(t *testing.T) {
	requireNotRoot(t)
	root := t.TempDir()
	dir := filepath.Join(root, "a", "b", "server")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	files := append(mtls.CertificateFiles(dir, mtls.Server), filepath.Join(dir, "server.key"))
	for _, file := range files {
		if err := os.WriteFile(file, nil, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	got := writableBundle(dir)
	for _, want := range append(mtls.CertificateFiles(dir, mtls.Server), dir, filepath.Join(root, "a", "b"), root) {
		if !slices.Contains(got, want) {
			t.Errorf("expected %s among the writable paths, got %v", want, got)
		}
	}
	if slices.Contains(got, files[2]) {
		t.Errorf("expected the key left out, got %v", got)
	}

	// the certificates and their directory out of reach: only the directories above are left
	for _, file := range files[:2] {
		readOnly(t, file, 0o444)
	}
	readOnly(t, dir, 0o555)
	got = writableBundle(dir)
	for _, path := range append(mtls.CertificateFiles(dir, mtls.Server), dir) {
		if slices.Contains(got, path) {
			t.Errorf("expected %s out of reach, got %v", path, got)
		}
	}
	if !slices.Contains(got, filepath.Join(root, "a", "b")) {
		t.Errorf("expected the directory above the bundle among the writable paths, got %v", got)
	}

	// through a link, in a directory out of reach: the directories above the target count
	links := filepath.Join(root, "links")
	if err := os.Mkdir(links, 0o755); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(links, "server")
	if err := os.Symlink(dir, link); err != nil {
		t.Fatal(err)
	}
	readOnly(t, links, 0o555)
	got = writableBundle(link)
	if !slices.Contains(got, filepath.Join(root, "a", "b")) || !slices.Contains(got, filepath.Join(root, "a")) {
		t.Errorf("expected the directories above the target of the link among the writable paths, got %v", got)
	}
	if slices.Contains(got, links) {
		t.Errorf("expected the directory of the link out of reach, got %v", got)
	}
}

// writableServerDir returns a server directory holding the certificates, writable by the user.
func writableServerDir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	for _, file := range mtls.CertificateFiles(dir, mtls.Server) {
		if err := os.WriteFile(file, nil, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

// readOnly sets the mode of path, and restores write access for its owner once the test ends, for
// the temporary directory to be removed.
func readOnly(t *testing.T, path string, mode os.FileMode) {
	t.Helper()
	if err := os.Chmod(path, mode); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(path, mode|0o200) })
}

// requireNotRoot skips a test of what the user can write when run as root, who can write anything.
func requireNotRoot(t *testing.T) {
	t.Helper()
	if os.Geteuid() == 0 {
		t.Skip("run as root, who can write anything")
	}
}
