package main

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/hekmon/rat/cmd/ratd/tools"
	"github.com/hekmon/rat/connect"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// addFileTools adds the tools copying files to and from the machine of ratd, which reads and writes
// them itself, as its user: tmux, its terminal backend, would only add a round trip. They act on
// files of rat's machine only: no open world.
func (d *daemon) addFileTools(server *mcp.Server, logger *slog.Logger) {
	mcp.AddTool(server, &mcp.Tool{
		Name:        tools.WriteFile,
		InputSchema: d.inputSchemas[tools.WriteFile],
		Description: "Write a whole text file on the machine of the terminals from your context, creating or replacing " +
			"it, with exactly the content given: nothing is added, end it with a new line if the file needs one. The " +
			"path is absolute or starts with ~/. Missing directories are created. " + tools.WriteFileCopyHint,
		Annotations: &mcp.ToolAnnotations{DestructiveHint: ptr(true), IdempotentHint: true, OpenWorldHint: ptr(false)},
	}, tool(logger, tools.WriteFile, func(tools.WriteFileInput) string { return "" },
		func(ctx context.Context, in tools.WriteFileInput) result { return d.writeFile(ctx, in) }))
	d.addReadFileTool(server, logger)
}

// errRelativePath refuses a path relative to nothing ratd knows: the directory of a window is the
// one of its foreground process, the local one of an ssh client for instance.
var errRelativePath = errors.New("relative path")

// errNULInPath refuses a path holding a NUL byte, which no file name can hold: the system would
// refuse it (EINVAL), once the missing directories were created for it.
var errNULInPath = errors.New("NUL byte in path")

// relativeText refuses path, relative (see errRelativePath).
func relativeText(path string) string {
	return fmt.Sprintf("Path %q is relative: give an absolute path, or one starting with ~/ (list_windows shows "+
		"the directories of your windows).", path)
}

// errDirectoryPath refuses, as the path of a file to write, one naming a directory by its form
// (see connect.NamesDirectory).
var errDirectoryPath = errors.New("destination naming a directory")

// destinationPath is absolutePath for the path of a file to write: one naming a directory by its
// form is refused (errDirectoryPath) before it is cleaned into the name of a file.
func destinationPath(path string) (string, error) {
	if connect.NamesDirectory(path) {
		return "", errDirectoryPath
	}
	return absolutePath(path)
}

// pathRefusal returns the text refusing path, which absolutePath or destinationPath refused with
// err.
func pathRefusal(path string, err error) string {
	switch {
	case errors.Is(err, errNULInPath):
		return fmt.Sprintf("Path %q holds a NUL byte, which no file name can hold.", path)
	case errors.Is(err, errDirectoryPath):
		return fmt.Sprintf("Path %q names a directory: give the name of the file to write in it.", path)
	case errors.Is(err, errRelativePath):
		return relativeText(path)
	default:
		// the home directory of ratd's user unknown (HOME unset): ~ leads nowhere
		return fmt.Sprintf("Path %q starts with ~, which can not be resolved: %v. Give an absolute path.", path, err)
	}
}

// absolutePath returns path, absolute or starting with ~/ for the home directory of ratd's user
// (where terminals start), as an absolute clean path. A path holding a NUL byte is refused
// (errNULInPath), before anything is created for it.
func absolutePath(path string) (string, error) {
	if strings.ContainsRune(path, 0) {
		return "", errNULInPath
	}
	if path == "~" || strings.HasPrefix(path, "~/") {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", err
		}
		path = home + path[1:]
	}
	if !filepath.IsAbs(path) {
		return "", errRelativePath
	}
	return filepath.Clean(path), nil
}

// fileTimeoutText tells an agent that writing path did not end in time, for either cause, which
// ratd can not tell apart when answering: a stale network file system, which blocks a human as
// well, or another write in place into the file through ratd (see fileLocks). Only the first may
// leave the file partly written by this call, but the agent checks it in both: in the second, the
// other write is changing it.
const fileTimeoutText = "Writing %s did not end in time: the file system does not answer, or another write into " +
	"the file through ratd is still under way. It may be written, partially or not: check it before writing again."

