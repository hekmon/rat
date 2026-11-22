package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"mime"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/hekmon/rat/cmd/ratd/tools"
	"github.com/hekmon/rat/connect"
	"github.com/hekmon/rat/internal/stamp"
	"github.com/hekmon/rat/internal/tempname"
	"github.com/hekmon/rat/internal/version"
	"github.com/hekmon/rat/mtls"
)

// The file transfers of rat copy files by path between the machine of a harness and this one (see
// File transfers in the README): their bytes travel over two endpoints next to the MCP one, never
// through MCP, the file being the body and headers describing it (see package connect for the
// contract). ratd reads and writes the files itself, as it does for write_file and read_file.

// direction tells a transfer's way: an upload writes a file here, a download reads one.
type direction string

const (
	upload   direction = "upload"
	download direction = "download"
)

// errSymbolicLink is the error of a transfer whose path ends with a symbolic link (see
// linkRefusal).
var errSymbolicLink = errors.New("symbolic link")

// errChanged is the error of a download whose file changed while being read.
var errChanged = errors.New("file changed while read")

// errNoTrueSize is the error of a download whose file tells a size that is not that of its
// content (see noTrueSizeRefusal).
var errNoTrueSize = errors.New("size not that of the content")

// addTransfers serves the file transfers on mux, behind the same client checks as MCP requests.
// GET also answers HEAD, with the headers of a download and no body.
func (d *daemon) addTransfers(mux *http.ServeMux) {
	mux.Handle(http.MethodPost+" "+connect.UploadPath, d.transferHandler(upload, d.upload))
	mux.Handle(http.MethodGet+" "+connect.DownloadPath, d.transferHandler(download, d.download))
}

// transferHandler returns the handler of the transfers going way, which serve runs, writing the
// answer itself and returning what the log line tells. It names the session by the client
// certificate, as for an MCP request, records it in the status with the transfer in flight, tells
// the version of ratd in every answer, cuts the transfer at once when ratd stops, and logs it in
// one line, as a tool call: the path and the size, never the content.
func (d *daemon) transferHandler(way direction, serve func(http.ResponseWriter, *http.Request) result) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		w.Header().Set(connect.HeaderVersion, version.Module())
		var session string
		if req.TLS != nil {
			session = mtls.PeerName(*req.TLS)
		}
		if session == "" {
			http.Error(w, "A client certificate naming the session is required.", http.StatusBadRequest)
			return
		}
		start := time.Now()
		d.status.see(session, start)
		d.status.transferStarted()
		defer d.status.transferEnded()
		defer d.cutOnStop(w)()
		r := serve(w, req)
		attrs := []any{"session", session, "remote", req.RemoteAddr, "direction", way, "outcome", r.outcome.String(),
			"duration", time.Since(start)}
		attrs = append(attrs, r.attrs...)
		if r.err != nil {
			attrs = append(attrs, "error", r.err)
		}
		d.logger.Log(context.Background(), r.level, "transfer", attrs...)
	})
}

// cutOnStop cuts the transfer answered by w as soon as ratd stops, until the function it returns
// is called: the reads and writes of its connection fail at once. http.Server cancels no request
// when shutting down, it waits for them, and a transfer can last hours: it would hold the door
// open for the whole grace, then be cut all the same. Nothing is cut before ratd serves.
func (d *daemon) cutOnStop(w http.ResponseWriter) (release func()) {
	if d.stopping == nil {
		return func() {}
	}
	controller := http.NewResponseController(w)
	done := make(chan struct{})
	go func() {
		select {
		case <-d.stopping:
			now := time.Now()
			_ = controller.SetReadDeadline(now)
			_ = controller.SetWriteDeadline(now)
		case <-done:
		}
	}()
	return func() { close(done) }
}

// refuse answers status with the text of r, a refused or failed transfer, for rat to relay to the
// agent, and returns r for the log line.
func refuse(w http.ResponseWriter, status int, r result) result {
	http.Error(w, r.text, status)
	r.outcome = failed
	r.attrs = append(r.attrs, "status", status)
	return r
}

