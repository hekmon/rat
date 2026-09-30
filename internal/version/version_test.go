package version

import (
	"runtime"
	"strings"
	"testing"
)

// TestString guards the version for humans: the module version, then the Go version and the
// platform.
func TestString(t *testing.T) {
	if s := String(); !strings.HasPrefix(s, Module()+" (go") || !strings.HasSuffix(s, runtime.GOOS+"/"+runtime.GOARCH+")") {
		t.Errorf("expected the module version, the Go version and the platform, got %q", s)
	}
}
