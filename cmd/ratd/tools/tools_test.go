package tools

import (
	"encoding/json"
	"testing"
	"time"
)

// TestWait guards how long rat expects a call of wait_window to wait, from its arguments: the
// seconds asked, the default when missing or not a number, never more than the maximum.
func TestWait(t *testing.T) {
	for arguments, expected := range map[string]time.Duration{
		`{"window":"main"}`:                    DefaultWaitSeconds * time.Second,
		`{"window":"main","max_seconds":5}`:    5 * time.Second,
		`{"window":"main","max_seconds":1.5}`:  1500 * time.Millisecond,
		`{"window":"main","max_seconds":600}`:  MaxWaitSeconds * time.Second,
		`{"window":"main","max_seconds":-3}`:   0,
		`{"window":"main","max_seconds":"x"}`:  DefaultWaitSeconds * time.Second,
		`not json`:                             DefaultWaitSeconds * time.Second,
		`{"window":"main","max_seconds":null}`: DefaultWaitSeconds * time.Second,
	} {
		if got := Wait(json.RawMessage(arguments)); got != expected {
			t.Errorf("%s: expected %s, got %s", arguments, expected, got)
		}
	}
}

// TestWaitSchema guards the bounds of max_seconds in the schema of wait_window, which the SDK
// enforces and agents read.
func TestWaitSchema(t *testing.T) {
	schemas, err := Schemas()
	if err != nil {
		t.Fatal(err)
	}
	property := schemas[WaitWindow].Properties["max_seconds"]
	if property.Minimum == nil || *property.Minimum != 1 || property.Maximum == nil || *property.Maximum != MaxWaitSeconds ||
		string(property.Default) != "20" {
		t.Errorf("unexpected schema of max_seconds: %+v", property)
	}
}
