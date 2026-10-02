package main

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"sync"
	"syscall"
)

// newLogHandler returns the handler of ratd's logs, written to logs from level on: text lines,
// which journald stores as they are when logs is its stream. The journal then gets each line
// without its time, which the journal records itself, nor its level, which a <N> prefix turns
// into the priority of the entry (SyslogLevelPrefix=, on by default): journalctl shows the time
// once, colors warnings and errors, and filters them (-p warning).
func newLogHandler(logs io.Writer, level slog.Level) slog.Handler {
	if !isJournalStream(logs) {
		return slog.NewTextHandler(logs, &slog.HandlerOptions{Level: level})
	}
	prefixed := &prefixWriter{out: logs}
	return journalHandler{
		Handler: slog.NewTextHandler(prefixed, &slog.HandlerOptions{
			Level: level,
			ReplaceAttr: func(groups []string, a slog.Attr) slog.Attr {
				if len(groups) == 0 && (a.Key == slog.TimeKey || a.Key == slog.LevelKey) {
					return slog.Attr{}
				}
				return a
			},
		}),
		prefixed: prefixed,
	}
}

// isJournalStream tells whether logs is the stream systemd connected to journald, which it names in
// JOURNAL_STREAM by device and inode. Rather than INVOCATION_ID, which every service gets, its
// output going to the journal or not (StandardError=file:…), and which a command started from a
// service inherits with another output.
func isJournalStream(logs io.Writer) bool {
	stream := os.Getenv("JOURNAL_STREAM")
	file, isFile := logs.(*os.File)
	if stream == "" || !isFile {
		return false
	}
	info, err := file.Stat()
	if err != nil {
		return false
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return false
	}
	return stream == fmt.Sprintf("%d:%d", stat.Dev, stat.Ino)
}

// journalHandler is a text handler writing each record behind the journal prefix of its level.
type journalHandler struct {
	slog.Handler
	prefixed *prefixWriter
}

// Handle sets the prefix of the record for the single write the text handler makes of it: the
// text handler formats a whole record before writing it, and knows nothing of journald.
func (h journalHandler) Handle(ctx context.Context, record slog.Record) error {
	h.prefixed.mu.Lock()
	defer h.prefixed.mu.Unlock()
	h.prefixed.prefix = journalPriority(record.Level)
	return h.Handler.Handle(ctx, record)
}

func (h journalHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	return journalHandler{Handler: h.Handler.WithAttrs(attrs), prefixed: h.prefixed}
}

func (h journalHandler) WithGroup(name string) slog.Handler {
	return journalHandler{Handler: h.Handler.WithGroup(name), prefixed: h.prefixed}
}

// prefixWriter writes to out what it is given behind prefix, which its mutex guards: the handlers
// derived from a journalHandler share it, as they share the output.
type prefixWriter struct {
	mu     sync.Mutex
	out    io.Writer
	prefix string
}

func (w *prefixWriter) Write(p []byte) (int, error) {
	if _, err := io.WriteString(w.out, w.prefix+string(p)); err != nil {
		return 0, err
	}
	return len(p), nil
}

// journalPriority returns the prefix giving journald the syslog priority of level: debug, info,
// warning or error, a level between two taking the lower one.
func journalPriority(level slog.Level) string {
	switch {
	case level < slog.LevelInfo:
		return "<7>"
	case level < slog.LevelWarn:
		return "<6>"
	case level < slog.LevelError:
		return "<4>"
	default:
		return "<3>"
	}
}