// fileTimeout is write_file's answer to a call ending before its write, err the error of its
// context: logged as a warning, a file system not answering being worth an admin's look.
func fileTimeout(err error, path string) result {
	return result{outcome: failed, err: err, level: slog.LevelWarn, text: fmt.Sprintf(fileTimeoutText, path)}
}

// stoppedWrite is write_file's answer to ratd stopping while the write waits for the lock of path,
// nothing written: an ordinary stop, as wait_window answers one, not an internal error.
func stoppedWrite(path string) result {
	return result{outcome: failed, err: errStopping, level: slog.LevelInfo,
		text: fmt.Sprintf("ratd is stopping: %s was not written.", path)}
}

// writeFile writes the content of in to its path, in place, within the context of the call. File
// operations can not be interrupted: on a file system not answering (a stale NFS mount), the call
// returns at the end of its context, logged as a warning, while the write stays blocked until the
// file system answers. A wait for a write in place into the file under way (see fileLocks) ends
// with the call, answered the same way, and nothing is written.
func (d *daemon) writeFile(ctx context.Context, in tools.WriteFileInput) result {
	attrs := []any{"path", in.Path, "bytes", len(in.Content)}
	path, err := destinationPath(in.Path)
	if err != nil {
		return result{outcome: failed, err: err, attrs: attrs, text: pathRefusal(in.Path, err)}
	}
	attrs[1] = path
	written := make(chan result, 1)
	go func() { written <- fileWriter(d, ctx, path, in.Content) }()
	// whichever comes first, a wait for the lock ended by ctx answers as ctx ending does
	select {
	case r := <-written:
		r.attrs = append(attrs, r.attrs...)
		return r
	case <-ctx.Done():
		return withAttrs(fileTimeout(ctx.Err(), path), attrs)
	}
}

// fileWriter writes a file for write_file: writeFileNow. A variable for tests, which need a file
// system not answering.
var fileWriter = (*daemon).writeFileNow

// writeFileNow writes content to the regular file at path, an absolute clean path, creating it and
// its missing directories if needed, or replacing it in place: as cp does, a replaced file keeps
// its mode, owner and links, and a symbolic link is followed. It never blocks on a FIFO, but waits
// for a write in place into the file through ratd under way to end, an upload's or another
// write_file's (see fileLocks). The wait ends with ctx, answered as writeFile answers ctx ending
// (fileTimeout), or with ratd stopping: either way nothing is written, the file left as it was.
func (d *daemon) writeFileNow(ctx context.Context, path, content string) result {
	info, err := os.Stat(path)
	var created []string
	switch {
	case err == nil:
		if refused := d.notRegular(path, info, written); refused != nil {
			return *refused
		}
	case errors.Is(err, fs.ErrNotExist), errors.Is(err, syscall.ENOTDIR):
		// the directories are created as mkdir -p does, and named: a typo in a path creates
		// directories the agent did not mean
		var r *result
		if created, r = d.createDirectories(filepath.Dir(path)); r != nil {
			return *r
		}
	default:
		return d.fileFailure(err, "Writing "+path, written, false)
	}
	// From now on, a failure names the directories created, which it leaves, as a success does.
	failed := func(r result) result {
		if len(created) > 0 {
			r.text += " " + createdText(created)
		}
		return r
	}
	file, isNew, err := openRegular(path)
	var notRegular *notRegularError
	switch {
	case errors.As(err, &notRegular):
		return failed(*d.notRegular(path, notRegular.info, written))
	case err != nil:
		return failed(d.fileFailure(err, "Writing "+path, written, false))
	}
	writeOpened(path)
	// One write in place at a time into a file (see fileLocks), held until the file is closed, taken
	// before the truncation: a stop or the end of the call ending the wait leaves the file as it was.
	release := func() {}
	if info, err = file.Stat(); err == nil {
		release, err = d.inPlace.lock(ctx, path, info, d.stopping)
		switch {
		case err == nil:
		case errors.Is(err, errStopping):
			_ = file.Close()
			return failed(stoppedWrite(path))
		default:
			_ = file.Close()
			return fileTimeout(err, path)
		}
		// looked at again once the lock is held: a write waited for changed the file, and the size
		// told as the one before is that of the file this write replaces, not of the one it found
		info, err = file.Stat()
	}
	defer release()
	// once truncated, a failure leaves the file partial
	truncated := false
	if err == nil {
		err = file.Truncate(0)
		truncated = err == nil
	}
	if err == nil {
		_, err = file.WriteString(content)
	}
	// a network file system may only report a failed write when closing
	if closeErr := file.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		return failed(d.fileFailure(err, "Writing "+path, written, truncated))
	}
	// Told created for a file this write created and found empty once it held it. Told from the
	// open alone, of two writes creating one file at once, the second to hold it would be told
	// created too, having replaced what the first wrote: the one that created it exclusively,
	// overtaken, or both through a dangling symbolic link, whose target neither knows it created
	// (see openRegular). The size once the file is held tells what a write replaced.
	isNew = isNew && info.Size() == 0
	text := fmt.Sprintf("Replaced %s (%s, %s before).", path, sizeText(len(content)), sizeText(int(info.Size())))
	if isNew {
		text = fmt.Sprintf("Created %s (%s).", path, sizeText(len(content)))
	}
	if len(created) > 0 {
		text += " " + createdText(created)
	}
	return result{text: text, attrs: []any{"created", isNew, "directories_created", len(created)}}
}

