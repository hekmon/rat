package tools

import (
	"encoding/base64"
	"encoding/json"
	"math"
	"strconv"
	"strings"

	"github.com/google/jsonschema-go/jsonschema"
)

const (
	// HeaderAnnotation is the schema keyword binding an argument to an HTTP header (SEP-2243).
	HeaderAnnotation = "x-mcp-header"
	// ParamHeaderPrefix starts the name of the header of an annotated argument.
	ParamHeaderPrefix = "Mcp-Param-"
)

// ParamHeaders returns the headers carrying the annotated arguments of a tool call (SEP-2243), by
// name: an argument of the arguments given, whose property in schema carries an x-mcp-header
// annotation. Missing or null arguments have none, as have values that are not a string, a
// boolean or an integer JavaScript represents exactly. The values are encoded as the MCP SDK
// checks them: an integer in decimal, a boolean as true or false, a string as is when made of
// printable ASCII without spaces around, otherwise in base64 between =?base64? and ?=.
func ParamHeaders(schema *jsonschema.Schema, arguments json.RawMessage) map[string]string {
	var args map[string]json.RawMessage
	if schema == nil || json.Unmarshal(arguments, &args) != nil {
		return nil
	}
	headers := map[string]string{}
	collectHeaders(schema.Properties, args, headers)
	if len(headers) == 0 {
		return nil
	}
	return headers
}

// collectHeaders adds to headers those of the annotated properties found in args, at any depth.
func collectHeaders(properties map[string]*jsonschema.Schema, args map[string]json.RawMessage, headers map[string]string) {
	for name, property := range properties {
		raw, ok := args[name]
		if property == nil || !ok || string(raw) == "null" {
			continue
		}
		if header, ok := property.Extra[HeaderAnnotation].(string); ok && header != "" {
			if value, ok := headerValue(raw); ok {
				headers[ParamHeaderPrefix+header] = value
			}
		}
		if len(property.Properties) > 0 {
			var nested map[string]json.RawMessage
			if json.Unmarshal(raw, &nested) == nil {
				collectHeaders(property.Properties, nested, headers)
			}
		}
	}
}

// maxExactInteger is the largest integer a double represents exactly, as JavaScript numbers are.
const maxExactInteger = 1<<53 - 1

// base64Prefix and base64Suffix wrap a value encoded in base64.
const (
	base64Prefix = "=?base64?"
	base64Suffix = "?="
)

// headerValue returns the header value of the JSON value raw, if it can be carried in a header.
func headerValue(raw json.RawMessage) (string, bool) {
	var value any
	if json.Unmarshal(raw, &value) != nil {
		return "", false
	}
	switch v := value.(type) {
	case bool:
		return strconv.FormatBool(v), true
	case float64:
		if math.IsNaN(v) || math.IsInf(v, 0) || v != math.Trunc(v) || math.Abs(v) > maxExactInteger {
			return "", false
		}
		return strconv.FormatInt(int64(v), 10), true
	case string:
		if needsBase64(v) {
			return base64Prefix + base64.StdEncoding.EncodeToString([]byte(v)) + base64Suffix, true
		}
		return v, true
	default:
		return "", false
	}
}

// needsBase64 tells whether a string can not travel as is in a header: spaces or tabs around it,
// a character out of printable ASCII, or the form of a value already encoded.
func needsBase64(s string) bool {
	if s == "" {
		return false
	}
	if strings.ContainsAny(s[:1], " \t") || strings.ContainsAny(s[len(s)-1:], " \t") {
		return true
	}
	for _, c := range s {
		if c < 0x20 || c > 0x7e {
			return true
		}
	}
	return strings.HasPrefix(s, base64Prefix) && strings.HasSuffix(s, base64Suffix)
}
