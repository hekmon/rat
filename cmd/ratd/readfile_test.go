package main

import (
	"fmt"
	"io"
	"os"
	"os/user"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/hekmon/rat/cmd/ratd/tools"
)

// numberedLines returns the lines "line 1" to "line count", each ending with a new line.
func numberedLines(count int) string {
	var lines strings.Builder
	for i := 1; i <= count; i++ {
		fmt.Fprintf(&lines, "line %d\n", i)
	}
	return lines.String()
}

// writeTestFile writes content to name in dir, and returns its path.
func writeTestFile(t *testing.T, dir, name, content string) string {
	t.Helper()
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// TestReadFile guards reading a whole file and ranges: the exact content after a header telling
// the lines, the size and the modification; ranges from the start or from the end (clamped to the
// first line), numbered on demand; a start past the end refused; and the log line.
func TestReadFile(t *testing.T) {
	session, logs, home := connectFiles(t)
	small := writeTestFile(t, home, "small.txt", "one\ntwo\nno end")
	expectTool(t, session, "read_file", map[string]any{"path": "~/small.txt"}, false,
		"["+small+": 3 lines, 14 B, modified 0s ago]\none\ntwo\nno end")
	expectTool(t, session, "read_file", map[string]any{"path": small, "line_numbers": true}, false,
		"]\n     1\tone\n     2\ttwo\n     3\tno end")
	expectTool(t, session, "read_file", map[string]any{"path": small, "start_line": 2, "max_lines": 1}, false,
		"["+small+": line 2 of 3, 14 B, modified 0s ago]\ntwo\n")
	empty := writeTestFile(t, home, "empty.txt", "")
	expectTool(t, session, "read_file", map[string]any{"path": empty}, false, "["+empty+": empty, modified 0s ago]")
	expectTool(t, session, "read_file", map[string]any{"path": empty, "start_line": -3}, true,
		empty+" has 0 lines: start_line -3 is past its end.")

	hundred := writeTestFile(t, home, "hundred.txt", numberedLines(100))
	for _, tc := range []struct {
		args     map[string]any
		expected string
	}{
		{map[string]any{"start_line": 10, "max_lines": 3}, "lines 10 to 12 of 100, 792 B, modified 0s ago]\nline 10\nline 11\nline 12\n"},
		{map[string]any{"max_lines": 2}, "lines 1 to 2 of 100, 792 B, modified 0s ago]\nline 1\nline 2\n"},
		{map[string]any{"start_line": 100}, "line 100 of 100, 792 B, modified 0s ago]\nline 100\n"},
		{map[string]any{"start_line": -3}, "lines 98 to 100 of 100, 792 B, modified 0s ago]\nline 98\nline 99\nline 100\n"},
		{map[string]any{"start_line": -3, "max_lines": 1, "line_numbers": true}, "line 98 of 100, 792 B, modified 0s ago]\n    98\tline 98\n"},
		{map[string]any{"start_line": -500, "max_lines": 1}, "line 1 of 100, 792 B, modified 0s ago]\nline 1\n"},
	} {
		tc.args["path"] = hundred
		expectTool(t, session, "read_file", tc.args, false, "["+hundred+": "+tc.expected)
	}
	expectTool(t, session, "read_file", map[string]any{"path": hundred, "start_line": 101}, true,
		hundred+" has 100 lines: start_line 101 is past its end.")
	if out := logs.String(); !strings.Contains(out, "tool=read_file outcome=ok") ||
		!strings.Contains(out, "path="+hundred+" lines=3 bytes=") || !strings.Contains(out, "cut=false counted=true") {
		t.Errorf("expected the path and the lines read to be logged:\n%s", out)
	}
}

// TestReadFileBudget guards the read budget: a file over it, read without a range, tells its size
// and line count rather than its content; a range is cut to the whole lines that fit, telling where
// to continue, and reading on from there gives the whole file back; a line over the budget alone
// is refused, pointing to the terminal.
func TestReadFileBudget(t *testing.T) {
	session, _, home := connectFiles(t)
	content := numberedLines(20000)
	big := writeTestFile(t, home, "big.txt", content)
	expectTool(t, session, "read_file", map[string]any{"path": big}, false,
		big+" holds "+sizeText(len(content))+", 20000 lines, modified 0s ago: more than the read budget (64 KiB). "+
			"Read a range with start_line and max_lines (start_line -100 for the last 100 lines).")

	continuation := regexp.MustCompile(`; cut to the read budget \(64 KiB\): continue with start_line (\d+)\]\n`)
	var readBack strings.Builder
	for start := 1; ; {
		text, isError := callTool(t, session, "read_file", map[string]any{"path": big, "start_line": start})
		if isError || len(text) > defaultReadBudget {
			t.Fatalf("start_line %d: expected at most the read budget, got %d bytes, error %v", start, len(text), isError)
		}
		_, lines, _ := strings.Cut(text, "]\n")
		readBack.WriteString(lines)
		match := continuation.FindStringSubmatch(text)
		if match == nil {
			break
		}
		start, _ = strconv.Atoi(match[1])
	}
	if readBack.String() != content {
		t.Errorf("reading on from each cut: expected the whole file back, got %d bytes of %d", readBack.Len(), len(content))
	}

	long := writeTestFile(t, home, "long.txt", "short\n"+strings.Repeat("x", 100*1024)+"\nafter\n")
	expectTool(t, session, "read_file", map[string]any{"path": long, "start_line": 1}, false,
		"line 1 of 3, 100 KiB", "cut to the read budget (64 KiB): continue with start_line 2]\nshort\n")
	expectTool(t, session, "read_file", map[string]any{"path": long, "start_line": 2}, true,
		"Line 2 of "+long+" holds 100 KiB alone, more than the read budget (64 KiB): read it in the terminal (head -c, cut -c).")
}

// TestReadFileRefused guards what read_file does not send, telling why: binary content (a NUL
// byte, invalid UTF-8 naming its line, but not a character cut by the budget), a directory, a
// device, a FIFO (without blocking on it), a relative path, a missing file and a denied
// permission.
func TestReadFileRefused(t *testing.T) {
	session, _, home := connectFiles(t)
	expectTool(t, session, "read_file", map[string]any{"path": writeTestFile(t, home, "nul", "a\x00b")}, true,
		"is not a text file: it holds NUL bytes. Look at it in the terminal (file, xxd, base64).")
	expectTool(t, session, "read_file", map[string]any{"path": writeTestFile(t, home, "latin1", "ok\ncaf\xe9\n")}, true,
		"is not a text file: it is not valid UTF-8 (line 2).")
	// 65537 bytes read to check the start, the last character cut in two
	accents := writeTestFile(t, home, "accents", strings.Repeat("é", 40000))
	expectTool(t, session, "read_file", map[string]any{"path": accents}, false, accents+" holds 78.1 KiB, 1 line")

	expectTool(t, session, "read_file", map[string]any{"path": "~"}, true,
		home+" is a directory, not a file: list it in the terminal (ls).")
	expectTool(t, session, "read_file", map[string]any{"path": "/dev/zero"}, true,
		"/dev/zero is not a regular file (a device): read_file only reads regular files.")
	fifo := filepath.Join(home, "fifo")
	if err := syscall.Mkfifo(fifo, 0o600); err != nil {
		t.Fatal(err)
	}
	expectTool(t, session, "read_file", map[string]any{"path": fifo}, true, fifo+" is not a regular file (a FIFO)")
	expectTool(t, session, "read_file", map[string]any{"path": "notes.txt"}, true, `Path "notes.txt" is relative`)
	expectTool(t, session, "read_file", map[string]any{"path": "~/missing"}, true,
		"Reading "+filepath.Join(home, "missing")+" failed: no such file or directory.")
	// no permission is denied to root
	if os.Geteuid() != 0 {
		locked := writeTestFile(t, home, "locked", "secret")
		if err := os.Chmod(locked, 0); err != nil {
			t.Fatal(err)
		}
		me, err := user.Current()
		if err != nil {
			t.Fatal(err)
		}
		expectTool(t, session, "read_file", map[string]any{"path": locked}, true,
			"Permission denied: "+locked+" can not be read by "+me.Username+", the user the terminals run as.")
	}
}

// TestReadFileTooFar guards a line that can not be reached before the deadline of the scan: the
// agent is told how else to read it, from the end or in the terminal.
func TestReadFileTooFar(t *testing.T) {
	path := writeTestFile(t, t.TempDir(), "f", numberedLines(10))
	d := &daemon{readBudget: defaultReadBudget}
	past := time.Now().Add(-time.Second)
	if r := d.readFileNow(path, tools.ReadFileInput{StartLine: 5}, past); r.outcome != failed ||
		r.text != "Line 5 of "+path+" (71 B) is too far to reach in time: count from the end (negative start_line), "+
			"or use the terminal (sed -n)." {
		t.Errorf("from the start: unexpected result %+v", r)
	}
	if r := d.readFileNow(path, tools.ReadFileInput{StartLine: -5}, past); r.outcome != failed ||
		!strings.HasSuffix(r.text, "is too far to reach in time: use the terminal (tail -n).") {
		t.Errorf("from the end: unexpected result %+v", r)
	}
}

// TestReadFileHeader guards the headers of ranges whose total could not be counted in time: a
// range from the start without a total, and one numbered from the end.
func TestReadFileHeader(t *testing.T) {
	r := &fileRead{d: &daemon{readBudget: defaultReadBudget}, path: "/f", size: 3 << 30, modified: time.Now()}
	for _, tc := range []struct {
		first, last, next int
		expected          string
	}{
		{30000, 30100, 0, "[/f: lines 30000 to 30100, 3 GiB (too large to count its lines), modified 0s ago]"},
		{-100, -41, -40, "[/f: lines -100 to -41 counted from the end, 3 GiB (too large to count its lines), modified 0s " +
			"ago; cut to the read budget (64 KiB): continue with start_line -40]"},
	} {
		if header := r.header(tc.first, tc.last, 0, false, tc.next); header != tc.expected {
			t.Errorf("expected %q, got %q", tc.expected, header)
		}
	}
}

// TestReadFileTimeout guards a file system not answering (a stale NFS mount): the call returns at
// the end of its time, and ratd logs a warning.
func TestReadFileTimeout(t *testing.T) {
	session, logs, home := connectFiles(t)
	timeout, reader := toolTimeout, fileReader
	toolTimeout = 200 * time.Millisecond
	// The call returns without waiting for the goroutine reading the file, which reads fileReader
	// on its own: restoring the hook is ordered after that read by called, or it races.
	stuck, called := make(chan struct{}), make(chan struct{})
	fileReader = func(*daemon, string, tools.ReadFileInput, time.Time) result {
		close(called)
		<-stuck
		return result{}
	}
	t.Cleanup(func() {
		close(stuck)
		<-called
		toolTimeout, fileReader = timeout, reader
	})
	expectTool(t, session, "read_file", map[string]any{"path": "~/x"}, true,
		"The file system did not answer in time: "+filepath.Join(home, "x")+" could not be read.")
	if !strings.Contains(logs.String(), "level=WARN msg=tool session=alice remote="+toolsRemote+" tool=read_file outcome=error") {
		t.Errorf("expected a warning:\n%s", logs)
	}
}

// TestReadFileNoSize guards files showing a size of 0 while holding content (/proc on Linux): they
// are read to their end, from the start or from the end, their size being what was read.
func TestReadFileNoSize(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("/proc is Linux only")
	}
	session, _, _ := connectFiles(t)
	text := expectTool(t, session, "read_file", map[string]any{"path": "/proc/self/status"}, false,
		"[/proc/self/status: ", " lines, ", "\nName:\t")
	if strings.Contains(text, ", 0 B,") {
		t.Errorf("expected the size read, got:\n%s", text)
	}
	expectTool(t, session, "read_file", map[string]any{"path": "/proc/self/status", "start_line": -1}, false,
		"[/proc/self/status: line ")
}

