package main

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/urfave/cli/v3"
)

// run runs rat-tool with args, and returns what it printed and its error.
func run(t *testing.T, args ...string) (string, error) {
	t.Helper()
	cmd := command()
	var out bytes.Buffer
	cmd.Writer, cmd.ErrWriter = &out, &out
	// never exit the test process, whatever the error
	cmd.ExitErrHandler = func(context.Context, *cli.Command, error) {}
	err := cmd.Run(context.Background(), append([]string{"rat-tool"}, args...))
	return out.String(), err
}

// newBundle generates the bundle of tenant prod, with clients alice and bob, and returns its
// directory.
func newBundle(t *testing.T) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "prod")
	if out, err := run(t, "bundle", "generate", "-t", "prod", "-c", "alice", "--client", "bob", "-o", dir); err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	return dir
}

// TestBundle guards generating a bundle, then inspecting its directories: every check passes, and
// they belong to the same bundle.
func TestBundle(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "prod")
	out, err := run(t, "bundle", "generate", "--tenant", "prod", "-c", "alice", "-c", "bob", "--output", dir)
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	for _, expected := range []string{"tenant prod", "client alice", "client bob", "CA fingerprint", "Valid until"} {
		if !strings.Contains(out, expected) {
			t.Errorf("generate does not tell %q:\n%s", expected, out)
		}
	}
	out, err = run(t, "bundle", "inspect", filepath.Join(dir, "server"), filepath.Join(dir, "clients", "alice"),
		filepath.Join(dir, "clients", "bob"))
	if err != nil || strings.Contains(out, "FAIL") || !strings.Contains(out, "These 3 directories belong to the same bundle") {
		t.Errorf("expected every check to pass, in one bundle, got %v:\n%s", err, out)
	}
	if !strings.Contains(out, "ratd, serving tenant prod") || !strings.Contains(out, "client alice of tenant prod") {
		t.Errorf("inspect does not tell what each directory is for:\n%s", out)
	}
}

// TestBundleRefuses guards the failures of the bundle commands: generating over an existing
// directory or without a client, and inspecting a directory failing a check, a missing one, none,
// or directories of different bundles, which all make rat-tool fail.
func TestBundleRefuses(t *testing.T) {
	dir := newBundle(t)
	if out, err := run(t, "bundle", "generate", "-t", "prod", "-c", "alice", "-o", dir); err == nil ||
		!strings.Contains(err.Error(), "never written over an existing directory") {
		t.Errorf("generating over an existing bundle: expected an error saying so, got %v:\n%s", err, out)
	}
	if out, err := run(t, "bundle", "generate", "-t", "prod", "-o", filepath.Join(t.TempDir(), "new")); err == nil {
		t.Errorf("generating without a client: expected an error, got:\n%s", out)
	}
	if out, err := run(t, "bundle", "inspect"); err == nil {
		t.Errorf("inspecting nothing: expected an error, got:\n%s", out)
	}
	missing := filepath.Join(t.TempDir(), "missing")
	if out, err := run(t, "bundle", "inspect", missing); err == nil || !strings.Contains(out, "FAIL  files of a side") {
		t.Errorf("inspecting a missing directory: expected a failure of the files check, got %v:\n%s", err, out)
	}
	if runtime.GOOS != "windows" {
		alice := filepath.Join(dir, "clients", "alice")
		if err := os.Chmod(filepath.Join(alice, "client.key"), 0o644); err != nil {
			t.Fatal(err)
		}
		out, err := run(t, "bundle", "inspect", alice)
		if err == nil || !strings.Contains(out, "ok    files of a side") || !strings.Contains(out, "FAIL  key readable by its owner only") {
			t.Errorf("inspecting a key readable by others: expected the files check to pass, then the key one to fail, got %v:\n%s", err, out)
		}
	}
	other := newBundle(t)
	out, err := run(t, "bundle", "inspect", filepath.Join(dir, "server"), filepath.Join(other, "clients", "bob"))
	if err == nil || !strings.Contains(out, "belong to 2 different bundles") {
		t.Errorf("inspecting directories of different bundles: expected an error, got %v:\n%s", err, out)
	}
}
