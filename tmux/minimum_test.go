package tmux

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// minimumTmuxDistribution is the Debian release TestMinimumTmux runs the tests on: its tmux,
// 3.3a, is the oldest rat supports. tmux versions differ in ways that matter to rat (3.3 to 3.6
// crash with a global window-size manual), and a developer machine usually has a recent one.
const minimumTmuxDistribution = "bookworm"

// inContainerEnv is set in the container TestMinimumTmux starts, where the tests must not start
// another one.
const inContainerEnv = "RAT_TEST_IN_CONTAINER"

// TestMinimumTmux guards that rat works with the oldest tmux it supports, on Linux, whatever the
// machine running the tests: it runs the package tests in a Docker container with that tmux.
// Docker is required: -short skips it, as an explicit choice to test less.
//
// It does not cover the oldest bash (bashMinVersion, 4.4): no image has Go, tmux 3.3 or later
// and bash 4.4, whose distributions shipped an older tmux. StartServer refuses older ones, and
// bracketed paste is enforced the same way from 4.4 on (checked in the bash:4.4 and bash:5.0
// images when it was introduced).
func TestMinimumTmux(t *testing.T) {
	if os.Getenv(inContainerEnv) != "" {
		t.Skip("already running in the minimum tmux container")
	}
	if testing.Short() {
		t.Skip("-short: not testing the minimum tmux version")
	}
	if _, err := exec.LookPath("docker"); err != nil {
		t.Fatal("docker is required to test the minimum tmux version (-short skips it):", err)
	}
	// the tests run from the package directory: the module is its parent
	module, err := filepath.Abs("..")
	if err != nil {
		t.Fatal(err)
	}
	// the same Go as the one running the tests: go1.27.1 runs in golang:1.27.1-<distribution>
	image := "golang:" + strings.TrimPrefix(runtime.Version(), "go") + "-" + minimumTmuxDistribution
	// The module is mounted read-only and copied: the tests must not write into it.
	// The packages installed are what the tests need beyond Go, and this test is responsible for
	// them: removing one breaks the tests relying on it, in the container only. tmux, and the
	// real less for TestCaptureFullScreen (the image has none, BusyBox's would not do).
	script := `set -e
apt-get update -qq >/dev/null
DEBIAN_FRONTEND=noninteractive apt-get install -y -qq tmux less >/dev/null
tmux -V
cp -R /src /tmp/src
cd /tmp/src
go test -count=1 ./tmux`
	cmd := exec.CommandContext(t.Context(), "docker", "run", "--rm", "-e", inContainerEnv+"=1",
		"-v", module+":/src:ro", image, "sh", "-c", script)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("tests with the minimum tmux (%s) failed: %v\n%s", image, err, out)
	}
	t.Logf("%s:\n%s", image, out)
}
