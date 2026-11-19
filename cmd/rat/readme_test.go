package main

import (
	"os"
	"reflect"
	"strings"
	"testing"

	"github.com/google/jsonschema-go/jsonschema"
)

// TestReadmeListsTransferTools guards that the README of rat lists the tools rat adds with a
// transfers directory, each with every parameter, as it lists those of ratd (see the tools
// package): users want to know what rat adds to their harness before adding it. A parameter
// added without documenting it fails here.
func TestReadmeListsTransferTools(t *testing.T) {
	data, err := os.ReadFile("../../README.md")
	if err != nil {
		t.Fatal(err)
	}
	readme := string(data)
	for name, input := range map[string]reflect.Type{
		uploadTool: reflect.TypeFor[uploadInput](), downloadTool: reflect.TypeFor[downloadInput](),
	} {
		schema, err := jsonschema.ForType(input, &jsonschema.ForOptions{})
		if err != nil {
			t.Fatal(err)
		}
		row := ""
		for line := range strings.SplitSeq(readme, "\n") {
			if strings.HasPrefix(line, "| `"+name+"` |") {
				row = line
			}
		}
		if row == "" {
			t.Errorf("%s: no row in the tools of the README", name)
			continue
		}
		for parameter := range schema.Properties {
			if !strings.Contains(row, "`"+parameter+"`") {
				t.Errorf("%s: parameter %s missing from its row in the README", name, parameter)
			}
		}
	}
}
