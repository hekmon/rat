package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"math"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/hekmon/rat/cmd/ratd/tools"
	"github.com/hekmon/rat/connect"
	"github.com/hekmon/rat/internal/tempname"

	"github.com/google/jsonschema-go/jsonschema"
	"github.com/hekmon/cunits/v3"
)

// The file transfers of rat copy files by path between the transfers directory, on the machine
// of the harness, and the machine of ratd, their bytes never reaching a model: rat offers
// upload_file and download_file when started with --transfers-dir, and runs them itself, over
// the transfer endpoints of ratd (see package connect). See File transfers in the README.

// Names of the tools of rat, declared with those of ratd, which its copy hints name.
const (
	uploadTool   = tools.UploadFile
	downloadTool = tools.DownloadFile
)

// replaceCopyHints returns body, an answer of ratd carrying its texts (the instructions, the
// descriptions of its tools), with the sentences naming the tools of rat in place of its generic
// copy hints (see tools.CopyHints): found as they are, none of their characters being one JSON
// escapes, and the rest of body untouched. A ratd of another version, whose hints differ, keeps
// its generic ones, still true.
func replaceCopyHints(body []byte) []byte {
	for _, hint := range tools.CopyHints() {
		body = bytes.ReplaceAll(body, []byte(hint.Generic), []byte(hint.Direct))
	}
	return body
}

// transferWait is how long a call of a transfer tool waits for its transfer: the most wait_window
// waits (tools.MaxWaitSeconds), several harnesses cutting a tool call at 60 seconds. A longer
// transfer goes on, the call telling its progress. A variable for tests.
var transferWait = time.Duration(tools.MaxWaitSeconds) * time.Second

const (
	// rateWindow is how far back the rate of a transfer is measured: recent enough for a link that
	// slowed down to show.
	rateWindow = 30 * time.Second
	// rateSamples is how many samples of the bytes moved a transfer keeps, one a second at most.
	rateSamples = 64
)

// uploadInput is the input of upload_file.
type uploadInput struct {
	LocalPath  string `json:"local_path" jsonschema:"the file to copy, relative to the transfers directory"`
	RemotePath string `json:"remote_path" jsonschema:"where to write it on the machine of the terminals: absolute, or starting with ~/"`
	Overwrite  bool   `json:"overwrite,omitempty" jsonschema:"true replaces an existing file; false, the default, refuses it"`
	Cancel     bool   `json:"cancel,omitempty" jsonschema:"true stops the transfer of these paths under way, leaving the destination as it was"`
}

// downloadInput is the input of download_file.
type downloadInput struct {
	RemotePath string `json:"remote_path" jsonschema:"the file to copy from the machine of the terminals: absolute, or starting with ~/"`
	LocalPath  string `json:"local_path" jsonschema:"where to write it, relative to the transfers directory"`
	Overwrite  bool   `json:"overwrite,omitempty" jsonschema:"true replaces an existing file; false, the default, refuses it"`
	Cancel     bool   `json:"cancel,omitempty" jsonschema:"true stops the transfer of these paths under way, leaving the destination as it was"`
}

// transferDescription tells what both tools do, after their first sentence: dir is the transfers
// directory, and other the side whose permissions a new file takes.
const transferDescription = "Its content never enters your context, whatever its size, binary included. local_path " +
	"is relative to %s, the only directory rat copies files from and to on your machine; remote_path is absolute or " +
	"starts with ~/. Missing directories are created. An existing file is replaced only with overwrite, and keeps " +
	"its permissions; a new one gets those of %s. A symbolic link at the end of a path is refused, naming its target, " +
	"and a file changing while it is copied fails the transfer: copy it once complete. The call waits up to %d " +
	"seconds: a longer copy goes on in the background, the call telling its progress, and calling again with the " +
	"same paths waits for the rest, or tells how it ended; cancel stops it, leaving the destination as it was."

// toolDefinition is a tool as tools/list tells it.
type toolDefinition struct {
	Name        string             `json:"name"`
	Description string             `json:"description"`
	InputSchema *jsonschema.Schema `json:"inputSchema"`
	Annotations map[string]any     `json:"annotations"`
}

// transferRequest is a call of a transfer tool.
type transferRequest struct {
	tool              string
	local, remote     string
	overwrite, cancel bool
}

