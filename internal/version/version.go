// Package version tells the version of the binaries of rat, read from their build: the module
// version Go stamps (a tag, or a pseudo-version from the commit, "+dirty" for uncommitted
// changes), with the Go version and platform for humans.
package version

import (
	"fmt"
	"runtime"
	"runtime/debug"
)

// Module returns the version of the module the binary was built from: for what expects a plain
// version, such as the information an MCP server or client gives its peer.
func Module() string {
	if info, ok := debug.ReadBuildInfo(); ok {
		return info.Main.Version
	}
	return "unknown"
}

// String returns the version of the binary for humans (--version, logs): the module version, the
// Go version and the platform it was built for.
func String() string {
	info, ok := debug.ReadBuildInfo()
	if !ok {
		return fmt.Sprintf("unknown (%s/%s)", runtime.GOOS, runtime.GOARCH)
	}
	return fmt.Sprintf("%s (%s, %s/%s)", info.Main.Version, info.GoVersion, runtime.GOOS, runtime.GOARCH)
}
