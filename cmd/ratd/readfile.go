package main

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math"
	"os"
	"strings"
	"syscall"
	"time"
	"unicode/utf8"

	"github.com/hekmon/rat/cmd/ratd/tools"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// addReadFileTool adds read_file.
func (d *daemon) addReadFileTool(server *mcp.Server, session string) {
	mcp.AddTool(server, &mcp.Tool{
		Name:        tools.ReadFile,
		InputSchema: d.inputSchemas[tools.ReadFile],
		Description: fmt.Sprintf("Read a text file on the machine of the terminals: whole lines, from start_line (1 by "+
			"default, negative counts from the end: -100 for the last 100 lines), at most max_lines (all by default). "+
			"The result holds at most %s: a larger file read without a range tells its size and line count, for you "+
			"to pick a range. The path is absolute or starts with ~/.", sizeText(d.readBudget)),
		Annotations: &mcp.ToolAnnotations{ReadOnlyHint: true, OpenWorldHint: ptr(false)},
	}, tool(d, session, tools.ReadFile, func(tools.ReadFileInput) string { return "" },
		func(ctx context.Context, in tools.ReadFileInput) result { return d.readFile(ctx, in) }))
}

// fileReader reads a file for read_file: readFileNow. A variable for tests, which need a file
// system not answering.
var fileReader = (*daemon).readFileNow

// readFile reads the lines of in, within the context of the call. Scanning a file (to reach a
// line, or to count them all) takes half of the time of the call at most: 1 GiB is scanned in
// 80 ms from the page cache, but in seconds from a disk. As for write_file, a file system not
// answering fails the call at the end of its context, logged as a warning, the read staying
// blocked until it answers.
func (d *daemon) readFile(ctx context.Context, in tools.ReadFileInput) result {
	attrs := []any{"path", in.Path}
	path, err := absolutePath(in.Path)
	if err != nil {
		return result{outcome: failed, err: err, attrs: attrs, text: fmt.Sprintf("Path %q is relative: give an "+
			"absolute path, or one starting with ~/ (list_windows shows the directories of your windows).", in.Path)}
	}
	attrs[1] = path
	scanTime := toolTimeout / 2
	if deadline, ok := ctx.Deadline(); ok {
		scanTime = time.Until(deadline) / 2
	}
	read := make(chan result, 1)
	go func() { read <- fileReader(d, path, in, time.Now().Add(scanTime)) }()
	select {
	case r := <-read:
		r.attrs = append(attrs, r.attrs...)
		return r
	case <-ctx.Done():
		return result{outcome: failed, err: ctx.Err(), level: slog.LevelWarn, attrs: attrs,
			text: fmt.Sprintf("The file system did not answer in time: %s could not be read.", path)}
	}
}

// fileRead is a read of read_file in progress.
type fileRead struct {
	d    *daemon
	path string
	file *os.File
	in   tools.ReadFileInput
	// size is the size of the file when opened, which bounds the read: a file growing meanwhile
	// (a log) is read as it was, consistently with the size told. Zero for files showing no size
	// (/proc), read to their end within the deadline.
	size int64
	// seen is how far the file was read, the size of a file showing none
	seen int64
	// modified is the last modification of the file, told relatively: a command may still be
	// writing it
	modified time.Time
	// deadline ends scanning: reaching a line, counting lines
	deadline time.Time
}

// errDeadline is returned by scans reaching the deadline of a read.
var errDeadline = errors.New("scan deadline reached")

