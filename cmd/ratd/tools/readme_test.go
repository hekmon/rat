package tools

import (
	"os"
	"slices"
	"strings"
	"testing"
)

// TestReadmeListsTools guards that the README of rat lists every tool and every parameter an MCP
// harness gets from ratd: users want to know what a server adds to their harness before adding
// it. A tool or a parameter added without documenting it fails here.
func TestReadmeListsTools(t *testing.T) {
	data, err := os.ReadFile("../../../README.md")
	if err != nil {
		t.Fatal(err)
	}
	readme := string(data)
	schemas, err := Schemas()
	if err != nil {
		t.Fatal(err)
	}
	for name, schema := range schemas {
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
		parameters := slices.Sorted(func(yield func(string) bool) {
			for parameter := range schema.Properties {
				if !yield(parameter) {
					return
				}
			}
		})
		for _, parameter := range parameters {
			if !strings.Contains(row, "`"+parameter+"`") {
				t.Errorf("%s: parameter %s missing from its row in the README", name, parameter)
			}
		}
	}
}
