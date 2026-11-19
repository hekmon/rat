package connect

import (
	"errors"
	"io/fs"
	"testing"
)

// TestPathEncoding guards that every path reaches ratd as sent: once encoded, a trailing space, a
// new line, a tab, non-ASCII characters and a percent sign travel in a header (printable ASCII,
// nothing a parser strips or refuses) and come back whole; slashes stay readable; and a value
// that is not an encoding is refused.
func TestPathEncoding(t *testing.T) {
	for _, path := range []string{
		"/home/agent/app.tgz", "~/my app.tgz", "/tmp/trailing ", "/tmp/new\nline", "/tmp/tab\tinside",
		"/tmp/été.txt", "/tmp/100%.txt",
	} {
		encoded := EncodePath(path)
		for _, c := range []byte(encoded) {
			if c <= ' ' || c >= 0x7f {
				t.Errorf("%q: encoded as %q, which a header does not carry as is", path, encoded)
				break
			}
		}
		if decoded, err := DecodePath(encoded); err != nil || decoded != path {
			t.Errorf("%q: encoded as %q, decoded as %q, %v", path, encoded, decoded, err)
		}
	}
	if encoded := EncodePath("/home/agent/app.tgz"); encoded != "/home/agent/app.tgz" {
		t.Errorf("expected a plain path kept as is, got %q", encoded)
	}
	if _, err := DecodePath("/x%zz"); !errors.Is(err, ErrHeader) {
		t.Errorf("expected ErrHeader, got %v", err)
	}
}

// TestMode guards the permission bits a transfer carries: four octal digits, without the setuid,
// setgid and sticky bits, which never travel; read with or without the leading zero, and refused
// beyond the permission bits or not octal.
func TestMode(t *testing.T) {
	if mode := FormatMode(fs.ModeSetuid | fs.ModeSticky | 0o754); mode != "0754" {
		t.Errorf("expected 0754, got %q", mode)
	}
	for value, expected := range map[string]fs.FileMode{"0644": 0o644, "644": 0o644, "0": 0, "0777": 0o777} {
		if mode, err := ParseMode(value); err != nil || mode != expected {
			t.Errorf("%q: expected %v, got %v, %v", value, expected, mode, err)
		}
	}
	for _, value := range []string{"", "rwx", "0800", "1777", "4755", "-1"} {
		if _, err := ParseMode(value); !errors.Is(err, ErrHeader) {
			t.Errorf("%q: expected ErrHeader, got %v", value, err)
		}
	}
}

// TestOverwrite guards the overwrite of an upload: true or false, false when absent, the safe
// side; anything else refused rather than guessed.
func TestOverwrite(t *testing.T) {
	for value, expected := range map[string]bool{"": false, "false": false, "true": true} {
		if overwrite, err := ParseOverwrite(value); err != nil || overwrite != expected {
			t.Errorf("%q: expected %v, got %v, %v", value, expected, overwrite, err)
		}
	}
	for _, value := range []string{"TRUE", "1", "yes", "t"} {
		if _, err := ParseOverwrite(value); !errors.Is(err, ErrHeader) {
			t.Errorf("%q: expected ErrHeader, got %v", value, err)
		}
	}
}

// TestHasFeature guards reading a list of features: a feature named alone or among others, the
// spaces around a name ignored, and never a name it is only a part of.
func TestHasFeature(t *testing.T) {
	for value, expected := range map[string]bool{
		"pipes": true, "resume, pipes": true, "pipes ,resume": true, "": false, "pipe": false, "pipes2": false,
		"resume": false,
	} {
		if got := HasFeature(value, FeaturePipes); got != expected {
			t.Errorf("%q: expected %v, got %v", value, expected, got)
		}
	}
}
