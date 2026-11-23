package connect

import (
	"errors"
	"io/fs"
	"strings"
	"testing"
)

// TestNamesDirectory guards the form of a destination naming a directory, which both sides
// refuse: ending with a slash, or with . or .. as its last part, and not a name holding dots.
func TestNamesDirectory(t *testing.T) {
	for path, names := range map[string]bool{
		"~/backups/": true, "/a/b/": true, "/": true, "/a/.": true, "/a/..": true, ".": true, "..": true,
		"/a/b": false, "~/a.b": false, "/a/b..": false, "/a/..b": false, "~/.config": false,
	} {
		if got := NamesDirectory(path); got != names {
			t.Errorf("%q: expected %v, got %v", path, names, got)
		}
	}
}

// TestErrorEncoding guards the sentence of a Rat-Error trailer: control characters and the
// percent sign percent-encoded, which a header can not carry or would take for an escape, the rest
// kept readable, UTF-8 and bytes that are not UTF-8 included; decoded back as it was; and a value
// that is not percent-encoded read as is.
func TestErrorEncoding(t *testing.T) {
	for text, encoded := range map[string]string{
		"/tmp/a.log changed while it was read.": "/tmp/a.log changed while it was read.",
		"/tmp/esc\x1b.log changed.":             "/tmp/esc%1B.log changed.",
		"/tmp/new\nline\tand\x7f.log":           "/tmp/new%0Aline%09and%7F.log",
		"/tmp/100% é \xff.log":                  "/tmp/100%25 é \xff.log",
	} {
		if got := EncodeError(text); got != encoded {
			t.Errorf("%q: expected it encoded %q, got %q", text, encoded, got)
		}
		if got := DecodeError(encoded); got != text {
			t.Errorf("%q: expected it decoded %q, got %q", encoded, text, got)
		}
	}
	if got := DecodeError("100% done"); got != "100% done" {
		t.Errorf("expected a value not encoded read as is, got %q", got)
	}
}

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

// TestUploadFlags guards the flags of an upload, its overwrite and in place: true or false, false
// when absent, the safe side; anything else refused rather than guessed, naming the header.
func TestUploadFlags(t *testing.T) {
	for header, parse := range map[string]func(string) (bool, error){HeaderOverwrite: ParseOverwrite,
		HeaderInPlace: ParseInPlace} {
		for value, expected := range map[string]bool{"": false, "false": false, "true": true} {
			if flag, err := parse(value); err != nil || flag != expected {
				t.Errorf("%s %q: expected %v, got %v, %v", header, value, expected, flag, err)
			}
		}
		for _, value := range []string{"TRUE", "1", "yes", "t"} {
			if _, err := parse(value); !errors.Is(err, ErrHeader) || !strings.Contains(err.Error(), header) {
				t.Errorf("%s %q: expected ErrHeader naming it, got %v", header, value, err)
			}
		}
	}
}