// procSource is a file showing no size whose content changes length at each read, as files of
// /proc do (counters growing a digit): each ReadAt generates it again, each counter a digit longer,
// while a sequential read from the start goes on in the content it started with, as procfs does.
type procSource struct {
	lines      int
	generation int
	// content is the one a sequential read goes through, pos where it is
	content []byte
	pos     int
}

func (p *procSource) generate() []byte {
	p.generation++
	var b strings.Builder
	for i := 1; i <= p.lines; i++ {
		fmt.Fprintf(&b, "counter%d:\t%s\n", i, strings.Repeat("9", p.generation))
	}
	return []byte(b.String())
}

func (p *procSource) ReadAt(b []byte, off int64) (int, error) {
	content := p.generate()
	if off >= int64(len(content)) {
		return 0, io.EOF
	}
	n := copy(b, content[off:])
	if n < len(b) {
		return n, io.EOF
	}
	return n, nil
}

func (p *procSource) Read(b []byte) (int, error) {
	if p.content == nil {
		p.content = p.generate()
	}
	if p.pos >= len(p.content) {
		return 0, io.EOF
	}
	n := copy(b, p.content[p.pos:])
	p.pos += n
	return n, nil
}

// TestReadFileNoSizeRange guards that a range of a file showing no size (/proc) comes from a single
// read of it: its content changes length from a read to the next, and an offset found in one read
// lands elsewhere in the next, cutting lines (an empty line 59 of /proc/self/status was read so,
// one run out of twenty of TestReadFileNoSize). The lines asked come whole, numbered consistently.
func TestReadFileNoSizeRange(t *testing.T) {
	d := &daemon{readBudget: defaultReadBudget, user: "u"}
	deadline := time.Now().Add(time.Second)
	for _, tc := range []struct {
		in       tools.ReadFileInput
		expected string
	}{
		{tools.ReadFileInput{StartLine: -1}, "[/proc/x: line 10 of 10, "},
		{tools.ReadFileInput{StartLine: 4, MaxLines: 2}, "[/proc/x: lines 4 to 5 of 10, "},
	} {
		r := d.readSource("/proc/x", &procSource{lines: 10}, 0, time.Now(), tc.in, deadline)
		header, content, _ := strings.Cut(r.text, "\n")
		if r.outcome != done || !strings.HasPrefix(header, tc.expected) {
			t.Errorf("%+v: expected a header starting with %q, got %q (%v)", tc.in, tc.expected, header, r.outcome)
		}
		// whole lines of one content: the same counter on each line
		lines := strings.Split(strings.TrimSuffix(content, "\n"), "\n")
		first := max(tc.in.StartLine, 1)
		if tc.in.StartLine < 0 {
			first = 10 + tc.in.StartLine + 1
		}
		for i, line := range lines {
			name, value, _ := strings.Cut(line, ":\t")
			if name != fmt.Sprintf("counter%d", first+i) || value != strings.Repeat("9", len(strings.Split(lines[0], "\t")[1])) {
				t.Errorf("%+v: line %d is not whole, or of another read: %q in\n%s", tc.in, first+i, line, content)
			}
		}
	}
}

// endless is a file showing no size that never ends, as /proc/self/pagemap does not.
type endless struct{}

func (endless) Read(b []byte) (int, error) {
	for i := range b {
		b[i] = "x\n"[i%2]
	}
	return len(b), nil
}

func (endless) ReadAt(b []byte, _ int64) (int, error) {
	return endless{}.Read(b)
}

// TestReadFileNoSizeBounded guards that the copy of a file showing no size, read by range, is
// bounded: a file that never ends is refused once maxCopy is read, pointing to the terminal.
func TestReadFileNoSizeBounded(t *testing.T) {
	d := &daemon{readBudget: defaultReadBudget, user: "u"}
	r := d.readSource("/proc/x", endless{}, 0, time.Now(), tools.ReadFileInput{StartLine: -1}, time.Now().Add(5*time.Second))
	if expected := "/proc/x shows no size, as files of /proc do, and holds more than 64 MiB, the most read_file copies " +
		"to read a range of it: use the terminal (sed -n, tail -n)."; r.outcome != failed || r.text != expected {
		t.Errorf("expected %q, got %q (%v)", expected, r.text, r.outcome)
	}
}
