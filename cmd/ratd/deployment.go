package main

import (
	"log/slog"
	"path/filepath"
	"slices"
	"strings"

	"github.com/hekmon/rat/mtls"

	"golang.org/x/sys/unix"
)

// warnDeployment warns, once at startup, of a deployment handing agents more than the user ratd
// runs as, euid: root, or a bundle that user can replace (see writableBundle). Warnings rather than
// refusals: root is a choice for a machine given whole to the agents, and ratd started by hand, as
// one tries it, runs as a user owning its bundle. Running as root, the bundle is not checked:
// root can write anything, and is the warning already.
func warnDeployment(logger *slog.Logger, bundle string, euid int) {
	if euid == 0 {
		logger.Warn("ratd runs as root, and the terminals with it: every agent, and whoever holds a client key, " +
			"is root. Run it as a user of its own (User= in its service), unless the machine is meant to be the agents'")
		return
	}
	if writable := writableBundle(bundle); len(writable) > 0 {
		logger.Warn("the user ratd runs as, the agents' as well, can replace the bundle: one of their own would let "+
			"its clients in at the next start of ratd. Make these root's, the key aside",
			"writable", strings.Join(writable, " "))
	}
}

// writableBundle returns, sorted, the paths through which the user ratd runs as can replace the
// server side of the bundle in dir: its certificates, and the directories holding them up to the
// root, any of which lets a file or a directory below be renamed and another put in its place. The
// key is left out, ratd having to own it (see the README of mtls). Symbolic links are followed as
// well: both the directories of a path as given, which hold the links, and those of its target.
// Writable means writable for the process (access(2)), groups and ACLs included. Directories
// sticky to others (/tmp) count as writable: a bundle there is a mistake anyway, and telling them
// apart needs owners compared. A path that can not be resolved is checked as given.
func writableBundle(dir string) []string {
	var paths []string
	for _, file := range mtls.CertificateFiles(dir, mtls.Server) {
		paths = append(paths, chain(file)...)
		if resolved, err := filepath.EvalSymlinks(file); err == nil {
			paths = append(paths, chain(resolved)...)
		}
	}
	var writable []string
	for _, path := range paths {
		if unix.Access(path, unix.W_OK) == nil {
			writable = append(writable, path)
		}
	}
	slices.Sort(writable)
	return slices.Compact(writable)
}

// chain returns path, absolute, then every directory above it up to the root.
func chain(path string) []string {
	if abs, err := filepath.Abs(path); err == nil {
		path = abs
	}
	paths := []string{path}
	for parent := filepath.Dir(path); parent != path; path, parent = parent, filepath.Dir(parent) {
		paths = append(paths, parent)
	}
	return paths
}