// writeOpened is called with the path of a write_file once its file is opened, before it holds it:
// a hook for tests, which write into the file there, as another write getting ahead.
var writeOpened = func(string) {}

// createdText names the directories created (by createDirectories) for a file, for an agent to
// catch a typo in its path.
func createdText(created []string) string {
	return fmt.Sprintf("Missing directories created: %s. Check the path if you did not mean them.",
		strings.Join(created, ", "))
}

// createDirectories creates dir and its missing parents, as mkdir -p does, and returns the ones it
// created, from the top. It refuses a parent that is not a directory, naming it. A failure
// creating them names those it created before, left as they are.
func (d *daemon) createDirectories(dir string) (created []string, refused *result) {
	for parent := dir; ; parent = filepath.Dir(parent) {
		info, err := os.Stat(parent)
		if err == nil {
			if !info.IsDir() {
				return nil, &result{outcome: failed, err: syscall.ENOTDIR, text: fmt.Sprintf(
					"%s is not a directory: %s can not be created in it.", parent, dir)}
			}
			break
		}
		switch {
		case errors.Is(err, fs.ErrNotExist):
			created = append([]string{parent}, created...)
		case errors.Is(err, syscall.ENOTDIR):
			// a parent further up is not a directory: found next
		default:
			r := d.fileFailure(err, "Creating "+dir, written, false)
			return nil, &r
		}
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		r := d.fileFailure(err, "Creating "+dir, written, false)
		if made := existing(created); len(made) > 0 {
			r.text += " " + createdText(made)
		}
		return nil, &r
	}
	return created, nil
}

// existing returns the first of dirs, from the top, that exist: those MkdirAll made, from the top,
// before failing on the next one.
func existing(dirs []string) []string {
	for i, dir := range dirs {
		if _, err := os.Stat(dir); err != nil {
			return dirs[:i]
		}
	}
	return dirs
}

// notRegularError is the error of openRegular for a file that is not a regular file.
type notRegularError struct {
	info fs.FileInfo
}

func (e *notRegularError) Error() string {
	return "not a regular file: " + e.info.Mode().Type().String()
}

