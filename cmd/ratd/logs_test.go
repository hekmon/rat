package main

import (
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
)

// logFile returns a file for logs, and the JOURNAL_STREAM naming it.
func logFile(t *testing.T) (*os.File, string) {
	t.Helper()
	file, err := os.Create(filepath.Join(t.TempDir(), "logs"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = file.Close() })
	info, err := file.Stat()
	if err != nil {
		t.Fatal(err)
	}
	stat := info.Sys().(*syscall.Stat_t)
	return file, fmt.Sprintf("%d:%d", stat.Dev, stat.Ino)
}

// readLogs returns what was written to file.
func readLogs(t *testing.T, file *os.File) string {
	t.Helper()
	content, err := os.ReadFile(file.Name())
	if err != nil {
		t.Fatal(err)
	}
	return string(content)
}

// TestLogsToJournal guards the lines ratd writes to the journal: the priority of each level as a
// prefix journald parses, and neither the time nor the level, which the journal records itself.
// Attributes and groups are kept, a group's own level and time attributes included, and the
// handlers derived with attributes share the prefix of the record they write.
func TestLogsToJournal(t *testing.T) {
	file, stream := logFile(t)
	t.Setenv("JOURNAL_STREAM", stream)
	logger := slog.New(newLogHandler(file, slog.LevelDebug))
	logger.Debug("debug", "a", 1)
	logger.Info("info")
	logger.Warn("warning")
	logger.Error("error")
	logger.Log(t.Context(), slog.LevelInfo+2, "notice")
	logger.With("session", "s").WithGroup("g").Warn("grouped", "level", "x", "time", "y")
	expected := "<7>msg=debug a=1\n" +
		"<6>msg=info\n" +
		"<4>msg=warning\n" +
		"<3>msg=error\n" +
		"<6>msg=notice\n" +
		"<4>msg=grouped session=s g.level=x g.time=y\n"
	if got := readLogs(t, file); got != expected {
		t.Errorf("expected\n%s\ngot\n%s", expected, got)
	}
}

// TestLogsOutsideJournal guards that the journal format is only used for the stream JOURNAL_STREAM
// names: another output of a service (StandardError=file:…), or a command started from a service,
// gets plain text lines, with their time and level.
func TestLogsOutsideJournal(t *testing.T) {
	file, _ := logFile(t)
	_, otherStream := logFile(t)
	t.Setenv("JOURNAL_STREAM", otherStream)
	slog.New(newLogHandler(file, slog.LevelInfo)).Warn("warning")
	if got := readLogs(t, file); !strings.HasPrefix(got, "time=") || !strings.Contains(got, " level=WARN msg=warning\n") {
		t.Errorf("expected a text line with its time and level, got %q", got)
	}
}
