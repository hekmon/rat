// Package tools holds the inputs of the tools of ratd: their names, types and JSON schemas. ratd
// serves these schemas, and rat reads them to build the requests it relays: from protocol
// 2026-07-28 on, an argument whose schema carries an x-mcp-header annotation travels in an HTTP
// header as well (SEP-2243), which [ParamHeaders] computes. A rat and a ratd of the same version
// thus agree on the tools, without rat learning them from ratd (tools/list): rat only needs the
// schemas, not what the tools do, which stays in ratd. The code of rat does not change with the
// tools, but a rat built with an older ratd does not know the annotations of newer tools.
//
// The schemas are inferred from the types, as the MCP SDK infers them ([Schemas]), with the
// annotations added by hand: the jsonschema struct tag only carries descriptions. No tool
// annotates an argument yet. Two limits to know before annotating one, both of the Go SDK
// v1.8.0:
//
//   - an invalid annotation (not on a string, an integer nor a boolean, a header name out of the
//     HTTP token characters, a duplicate) makes the SDK client drop the tool from tools/list,
//     which the tests of ratd listing its tools catch;
//   - an annotated string argument must not be empty: the SDK client then sends an empty header,
//     which its server reads as missing, refusing the call.
package tools
