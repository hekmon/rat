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
	"path"
	"path/filepath"
	"reflect"
	"runtime"
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

// modesTold tells whether a transfer replacing a file tells the agent it kept permissions other
// than those of its source: not on Windows, whose files have no permission bits (Go shows 0666, or
// 0444 for a read-only file), which almost every file of the other side differs from. A variable
// for tests.
var modesTold = runtime.GOOS != "windows"

// readOnlyKept tells whether a rename leaves a read-only file in place rather than replace it: on
// Windows, whose os.Root.Rename does not ignore the read-only attribute, as its removal does; on
// Unix, the permissions of the directory decide. A variable for tests.
var readOnlyKept = runtime.GOOS == "windows"

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
	InPlace    bool   `json:"in_place,omitempty" jsonschema:"false, the default, copies into a new file put in place of the old one once whole: never partly written, but the disk must hold both meanwhile. true, with overwrite, writes into the file itself: no room needed for a second copy, nor write permission on its directory (the file's own decides), the file keeping its hard links, owner and attributes; but a copy cut or canceled leaves it partly written, harmless for a file nothing uses until it is uploaded whole. A missing file is created as usual. Never into the very file you upload, when the terminals run on your machine: emptied as it is read, it would be lost"`
	Cancel     bool   `json:"cancel,omitempty" jsonschema:"true stops the transfer of these paths under way, leaving the destination as it was (written in place, partly written), unless too late"`
}

// downloadInput is the input of download_file.
type downloadInput struct {
	RemotePath string `json:"remote_path" jsonschema:"the file to copy from the machine of the terminals: absolute, or starting with ~/"`
	LocalPath  string `json:"local_path" jsonschema:"where to write it, relative to the transfers directory"`
	Overwrite  bool   `json:"overwrite,omitempty" jsonschema:"true replaces an existing file; false, the default, refuses it"`
	Cancel     bool   `json:"cancel,omitempty" jsonschema:"true stops the transfer of these paths under way, leaving the destination as it was, unless too late"`
}

// transferDescription tells what both tools do, after their first sentence: dir is the transfers
// directory, other the side whose permissions a new file takes, and inPlace what a cancel leaves
// of a file written in place, which upload_file alone writes.
const transferDescription = "Its content never enters your context, whatever its size, binary included. local_path " +
	"is relative to %s, the only directory rat copies files from and to on your machine; remote_path is absolute or " +
	"starts with ~/. Missing directories are created. An existing file is replaced only with overwrite, and keeps " +
	"its permissions; a new one gets those of %s. A symbolic link at the end of a path is refused, naming its target, " +
	"and a file changing while it is copied fails the transfer: copy it once complete. The call waits up to %d " +
	"seconds: a longer copy goes on in the background, the call telling its progress, and calling again with the " +
	"same paths waits for the rest, or tells how it ended, the copy keeping the parameters it started with: to " +
	"change them, cancel it, then call again. cancel stops it, leaving the destination as it was%s, unless it " +
	"comes too late, as the copy ends."

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
	// inPlace writes an upload into the file it replaces (connect.UploadOptions)
	inPlace bool
}

// upload tells whether the request copies a file to the machine of ratd.
func (q transferRequest) upload() bool {
	return q.tool == uploadTool
}

