// Package names holds the naming rule of rat: tenants, sessions and windows are plain names, made
// of letters, digits, '_' and '-', up to 32 characters. The rule comes from tmux, where the names
// end up:
//
//   - a tenant names a tmux socket (rat-<tenant>), where '/' or ".." would move the socket out of
//     the private directory of tmux, and whose path is bounded (104 bytes on macOS, 108 on Linux);
//   - session and window names end up in tmux targets, where ':' and '.' separate the session,
//     window and pane parts, and a leading '=' asks for an exact match: any of them in a name
//     would change what it targets. A window name is not made of digits only either
//     (CheckWindow): tmux reads the window part of a target as an index first, even after '='.
//
// It is a package of its own so that packages naming tenants and sessions without driving tmux
// apply the same rule without depending on the controller: package mtls, whose certificates
// carry these names, and whose clients (the rat bridge) run where tmux is not needed. The tmux
// pitfalls behind the rule are described in the README of package tmux.
package names