// openRegular opens path for writing, creating it if missing, and tells whether it created it,
// from the open that succeeded: exclusively first, then the existing file, then creating again,
// which may open a file another created meanwhile, told created all the same (the caller holds it
// before telling what it replaced). It
// never blocks on a FIFO (without a reader, opening it fails), and refuses what it opened if it is
// not a regular file, with a notRegularError: checked again on the file opened, which may have
// replaced the one checked before opening.
func openRegular(path string) (file *os.File, created bool, err error) {
	file, err = os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL|syscall.O_NONBLOCK, 0o644)
	created = err == nil
	if errors.Is(err, fs.ErrExist) {
		file, err = os.OpenFile(path, os.O_WRONLY|syscall.O_NONBLOCK, 0)
		if errors.Is(err, fs.ErrNotExist) {
			// A name existing without a file behind it: a dangling symbolic link, which O_EXCL
			// refuses and O_CREATE alone follows, creating its target, as a human writing to it
			// would; or a file removed in between. Created either way, which one open with O_CREATE
			// for both the existing file and the link would tell replaced, 0 B before.
			file, err = os.OpenFile(path, os.O_WRONLY|os.O_CREATE|syscall.O_NONBLOCK, 0o644)
			created = err == nil
		}
	}
	if err != nil {
		return nil, false, err
	}
	if _, err = checkOpened(file); err != nil {
		return nil, false, err
	}
	return file, created, nil
}

// checkOpened returns the description of file, just opened, or a notRegularError if it is not a
// regular file, closing it on failure.
func checkOpened(file *os.File) (fs.FileInfo, error) {
	info, err := file.Stat()
	if err == nil && !info.Mode().IsRegular() {
		err = &notRegularError{info: info}
	}
	if err != nil {
		_ = file.Close()
		return nil, err
	}
	return info, nil
}

// access tells how a file tool accesses files, for the texts refusing it.
type access bool

const (
	written access = false
	read    access = true
)

// notRegular returns the refusal of path, described by info, if it is not a regular file: a
// directory, or what the file tools do not access (a FIFO would block, a device is not a file to
// replace, /dev/zero never ends).
func (d *daemon) notRegular(path string, info fs.FileInfo, how access) *result {
	mode := info.Mode()
	switch {
	case mode.IsRegular():
		return nil
	case mode.IsDir():
		text := fmt.Sprintf("%s is a directory, not a file.", path)
		if how == read {
			text = fmt.Sprintf("%s is a directory, not a file: list it in the terminal (ls).", path)
		}
		return &result{outcome: failed, err: syscall.EISDIR, text: text}
	}
	tool := "write_file only writes"
	if how == read {
		tool = "read_file only reads"
	}
	return &result{outcome: failed, err: fmt.Errorf("not a regular file: %s", mode.Type()), text: fmt.Sprintf(
		"%s is not a regular file (%s): %s regular files.", path, kindText(mode), tool)}
}

// kindText tells what a file of mode is, when not a regular file nor a directory.
func kindText(mode fs.FileMode) string {
	switch {
	case mode&fs.ModeNamedPipe != 0:
		return "a FIFO"
	case mode&fs.ModeDevice != 0:
		return "a device"
	case mode&fs.ModeSocket != 0:
		return "a socket"
	default:
		return "of type " + mode.Type().String()
	}
}

// fileFailure returns the result of a file operation, what telling it ("Writing /x"), failed with
// err. The reason given by the system is told as is: a low level operation failing is what an
// agent expects, and can act upon (no space left, a read-only file system). A denied permission
// names the user. partial tells the file was truncated before failing.
func (d *daemon) fileFailure(err error, what string, how access, partial bool) result {
	r := result{outcome: failed, err: err}
	var pathErr *fs.PathError
	switch {
	case errors.Is(err, fs.ErrPermission) && errors.As(err, &pathErr):
		verb := "written"
		if how == read {
			verb = "read"
		}
		r.text = fmt.Sprintf("Permission denied: %s can not be %s by %s, the user the terminals run as.",
			pathErr.Path, verb, d.user)
	case errors.As(err, &pathErr):
		r.text = fmt.Sprintf("%s failed: %s.", what, pathErr.Err)
	default:
		r.text, r.level = fmt.Sprintf("%s failed: internal error, logged.", what), slog.LevelWarn
	}
	if partial {
		r.text += " The file may be partial: write it again once fixed."
	}
	return r
}