// readFileNow reads the lines of in from the regular file at path, an absolute clean path,
// scanning it until deadline at most. What it sends is checked first, as it can not be taken back
// from the context of a model: a regular file, a bounded read, text, the read budget.
func (d *daemon) readFileNow(path string, in tools.ReadFileInput, deadline time.Time) result {
	info, err := os.Stat(path)
	if err != nil {
		return d.fileFailure(err, "Reading "+path, read, false)
	}
	if refused := d.notRegular(path, info, read); refused != nil {
		return *refused
	}
	// without blocking: a FIFO replacing the file checked would block until written to
	file, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NONBLOCK, 0)
	if err == nil {
		info, err = checkOpened(file)
	}
	var notRegular *notRegularError
	switch {
	case errors.As(err, &notRegular):
		return *d.notRegular(path, notRegular.info, read)
	case err != nil:
		return d.fileFailure(err, "Reading "+path, read, false)
	}
	defer file.Close()
	r := &fileRead{d: d, path: path, file: file, in: in, size: info.Size(), modified: info.ModTime(), deadline: deadline}
	if in.StartLine == 0 && in.MaxLines == 0 {
		return r.whole()
	}
	return r.lines()
}

// end is the offset reads stop at.
func (r *fileRead) end() int64 {
	if r.size > 0 {
		return r.size
	}
	return math.MaxInt64
}

// whole reads the whole file, if it fits the read budget. Otherwise, it tells its size, line count
// and modification, for the agent to pick a range: nothing big lands in its context by surprise.
// The start of the file is checked to be text all the same, as a range of it would be refused.
func (r *fileRead) whole() result {
	budget := r.d.readBudget
	data, err := io.ReadAll(io.LimitReader(io.NewSectionReader(r.file, 0, r.end()), int64(budget)+1))
	if err != nil {
		return r.d.fileFailure(err, "Reading "+r.path, read, false)
	}
	r.seen = int64(len(data))
	if refused := r.checkText(data, 1, len(data) > budget); refused != nil {
		return *refused
	}
	if len(data) <= budget {
		lines := countLines(data)
		header := fmt.Sprintf("[%s: empty, modified %s ago]", r.path, sinceText(time.Since(r.modified)))
		if lines > 0 {
			header = fmt.Sprintf("[%s: %s, %s, modified %s ago]", r.path, plural(lines, "line"), sizeText(len(data)),
				sinceText(time.Since(r.modified)))
		}
		if text := withHeader(header, r.numbered(data, 1)); len(text) <= budget {
			return result{text: text, attrs: []any{"lines", lines, "bytes", len(text), "cut", false, "counted", true}}
		}
	}
	lines, counted, err := r.countLines(0)
	if err != nil {
		return r.d.fileFailure(err, "Reading "+r.path, read, false)
	}
	count := "too large to count its lines"
	if counted {
		count = plural(lines, "line")
	}
	return result{outcome: message, attrs: []any{"counted", counted}, text: fmt.Sprintf("%s holds %s, %s, modified "+
		"%s ago: more than the read budget (%s). Read a range with start_line and max_lines (start_line -100 for "+
		"the last 100 lines).", r.path, sizeText(int(max(r.size, r.seen))), count, sinceText(time.Since(r.modified)),
		sizeText(budget))}
}

// lines reads the range of lines asked. A negative start is found reading backwards, instant on
// any size: its lines are numbered from the end when the lines before them can not be counted in
// time.
func (r *fileRead) lines() result {
	var offset int64
	// first is the number of the first line read, negative when counted from the end; before is
	// the number of lines before it, -1 when unknown
	var first, before int
	var err error
	switch start := r.in.StartLine; {
	case start < 0 && r.size > 0:
		var clamped bool
		if offset, clamped, err = r.lineFromEnd(-start); errors.Is(err, errDeadline) {
			return r.tooFar(start, "use the terminal (tail -n)")
		} else if err != nil {
			return r.d.fileFailure(err, "Reading "+r.path, read, false)
		}
		first, before = start, -1
		if clamped {
			first, before = 1, 0
		} else if newLines, _, _, scanErr := r.scan(0, offset); scanErr == nil {
			first, before = newLines+1, newLines
		}
	default:
		if start < 0 {
			// no size to read backwards from (/proc): the lines are counted first
			lines, counted, err := r.countLines(0)
			if err != nil {
				return r.d.fileFailure(err, "Reading "+r.path, read, false)
			} else if !counted {
				return r.tooFar(start, "use the terminal (tail -n)")
			}
			start = lines + start + 1
		}
		first, before = max(start, 1), max(start, 1)-1
		var found bool
		var lines int
		if offset, lines, found, err = r.lineFromStart(first); errors.Is(err, errDeadline) {
			return r.tooFar(first, "count from the end (negative start_line), or use the terminal (sed -n)")
		} else if err != nil {
			return r.d.fileFailure(err, "Reading "+r.path, read, false)
		} else if !found {
			return result{outcome: failed, text: fmt.Sprintf("%s has %s: start_line %d is past its end.", r.path,
				plural(lines, "line"), r.in.StartLine)}
		}
	}
	return r.collect(offset, first, before)
}