// upload tells whether the request copies a file to the machine of ratd.
func (q transferRequest) upload() bool {
	return q.tool == uploadTool
}

// source and destination return the paths of the request, as the agent gave them.
func (q transferRequest) source() string {
	if q.upload() {
		return q.local
	}
	return q.remote
}

func (q transferRequest) destination() string {
	if q.upload() {
		return q.remote
	}
	return q.local
}

// key identifies the transfer of the request: its destination, which one transfer at a time
// writes.
func (q transferRequest) key() string {
	return q.tool + "\x00" + q.destination()
}

// transfers runs the file transfers of rat. Each transfer is a job, identified by its destination,
// found again by its paths, as terminals are by name: a call starts it or joins it, and waits for
// it transferWait at most; a longer one goes on, and the same call again waits for the rest, or
// tells how it ended. Jobs are kept in memory, until told or until rat stops, which ends them.
type transfers struct {
	logger *slog.Logger
	client *http.Client
	server string
	// reachText tells why ratd could not be reached, as the relay tells it
	reachText func(error) string
	// dir is the transfers directory, absolute and its links resolved, to which root confines the
	// local paths
	dir  string
	root *os.Root
	// ctx ends the jobs once rat stops (see close): an upload sent whole is cut too, canceled by
	// the agent or not, rather than waited for, its answer read by nobody (see runUpload)
	ctx  context.Context
	stop context.CancelFunc
	// jobsMu guards jobs, the transfers under way or ended and not told yet, by key
	jobsMu sync.Mutex
	jobs   map[string]*job
	// running counts the jobs running, waited for when rat stops: each removes its temporary file
	running sync.WaitGroup
	// definitions are the definitions of the two tools, added to the tool list of ratd
	definitions []json.RawMessage
}

// newTransfers returns the transfers of rat between dir, an existing directory, and the ratd at
// server, through client, reachText telling why ratd could not be reached.
func newTransfers(logger *slog.Logger, client *http.Client, server, dir string, reachText func(error) string) (*transfers, error) {
	absolute, err := filepath.Abs(dir)
	if err == nil {
		absolute, err = filepath.EvalSymlinks(absolute)
	}
	if err != nil {
		return nil, err
	}
	if info, err := os.Stat(absolute); err != nil || !info.IsDir() {
		return nil, fmt.Errorf("%s is not a directory", dir)
	}
	root, err := os.OpenRoot(absolute)
	if err != nil {
		return nil, err
	}
	t := &transfers{logger: logger, client: client, server: server, reachText: reachText, dir: absolute, root: root,
		jobs: map[string]*job{}}
	t.ctx, t.stop = context.WithCancel(context.Background())
	for _, tool := range []struct {
		name, first, other string
		input              reflect.Type
	}{
		{uploadTool, "Copy a file from your filesystem to the machine of the terminals, by path.", "your file",
			reflect.TypeFor[uploadInput]()},
		{downloadTool, "Copy a file from the machine of the terminals to your filesystem, by path.", "the remote file",
			reflect.TypeFor[downloadInput]()},
	} {
		schema, err := jsonschema.ForType(tool.input, &jsonschema.ForOptions{})
		if err != nil {
			return nil, fmt.Errorf("input schema of %s: %w", tool.name, err)
		}
		definition, err := json.Marshal(toolDefinition{
			Name:        tool.name,
			Description: tool.first + " " + fmt.Sprintf(transferDescription, absolute, tool.other, tools.MaxWaitSeconds),
			InputSchema: schema,
			Annotations: map[string]any{"destructiveHint": true, "openWorldHint": false},
		})
		if err != nil {
			return nil, err
		}
		t.definitions = append(t.definitions, definition)
	}
	return t, nil
}

// handles tells whether tool is one of the transfer tools.
func (t *transfers) handles(tool string) bool {
	return tool == uploadTool || tool == downloadTool
}

// close ends the jobs running, cut, and waits for them to remove their temporary files.
func (t *transfers) close() {
	t.stop()
	t.running.Wait()
	_ = t.root.Close()
}

