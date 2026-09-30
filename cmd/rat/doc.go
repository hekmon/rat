// Rat is the stdio bridge of rat: a stdio MCP server, started by a harness, relaying every
// message to ratd over HTTP with mutual TLS. It serves the harnesses that can not present a client
// certificate when they configure an HTTP MCP server, which is most of them, while all of them
// can start a stdio one. The design decisions, and the alternatives they rejected, are described
// in the README of this command.
//
// # Relay
//
// Rat relays JSON-RPC messages as they are, in both directions, presenting the certificate of the
// client directory it is given, which names its session. It reads inside a single message, the
// result of initialize, for the protocol version to send along. It keeps no other state and holds
// no terminal: persistence stays in ratd, and a ratd restart only fails the requests sent
// meanwhile.
//
// # Errors
//
// Tool and protocol errors come from ratd and are relayed untouched. When ratd can not be reached
// (down, TLS refused, an HTTP error), rat answers the pending request with a JSON-RPC error of its
// own, so that the harness does not wait forever, and logs the details on its stderr. Its own
// failures at startup (flags, bundle) are written on stderr before it exits.
package main