// collect returns the lines from offset, numbered from first, at most max_lines of them: the whole
// lines that fit the read budget, telling where to continue after a cut. before is the number of
// lines before offset, -1 when unknown: the total is then not told.
func (r *fileRead) collect(offset int64, first, before int) result {
	budget := r.d.readBudget
	// The header is written once the lines are read: room is left for its longest form, a fixed
	// text with numbers and sizes around the path.
	room := budget - len(r.path) - 256
	reader := bufio.NewReader(io.NewSectionReader(r.file, offset, r.end()-offset))
	maxLines := int(min(r.in.MaxLines, math.MaxInt))
	var content bytes.Buffer
	// consumed is the size of the lines read, from offset; next is the line to continue with
	// after a cut
	n, consumed, next := 0, int64(0), 0
	for ; maxLines == 0 || n < maxLines; n++ {
		number := first + n
		// the first line is sized when too long, to tell it; the others are only cut before
		line, size, err := r.readLine(reader, room+1, n == 0)
		switch {
		case errors.Is(err, errDeadline):
			return r.lineTooLong(number, size, true)
		case err != nil && !errors.Is(err, io.EOF):
			return r.d.fileFailure(err, "Reading "+r.path, read, false)
		}
		if size == 0 {
			break
		}
		if refused := r.checkText(line, number, len(line) < size); refused != nil {
			return *refused
		}
		entry := r.numbered(line, number)
		if content.Len()+len(entry) > room {
			if n == 0 {
				return r.lineTooLong(number, size, false)
			}
			next = number
			break
		}
		content.WriteString(entry)
		consumed += int64(size)
	}
	r.seen = max(r.seen, offset+consumed)
	total, counted := 0, false
	if before >= 0 {
		// the lines after the ones read, the one cut included
		if after, ok, err := r.countLines(offset + consumed); err == nil && ok {
			total, counted = before+n+after, true
		}
	}
	text := r.header(first, first+n-1, total, counted, next) + "\n" + content.String()
	return result{text: text, attrs: []any{"lines", n, "bytes", len(text), "cut", next != 0, "counted", counted}}
}

// header returns the header of the lines first to last, out of total if counted, telling where to
// continue if cut at next.
func (r *fileRead) header(first, last, total int, counted bool, next int) string {
	lines := fmt.Sprintf("lines %d to %d", first, last)
	if first == last {
		lines = fmt.Sprintf("line %d", first)
	}
	size := sizeText(int(max(r.size, r.seen)))
	switch {
	case first < 0:
		lines += " counted from the end"
		size += " (too large to count its lines)"
	case counted:
		lines += fmt.Sprintf(" of %d", total)
	default:
		size += " (too large to count its lines)"
	}
	cut := ""
	if next != 0 {
		cut = fmt.Sprintf("; cut to the read budget (%s): continue with start_line %d", sizeText(r.d.readBudget), next)
	}
	return fmt.Sprintf("[%s: %s, %s, modified %s ago%s]", r.path, lines, size, sinceText(time.Since(r.modified)), cut)
}