// addTools returns body, the answer of ratd to tools/list, with the transfer tools added to the
// last page of tools; body unchanged when it can not be read.
func (t *transfers) addTools(body []byte) []byte {
	var answer map[string]json.RawMessage
	var result map[string]json.RawMessage
	var list []json.RawMessage
	if json.Unmarshal(body, &answer) != nil || json.Unmarshal(answer["result"], &result) != nil ||
		json.Unmarshal(result["tools"], &list) != nil {
		t.logger.Warn("tools/list answered by ratd unreadable: the transfer tools not added")
		return body
	}
	// a cursor tells more pages: the tools of rat end the last one
	var cursor string
	_ = json.Unmarshal(result["nextCursor"], &cursor)
	if cursor != "" {
		return body
	}
	listed, err := json.Marshal(append(list, t.definitions...))
	if err != nil {
		return body
	}
	result["tools"] = listed
	if answer["result"], err = json.Marshal(result); err != nil {
		return body
	}
	added, err := json.Marshal(answer)
	if err != nil {
		return body
	}
	return added
}

// call runs a call of tool with arguments, and returns the text it tells the agent, and whether
// it is an error. It waits for its transfer until ctx is done (the call canceled, or rat
// stopping), which never cuts the transfer.
func (t *transfers) call(ctx context.Context, tool string, arguments json.RawMessage) (string, bool) {
	q, refused := t.request(tool, arguments)
	if refused != "" {
		return refused, true
	}
	if q.cancel {
		return t.cancel(ctx, q)
	}
	called := time.Now()
	j, refused := t.join(q)
	if refused != "" {
		return refused, true
	}
	return t.await(ctx, j, called)
}

// request reads the call of tool with arguments, or tells why it is refused.
func (t *transfers) request(tool string, arguments json.RawMessage) (transferRequest, string) {
	q := transferRequest{tool: tool}
	var err error
	if tool == uploadTool {
		var in uploadInput
		err = json.Unmarshal(arguments, &in)
		q.local, q.remote, q.overwrite, q.cancel = in.LocalPath, in.RemotePath, in.Overwrite, in.Cancel
	} else {
		var in downloadInput
		err = json.Unmarshal(arguments, &in)
		q.local, q.remote, q.overwrite, q.cancel = in.LocalPath, in.RemotePath, in.Overwrite, in.Cancel
	}
	switch {
	case err != nil:
		return q, fmt.Sprintf("Invalid arguments: %v.", err)
	case q.local == "" || q.remote == "":
		return q, "local_path and remote_path are both required."
	case !filepath.IsLocal(q.local):
		return q, fmt.Sprintf("local_path %q is not within %s: give a path relative to it, without going up (..).",
			q.local, t.dir)
	}
	q.local = filepath.Clean(q.local)
	return q, ""
}

// join returns the job of q: the one under way for its destination, or a new one, started once
// the local file checked. Refused: another source to the same destination, and what the local
// side refuses before any request (see prepare).
func (t *transfers) join(q transferRequest) (*job, string) {
	t.jobsMu.Lock()
	defer t.jobsMu.Unlock()
	if j := t.jobs[q.key()]; j != nil {
		if j.request.source() != q.source() {
			return nil, fmt.Sprintf("A transfer to %s from %s is under way: wait for it with its paths, or stop it with "+
				"cancel.", q.destination(), j.request.source())
		}
		return j, ""
	}
	source, refused := t.prepare(q)
	if refused != "" {
		return nil, refused
	}
	ctx, cancel := context.WithCancel(t.ctx)
	j := &job{request: q, started: time.Now(), cancel: cancel, done: make(chan struct{})}
	t.jobs[q.key()] = j
	t.running.Add(1)
	go t.run(ctx, j, source)
	return j, ""
}