// transferStatus returns the HTTP status of a transfer that failed with err, a file operation:
// its text tells the rest.
func transferStatus(err error) int {
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return http.StatusNotFound
	case errors.Is(err, fs.ErrPermission), errors.Is(err, syscall.EROFS):
		return http.StatusForbidden
	case errors.Is(err, syscall.ENOSPC), errors.Is(err, syscall.EDQUOT), errors.Is(err, syscall.EFBIG):
		return http.StatusInsufficientStorage
	case errors.Is(err, syscall.ENOTDIR), errors.Is(err, syscall.EISDIR), errors.Is(err, fs.ErrExist),
		errors.Is(err, syscall.ELOOP):
		// the path meets what it can not go through: a file, a link leading nowhere where a
		// directory must be created, links looping
		return http.StatusConflict
	case errors.Is(err, syscall.ENAMETOOLONG), errors.Is(err, syscall.EILSEQ):
		// the path the request names can be no file: a name too long, or one that is not UTF-8
		// where the file system takes UTF-8 alone (APFS); a mistake of the client, not of ratd
		return http.StatusBadRequest
	default:
		return http.StatusInternalServerError
	}
}

// transferPath returns the path a transfer request names (connect.HeaderPath), absolute and clean,
// or the refusal of a missing, undecodable or relative one.
func transferPath(req *http.Request) (string, *result) {
	value := req.Header.Get(connect.HeaderPath)
	if value == "" {
		return "", &result{outcome: failed, err: connect.ErrHeader,
			text: connect.HeaderPath + " is missing: a transfer names its file on the machine of the terminals."}
	}
	named, err := connect.DecodePath(value)
	if err != nil {
		return "", &result{outcome: failed, err: err, text: err.Error() + "."}
	}
	path, err := absolutePath(named)
	if err != nil {
		return "", &result{outcome: failed, err: err, text: pathRefusal(named, err)}
	}
	return path, nil
}

// errStopping is the error of a transfer cut because ratd stops (see cutOnStop).
var errStopping = errors.New("cut: ratd stopping")

// cutError returns err, a transfer's connection failing, as errStopping when ratd stops.
func (d *daemon) cutError(err error) error {
	select {
	case <-d.stopping:
		return errStopping
	default:
		return err
	}
}

// targetError returns err, of a file operation of an upload to path, naming path rather than the
// temporary file: the agent never sees it.
func targetError(err error, path string) error {
	var pathErr *fs.PathError
	if errors.As(err, &pathErr) {
		return &fs.PathError{Op: pathErr.Op, Path: path, Err: pathErr.Err}
	}
	var linkErr *os.LinkError
	if errors.As(err, &linkErr) {
		return &fs.PathError{Op: linkErr.Op, Path: path, Err: linkErr.Err}
	}
	return err
}

// linkRefusal refuses a transfer of path, a symbolic link: a transfer copies files, and never
// follows a link at the end of its path (the links among the directories above are followed, as
// any program does). A rename would replace the link itself, where write_file follows it, and
// both would surprise. The text tells the path the link leads to, every link resolved, for the
// agent to use it; for a link leading nowhere, where it leads as written.
func linkRefusal(path string, way direction) *result {
	r := &result{outcome: failed, err: errSymbolicLink}
	real, err := filepath.EvalSymlinks(path)
	if err == nil {
		r.text = fmt.Sprintf("%s is a symbolic link to %s, which transfers do not follow: use that path instead.",
			path, real)
		return r
	}
	target, readErr := os.Readlink(path)
	if readErr != nil {
		r.text = fmt.Sprintf("%s is a symbolic link, which transfers do not follow.", path)
		return r
	}
	if !filepath.IsAbs(target) {
		target = filepath.Join(filepath.Dir(path), target)
	}
	// links looping: EvalSymlinks tells it too many links, the system ELOOP
	_, statErr := os.Stat(path)
	switch {
	case errors.Is(statErr, syscall.ELOOP):
		r.text = fmt.Sprintf("%s is a symbolic link to %s, a loop of links leading to no file.", path, target)
	case !errors.Is(err, fs.ErrNotExist):
		r.text = fmt.Sprintf("%s is a symbolic link to %s, which transfers do not follow: use that path instead.",
			path, target)
	case way == upload:
		r.text = fmt.Sprintf("%s is a symbolic link to %s, which does not exist: transfers do not follow links, "+
			"upload to that path to create the file.", path, target)
	default:
		r.text = fmt.Sprintf("%s is a symbolic link to %s, which does not exist: there is no file to download.",
			path, target)
	}
	return r
}