// readLine reads the next line from reader, its new line included, and returns up to keep bytes of
// it, with io.EOF for a last line without a new line. A line longer than keep is sized if asked
// (its size returned, up to the deadline, then errDeadline with the size read so far), otherwise
// returned as soon as longer.
func (r *fileRead) readLine(reader *bufio.Reader, keep int, sized bool) (line []byte, size int, err error) {
	for {
		chunk, err := reader.ReadSlice('\n')
		size += len(chunk)
		if room := keep - len(line); room > 0 {
			line = append(line, chunk[:min(len(chunk), room)]...)
		}
		if !errors.Is(err, bufio.ErrBufferFull) {
			return line, size, err
		}
		if size > keep && (!sized || time.Now().After(r.deadline)) {
			if sized {
				return line, size, errDeadline
			}
			return line, size, nil
		}
	}
}

// lineTooLong refuses the line number, of size bytes (at least, if atLeast), over the read budget
// alone: it can not be cut into lines.
func (r *fileRead) lineTooLong(number, size int, atLeast bool) result {
	more := ""
	if atLeast {
		more = "more than "
	}
	return result{outcome: failed, text: fmt.Sprintf("Line %d of %s holds %s%s alone, more than the read budget "+
		"(%s): read it in the terminal (head -c, cut -c).", number, r.path, more, sizeText(size),
		sizeText(r.d.readBudget))}
}

// tooFar refuses the line number, which could not be reached before the deadline, pointing to how.
func (r *fileRead) tooFar(number int, how string) result {
	return result{outcome: failed, err: errDeadline, text: fmt.Sprintf("Line %d of %s (%s) is too far to reach in time: "+
		"%s.", number, r.path, sizeText(int(max(r.size, r.seen))), how)}
}

// numbered returns data, lines numbered from first if asked, as cat -n does.
func (r *fileRead) numbered(data []byte, first int) string {
	if !r.in.LineNumbers || len(data) == 0 {
		return string(data)
	}
	var out strings.Builder
	for number := first; len(data) > 0; number++ {
		line := data
		if i := bytes.IndexByte(data, '\n'); i >= 0 {
			line = data[:i+1]
		}
		fmt.Fprintf(&out, "%6d\t%s", number, line)
		data = data[len(line):]
	}
	return out.String()
}

// checkText refuses data, the lines from first, if it is not text: a NUL byte (the heuristic of git
// and grep -I), or invalid UTF-8. partial tells data was cut, maybe within a character.
func (r *fileRead) checkText(data []byte, first int, partial bool) *result {
	if bytes.IndexByte(data, 0) >= 0 {
		return &result{outcome: failed, text: fmt.Sprintf("%s is not a text file: it holds NUL bytes. Look at it in the "+
			"terminal (file, xxd, base64).", r.path)}
	}
	if partial {
		// a character cut at the end is not invalid
		for i := len(data) - 1; i >= 0 && i >= len(data)-utf8.UTFMax; i-- {
			if utf8.RuneStart(data[i]) {
				if !utf8.FullRune(data[i:]) {
					data = data[:i]
				}
				break
			}
		}
	}
	for i := 0; i < len(data); {
		c, size := utf8.DecodeRune(data[i:])
		if c == utf8.RuneError && size == 1 {
			return &result{outcome: failed, text: fmt.Sprintf("%s is not a text file: it is not valid UTF-8 (line %d). "+
				"Look at it in the terminal (file, xxd, base64).", r.path, first+bytes.Count(data[:i], []byte{'\n'}))}
		}
		i += size
	}
	return nil
}

// scanChunk is the size of the chunks scans read.
const scanChunk = 1 << 20

// scan counts the new lines between the offsets from and to (the end of the file at most), and
// tells how many bytes it read, and whether the last one is a new line. It fails with errDeadline
// at the deadline.
func (r *fileRead) scan(from, to int64) (newLines int, scanned int64, endsWithNewLine bool, err error) {
	buf := make([]byte, scanChunk)
	for offset := from; offset < to; {
		if time.Now().After(r.deadline) {
			return 0, 0, false, errDeadline
		}
		n, err := r.file.ReadAt(buf[:min(int64(len(buf)), to-offset)], offset)
		newLines += bytes.Count(buf[:n], []byte{'\n'})
		if n > 0 {
			endsWithNewLine = buf[n-1] == '\n'
		}
		offset += int64(n)
		scanned += int64(n)
		r.seen = max(r.seen, offset)
		if errors.Is(err, io.EOF) || n == 0 {
			break
		} else if err != nil {
			return 0, 0, false, err
		}
	}
	return newLines, scanned, endsWithNewLine, nil
}