// writesInPlace tells whether the request writes into the file it replaces: in_place counts with
// overwrite alone, a missing file being created all or nothing, an existing one refused.
func (q transferRequest) writesInPlace() bool {
	return q.inPlace && q.overwrite
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
// writes, its paths cleaned as they were read (see request).
func (q transferRequest) key() string {
	return q.tool + "\x00" + q.destination()
}

// cleanRemote returns remote, a path of ratd's machine, lexically clean, its ~ kept: with path,
// not filepath, for a path of that machine, rat running on Windows too. Its spellings (a doubled
// slash, a dot, a parent) then make one transfer; what ~ stands for, and the links of that
// machine, rat can not know, ~/x and its absolute path staying two. A relative path is left as it
// is, for ratd to refuse: cleaned, ./~/x would become ~/x, which ratd reads as its home.
func cleanRemote(remote string) string {
	if rest, ok := strings.CutPrefix(remote, "~/"); ok {
		// ratd cleans its home followed by the rest, ~//x being ~/x: the rest is cleaned without
		// the slashes starting it, which path.Clean would keep as a root
		return "~/" + path.Clean(strings.TrimLeft(rest, "/"))
	}
	if path.IsAbs(remote) {
		return path.Clean(remote)
	}
	return remote
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
		name, first, other, inPlace string
		input                       reflect.Type
	}{
		{uploadTool, "Copy a file from your filesystem to the machine of the terminals, by path.", "your file",
			" (written in place, partly written)", reflect.TypeFor[uploadInput]()},
		{downloadTool, "Copy a file from the machine of the terminals to your filesystem, by path.", "the remote file",
			"", reflect.TypeFor[downloadInput]()},
	} {
		schema, err := jsonschema.ForType(tool.input, &jsonschema.ForOptions{})
		if err != nil {
			return nil, fmt.Errorf("input schema of %s: %w", tool.name, err)
		}
		description := tool.first + " " + fmt.Sprintf(transferDescription, absolute, tool.other,
			tools.MaxWaitSeconds, tool.inPlace)
		definition, err := json.Marshal(toolDefinition{
			Name:        tool.name,
			Description: description,
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
	called := time.Now()
	if q.cancel {
		return t.cancel(ctx, q, called)
	}
	j, refused := t.join(q)
	if refused != "" {
		return refused, true
	}
	return t.await(ctx, j, called)
}

// request reads the call of tool with arguments, or tells why it is refused. A path holding a NUL
// byte, which no file name can hold, is refused here, before any request or directory created for
// it, as ratd refuses one, and so is a destination naming a directory by its form
// (connect.NamesDirectory). Both paths are cleaned, the remote one lexically when ratd takes it
// (see cleanRemote).
func (t *transfers) request(tool string, arguments json.RawMessage) (transferRequest, string) {
	q := transferRequest{tool: tool}
	var err error
	if tool == uploadTool {
		var in uploadInput
		err = json.Unmarshal(arguments, &in)
		q.local, q.remote, q.overwrite, q.cancel = in.LocalPath, in.RemotePath, in.Overwrite, in.Cancel
		q.inPlace = in.InPlace
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
	case strings.ContainsRune(q.local, 0):
		return q, fmt.Sprintf("local_path %q holds a NUL byte, which no file name can hold.", q.local)
	case strings.ContainsRune(q.remote, 0):
		return q, fmt.Sprintf("remote_path %q holds a NUL byte, which no file name can hold.", q.remote)
	case !filepath.IsLocal(q.local):
		return q, fmt.Sprintf("local_path %q is not within %s: give a path relative to it, without going up (..).",
			q.local, t.dir)
	// the destination naming a directory, which cleaning would turn into the name of a file
	case !q.upload() && connect.NamesDirectory(filepath.ToSlash(q.local)):
		return q, fmt.Sprintf("local_path %q names a directory: give the name of the file to write in it.", q.local)
	case q.upload() && connect.NamesDirectory(q.remote):
		return q, fmt.Sprintf("remote_path %q names a directory: give the name of the file to write in it.", q.remote)
	}
	q.local, q.remote = filepath.Clean(q.local), cleanRemote(q.remote)
	return q, ""
}

// join returns the job of the destination of q, or a new one, which checks the local file first
// (see run). A call from the source of that job joins it, running or ended, whatever its overwrite
// and in_place: the job keeps what the call starting it asked, and the answer tells what the job
// did (see runUpload), never what the joining call asked. Its options are not compared: a
// comparison makes the answer depend on the call and on the state of the job at once (joined,
// refused, or the job forgotten for the copy asked to start), a case analysis the agent can not
// foresee from its call, and the job forgotten loses an outcome not told yet. The rule an agent
// learns from the descriptions instead: the same paths join the copy as it was asked; to copy with
// other options, cancel it (or let it end and be told), then call again, at the price of a call or
// two. A call from another source is another copy rather than other options: refused while that
// job runs, and once it ended, its outcome not told yet, told that outcome instead, the job
// forgotten: the next call starts the transfer asked.
func (t *transfers) join(q transferRequest) (*job, string) {
	t.jobsMu.Lock()
	defer t.jobsMu.Unlock()
	if j := t.jobs[q.key()]; j != nil {
		switch {
		case j.request.source() == q.source():
			// running or ended: await tells which, an ended job told once, then forgotten
			return j, ""
		case j.hasEnded():
			delete(t.jobs, q.key())
			return nil, endedText(j) + fmt.Sprintf(" Nothing was copied from %s: call again to copy it.", q.source())
		default:
			return nil, fmt.Sprintf("A transfer to %s from %s is under way: wait for it with its paths, or stop it "+
				"with cancel.", q.destination(), j.request.source())
		}
	}
	ctx, cancel := context.WithCancel(t.ctx)
	j := &job{request: q, started: time.Now(), cancel: cancel, done: make(chan struct{})}
	t.jobs[q.key()] = j
	t.running.Add(1)
	go t.run(ctx, j)
	return j, ""
}

// preparing is called with the request of a job as it checks its local side (see prepare): a hook
// for tests, which hold it as a file system not answering would.
var preparing = func(transferRequest) {}

// downloaded is called with the job of a download once its file came whole, before it is put in
// place: a hook for tests, which cancel it there, where a cancel is still in time.
var downloaded = func(*job) {}

// uploadLooked is called with the local path of an upload once looked at, before it is opened: a
// hook for tests, which replace the file there.
var uploadLooked = func(string) {}

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
		uploadLooked(q.local)
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
	case errors.Is(err, fs.ErrNotExist):
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
		return fmt.Sprintf("%s %s failed: %s.", what, local, sentence(pathErr.Err))
	}
	var linkErr *os.LinkError
	if errors.As(err, &linkErr) {
		return fmt.Sprintf("%s %s failed: %s.", what, local, sentence(linkErr.Err))
	}
	return fmt.Sprintf("%s %s failed: %s.", what, local, sentence(err))
}

// sentence returns the message of err, to end a sentence of rat: without the period that ends the
// messages of Windows ("Access is denied."), the sentence adding its own.
func sentence(err error) string {
	return strings.TrimSuffix(err.Error(), ".")
}

// await waits for j, for a call made at called, until it ends, transferWait passes, or ctx is done,
// and returns what the call tells: how the transfer ended, or its progress. A job ended is told
// once, then forgotten: the same call again starts a new transfer. A call canceled tells nothing
// and forgets nothing, even as its job ends, which select may pick as well: the harness drops its
// text (see callTransfer), and the next call tells how the job ended.
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
	if ctx.Err() != nil {
		// canceled as the job ended: kept for the next call
		return "", false
	}
	t.forget(j)
	return j.text + endedAgo(j, called), j.isError
}