// prepare checks the local side of q before any request: for an upload, the file to send, opened
// and returned; for a download, the file to write, refused if it exists without overwrite. A
// symbolic link at the end of the local path is refused, as ratd refuses one at the end of the
// remote path.
func (t *transfers) prepare(q transferRequest) (*os.File, string) {
	info, err := t.root.Lstat(q.local)
	if q.upload() {
		if err != nil {
			return nil, t.localFailure(err, "Reading", q.local)
		}
		if refused := t.checkLocal(q.local, info, q); refused != "" {
			return nil, refused
		}
		// without blocking: a FIFO replacing the file looked at would block until written to
		file, err := t.root.OpenFile(q.local, os.O_RDONLY|syscall.O_NONBLOCK, 0)
		if err != nil {
			return nil, t.localFailure(err, "Reading", q.local)
		}
		// the file opened may not be the one looked at: replaced meanwhile, by a link or a FIFO
		if opened, err := file.Stat(); err != nil || !os.SameFile(info, opened) {
			_ = file.Close()
			return nil, fmt.Sprintf("%s changed while it was opened: upload it again.", q.local)
		}
		return file, ""
	}
	switch {
	case err == nil:
		return nil, t.checkLocal(q.local, info, q)
	case errors.Is(err, fs.ErrNotExist), errors.Is(err, syscall.ENOTDIR):
		return nil, ""
	default:
		return nil, t.localFailure(err, "Writing", q.local)
	}
}

// checkLocal refuses the local file of q, described by info (from Lstat): a symbolic link, what is
// not a regular file, and a file to replace without overwrite.
func (t *transfers) checkLocal(local string, info fs.FileInfo, q transferRequest) string {
	mode := info.Mode()
	switch {
	case mode&fs.ModeSymlink != 0:
		return t.linkRefusal(local)
	case mode.IsDir():
		return fmt.Sprintf("%s is a directory, not a file.", local)
	case !mode.IsRegular():
		return fmt.Sprintf("%s is not a regular file (%s): transfers only copy regular files.", local, kindText(mode))
	case !q.upload() && !q.overwrite:
		return fmt.Sprintf("%s exists: set overwrite to replace it.", local)
	}
	return ""
}