// countLines counts the lines from offset to the end of the file, a last line without a new line
// included, and tells whether it could before the deadline.
func (r *fileRead) countLines(offset int64) (lines int, counted bool, err error) {
	newLines, scanned, endsWithNewLine, err := r.scan(offset, r.end())
	switch {
	case errors.Is(err, errDeadline):
		return 0, false, nil
	case err != nil:
		return 0, false, err
	}
	if scanned > 0 && !endsWithNewLine {
		newLines++
	}
	return newLines, true, nil
}

// lineFromStart returns the offset of line number, counted from 1, and whether the file has it:
// if not, lines tells how many it has. It fails with errDeadline at the deadline.
func (r *fileRead) lineFromStart(number int) (offset int64, lines int, found bool, err error) {
	buf := make([]byte, scanChunk)
	newLines, last := 0, byte('\n')
	for offset = 0; offset < r.end(); {
		if newLines == number-1 {
			break
		}
		if time.Now().After(r.deadline) {
			return 0, 0, false, errDeadline
		}
		n, err := r.file.ReadAt(buf[:min(int64(len(buf)), r.end()-offset)], offset)
		chunk := buf[:n]
		for len(chunk) > 0 && newLines < number-1 {
			i := bytes.IndexByte(chunk, '\n')
			if i < 0 {
				offset += int64(len(chunk))
				last = chunk[len(chunk)-1]
				break
			}
			newLines++
			offset += int64(i + 1)
			last, chunk = '\n', chunk[i+1:]
		}
		r.seen = max(r.seen, offset)
		if errors.Is(err, io.EOF) || n == 0 {
			break
		} else if err != nil {
			return 0, 0, false, err
		}
	}
	// the line exists if a byte follows the new line before it
	var probe [1]byte
	if n, _ := r.file.ReadAt(probe[:], offset); newLines == number-1 && n == 1 {
		return offset, 0, true, nil
	}
	lines = newLines
	if last != '\n' {
		lines++
	}
	return 0, lines, false, nil
}

// lineFromEnd returns the offset of the line count lines before the end of the file (1 is the
// last line), reading backwards from the size of the file. clamped tells the file has no more
// lines than count: the offset is then 0. It fails with errDeadline at the deadline.
func (r *fileRead) lineFromEnd(count int) (offset int64, clamped bool, err error) {
	buf := make([]byte, scanChunk)
	end := r.size
	// a new line ending the file ends its last line, it does not start another
	var lastByte [1]byte
	if _, err = r.file.ReadAt(lastByte[:], end-1); err != nil && !errors.Is(err, io.EOF) {
		return 0, false, err
	}
	if lastByte[0] == '\n' {
		end--
	}
	found := 0
	for end > 0 {
		if time.Now().After(r.deadline) {
			return 0, false, errDeadline
		}
		start := max(0, end-int64(len(buf)))
		n, err := r.file.ReadAt(buf[:end-start], start)
		if err != nil && !errors.Is(err, io.EOF) {
			return 0, false, err
		}
		chunk := buf[:n]
		for {
			i := bytes.LastIndexByte(chunk, '\n')
			if i < 0 {
				break
			}
			if found++; found == count {
				return start + int64(i) + 1, false, nil
			}
			chunk = chunk[:i]
		}
		end = start
	}
	return 0, true, nil
}

// countLines returns the number of lines of data, a last line without a new line included.
func countLines(data []byte) int {
	lines := bytes.Count(data, []byte{'\n'})
	if len(data) > 0 && data[len(data)-1] != '\n' {
		lines++
	}
	return lines
}

// plural returns count and noun, in the plural if needed.
func plural(count int, noun string) string {
	if count == 1 {
		return "1 " + noun
	}
	return fmt.Sprintf("%d %ss", count, noun)
}
