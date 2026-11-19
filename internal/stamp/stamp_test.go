package stamp

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"
)

// TestStamp guards what tells a file changed: its size (a log appended to), and its times, the
// change time catching a rewrite whose size and modification time were restored (touch -r, cp -p)
// where Go exposes it, on Linux and macOS. A file unchanged stamps the same.
func TestStamp(t *testing.T) {
	path := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(path, []byte("content\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	stampOf := func() (Stamp, os.FileInfo) {
		info, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		return Of(info), info
	}
	before, _ := stampOf()
	if now, _ := stampOf(); !now.Equal(before) {
		t.Error("expected a file unchanged to stamp the same")
	}

	file, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND, 0)
	if err != nil {
		t.Fatal(err)
	}
	_, err = file.WriteString("more\n")
	if closeErr := file.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		t.Fatal(err)
	}
	if now, _ := stampOf(); now.Equal(before) {
		t.Error("expected a file appended to to stamp differently")
	}

	before, info := stampOf()
	// past a tick of the clock the system stamps times with
	time.Sleep(50 * time.Millisecond)
	if file, err = os.OpenFile(path, os.O_WRONLY, 0); err != nil {
		t.Fatal(err)
	}
	_, err = file.WriteAt([]byte("CONTENT"), 0)
	if closeErr := file.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		t.Fatal(err)
	}
	if err = os.Chtimes(path, info.ModTime(), info.ModTime()); err != nil {
		t.Fatal(err)
	}
	now, _ := stampOf()
	switch exposed := runtime.GOOS == "linux" || runtime.GOOS == "darwin"; {
	case exposed && now.Equal(before):
		t.Error("expected a rewrite restoring the size and modification time to stamp differently by its change time")
	case !exposed && !now.Equal(before):
		t.Error("expected no change time here, a rewrite restoring the size and modification time unseen")
	}
}