// linkRefusal refuses local, a symbolic link, telling where it leads within the transfers
// directory, or that it leads out of it.
func (t *transfers) linkRefusal(local string) string {
	real, err := filepath.EvalSymlinks(filepath.Join(t.dir, local))
	if err != nil {
		return fmt.Sprintf("%s is a symbolic link leading nowhere, which transfers do not follow.", local)
	}
	within, err := filepath.Rel(t.dir, real)
	if err != nil || !filepath.IsLocal(within) {
		return fmt.Sprintf("%s is a symbolic link leading out of %s, which transfers do not follow.", local, t.dir)
	}
	return fmt.Sprintf("%s is a symbolic link to %s, which transfers do not follow: use that path instead.", local, within)
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

// localFailure tells an operation on the local file local failing with err, what telling it
// ("Reading"): the reason the system gives, as ratd tells the failures on its side.
func (t *transfers) localFailure(err error, what, local string) string {
	var pathErr *fs.PathError
	if errors.As(err, &pathErr) {
		return fmt.Sprintf("%s %s failed: %v.", what, local, pathErr.Err)
	}
	var linkErr *os.LinkError
	if errors.As(err, &linkErr) {
		return fmt.Sprintf("%s %s failed: %v.", what, local, linkErr.Err)
	}
	return fmt.Sprintf("%s %s failed: %v.", what, local, err)
}

// await waits for j, for a call made at called, until it ends, transferWait passes, or ctx is done,
// and returns what the call tells: how the transfer ended, or its progress. A job ended is told
// once, then forgotten: the same call again starts a new transfer.
func (t *transfers) await(ctx context.Context, j *job, called time.Time) (string, bool) {
	before := j.moved.Load()
	timer := time.NewTimer(transferWait)
	defer timer.Stop()
	select {
	case <-j.done:
	case <-timer.C:
		return t.progressText(j, before), false
	case <-ctx.Done():
		return "", false
	}
	t.forget(j)
	text := j.text
	if j.ended.Before(called) {
		text += fmt.Sprintf(" It ended %s ago.", agoText(time.Since(j.ended)))
	}
	return text, j.isError
}

// forget removes j from the jobs, unless another job took its place.
func (t *transfers) forget(j *job) {
	t.jobsMu.Lock()
	defer t.jobsMu.Unlock()
	if t.jobs[j.request.key()] == j {
		delete(t.jobs, j.request.key())
	}
}

// cancel stops the transfer of the paths of q, and tells how it ended: canceled, the destination
// left as it was, or ended before the cancel could stop it.
func (t *transfers) cancel(ctx context.Context, q transferRequest) (string, bool) {
	t.jobsMu.Lock()
	j := t.jobs[q.key()]
	t.jobsMu.Unlock()
	switch {
	case j == nil:
		return fmt.Sprintf("No transfer to %s is under way: nothing to cancel.", q.destination()), false
	case j.request.source() != q.source():
		return fmt.Sprintf("The transfer to %s under way is from %s, not %s: nothing canceled.", q.destination(),
			j.request.source(), q.source()), true
	}
	j.askCancel()
	timer := time.NewTimer(transferWait)
	defer timer.Stop()
	select {
	case <-j.done:
	case <-timer.C:
		return fmt.Sprintf("The transfer to %s is still ending: call %s again with the same paths to know how it "+
			"ended.", q.destination(), q.tool), true
	case <-ctx.Done():
		return "", false
	}
	t.forget(j)
	if !j.canceled {
		return "Too late to cancel: " + j.text, j.isError
	}
	return j.text, j.isError
}

// job is a transfer under way, or ended and not told yet.
type job struct {
	request transferRequest
	started time.Time
	// cancel cuts the transfer
	cancel context.CancelFunc
	// moved counts the bytes moved; size is the size of the file, once known
	moved, size atomic.Int64
	// mu guards the fields below
	mu sync.Mutex
	// samples are the bytes moved over time, one a second at most, for the rate
	samples []sample
	// cancelAsked tells a cancel was asked; placing that a download is being put in place, too
	// late to cancel
	cancelAsked, placing bool
	// done is closed once the transfer ended, its outcome set: what it tells, whether it is an
	// error, whether a cancel stopped it, and when it ended
	done     chan struct{}
	text     string
	isError  bool
	canceled bool
	ended    time.Time
}

// sample is the bytes a transfer moved, at a moment.
type sample struct {
	at    time.Time
	bytes int64
}

// count adds n bytes moved.
func (j *job) count(n int) {
	moved := j.moved.Add(int64(n))
	now := time.Now()
	j.mu.Lock()
	defer j.mu.Unlock()
	if len(j.samples) > 0 && now.Sub(j.samples[len(j.samples)-1].at) < time.Second {
		return
	}
	if len(j.samples) == rateSamples {
		j.samples = append(j.samples[:0], j.samples[1:]...)
	}
	j.samples = append(j.samples, sample{at: now, bytes: moved})
}

// rate returns the bytes moved per second over about rateWindow, as the samples of j tell it: from
// the oldest one within rateWindow, since its first bytes for a transfer younger; from the last one
// when none is within it, near nothing for a transfer stalled since, rather than its average since
// its start.
func (j *job) rate(now time.Time) float64 {
	moved := j.moved.Load()
	j.mu.Lock()
	defer j.mu.Unlock()
	// the start, with no sample: nothing moved then
	from := sample{at: j.started}
	for _, s := range j.samples {
		from = s
		if now.Sub(s.at) <= rateWindow {
			break
		}
	}
	if elapsed := now.Sub(from.at).Seconds(); elapsed > 0 {
		return float64(moved-from.bytes) / elapsed
	}
	return 0
}

// askCancel asks the transfer to stop, unless a download is being put in place already.
func (j *job) askCancel() {
	j.mu.Lock()
	defer j.mu.Unlock()
	if !j.placing {
		j.cancelAsked = true
		j.cancel()
	}
}

// beginPlacing tells whether a download may be put in place, no cancel having been asked; once
// it returns true, a cancel is too late.
func (j *job) beginPlacing() bool {
	j.mu.Lock()
	defer j.mu.Unlock()
	if j.cancelAsked {
		return false
	}
	j.placing = true
	return true
}

// cancelWasAsked tells whether a cancel was asked.
func (j *job) cancelWasAsked() bool {
	j.mu.Lock()
	defer j.mu.Unlock()
	return j.cancelAsked
}

// run runs j, the transfer of source for an upload, until it ends or ctx is done, and sets its
// outcome.
func (t *transfers) run(ctx context.Context, j *job, source *os.File) {
	defer t.running.Done()
	defer j.cancel()
	var text string
	var isError, canceled bool
	if j.request.upload() {
		text, isError, canceled = t.runUpload(ctx, j, source)
	} else {
		text, isError, canceled = t.runDownload(ctx, j)
	}
	j.text, j.isError, j.canceled, j.ended = text, isError, canceled, time.Now()
	outcome := "ok"
	switch {
	case canceled:
		outcome = "canceled"
	case isError:
		outcome = "error"
	}
	t.logger.Info("transfer", "direction", j.request.tool, "local", j.request.local, "remote", j.request.remote,
		"outcome", outcome, "bytes", j.moved.Load(), "duration", time.Since(j.started))
	close(j.done)
}

// canceledText tells j canceled, its destination left as it was.
func canceledText(j *job) string {
	text := fmt.Sprintf("Canceled the copy of %s to %s after %s", j.request.source(), j.request.destination(),
		sizeText(j.moved.Load()))
	if size := j.size.Load(); size > 0 {
		text += " of " + sizeText(size)
	}
	return text + fmt.Sprintf(": %s is as it was.", j.request.destination())
}

// failureText tells j failing with err: its answer lost once all of it was sent, ratd possibly
// having written it, a cancel coming then too late to cut it; canceled (whatever the error the cut
// gave); refused or failed by ratd (its sentence); ratd older than the transfers or not reached;
// the file changed while read, or failing to read, as the system tells it; the copy cut.
func (t *transfers) failureText(j *job, err error) (string, bool, bool) {
	var refused *connect.Refusal
	var failed *connect.TransferError
	switch {
	case errors.Is(err, connect.ErrAnswerLost) && errors.As(err, &failed):
		return fmt.Sprintf("The answer of ratd to the copy of %[1]s to %[2]s was lost once all of it was sent "+
			"(%[3]v): %[2]s may be written or not. Check it before using it, or call %[4]s again with overwrite.",
			j.request.source(), j.request.destination(), failed.Reason, j.request.tool), true, false
	case j.cancelWasAsked():
		return canceledText(j), false, true
	case errors.As(err, &refused):
		return refused.Text, true, false
	case errors.Is(err, connect.ErrNoTransfers):
		return fmt.Sprintf("The ratd at %s is older than this rat and serves no file transfers: whoever runs it must "+
			"upgrade it.", t.server), true, false
	case errors.Is(err, connect.ErrChanged):
		return fmt.Sprintf("%s changed while it was read: it may still be written. Upload it again once it is "+
			"complete.", j.request.local), true, false
	case errors.Is(err, connect.ErrRead) && errors.As(err, &failed):
		return t.localFailure(failed.Reason, "Reading", j.request.local) + fmt.Sprintf(" %s is as it was.",
			j.request.destination()), true, false
	case errors.Is(err, connect.ErrCut) && errors.As(err, &failed):
		return fmt.Sprintf("The copy of %s to %s was cut: %v. %s is as it was.", j.request.source(),
			j.request.destination(), failed.Reason, j.request.destination()), true, false
	default:
		return t.reachText(err), true, false
	}
}

// countedFile counts the bytes read from the file uploaded by its job.
type countedFile struct {
	*os.File
	job *job
}

func (f countedFile) Read(p []byte) (int, error) {
	n, err := f.File.Read(p)
	f.job.count(n)
	return n, err
}

// runUpload uploads source, the local file of j, and tells how it ended. ctx, which a cancel of the
// agent ends, cancels the upload, which waits for the answer of ratd once sent whole, while rat
// stopping cuts it whatever was sent (see connect.Upload).
func (t *transfers) runUpload(ctx context.Context, j *job, source *os.File) (string, bool, bool) {
	defer source.Close()
	info, err := source.Stat()
	if err != nil {
		return t.localFailure(err, "Reading", j.request.local), true, false
	}
	j.size.Store(info.Size())
	up, err := connect.Upload(t.ctx, ctx.Done(), t.client, t.server, countedFile{File: source, job: j},
		j.request.remote, j.request.overwrite)
	if err != nil {
		return t.failureText(j, err)
	}
	how := "created"
	if up.Replaced {
		how = "replaced"
	}
	text := fmt.Sprintf("Uploaded %s to %s (%s): %s.", j.request.local, j.request.remote, sizeText(up.Sent), how)
	if up.Replaced && up.Mode != info.Mode().Perm() {
		text += fmt.Sprintf(" It kept its permissions (%s), not those of your file (%s): change them in a terminal "+
			"(chmod) if it needs yours.", connect.FormatMode(up.Mode), connect.FormatMode(info.Mode()))
	}
	if up.Note != "" {
		text += " " + up.Note
	}
	return text, false, false
}

// countedWriter counts the bytes written to the file a download of its job writes.
type countedWriter struct {
	w   io.Writer
	job *job
}

func (w countedWriter) Write(p []byte) (int, error) {
	n, err := w.w.Write(p)
	w.job.count(n)
	return n, err
}

// runDownload downloads the remote file of j to a temporary file next to its local path, put in
// place once complete and no cancel asked, and tells how it ended. Missing directories are
// created, and named.
func (t *transfers) runDownload(ctx context.Context, j *job) (string, bool, bool) {
	local := j.request.local
	download, err := connect.StartDownload(ctx, t.client, t.server, j.request.remote)
	if err != nil {
		return t.failureText(j, err)
	}
	defer download.Close()
	j.size.Store(download.Size)
	created, err := t.createDirectories(filepath.Dir(local))
	if err != nil {
		return t.localFailure(err, "Writing", local), true, false
	}
	tmp, tmpName, err := t.createTemporary(local, download.Mode)
	if err != nil {
		return t.localFailure(err, "Writing", local), true, false
	}
	// Whatever happens next, the temporary file goes: put in place, it is already gone, or left as
	// a second name of the file (see place).
	defer func() {
		if err := t.root.Remove(tmpName); err != nil && !errors.Is(err, fs.ErrNotExist) {
			t.logger.Warn("failed to remove the temporary file of a download", "path", tmpName, "error", err)
		}
	}()
	_, err = download.WriteTo(countedWriter{w: tmp, job: j})
	if closeErr := tmp.Close(); err == nil {
		err = closeErr
	}
	var pathErr *fs.PathError
	switch {
	case errors.As(err, &pathErr):
		// writing the local file failed
		return t.localFailure(err, "Writing", local), true, false
	case err != nil:
		return t.failureText(j, err)
	case !j.beginPlacing():
		return canceledText(j), false, true
	}
	replaced, kept, refused, err := t.place(tmpName, local, j.request.overwrite)
	switch {
	case refused != "":
		return refused, true, false
	case err != nil:
		return t.localFailure(err, "Writing", local), true, false
	}
	how := "created"
	if replaced {
		how = "replaced"
	}
	text := fmt.Sprintf("Downloaded %s to %s (%s): %s.", j.request.remote, local, sizeText(j.moved.Load()), how)
	if replaced && kept != download.Mode {
		text += fmt.Sprintf(" It kept its permissions (%s), not those of the remote file (%s).",
			connect.FormatMode(kept), connect.FormatMode(download.Mode))
	}
	if len(created) > 0 {
		text += fmt.Sprintf(" Missing directories created in %s: %s. Check the path if you did not mean them.", t.dir,
			strings.Join(created, ", "))
	}
	return text, false, false
}

// createDirectories creates dir, within the transfers directory, and its missing parents, and
// returns the ones it created, from the top.
func (t *transfers) createDirectories(dir string) ([]string, error) {
	var created []string
	for parent := dir; parent != "."; parent = filepath.Dir(parent) {
		if _, err := t.root.Stat(parent); err == nil {
			break
		}
		created = append([]string{parent}, created...)
	}
	if len(created) == 0 {
		return nil, nil
	}
	return created, t.root.MkdirAll(dir, 0o755)
}

// createTemporary creates the temporary file of a download to local: next to it, hidden and named
// after it (.NAME.rat-RANDOM, see package tempname), as ratd names its own, with the permission
// bits of mode minus the umask, as any new file. It returns the file and its name within the
// transfers directory: the name of a file opened through the root is its full path, which the root
// refuses as escaping it.
func (t *transfers) createTemporary(local string, mode fs.FileMode) (*os.File, string, error) {
	dir, name := filepath.Split(local)
	for {
		tmpName := filepath.Join(dir, tempname.For(name))
		file, err := t.root.OpenFile(tmpName, os.O_WRONLY|os.O_CREATE|os.O_EXCL, mode.Perm())
		if !errors.Is(err, fs.ErrExist) {
			return file, tmpName, err
		}
	}
}

// place puts tmp, a complete temporary file next to local, in its place, as ratd places an upload
// (see placeFile in cmd/ratd): a file found there is replaced with overwrite only, keeping its
// mode, returned; otherwise tmp is linked under its name, which fails if a file appeared since it
// was checked, looked at again then; on a file system without hard links, renamed. refused tells
// what is found there and not replaced.
func (t *transfers) place(tmp, local string, overwrite bool) (replaced bool, kept fs.FileMode, refused string, err error) {
	q := transferRequest{tool: downloadTool, local: local, overwrite: overwrite}
	for range 2 {
		info, err := t.root.Lstat(local)
		switch {
		case err == nil:
			if refused := t.checkLocal(local, info, q); refused != "" {
				return false, 0, refused, nil
			}
			mode := info.Mode() & (fs.ModePerm | fs.ModeSetuid | fs.ModeSetgid | fs.ModeSticky)
			if err = t.root.Chmod(tmp, mode); err != nil {
				return false, 0, "", err
			}
			return true, mode.Perm(), "", t.root.Rename(tmp, local)
		case !errors.Is(err, fs.ErrNotExist):
			return false, 0, "", err
		}
		err = t.root.Link(tmp, local)
		switch {
		case err == nil:
			return false, 0, "", nil
		case errors.Is(err, fs.ErrExist):
			continue
		case errors.Is(err, syscall.EPERM), errors.Is(err, syscall.ENOTSUP), errors.Is(err, syscall.EOPNOTSUPP),
			errors.Is(err, syscall.EMLINK):
			return false, 0, "", t.root.Rename(tmp, local)
		default:
			return false, 0, "", err
		}
	}
	return false, 0, fmt.Sprintf("%s exists: set overwrite to replace it.", local), nil
}

// progressText tells the progress of j, still under way after a wait during which before bytes had
// moved: the bytes moved of the size, the rate and the time left, or that nothing moved.
func (t *transfers) progressText(j *job, before int64) string {
	moved, size := j.moved.Load(), j.size.Load()
	text := fmt.Sprintf("Still copying %s to %s: %s", j.request.source(), j.request.destination(), sizeText(moved))
	if size > 0 {
		text += " of " + sizeText(size)
	}
	if moved == before {
		return text + fmt.Sprintf(", and nothing moved in the last %d seconds. Call %s again with the same paths to "+
			"keep waiting, or with cancel to stop it.", int(transferWait.Round(time.Second).Seconds()), j.request.tool)
	}
	rate := j.rate(time.Now())
	text += fmt.Sprintf(", %s/s", sizeText(int64(rate)))
	if size > moved && rate > 0 {
		text += ", " + leftText(time.Duration(float64(size-moved)/rate*float64(time.Second))) + " left"
	}
	return text + fmt.Sprintf(". The copy goes on while you do something else: call %s again with the same paths "+
		"for the rest, or with cancel to stop it.", j.request.tool)
}

// leftText tells a time left, coarsely: an estimate from a rate that varies claims no precision.
func leftText(left time.Duration) string {
	switch minutes := int(math.Round(left.Minutes())); {
	case left < time.Minute:
		return "less than a minute"
	case minutes == 1:
		return "about a minute"
	case left < 90*time.Minute:
		return fmt.Sprintf("about %d minutes", minutes)
	default:
		return fmt.Sprintf("about %d hours", int(math.Round(left.Hours())))
	}
}

// agoText tells a duration past, coarsely: seconds under a minute, then minutes, then hours.
func agoText(d time.Duration) string {
	switch {
	case d < time.Second:
		return "under a second"
	case d < time.Minute:
		return fmt.Sprintf("%d seconds", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%d minutes", int(d.Minutes()))
	default:
		return fmt.Sprintf("%d hours", int(d.Hours()))
	}
}

// sizeText tells a size in bytes as ratd tells sizes: in its largest binary unit, with a decimal
// at most (64 KiB, 97.7 KiB, 500 B).
func sizeText(bytes int64) string {
	value, unit := (cunits.Bits(bytes) * cunits.Byte).GetHumanSizeAndSuffix()
	return strconv.FormatFloat(math.Round(value*10)/10, 'f', -1, 64) + " " + unit
}