// transferNotRegular refuses a transfer of path, described by info, which is not a regular file:
// a directory, a FIFO (it would block, and never end), a device (/dev/zero never ends).
func transferNotRegular(path string, info fs.FileInfo, way direction) *result {
	mode := info.Mode()
	if mode.IsDir() {
		text := fmt.Sprintf("%s is a directory, not a file.", path)
		if way == download {
			text = fmt.Sprintf("%s is a directory, not a file: transfers copy one file, archive it first (tar).", path)
		}
		return &result{outcome: failed, err: syscall.EISDIR, text: text}
	}
	return &result{outcome: failed, err: fmt.Errorf("not a regular file: %s", mode.Type()), text: fmt.Sprintf(
		"%s is not a regular file (%s): transfers only copy regular files.", path, kindText(mode))}
}

// upload writes the file of req at the path it names, all or nothing: into a temporary file next
// to it, put in its place once complete (see placeFile), the file replaced keeping its own mode.
// What the headers alone refuse is refused before the client sends any byte, when it waits for
// ratd's go-ahead (Expect: 100-continue): a file existing without Rat-Overwrite, a symbolic link,
// what is not a regular file, a path the system fails to look at (a name too long), a body that is
// not bytes. A body cut short (the client failing it, its file having changed, or the connection
// lost) leaves no file. Missing directories are created, as write_file does, and named in the
// answer, a 201 or a failure leaving them, for an agent to catch a typo.
func (d *daemon) upload(w http.ResponseWriter, req *http.Request) result {
	// No Content-Type reads as bytes (RFC 9110). Another one is refused: a client sending a form
	// (curl -d @file strips the new lines of the file) must find out.
	if declared := req.Header.Get("Content-Type"); declared != "" {
		if media, _, err := mime.ParseMediaType(declared); err != nil || media != connect.OctetStream {
			return refuse(w, http.StatusUnsupportedMediaType, result{err: connect.ErrHeader, text: fmt.Sprintf(
				"Content-Type %q: files travel as %s.", declared, connect.OctetStream)})
		}
	}
	path, refused := transferPath(req)
	if refused != nil {
		return refuse(w, http.StatusBadRequest, *refused)
	}
	attrs := []any{"path", path}
	overwrite, err := connect.ParseOverwrite(req.Header.Get(connect.HeaderOverwrite))
	if err != nil {
		return refuse(w, http.StatusBadRequest, result{err: err, attrs: attrs, text: err.Error() + "."})
	}
	mode, err := connect.ParseMode(req.Header.Get(connect.HeaderMode))
	if err != nil {
		return refuse(w, http.StatusBadRequest, result{err: err, attrs: attrs, text: err.Error() + "."})
	}
	created, refused := d.createDirectories(filepath.Dir(path))
	if refused != nil {
		return refuse(w, transferStatus(refused.err), withAttrs(*refused, attrs))
	}
	// From now on, a failure names the directories created, which it leaves, as the answer of a
	// file created does: an agent catches a typo the same way.
	fail := func(status int, r result) result {
		if len(created) > 0 {
			r.text += " " + createdText(created)
		}
		return refuse(w, status, r)
	}
	writeFailure := func(err error) result {
		err = targetError(err, path)
		return fail(transferStatus(err), withAttrs(d.fileFailure(err, "Writing "+path, written, false), attrs))
	}
	// The place of the file is checked now, before any byte is sent, and again once they all came.
	// A failure to look is refused now too, rather than met when placing the file, once all the
	// bytes came: the temporary file, under a name of its own, may not meet it (a name too long,
	// the temporary one cut to fit: see package tempname). It is looked at once its directories
	// are there: under one missing, the system stops at it, telling the file missing whatever its
	// name. Directories are only created for a file that can not exist yet, so only a failure to
	// look can refuse it then, leaving them, as a write failing later does.
	switch info, err := os.Lstat(path); {
	case err == nil:
		if r := checkReplaced(path, info, overwrite); r != nil {
			return fail(refusalStatus(r), withAttrs(*r, attrs))
		}
	case !errors.Is(err, fs.ErrNotExist):
		return writeFailure(err)
	}
	tmp, err := createTemporary(path, mode)
	if err != nil {
		return writeFailure(err)
	}
	// Whatever happens next, the temporary file goes: put in place, it is already gone, or left
	// as a second name of the file (see placeFile).
	defer func() {
		if err := os.Remove(tmp.Name()); err != nil && !errors.Is(err, fs.ErrNotExist) {
			d.logger.Warn("failed to remove the temporary file of an upload", "path", tmp.Name(), "error", err)
		}
	}()
	// The mode of a file created, the umask of ratd taken, which the answer tells, rather than what
	// a look at the file once placed finds: a file moved at once (a watcher of a spool directory)
	// would leave it untold, and one replaced meanwhile would tell the mode of another.
	tmpInfo, err := tmp.Stat()
	if err != nil {
		_ = tmp.Close()
		return writeFailure(err)
	}
	// Reading the body sends the go-ahead the client waits for.
	body := &readErrors{r: req.Body}
	n, err := io.Copy(tmp, body)
	// a network file system may only report a failed write when closing
	if closeErr := tmp.Close(); err == nil {
		err = closeErr
	}
	attrs = append(attrs, "bytes", n)
	switch {
	case body.err != nil:
		// The client cut its body, rat failing it on purpose (its file changed, a cancel), or the
		// connection was lost, or ratd stops (see cutOnStop): nobody may be left to read an answer.
		return fail(http.StatusBadRequest, result{err: d.cutError(body.err), attrs: attrs,
			text: fmt.Sprintf("The upload to %s was cut before its end: nothing was written.", path)})
	case err != nil:
		return writeFailure(err)
	}
	replaced, kept, refused, err := placeFile(tmp.Name(), path, overwrite)
	switch {
	case refused != nil:
		return fail(refusalStatus(refused), withAttrs(*refused, attrs))
	case err != nil:
		return writeFailure(err)
	}
	modeSet := tmpInfo.Mode()
	if replaced {
		modeSet = kept
	}
	w.Header().Set(connect.HeaderMode, connect.FormatMode(modeSet))
	attrs = append(attrs, "replaced", replaced, "directories_created", len(created))
	if replaced {
		w.WriteHeader(http.StatusNoContent)
		return result{attrs: append(attrs, "status", http.StatusNoContent)}
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.WriteHeader(http.StatusCreated)
	if len(created) > 0 {
		_, _ = io.WriteString(w, createdText(created))
	}
	return result{attrs: append(attrs, "status", http.StatusCreated)}
}

// checkReplaced refuses an upload replacing the file at path, described by info (from Lstat): a
// symbolic link, what is not a regular file, or a regular file without overwrite.
func checkReplaced(path string, info fs.FileInfo, overwrite bool) *result {
	switch {
	case info.Mode()&fs.ModeSymlink != 0:
		return linkRefusal(path, upload)
	case !info.Mode().IsRegular():
		return transferNotRegular(path, info, upload)
	case !overwrite:
		r := existsRefusal(path)
		return &r
	}
	return nil
}

// existsRefusal refuses an upload to path, an existing file, without overwrite.
func existsRefusal(path string) result {
	return result{outcome: failed, err: fs.ErrExist, text: fmt.Sprintf("%s exists: set overwrite to replace it.", path)}
}

// refusalStatus returns the HTTP status of r, a refusal of checkReplaced: 412 for a file existing
// without overwrite, the precondition of the request failing, 409 for what can not be replaced.
func refusalStatus(r *result) int {
	if errors.Is(r.err, fs.ErrExist) {
		return http.StatusPreconditionFailed
	}
	return http.StatusConflict
}

// readErrors reads r, keeping the error it failed with, other than its end: an upload tells a
// body cut short from a write failing.
type readErrors struct {
	r   io.Reader
	err error
}

func (e *readErrors) Read(p []byte) (int, error) {
	n, err := e.r.Read(p)
	if err != nil && err != io.EOF {
		e.err = err
	}
	return n, err
}

// createTemporary creates the temporary file of an upload to path: next to it, for the rename to
// stay within its file system, hidden and named after it (.NAME.rat-RANDOM, see package tempname),
// with the permission bits of mode minus the umask, as any new file. A ratd killed during an
// upload leaves it behind.
func createTemporary(path string, mode fs.FileMode) (*os.File, error) {
	dir, name := filepath.Split(path)
	for {
		file, err := os.OpenFile(filepath.Join(dir, tempname.For(name)), os.O_WRONLY|os.O_CREATE|os.O_EXCL,
			mode.Perm())
		if !errors.Is(err, fs.ErrExist) {
			return file, err
		}
	}
}

// linkFile is os.Link, a variable for tests needing a file system without hard links.
var linkFile = os.Link

// placeFile puts tmp, a complete temporary file next to path, in its place, and tells whether it
// replaced a file. A file found there is replaced with overwrite only, and keeps its mode, tmp
// taking it first, returned as kept; checkReplaced refuses the others. Without a file there, tmp
// is linked under its name, which fails if a file appeared since it was checked, where a rename
// would replace it; that file is then looked at again. On a file system without hard links (FAT,
// some network ones), it is renamed: a file appearing in between is then replaced. On success, tmp
// may remain as a second name of the file, for the caller to remove.
func placeFile(tmp, path string, overwrite bool) (replaced bool, kept fs.FileMode, refused *result, err error) {
	for range 2 {
		info, err := os.Lstat(path)
		switch {
		case err == nil:
			if r := checkReplaced(path, info, overwrite); r != nil {
				return false, 0, r, nil
			}
			kept = info.Mode() & (fs.ModePerm | fs.ModeSetuid | fs.ModeSetgid | fs.ModeSticky)
			if err = os.Chmod(tmp, kept); err != nil {
				return false, 0, nil, err
			}
			return true, kept, nil, os.Rename(tmp, path)
		case !errors.Is(err, fs.ErrNotExist):
			return false, 0, nil, err
		}
		err = linkFile(tmp, path)
		switch {
		case err == nil:
			return false, 0, nil, nil
		case errors.Is(err, fs.ErrExist):
			continue
		case errors.Is(err, syscall.EPERM), errors.Is(err, syscall.EMLINK), errors.Is(err, errors.ErrUnsupported):
			// a file system without hard links: FAT refuses with EPERM, FUSE without link (sshfs)
			// with ENOSYS, others with ENOTSUP or EOPNOTSUPP, the three told by ErrUnsupported
			return false, 0, nil, os.Rename(tmp, path)
		default:
			return false, 0, nil, err
		}
	}
	r := existsRefusal(path)
	return false, 0, &r, nil
}

// downloadOpened is called with the path of a download once its file is opened and checked: a
// hook for tests, which change the file there.
var downloadOpened = func(string) {}

// download sends the file at the path req names, as it was when opened: its size then is told
// (Rat-Size), and that many bytes sent, so that a file growing forever (a log) still ends. The
// body is chunked: once sent, the file is stamped again (see package stamp), and a file that
// changed while being read, or failed to be, or ended before its size, unchanged (a file the
// system makes as it is read), is told in the Rat-Error trailer, which makes the download fail. A
// file showing no size yet giving bytes is refused before. A symbolic link at the end of the path
// is refused, and so is what is not a regular file, both looked at before opening, as opening
// acts on what is not a regular file: it releases a command waiting to write into a FIFO, whose
// next write then fails once the refusal closes it, and a device may act on it (a tape rewinds).
// The file opened is checked again, as it may have been replaced meanwhile, opened without
// blocking (a FIFO without a writer would block forever).
func (d *daemon) download(w http.ResponseWriter, req *http.Request) result {
	path, refused := transferPath(req)
	if refused != nil {
		return refuse(w, http.StatusBadRequest, *refused)
	}
	attrs := []any{"path", path}
	// a failure to look at it is left to the open, which tells it
	if info, err := os.Lstat(path); err == nil {
		switch {
		case info.Mode()&fs.ModeSymlink != 0:
			return refuse(w, http.StatusConflict, withAttrs(*linkRefusal(path, download), attrs))
		case !info.Mode().IsRegular():
			return refuse(w, http.StatusConflict, withAttrs(*transferNotRegular(path, info, download), attrs))
		}
	}
	// O_NOFOLLOW refuses a link as the last part of the path only (ELOOP), following those above
	file, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if errors.Is(err, syscall.ELOOP) {
		if info, lerr := os.Lstat(path); lerr == nil && info.Mode()&fs.ModeSymlink != 0 {
			return refuse(w, http.StatusConflict, withAttrs(*linkRefusal(path, download), attrs))
		}
	}
	if err != nil {
		return refuse(w, transferStatus(err), withAttrs(d.fileFailure(err, "Reading "+path, read, false), attrs))
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return refuse(w, transferStatus(err), withAttrs(d.fileFailure(err, "Reading "+path, read, false), attrs))
	}
	if !info.Mode().IsRegular() {
		return refuse(w, http.StatusConflict, withAttrs(*transferNotRegular(path, info, download), attrs))
	}
	downloadOpened(path)
	opened := stamp.Of(info)
	// A file showing no size may hold bytes yet: one the system makes as it is read (/proc,
	// cgroups), which a copy of its size would send empty, told complete. A read tells it apart
	// from a file truly empty before anything is sent; a file filling meanwhile changed.
	if info.Size() == 0 {
		n, err := file.Read(make([]byte, 1))
		switch {
		case n > 0:
			switch changed, lookFailed := d.lookedAgain(file, path, opened); {
			case lookFailed != nil:
				return refuse(w, transferStatus(lookFailed.err), withAttrs(*lookFailed, attrs))
			case changed:
				return refuse(w, http.StatusConflict, withAttrs(changedRefusal(path), attrs))
			}
			return refuse(w, http.StatusConflict, withAttrs(noTrueSizeRefusal(path), attrs))
		case err != nil && !errors.Is(err, io.EOF):
			return refuse(w, transferStatus(err), withAttrs(d.fileFailure(err, "Reading "+path, read, false), attrs))
		}
	}
	header := w.Header()
	header.Set("Content-Type", connect.OctetStream)
	header.Set(connect.HeaderMode, connect.FormatMode(info.Mode()))
	header.Set(connect.HeaderSize, strconv.FormatInt(info.Size(), 10))
	header.Set("Trailer", connect.HeaderError)
	w.WriteHeader(http.StatusOK)
	attrs = append(attrs, "status", http.StatusOK)
	if req.Method == http.MethodHead {
		return result{attrs: append(attrs, "bytes", 0)}
	}
	source := &readErrors{r: file}
	n, err := io.CopyN(w, source, info.Size())
	attrs = append(attrs, "bytes", n)
	var failure result
	switch {
	case source.err != nil:
		failure = d.fileFailure(source.err, "Reading "+path, read, false)
	case errors.Is(err, io.EOF):
		// shorter than when opened: truncated or replaced in place while being read, or, unchanged,
		// a file the system makes as it is read, its size telling nothing of its content (/sys)
		switch changed, lookFailed := d.lookedAgain(file, path, opened); {
		case lookFailed != nil:
			failure = *lookFailed
		case changed:
			failure = changedRefusal(path)
		default:
			failure = noTrueSizeRefusal(path)
		}
	case err != nil:
		// writing failed: the client left, or ratd stops (see cutOnStop); nobody reads a trailer
		return result{outcome: failed, err: d.cutError(err), attrs: attrs}
	default:
		switch changed, lookFailed := d.lookedAgain(file, path, opened); {
		case lookFailed != nil:
			failure = *lookFailed
		case changed:
			failure = changedRefusal(path)
		}
	}
	if failure.text == "" {
		return result{attrs: attrs}
	}
	header.Set(connect.HeaderError, connect.EncodeError(failure.text))
	failure.outcome, failure.attrs = failed, attrs
	return failure
}

// statOnceRead looks at the file of a download once read: os.File.Stat, a variable for tests
// making the look fail.
var statOnceRead = (*os.File).Stat

// lookedAgain looks at file, opened at path with the stamp opened, once read, and tells whether it
// changed since. A look failing is told as a read failing (lookFailed), with the reason of the
// system, rather than as a change the file may not have.
func (d *daemon) lookedAgain(file *os.File, path string, opened stamp.Stamp) (changed bool, lookFailed *result) {
	now, err := statOnceRead(file)
	if err != nil {
		r := d.fileFailure(err, "Reading "+path, read, false)
		return false, &r
	}
	return !stamp.Of(now).Equal(opened), nil
}

// changedRefusal fails the download of path, changed while it was read.
func changedRefusal(path string) result {
	return result{outcome: failed, err: errChanged, text: fmt.Sprintf("%s changed while it was read: it may still "+
		"be written. Download it again once it is complete.", path)}
}

// noTrueSizeRefusal fails the download of path, a file whose size is not that of its content: one
// the system makes as it is read (/proc shows 0, /sys a page), which a download, sending the size
// a file had when opened, can not copy, and read_file reads to its end.
func noTrueSizeRefusal(path string) result {
	return result{outcome: failed, err: errNoTrueSize, text: fmt.Sprintf("%s tells a size that is not that of its "+
		"content: a file the system makes as it is read, as in /proc and /sys. Read it with %s if it is text, or "+
		"copy it in a terminal (cat %s > ~/copy) and download the copy.", path, tools.ReadFile, shellWord(path))}
}

// shellWord returns s quoted for bash, between single quotes: a path for a command to copy as is
// into a terminal, spaces and all.
func shellWord(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}
