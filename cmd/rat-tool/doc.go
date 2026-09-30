// Rat-tool is the utility of rat: it holds every secondary feature, so that ratd and rat keep a
// single mode each. The design decisions, and the alternatives they rejected, are described in
// the README of this command.
//
// # Bundles
//
// It generates the mTLS bundle of a tenant (package mtls): a CA, a server certificate naming the
// tenant, a client certificate per named client, each naming its session. It inspects bundle
// directories offline, with the checks ratd and rat run at startup, and tells whether several
// directories belong to the same bundle.
//
// # Check
//
// It checks a running ratd from a client directory, step by step, as a harness would reach it:
// files, network, TLS, MCP initialization, tools. It calls no tool, leaving nothing behind on the
// server.
package main