// endedAgo tells how long ago j ended, for a call made at called, if it ended before the call: the
// call found it ended rather than saw it end, which an agent coming back to it later (a sleep in
// its shell, a context compaction) would take for a transfer just ended.
func endedAgo(j *job, called time.Time) string {
	if !j.ended.Before(called) {
		return ""
	}
	return fmt.Sprintf(" It ended %s ago.", agoText(time.Since(j.ended)))
}

// endedText tells how j ended, for a call to its destination from another source, which found it
// ended, its outcome not told yet: as a call with its paths would tell it.
func endedText(j *job) string {
	return fmt.Sprintf("The transfer to %s from %s ended %s ago: %s", j.request.destination(), j.request.source(),
		agoText(time.Since(j.ended)), j.text)
}

// forget removes j from the jobs, unless another job took its place.
func (t *transfers) forget(j *job) {
	t.jobsMu.Lock()
	defer t.jobsMu.Unlock()
	if t.jobs[j.request.key()] == j {
		delete(t.jobs, j.request.key())
	}
}

// cancel stops the transfer of the paths of q, for a call made at called, and tells how it ended:
// canceled, the destination left as it was, or ended before the cancel could stop it, how long ago
// if before the call (see endedAgo).
func (t *transfers) cancel(ctx context.Context, q transferRequest, called time.Time) (string, bool) {
	t.jobsMu.Lock()
	j := t.jobs[q.key()]
	t.jobsMu.Unlock()
	switch {
	case j == nil:
		return fmt.Sprintf("No transfer to %s is under way: nothing to cancel.", q.destination()), false
	case j.request.source() != q.source() && j.hasEnded():
		// ended, its outcome not told yet: told now, and forgotten, as join does
		t.forget(j)
		return endedText(j) + " Nothing to cancel.", false
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
	if ctx.Err() != nil {
		// canceled as the job ended: kept for the next call (see await)
		return "", false
	}
	t.forget(j)
	ago := endedAgo(j, called)
	switch {
	case j.canceled, j.refused:
		// stopped, or refused by its local side before any request: nothing ran for the cancel to
		// come too late for
		return j.text + ago, j.isError
	case j.isError:
		// a failure, or an answer lost: told first, rather than behind "too late to cancel", which
		// reads as a copy done
		return j.text + " The cancel came too late to stop it." + ago, true
	default:
		return "Too late to cancel: " + j.text + ago, false
	}
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
	// samples are the bytes moved over time, one a second at most, for the rate; lastMoved is when
	// bytes last moved, which a sample may come up to a second before
	samples   []sample
	lastMoved time.Time
	// cancelAsked tells a cancel was asked; placing that a download is being put in place, too
	// late to cancel
	cancelAsked, placing bool
	// done is closed once the transfer ended, its outcome set: what it tells, whether it is an
	// error, whether a cancel stopped it, whether its local side refused it before any request,
	// and when it ended
	done     chan struct{}
	text     string
	isError  bool
	canceled bool
	refused  bool
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
	if n > 0 {
		j.lastMoved = now
	}
	if len(j.samples) > 0 && now.Sub(j.samples[len(j.samples)-1].at) < time.Second {
		return
	}
	if len(j.samples) == rateSamples {
		j.samples = append(j.samples[:0], j.samples[1:]...)
	}
	j.samples = append(j.samples, sample{at: now, bytes: moved})
}

// rate returns the bytes moved per second over about rateWindow, as the samples of j tell it: from
// the oldest one within rateWindow, since its first bytes for a transfer younger, from the last one
// when none is within it; and nothing for a transfer that moved nothing within rateWindow, stalled,
// rather than its average since its start, or the bytes counted after its last sample over the
// time since.
func (j *job) rate(now time.Time) float64 {
	moved := j.moved.Load()
	j.mu.Lock()
	defer j.mu.Unlock()
	if now.Sub(j.lastMoved) > rateWindow {
		return 0
	}
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

// hasEnded tells whether j ended, its outcome set.
func (j *job) hasEnded() bool {
	select {
	case <-j.done:
		return true
	default:
		return false
	}
}

// run runs j until it ends or ctx is done, and sets its outcome: its local side checked first (see
// prepare), what it refuses ending j before any request.
func (t *transfers) run(ctx context.Context, j *job) {
	defer t.running.Done()
	defer j.cancel()
	// Checked here, rather than by the call starting j, under the lock of the jobs: a file system
	// not answering (a stale NFS mount in the transfers directory) holds j alone, its call telling
	// once it waited that nothing moved, rather than every transfer call, and the stop of rat
	// waiting for them.
	preparing(j.request)
	source, refused := t.prepare(j.request)
	var text string
	var isError, canceled bool
	switch {
	case refused != "":
		text, isError = refused, true
	case j.request.upload():
		text, isError, canceled = t.runUpload(ctx, j, source)
	default:
		text, isError, canceled = t.runDownload(ctx, j)
	}
	j.text, j.isError, j.canceled, j.refused, j.ended = text, isError, canceled, refused != "", time.Now()
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

// canceledText tells j canceled, its destination left as it was, or partly written in place (see
// destinationText).
func canceledText(j *job) string {
	text := fmt.Sprintf("Canceled the copy of %s to %s after %s", j.request.source(), j.request.destination(),
		sizeText(j.moved.Load()))
	if size := j.size.Load(); size > 0 {
		text += " of " + sizeText(size)
	}
	return text + ": " + destinationText(j) + "."
}

// destinationText tells what j, cut or canceled before the end of its file, left of its
// destination: as it was, all or nothing, unless an upload in place with overwrite was writing into
// it, which may leave it partly written (ratd empties it as the first bytes come). rat does not
// know whether that file existed, a missing one being created all or nothing.
func destinationText(j *job) string {
	if j.request.writesInPlace() {
		return fmt.Sprintf("%s, written in place, may be partly written", j.request.destination())
	}
	return fmt.Sprintf("%s is as it was", j.request.destination())
}

// failureText tells j failing with err: its answer lost once all of it was sent, ratd possibly
// having written it (partly, written in place), a cancel coming then too late to cut it; refused or failed by ratd (its
// sentence), an answer of ratd, which a cancel never brings, cutting the connection first: a cancel
// came too late then; canceled (whatever the error the cut gave), unless rat is stopping, which
// cuts an upload whatever was sent, ratd possibly writing it; ratd older than the transfers or
// not reached; the file changed while read, or failing to read, as the system tells it; the copy
// cut.
func (t *transfers) failureText(j *job, err error) (string, bool, bool) {
	var refused *connect.Refusal
	var failed *connect.TransferError
	switch {
	case errors.Is(err, connect.ErrAnswerLost) && errors.As(err, &failed):
		left, again := j.request.destination()+" may be written or not", "overwrite"
		if j.request.writesInPlace() {
			// written into as the body came, which ratd may not have received whole: whatever it
			// received, the old content is gone; called again in place too, rather than read as
			// called without it
			left, again = destinationText(j), "overwrite and in_place"
		}
		return fmt.Sprintf("The answer of ratd to the copy of %s to %s was lost once all of it was sent (%s): %s. "+
			"Check it before using it, or call %s again with %s.", j.request.source(), j.request.destination(),
			sentence(failed.Reason), left, j.request.tool, again), true, false
	case errors.As(err, &refused):
		return refused.Text, true, false
	case j.cancelWasAsked() && t.ctx.Err() == nil:
		return canceledText(j), false, true
	case errors.Is(err, connect.ErrNoTransfers):
		return fmt.Sprintf("The ratd at %s is older than this rat and serves no file transfers: whoever runs it must "+
			"upgrade it.", t.server), true, false
	case errors.Is(err, connect.ErrChanged):
		return fmt.Sprintf("%s changed while it was read: it may still be written. Upload it again once it is "+
			"complete. %s.", j.request.local, destinationText(j)), true, false
	case errors.Is(err, connect.ErrRead) && errors.As(err, &failed):
		return t.localFailure(failed.Reason, "Reading", j.request.local) + " " + destinationText(j) + ".", true, false
	case errors.Is(err, connect.ErrCut) && errors.As(err, &failed):
		return fmt.Sprintf("The copy of %s to %s was cut: %s. %s.", j.request.source(), j.request.destination(),
			sentence(failed.Reason), destinationText(j)), true, false
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
		j.request.remote, connect.UploadOptions{Overwrite: j.request.overwrite, InPlace: j.request.inPlace})
	if err != nil {
		return t.failureText(j, err)
	}
	// How the file was replaced, told by the answer itself rather than left to the call: a call
	// joining the job may have asked otherwise (see join), and the two keep different things of the
	// file replaced (its hard links, owner and attributes, kept written in place only). Told from
	// the answer of ratd, not from what the job asked: asked in place, ratd puts a new file in
	// place of one appearing at a path it found empty.
	how := "created"
	switch {
	case up.InPlace:
		how = "written in place"
	case up.Replaced:
		how = "replaced by a new file"
	}
	text := fmt.Sprintf("Uploaded %s to %s (%s): %s.", j.request.local, j.request.remote, sizeText(up.Sent), how)
	// told only from a mode ratd told: an answer without one is the file written all the same
	if modesTold && up.ModeTold && up.Replaced && up.Mode != info.Mode().Perm() {
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
// created, and named; a failure to look at the local path under them refuses the download before
// any byte is written.
func (t *transfers) runDownload(ctx context.Context, j *job) (string, bool, bool) {
	local := j.request.local
	download, err := connect.StartDownload(ctx, t.client, t.server, j.request.remote)
	if err != nil {
		return t.failureText(j, err)
	}
	defer download.Close()
	j.size.Store(download.Size)
	created, err := t.createDirectories(filepath.Dir(local))
	// From now on, a failure names the directories created, which it leaves, as a download
	// creating the file does: an agent catches a typo the same way. Creating them failing, those
	// created before are named.
	failed := func(text string, isError, canceled bool) (string, bool, bool) {
		if len(created) > 0 {
			text += " " + t.createdText(created)
		}
		return text, isError, canceled
	}
	if err != nil {
		return failed(t.localFailure(err, "Writing", local), true, false)
	}
	// The file was looked at before the request (see prepare), a look stopping at the first
	// directory missing, which tells the file missing whatever its name. Once they are there,
	// created by this download or by another transfer meanwhile, it is looked at again, before
	// any byte is written: the temporary file, under a name of its own, may not meet a failure to
	// look (a name too long, the temporary one cut to fit: see package tempname), which placing
	// it would meet once all the bytes came. The download is cut, only the bytes already on their
	// way lost.
	if _, err := t.root.Lstat(local); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return failed(t.localFailure(err, "Writing", local), true, false)
	}
	tmp, tmpName, err := t.createTemporary(local, download.Mode)
	if err != nil {
		return failed(t.localFailure(err, "Writing", local), true, false)
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
	if err == nil {
		downloaded(j)
	}
	var pathErr *fs.PathError
	switch {
	case errors.As(err, &pathErr):
		// writing the local file failed
		return failed(t.localFailure(err, "Writing", local), true, false)
	case err != nil:
		return failed(t.failureText(j, err))
	case !j.beginPlacing():
		return failed(canceledText(j), false, true)
	}
	replaced, found, refused, err := t.place(tmpName, local, j.request.overwrite)
	switch {
	case refused != "":
		return failed(refused, true, false)
	case err != nil:
		return failed(t.localFailure(err, "Writing", local), true, false)
	}
	how := "created"
	if replaced {
		how = "replaced"
	}
	text := fmt.Sprintf("Downloaded %s to %s (%s): %s.", j.request.remote, local, sizeText(j.moved.Load()), how)
	if modesTold && replaced && found.Perm() != download.Mode {
		text += fmt.Sprintf(" It kept its permissions (%s), not those of the remote file (%s).",
			connect.FormatMode(found), connect.FormatMode(download.Mode))
	}
	if note := connect.SpecialBitsNote(found); replaced && note != "" {
		text += " " + note
	}
	if len(created) > 0 {
		text += " " + t.createdText(created)
	}
	return text, false, false
}

// createdText names the directories a download created in the transfers directory (see
// createDirectories), for an agent to catch a typo in its path.
func (t *transfers) createdText(created []string) string {
	return fmt.Sprintf("Missing directories created in %s: %s. Check the path if you did not mean them.", t.dir,
		strings.Join(created, ", "))
}

// createDirectories creates dir, within the transfers directory, and its missing parents, and
// returns the ones it created, from the top: failing, those it created before, left as they are.
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
	if err := t.root.MkdirAll(dir, 0o755); err != nil {
		// made from the top: those before the first missing one were made, and are left
		for i, made := range created {
			if _, statErr := t.root.Stat(made); statErr != nil {
				return created[:i], err
			}
		}
		return created, err
	}
	return created, nil
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

// linkLocal is os.Root.Link, a variable for tests needing a file system without hard links.
var linkLocal = (*os.Root).Link

// renameLocal is os.Root.Rename, a variable for tests needing a rename that fails once made.
var renameLocal = (*os.Root).Rename

// place puts tmp, a complete temporary file next to local, in its place, as ratd places an upload
// (see placeFile in cmd/ratd): a file found there is replaced with overwrite only, keeping its nine
// permission bits, its mode as looked at then returned as found, special bits included, for the
// caller to tell the setuid or setgid bit lost, never put back; otherwise tmp is linked under its
// name, which fails if a file appeared since it was checked, looked at again then; on a file
// system without hard links (see linkUnsupported), renamed; a link or a rename failing with tmp
// found in place, as on NFS, told done. refused tells what is found there and not replaced.
func (t *transfers) place(tmp, local string, overwrite bool) (replaced bool, found fs.FileMode, refused string, err error) {
	q := transferRequest{tool: downloadTool, local: local, overwrite: overwrite}
	tmpInfo, err := t.root.Lstat(tmp)
	if err != nil {
		return false, 0, "", err
	}
	for range 2 {
		info, err := t.root.Lstat(local)
		switch {
		case err == nil && os.SameFile(info, tmpInfo):
			// linked, the link failing all the same
			return false, 0, "", nil
		case err == nil:
			if refused := t.checkLocal(local, info, q); refused != "" {
				return false, 0, refused, nil
			}
			if err = t.root.Chmod(tmp, info.Mode().Perm()); err != nil {
				return false, 0, "", err
			}
			err = t.renameInPlace(tmp, local, tmpInfo)
			if err != nil && readOnlyKept && errors.Is(err, fs.ErrPermission) && info.Mode()&0o200 == 0 {
				return false, 0, fmt.Sprintf("%s is read-only, which Windows keeps from being replaced: clear its "+
					"read-only attribute, or download to another name.", local), nil
			}
			return true, info.Mode(), "", err
		case !errors.Is(err, fs.ErrNotExist):
			return false, 0, "", err
		}
		err = linkLocal(t.root, tmp, local)
		switch {
		case err == nil:
			return false, 0, "", nil
		case errors.Is(err, fs.ErrExist):
			continue
		case linkUnsupported(err):
			return false, 0, "", t.renameInPlace(tmp, local, tmpInfo)
		default:
			return false, 0, "", err
		}
	}
	return false, 0, fmt.Sprintf("%s exists: set overwrite to replace it.", local), nil
}

// renameInPlace renames tmp, looked at as tmpInfo, over local, a rename failing with tmp found in
// place told done, as ratd's does.
func (t *transfers) renameInPlace(tmp, local string, tmpInfo fs.FileInfo) error {
	err := renameLocal(t.root, tmp, local)
	if err != nil {
		if info, lookErr := t.root.Lstat(local); lookErr == nil && os.SameFile(info, tmpInfo) {
			return nil
		}
	}
	return err
}

// progressText tells the progress of j, still under way after a wait, before being the bytes it had
// moved as the wait began: the bytes moved of the size, the rate and the time left, or that nothing
// moved during the wait.
func (t *transfers) progressText(j *job, before int64) string {
	moved, size := j.moved.Load(), j.size.Load()
	// An upload read whole is not told sent whole, a cancel too late: connect decides that as it
	// tells the end of the body, under its lock, which the bytes read do not show (a link stalled on
	// the last chunk, an empty file waiting for the go-ahead). The cancel offered may then come too
	// late, which its call tells, with how the transfer ended.
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
	// From a rate of 1 B/s, the least the text tells: under it, "0 B/s" would come with a time
	// left. In seconds, capped before the conversion: a duration, an int64 of nanoseconds, holds 292
	// years, which a large file at a few bytes a second exceeds, its overflow told "less than a
	// minute" on some systems.
	if size > moved && rate >= 1 {
		left := min(float64(size-moved)/rate, float64(math.MaxInt64/int64(time.Second)))
		text += ", " + leftText(time.Duration(left)*time.Second) + " left"
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
		return countText(int(d.Seconds()), "second")
	case d < time.Hour:
		return countText(int(d.Minutes()), "minute")
	default:
		return countText(int(d.Hours()), "hour")
	}
}

// countText tells n of unit, unit taking an s but for one.
func countText(n int, unit string) string {
	if n == 1 {
		return "1 " + unit
	}
	return fmt.Sprintf("%d %ss", n, unit)
}

// sizeText tells a size in bytes as ratd tells sizes: in its largest binary unit, with a decimal
// at most (64 KiB, 97.7 KiB, 500 B).
func sizeText(bytes int64) string {
	value, unit := (cunits.Bits(bytes) * cunits.Byte).GetHumanSizeAndSuffix()
	return strconv.FormatFloat(math.Round(value*10)/10, 'f', -1, 64) + " " + unit
}
