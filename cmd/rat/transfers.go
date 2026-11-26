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
// Windows, whose os.Rename (MoveFileEx, replacing an existing file) is denied a read-only
// destination, where os.Remove clears the attribute first; on Unix, the permissions of the
// directory decide. A variable for tests.
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
	LocalPath  string `json:"local_path" jsonschema:"the file to copy, within the transfers directory: relative to it, or absolute"`
	RemotePath string `json:"remote_path" jsonschema:"where to write it on the machine of the terminals: absolute, or starting with ~/"`
	Overwrite  bool   `json:"overwrite,omitempty" jsonschema:"true replaces an existing file; false, the default, refuses it"`
	InPlace    bool   `json:"in_place,omitempty" jsonschema:"false, the default, copies into a new file put in place of the old one once whole: never partly written, but the disk must hold both meanwhile. true, with overwrite, writes into the file itself: no room needed for a second copy, nor write permission on its directory (the file's own decides), the file keeping its hard links, owner and attributes; but a copy cut or canceled leaves it partly written, harmless for a file nothing uses until it is uploaded whole. A missing file is created as usual. Never into the very file you upload, when the terminals run on your machine: emptied as it is read, it would be lost"`
	Cancel     bool   `json:"cancel,omitempty" jsonschema:"true stops the transfer of these paths under way, leaving the destination as it was (written in place, partly written), unless too late"`
	RemotePipe bool   `json:"remote_pipe,omitempty" jsonschema:"true makes remote_path a named pipe, created on the machine of the terminals where nothing exists yet, and copies your file into it for a command of yours to read: see the description"`
}

// downloadInput is the input of download_file.
type downloadInput struct {
	RemotePath string `json:"remote_path" jsonschema:"the file to copy from the machine of the terminals: absolute, or starting with ~/"`
	LocalPath  string `json:"local_path" jsonschema:"where to write it, within the transfers directory: relative to it, or absolute"`
	Overwrite  bool   `json:"overwrite,omitempty" jsonschema:"true replaces an existing file; false, the default, refuses it"`
	Cancel     bool   `json:"cancel,omitempty" jsonschema:"true stops the transfer of these paths under way, leaving the destination as it was, unless too late"`
	RemotePipe bool   `json:"remote_pipe,omitempty" jsonschema:"true makes remote_path a named pipe, created on the machine of the terminals where nothing exists yet, and copies what a command of yours writes into it: see the description"`
}

// transferDescription tells what both tools do, after their first sentence: dir is the transfers
// directory, other the side whose permissions a new file takes, and inPlace what a cancel leaves
// of a file written in place, which upload_file alone writes.
const transferDescription = "Its content never enters your context, whatever its size, binary included. local_path " +
	"is a path under %s, relative to it or absolute: rat copies files from and to no other path on your machine; " +
	"remote_path is absolute or " +
	"starts with ~/. Missing directories are created. An existing file is replaced only with overwrite, and keeps " +
	"its permissions; a new one gets those of %s minus the umask. A symbolic link at the end of a path is refused, never followed, " +
	"and a file changing while it is copied fails the transfer: copy it once complete. The call waits up to %d " +
	"seconds: a longer copy goes on in the background, the call telling its progress, and calling again with the " +
	"same paths waits for the rest, or tells how it ended, the copy keeping the parameters it started with: to " +
	"change them, cancel it, then call again. cancel stops it, leaving the destination as it was%s, unless it " +
	"comes too late, as the copy ends."

// downloadPortalDescription tells, after transferDescription, what download_file does with
// remote_pipe.
const downloadPortalDescription = " With remote_pipe, the source is a portal rather than a file: rat creates a named pipe " +
	"at remote_path on the machine of the terminals (nothing may exist there, its directory must) and answers as " +
	"soon as it is ready, before anything flowed. Then start, in a terminal, the command writing into it (for " +
	"example: tar -c dir > PIPE, PIPE being remote_path), and call download_file again with the same paths to " +
	"follow the copy. The local file is put in place once that command closes the pipe, its exit status telling " +
	"whether what it wrote was all. There is no size: progress tells bytes only. cancel closes the portal, " +
	"deleting the pipe, the command writing into it getting a broken pipe, and works with the same paths even " +
	"after rat restarted."

// uploadPortalDescription tells, after transferDescription, what upload_file does with remote_pipe:
// to stop midway, the command reading goes first, a close ending its input (Q2).
const uploadPortalDescription = " With remote_pipe, the destination is a portal rather than a file: rat creates a " +
	"named pipe at remote_path on the machine of the terminals (nothing may exist there, its directory must; " +
	"overwrite and in_place mean nothing then) and answers as soon as it is ready, before anything flowed. Then " +
	"start, in a terminal, the command reading it (for example: tar -x < PIPE, PIPE being remote_path), and call " +
	"upload_file again with the same paths to follow the copy. Done means your file was delivered whole into the " +
	"pipe and its end closed, the command seeing the end of its input: its exit status tells what it made of it. " +
	"To stop midway, stop the command reading first, then cancel: cancel closes the portal, deleting the pipe, a " +
	"command still reading it seeing the end of its input on a stream that is not whole; it works with the same " +
	"paths even after rat restarted."

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
	// remotePipe makes the remote end a pipe ratd makes, a portal: the source of a download
	// (connect.DownloadOptions), the destination of an upload (connect.StartPipeUpload)
	remotePipe bool
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
	// dir is the transfers directory, absolute and its links resolved: the prefix of every local
	// path rat opens (see withinDir and fullPath)
	dir string
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
	t := &transfers{logger: logger, client: client, server: server, reachText: reachText, dir: absolute,
		jobs: map[string]*job{}}
	t.ctx, t.stop = context.WithCancel(context.Background())
	for _, tool := range []struct {
		name, first, other, inPlace, more string
		input                             reflect.Type
	}{
		{uploadTool, "Copy a file from your filesystem to the machine of the terminals, by path.", "your file",
			" (written in place, partly written)", uploadPortalDescription, reflect.TypeFor[uploadInput]()},
		{downloadTool, "Copy a file from the machine of the terminals to your filesystem, by path.", "the remote file",
			"", downloadPortalDescription, reflect.TypeFor[downloadInput]()},
	} {
		schema, err := jsonschema.ForType(tool.input, &jsonschema.ForOptions{})
		if err != nil {
			return nil, fmt.Errorf("input schema of %s: %w", tool.name, err)
		}
		description := tool.first + " " + fmt.Sprintf(transferDescription, absolute, tool.other,
			tools.MaxWaitSeconds, tool.inPlace) + tool.more
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

// call runs a call of tool with arguments, and returns the text it tells the agent, whether it is
// an error, and told, never nil, which the caller calls as it writes that text, and only then: it
// forgets the job whose outcome the text tells. Forgotten before, by call itself, the outcome
// would be lost with a text dropped, the harness having canceled the call meanwhile (G8: kept
// until told). It waits for its transfer until ctx is done (the call canceled, or rat stopping),
// which never cuts the transfer.
func (t *transfers) call(ctx context.Context, tool string, arguments json.RawMessage) (text string, isError bool,
	told func()) {
	q, refused := t.request(tool, arguments)
	if refused != "" {
		return refused, true, func() {}
	}
	called := time.Now()
	if q.cancel {
		return t.cancel(ctx, q, called)
	}
	j, refused, ended := t.join(q)
	if refused != "" {
		return refused, true, t.forgetting(ended)
	}
	return t.await(ctx, j, called)
}

// forgetting returns the function forgetting j, its outcome told, which does nothing without a job.
func (t *transfers) forgetting(j *job) func() {
	if j == nil {
		return func() {}
	}
	return func() { t.forget(j) }
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
		q.inPlace, q.remotePipe = in.InPlace, in.RemotePipe
	} else {
		var in downloadInput
		err = json.Unmarshal(arguments, &in)
		q.local, q.remote, q.overwrite, q.cancel = in.LocalPath, in.RemotePath, in.Overwrite, in.Cancel
		q.remotePipe = in.RemotePipe
	}
	within, underDir := "", false
	if q.local != "" {
		within, underDir = t.withinDir(q.local)
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
	case !underDir:
		return q, fmt.Sprintf("local_path %q is not within %s: give a path within it, relative to it or absolute.",
			q.local, t.dir)
	// the destination naming a directory, which cleaning would turn into the name of a file
	case !q.upload() && connect.NamesDirectory(filepath.ToSlash(q.local)):
		return q, fmt.Sprintf("local_path %q names a directory: give the name of the file to write in it.", q.local)
	case q.remotePipe && connect.NamesDirectory(q.remote):
		return q, fmt.Sprintf("remote_path %q names a directory: give the name of the pipe to make in it.", q.remote)
	case q.upload() && connect.NamesDirectory(q.remote):
		return q, fmt.Sprintf("remote_path %q names a directory: give the name of the file to write in it.", q.remote)
	case q.upload() && q.remotePipe && (q.overwrite || q.inPlace):
		// what they choose for a file replaced, a pipe made where nothing is has no use for
		return q, "overwrite and in_place mean nothing with remote_pipe: rat makes the pipe at remote_path, where " +
			"nothing may exist. Call upload_file again without them."
	}
	q.local, q.remote = filepath.Clean(within), cleanRemote(q.remote)
	return q, ""
}

// withinDir applies the contract of rat for a local path, its prefix: local, joined to the
// transfers directory when relative, is cleaned, and must then begin with that directory, by whole
// names (x/../../etc and a sibling named as the directory with more letters do not). One rule for
// a relative path and an absolute one, as an agent gives back a full path rat told it. It returns
// the path relative to the directory, as answers name it, and whether the path is under it. A
// final separator is kept: it names a directory, refused as such.
func (t *transfers) withinDir(local string) (within string, ok bool) {
	full := local
	if !filepath.IsAbs(full) {
		full = filepath.Join(t.dir, full)
	}
	within, err := filepath.Rel(t.dir, filepath.Clean(full))
	if err != nil || !filepath.IsLocal(within) {
		return local, false
	}
	if os.IsPathSeparator(local[len(local)-1]) {
		within += string(filepath.Separator)
	}
	return within, true
}

// fullPath returns local, a path relative to the transfers directory as request leaves it, as rat
// opens it: joined to the directory, opened by the system as any path. The contract of rat is the
// prefix of the path, not a confinement: a symbolic link among the directories of a path is
// followed wherever it leads, put there by the agent or by the user who chose the directory, the
// only ones who can, and their call. Rejected: opening the paths through an os.Root of the
// directory, which refused such a link, guarding against nobody rat works for. A symbolic link at
// the end of the path is still refused (see checkLocal): a transfer copies regular files.
func (t *transfers) fullPath(local string) string {
	return filepath.Join(t.dir, local)
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
// job runs, and once it ended, its outcome not told yet, told that outcome instead, the job, ended,
// for the caller to forget once told: the next call starts the transfer asked.
func (t *transfers) join(q transferRequest) (j *job, refused string, ended *job) {
	t.jobsMu.Lock()
	defer t.jobsMu.Unlock()
	if j := t.jobs[q.key()]; j != nil {
		switch {
		case j.request.source() == q.source():
			// running or ended: await tells which, an ended job told once, then forgotten
			return j, "", nil
		case j.hasEnded():
			return nil, endedText(j) + fmt.Sprintf(" Nothing was copied from %s: call again to copy it.", q.source()), j
		default:
			return nil, fmt.Sprintf("A transfer to %s from %s is under way: wait for it with its paths, or stop it "+
				"with cancel.", q.destination(), j.request.source()), nil
		}
	}
	ctx, cancel := context.WithCancel(t.ctx)
	j = &job{request: q, started: time.Now(), cancel: cancel, ready: make(chan struct{}), done: make(chan struct{}),
		closeAsked: make(chan struct{})}
	t.jobs[q.key()] = j
	t.running.Add(1)
	go t.run(ctx, j)
	if q.remotePipe {
		go t.giveUpOpening(j)
	}
	return j, "", nil
}

// giveUpOpening cuts the opening of the portal of j, a cancel asked, if it is not answered within
// transferWait of it: the agent asked to stop, and is told how it ended, rather than "still ending"
// for as long as the system keeps a connection nobody answers (minutes), a cancel waiting for the
// opening to settle before it asks ratd to close (see closeOnCancel). Not a time limit of a portal:
// without a cancel asked, an opening waits as long as it takes. What ratd holds, if the opening
// reached it, is closed by the cancel the outcome tells to make, which asks ratd whatever rat
// remembers (see cancel).
func (t *transfers) giveUpOpening(j *job) {
	select {
	case <-j.closeAsked:
	case <-j.ready:
		return
	case <-j.done:
		return
	}
	timer := time.NewTimer(transferWait)
	defer timer.Stop()
	select {
	case <-j.ready:
	case <-j.done:
	case <-timer.C:
		j.gaveUp.Store(true)
		j.cancel()
	}
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
	info, err := os.Lstat(t.fullPath(q.local))
	if q.upload() {
		if err != nil {
			return nil, t.localFailure(err, "Reading", q.local)
		}
		if refused := t.checkLocal(q.local, info, q); refused != "" {
			return nil, refused
		}
		uploadLooked(q.local)
		// without blocking: a FIFO replacing the file looked at would block until written to
		file, err := os.OpenFile(t.fullPath(q.local), os.O_RDONLY|syscall.O_NONBLOCK, 0)
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

// linkRefusal refuses local, a symbolic link, whatever it leads to: telling the path to use
// instead when it leads to a file under the transfers directory, as its links resolved show it.
// Elsewhere, or nowhere, it is told a link alone: a path through a link among the directories may
// reach the file all the same, which rat does not look for.
func (t *transfers) linkRefusal(local string) string {
	if real, err := filepath.EvalSymlinks(t.fullPath(local)); err == nil {
		if within, err := filepath.Rel(t.dir, real); err == nil && filepath.IsLocal(within) {
			return fmt.Sprintf("%s is a symbolic link to %s, which transfers do not follow: use that path instead.",
				local, within)
		}
	}
	return fmt.Sprintf("%s is a symbolic link, which transfers do not follow.", local)
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
// and returns what the call tells: how the transfer ended, or its progress, with told (see call). A
// job ended is told once, then forgotten, by told: the same call again starts a new transfer. A
// call canceled as its job ends may be handed its outcome all the same, select picking either: the
// caller, which alone knows whether it writes it, drops the text and does not call told, and the
// next call tells how the job ended. Rejected: looking at ctx here before forgetting, a cancel
// coming right after the look losing the outcome.
//
// A portal (remote_pipe) is answered as soon as it is ready, unless that was told already: the call
// returns without waiting for a command, which the agent starts once told. Ready is marked told by
// told, as an outcome is forgotten: a ready answer dropped is told by the next call. A portal ended
// before its ready was told tells its outcome instead.
func (t *transfers) await(ctx context.Context, j *job, called time.Time) (text string, isError bool, told func()) {
	before := j.moved.Load()
	timer := time.NewTimer(transferWait)
	defer timer.Stop()
	var ready <-chan struct{}
	// not of a portal whose close was asked: it is being closed, whatever the call asking it was told
	closing := j.request.remotePipe && j.cancelWasAsked()
	if j.request.remotePipe && !j.readyWasTold() && !closing {
		ready = j.ready
	}
	select {
	case <-j.done:
	case <-ready:
		if !j.hasEnded() {
			return readyText(j), false, j.tellReady
		}
		<-j.done
	case <-timer.C:
		if closing {
			// as the cancel that asked it tells a close ratd has not answered yet
			return fmt.Sprintf("The transfer to %s is still ending: call %s again with the same paths to know how it "+
				"ended.", j.request.destination(), j.request.tool), true, func() {}
		}
		return t.progressText(j, before), false, func() {}
	case <-ctx.Done():
		return "", false, func() {}
	}
	return j.text + endedAgo(j, called), j.isError, t.forgetting(j)
}

// readyText tells the portal of j open: its pipe made on the machine of the terminals, nothing
// flowed, the command at it, writing or reading, the agent's to start.
func readyText(j *job) string {
	if j.request.upload() {
		return fmt.Sprintf("The portal is open: %[1]s is a pipe on the machine of the terminals, and what a command "+
			"reads from it is %[2]s. Start that command in a terminal (for example: tar -x < %[1]s), then call %[3]s "+
			"again with the same paths to follow it, or with cancel to close it.", j.request.remote, j.request.local,
			j.request.tool)
	}
	return fmt.Sprintf("The portal is open: %[1]s is a pipe on the machine of the terminals, and what a command writes "+
		"into it is copied to %[2]s. Start that command in a terminal (for example: tar -c dir > %[1]s), then call %[3]s "+
		"again with the same paths to follow it, or with cancel to close it.", j.request.remote, j.request.local,
		j.request.tool)
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
// if before the call (see endedAgo), with told, which forgets the job told (see call). It waits
// for the job transferWait at most, whatever the cancel takes, the job going on after: a portal
// closed by its job (see closeOnCancel), the outcome then told by the next call.
func (t *transfers) cancel(ctx context.Context, q transferRequest, called time.Time) (text string, isError bool,
	told func()) {
	t.jobsMu.Lock()
	j := t.jobs[q.key()]
	t.jobsMu.Unlock()
	switch {
	case j == nil:
		// rat holds no job at these paths, which it would not after a restart, or once a cut was told:
		// ratd may still hold a portal there, known by its path alone (P8), so it is asked whatever the
		// call says of a pipe, an agent coming back to a portal not having to remember how it opened
		// it. One request, made by the call, which waits transferWait at most (P12).
		closing, stop := context.WithTimeout(ctx, transferWait)
		defer stop()
		text, isError, closed := t.closeUnknown(closing, q)
		if !closed {
			return text, isError, func() {}
		}
		// ratd closed a portal: an outcome, kept until told as any other (P12). This call's answer
		// dropped, the harness having given up on it, the same call again tells it, where ratd,
		// holding nothing any more, would answer that nothing is under way, and what the command at
		// the pipe saw would be told to nobody.
		return text, false, t.forgetting(t.keepClosed(q, text, called))
	case j.request.source() != q.source() && j.hasEnded():
		// ended, its outcome not told yet: told now, and forgotten once told, as join does
		return endedText(j) + " Nothing to cancel.", false, t.forgetting(j)
	case j.request.source() != q.source():
		return fmt.Sprintf("The transfer to %s under way is from %s, not %s: nothing canceled.", q.destination(),
			j.request.source(), q.source()), true, func() {}
	}
	// A portal is closed by its job (see closeOnCancel), which asks ratd, then cuts its link, and
	// keeps ratd's answer in its outcome: the cancel only marks it, the close outliving the call, and
	// waits for the job as for a file transfer.
	if j.request.remotePipe {
		j.markCancel()
	} else {
		j.askCancel()
	}
	timer := time.NewTimer(transferWait)
	defer timer.Stop()
	select {
	case <-j.done:
	case <-timer.C:
		return fmt.Sprintf("The transfer to %s is still ending: call %s again with the same paths to know how it "+
			"ended.", q.destination(), q.tool), true, func() {}
	case <-ctx.Done():
		return "", false, func() {}
	}
	// its call canceled as the job ended, the caller drops the text and keeps the job (see await)
	told = t.forgetting(j)
	ago := endedAgo(j, called)
	switch {
	case j.canceled, j.refused, j.request.remotePipe && !isReady(j):
		// stopped; refused by its local side before any request; or a portal that never opened,
		// refused or failing: nothing ran for the cancel to come too late for
		return j.text + ago, j.isError, told
	case j.isError:
		// a failure, or an answer lost: told first, rather than behind "too late to cancel", which
		// reads as a copy done
		return j.text + " The cancel came too late to stop it." + ago, true, told
	default:
		return "Too late to cancel: " + j.text + ago, false, told
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
	// gaveUp tells the opening of a portal was cut, a cancel asked, ratd not answering it (see
	// giveUpOpening)
	gaveUp atomic.Bool
	// closeAsked is closed once a cancel of a portal was marked (see markCancel), for its job to
	// close it (see closeOnCancel)
	closeAsked chan struct{}
	// readyTold tells the ready of a portal was told (see await)
	readyTold bool
	// ready is closed once the portal of the job is ready: ratd made its pipe, the local file was
	// checked (opened and stamped for an upload, its temporary file created for a download), nothing
	// flowed into a pipe. Never closed for a file transfer.
	ready chan struct{}
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

// markCancel marks a cancel of a portal asked, without cutting the transfer, unless a download is
// being put in place already: its job closes the portal (see closeOnCancel).
func (j *job) markCancel() {
	j.mu.Lock()
	defer j.mu.Unlock()
	if !j.placing && !j.cancelAsked {
		j.cancelAsked = true
		close(j.closeAsked)
	}
}

// readyWasTold tells whether the ready of the portal of j was told.
func (j *job) readyWasTold() bool {
	j.mu.Lock()
	defer j.mu.Unlock()
	return j.readyTold
}

// tellReady marks the ready of the portal of j told, as the answer telling it is written.
func (j *job) tellReady() {
	j.mu.Lock()
	defer j.mu.Unlock()
	j.readyTold = true
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
	if j.request.remotePipe && j.request.upload() {
		// what the command reading saw, ratd tells (see closedText)
		return fmt.Sprintf("Closed the portal from %s to %s after %s sent.", j.request.local, j.request.remote,
			sentText(j))
	}
	if j.request.remotePipe {
		return fmt.Sprintf("Closed the portal from %s to %s after %s: %s.", j.request.remote, j.request.local,
			sizeText(j.moved.Load()), destinationText(j))
	}
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
// came too late then; a file transfer canceled (whatever the error the cut gave; a portal closed is
// told by its job, see closedText), unless rat is stopping, which
// cuts an upload whatever was sent, ratd possibly writing it; ratd older than the transfers or
// not reached; the file changed while read, or failing to read, as the system tells it; the copy
// cut.
func (t *transfers) failureText(j *job, err error) (string, bool, bool) {
	var refused *connect.Refusal
	var failed *connect.TransferError
	switch {
	case j.gaveUp.Load() && !isReady(j):
		// the opening cut by rat, a cancel asked and ratd not answering: whether it reached ratd is
		// not known
		return fmt.Sprintf("The opening of the portal from %s to %s was not answered by ratd, and is cut on this "+
			"side, as asked. %s", j.request.source(), j.request.destination(), notToldText(j.request)), true, false
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
	case errors.As(err, &refused) && j.request.remotePipe && j.request.upload() && refused.Status == http.StatusOK:
		// the trailer of a portal: the command reading left, closed by another rat, a write of its pipe
		// failing
		return fmt.Sprintf("The portal from %s to %s ended after %s sent: %s", j.request.local, j.request.remote,
			sentText(j), refused.Text), true, false
	case errors.As(err, &refused) && j.request.remotePipe && refused.Status == http.StatusOK:
		// the trailer of a portal: closed by another rat, ratd stopping, a read of its pipe failing
		return fmt.Sprintf("The portal from %s to %s was cut after %s: %s %s", j.request.remote, j.request.local,
			sizeText(j.moved.Load()), refused.Text, portalCutText(j)), true, false
	case errors.As(err, &refused):
		return refused.Text, true, false
	case !j.request.remotePipe && j.cancelWasAsked() && t.ctx.Err() == nil:
		// a portal closed is told by its job, from what the close did (see closedText): a portal
		// refused or failing as it opens is told as it is, a cancel racing it or not
		return canceledText(j), false, true
	case errors.Is(err, connect.ErrNoPortals):
		return noPortalsText(t.server), true, false
	case errors.Is(err, connect.ErrNoTransfers):
		return fmt.Sprintf("The ratd at %s is older than this rat and serves no file transfers: whoever runs it must "+
			"upgrade it.", t.server), true, false
	case errors.Is(err, connect.ErrChanged) && j.request.remotePipe:
		return fmt.Sprintf("%s changed while it was read: it may still be written. The portal from %s to %s is cut "+
				"after %s sent. %s", j.request.local, j.request.local, j.request.remote, sentText(j), portalCutText(j)),
			true, false
	case errors.Is(err, connect.ErrRead) && errors.As(err, &failed) && j.request.remotePipe:
		return fmt.Sprintf("%s The portal from %s to %s is cut after %s sent. %s", t.localFailure(failed.Reason,
			"Reading", j.request.local), j.request.local, j.request.remote, sentText(j), portalCutText(j)), true, false
	case errors.Is(err, connect.ErrChanged):
		return fmt.Sprintf("%s changed while it was read: it may still be written. Upload it again once it is "+
			"complete. %s.", j.request.local, destinationText(j)), true, false
	case errors.Is(err, connect.ErrRead) && errors.As(err, &failed):
		return t.localFailure(failed.Reason, "Reading", j.request.local) + " " + destinationText(j) + ".", true, false
	case errors.Is(err, connect.ErrCut) && errors.As(err, &failed) && j.request.remotePipe && j.request.upload():
		return fmt.Sprintf("The portal from %s to %s was cut after %s sent: %s. %s", j.request.local, j.request.remote,
			sentText(j), sentence(failed.Reason), portalCutText(j)), true, false
	case errors.Is(err, connect.ErrCut) && errors.As(err, &failed) && j.request.remotePipe:
		return fmt.Sprintf("The portal from %s to %s was cut after %s: %s. %s", j.request.remote, j.request.local,
			sizeText(j.moved.Load()), sentence(failed.Reason), portalCutText(j)), true, false
	case errors.Is(err, connect.ErrCut) && errors.As(err, &failed):
		return fmt.Sprintf("The copy of %s to %s was cut: %s. %s.", j.request.source(), j.request.destination(),
			sentence(failed.Reason), destinationText(j)), true, false
	case j.request.remotePipe && isReady(j):
		// ratd lost once the portal was ready: the link is cut, ratd packs up as it sees it
		return t.reachText(err) + " " + portalCutText(j), true, false
	default:
		return t.reachText(err), true, false
	}
}

// isReady tells whether the portal of j was ready.
func isReady(j *job) bool {
	select {
	case <-j.ready:
		return true
	default:
		return false
	}
}

// portalCutText tells what a portal cut leaves. Its destination a file: the file as it was, the
// pipe deleted by ratd as it sees the cut, the command writing into it getting a broken pipe, and
// that the agent opens it again once the cause is fixed. Its destination a pipe, broken: ratd keeps
// the end of the command reading open, which sees no end (P5), until the portal is closed; rat
// does not know whether a command was reading, and ratd sees the cut only as one reads (Q15), its
// pipe there until then: the agent is told the portal is not closed.
func portalCutText(j *job) string {
	if j.request.upload() {
		return fmt.Sprintf("The portal is not closed on the machine of the terminals, which sees the cut only as a "+
			"command reads its pipe. If a command was reading %s it still waits, it has seen no end: stop it in its terminal, "+
			"then call %s again with cancel to close the portal.", j.request.remote, j.request.tool)
	}
	return fmt.Sprintf("%s is as it was. ratd deletes the pipe on the machine of the terminals as it sees the cut, "+
		"the command writing into it getting a broken pipe: open the portal again once the cause is fixed.",
		j.request.local)
}

// noPortalsText tells the ratd at server older than the portals.
func noPortalsText(server string) string {
	return fmt.Sprintf("The ratd at %s is older than this rat and opens no portal: whoever runs it must upgrade it.",
		server)
}

// closeUnknown asks ratd to close the portal it may hold at the remote path of q, a cancel finding
// no job of rat there, and tells what it closed; holding none, or older than the portals, that
// nothing is under way; not reached, or failing, that its side could not be asked.
func (t *transfers) closeUnknown(ctx context.Context, q transferRequest) (text string, isError, closed bool) {
	endpoint := connect.DownloadPath
	if q.upload() {
		endpoint = connect.UploadPath
	}
	none := fmt.Sprintf("No transfer to %s is under way: nothing to cancel.", q.destination())
	text, closed, err := connect.ClosePortal(ctx, t.client, t.server, endpoint, q.remote)
	var refused *connect.Refusal
	switch {
	case closed:
		return text, false, true
	case err == nil, errors.Is(err, connect.ErrNoPortals), errors.Is(err, connect.ErrNoTransfers):
		return none, false, false
	case errors.As(err, &refused):
		return fmt.Sprintf("No transfer to %s is under way on this side. %s %s", q.destination(), refused.Text,
			notToldText(q)), true, false
	default:
		return fmt.Sprintf("No transfer to %s is under way on this side. %s %s", q.destination(), t.reachText(err),
			notToldText(q)), true, false
	}
}

// keepClosed keeps text, what ratd answered a cancel of q that closed a portal rat held no job of,
// as a job ended at called, and returns it: found by the paths of q until told, then forgotten (see
// call). Another job having taken its place meanwhile, it is told by this call alone.
func (t *transfers) keepClosed(q transferRequest, text string, called time.Time) *job {
	j := &job{request: q, started: called, cancel: func() {}, ready: make(chan struct{}), done: make(chan struct{}),
		closeAsked: make(chan struct{}), text: text, canceled: true, ended: time.Now()}
	close(j.done)
	t.jobsMu.Lock()
	defer t.jobsMu.Unlock()
	if t.jobs[q.key()] == nil {
		t.jobs[q.key()] = j
	}
	return j
}

// closeRemote asks ratd to close the portal of q it holds at its remote path, and tells its answer,
// whether it closed one or held none: what the command at its pipe saw (see connect.ClosePortal).
// ratd not reached, it tells so, and to cancel again, its side not told to close.
func (t *transfers) closeRemote(ctx context.Context, q transferRequest) (string, bool) {
	endpoint := connect.DownloadPath
	if q.upload() {
		endpoint = connect.UploadPath
	}
	remote := q.remote
	text, _, err := connect.ClosePortal(ctx, t.client, t.server, endpoint, remote)
	var refused *connect.Refusal
	switch {
	case err == nil:
		return text, false
	case errors.Is(err, connect.ErrNoPortals):
		return noPortalsText(t.server), true
	case errors.Is(err, connect.ErrNoTransfers):
		return fmt.Sprintf("The ratd at %s is older than this rat and serves no file transfers: whoever runs it must "+
			"upgrade it.", t.server), true
	case errors.As(err, &refused):
		return refused.Text + " " + notToldText(q), true
	default:
		return t.reachText(err) + " " + notToldText(q), true
	}
}

// notToldText tells ratd not told to close the portal of q.
func notToldText(q transferRequest) string {
	return fmt.Sprintf("Its side of the portal at %s could not be told to close, and may still be open: call %s again "+
		"with cancel.", q.remote, q.tool)
}

// closeAnswer is how the job of a portal closed it, a cancel asked (see closeOnCancel): asked tells
// it asked ratd; text is what ratd answered of its pipe, or why it could not be told, failed then.
type closeAnswer struct {
	asked, failed bool
	text          string
}

// closeOnCancel watches j, its portal ready, for a cancel (see markCancel): once one is asked, it
// asks ratd to close the portal, then cuts its link with cut, ending the job's request. It returns
// the function the job calls once its request ended, by itself or so cut, which returns how the
// portal was closed, asked false without a cancel asked by then. Run under the context of rat
// rather than of the call asking the cancel: the close is the job's, kept in its outcome whoever
// tells it and whenever (G8), and never delays a stop of rat, which cancels it. ratd asked first,
// then the link cut: a link cut first would leave the portal of an upload held, broken, on its
// side.
func (t *transfers) closeOnCancel(j *job, cut context.CancelFunc) func() closeAnswer {
	stop, done := make(chan struct{}), make(chan struct{})
	var answer closeAnswer
	go func() {
		defer close(done)
		select {
		case <-j.closeAsked:
		case <-stop:
			// a cancel asked before the request ended is a close all the same
			select {
			case <-j.closeAsked:
			default:
				return
			}
		}
		answer.asked = true
		answer.text, answer.failed = t.closeRemote(t.ctx, j.request)
		cut()
	}()
	return func() closeAnswer {
		close(stop)
		<-done
		return answer
	}
}

// closedText tells j, a portal, closed as a cancel asked, its outcome: its local side as
// canceledText tells it, then what ratd answered of its pipe (P11). ratd not told, it is an error,
// its side possibly still open, the agent to cancel again.
func closedText(j *job, answer closeAnswer) (string, bool, bool) {
	if !answer.failed {
		return canceledText(j) + " " + answer.text, false, true
	}
	if j.request.upload() {
		return fmt.Sprintf("Cut the portal from %s to %s on this side after %s sent. %s", j.request.local,
			j.request.remote, sentText(j), answer.text), true, true
	}
	return fmt.Sprintf("Cut the portal from %s to %s on this side after %s: %s. %s", j.request.remote,
		j.request.local, sizeText(j.moved.Load()), destinationText(j), answer.text), true, true
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
	if j.request.remotePipe {
		return t.runUploadPipe(ctx, j, source)
	}
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

// runUploadPipe uploads source, the local file of j, into the pipe of a portal, and tells how it
// ended: the portal opened, ratd making its pipe, the job is ready (see await), then the file goes
// as a command reads the pipe, until the portal ends. ctx, which the job ends once a cancel asked
// ratd to close the portal (see closeOnCancel), and rat stopping, cut it whatever was sent: a
// portal closed always closes, where a file upload sent whole waits for its answer.
func (t *transfers) runUploadPipe(ctx context.Context, j *job, source *os.File) (string, bool, bool) {
	up, err := connect.StartPipeUpload(ctx, t.client, t.server, countedFile{File: source, job: j}, j.request.remote)
	if err != nil {
		return t.failureText(j, err)
	}
	defer up.Close()
	closed := t.closeOnCancel(j, j.cancel)
	close(j.ready)
	_, err = up.Wait()
	answer := closed()
	switch {
	case err != nil && answer.asked:
		// the close in its trailer, or the cut that followed it
		return closedText(j, answer)
	case err != nil:
		return t.failureText(j, err)
	}
	// Q4: delivered, not used
	return fmt.Sprintf("Delivered %s into %s (%s): all of it was written into the pipe, and the command reading it "+
		"saw the end of its input; the pipe is deleted. Its exit status tells what it made of it.", j.request.local,
		j.request.remote, sizeText(j.moved.Load())), false, false
}

// sentText tells the bytes of the file of j, an upload, read and sent, of its size.
func sentText(j *job) string {
	return sizeText(j.moved.Load()) + " of " + sizeText(j.size.Load())
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
//
// A portal (remote_pipe) goes the same way, its local file being a file end as any download's:
// checked before any request (see prepare), then ratd makes its pipe and answers, then the
// directories, the second look and the temporary file, and only then is the job ready (see await),
// nothing having flowed. A failure in between closes the download, cutting the link, which ratd
// takes as the end of the portal, deleting its pipe.
func (t *transfers) runDownload(ctx context.Context, j *job) (string, bool, bool) {
	local := j.request.local
	download, err := connect.StartDownload(ctx, t.client, t.server, j.request.remote,
		connect.DownloadOptions{Pipe: j.request.remotePipe})
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
	if _, err := os.Lstat(t.fullPath(local)); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return failed(t.localFailure(err, "Writing", local), true, false)
	}
	tmp, tmpName, err := t.createTemporary(local, download.Mode)
	if err != nil {
		return failed(t.localFailure(err, "Writing", local), true, false)
	}
	// Whatever happens next, the temporary file goes: put in place, it is already gone, or left as
	// a second name of the file (see place).
	defer func() {
		if err := os.Remove(tmpName); err != nil && !errors.Is(err, fs.ErrNotExist) {
			t.logger.Warn("failed to remove the temporary file of a download", "path", tmpName, "error", err)
		}
	}()
	// how the portal was closed, a cancel asked: nothing for a file
	closed := func() closeAnswer { return closeAnswer{} }
	if download.Pipe {
		closed = t.closeOnCancel(j, j.cancel)
		close(j.ready)
	}
	_, err = download.WriteTo(countedWriter{w: tmp, job: j})
	if closeErr := tmp.Close(); err == nil {
		err = closeErr
	}
	answer := closed()
	if err == nil {
		downloaded(j)
	}
	var pathErr *fs.PathError
	switch {
	case err != nil && answer.asked:
		// the close in its trailer, or the cut that followed it
		return failed(closedText(j, answer))
	case errors.As(err, &pathErr) && download.Pipe:
		// writing the local file failed: the portal is cut, the download closed on return
		return failed(t.localFailure(err, "Writing", local)+" "+portalCutText(j), true, false)
	case errors.As(err, &pathErr):
		// writing the local file failed
		return failed(t.localFailure(err, "Writing", local), true, false)
	case err != nil:
		return failed(t.failureText(j, err))
	case !j.beginPlacing():
		if answer.asked {
			// the portal had ended by itself as its close was asked: what ratd answered is told
			return failed(closedText(j, answer))
		}
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
	if download.Pipe {
		// Q3: a command failing closes the pipe too, the end of the source all the same
		text = fmt.Sprintf("Downloaded what was written into %s to %s (%s): %s. The command writing closed its end "+
			"of the pipe: its exit status tells whether that was all.", j.request.remote, local,
			sizeText(j.moved.Load()), how)
	}
	// the bits of a pipe are not those of what came through: a file replaced keeps its own unsaid
	if modesTold && replaced && !download.Pipe && found.Perm() != download.Mode {
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
		if _, err := os.Stat(t.fullPath(parent)); err == nil {
			break
		}
		created = append([]string{parent}, created...)
	}
	if len(created) == 0 {
		return nil, nil
	}
	if err := os.MkdirAll(t.fullPath(dir), 0o755); err != nil {
		// made from the top: those before the first missing one were made, and are left
		for i, made := range created {
			if _, statErr := os.Stat(t.fullPath(made)); statErr != nil {
				return created[:i], err
			}
		}
		return created, err
	}
	return created, nil
}

// createTemporary creates the temporary file of a download to local: next to it, hidden and named
// after it (.NAME.rat-RANDOM, see package tempname), as ratd names its own, with the permission
// bits of mode minus the umask, as any new file. It returns the file and its full path.
func (t *transfers) createTemporary(local string, mode fs.FileMode) (*os.File, string, error) {
	dir, name := filepath.Split(t.fullPath(local))
	for {
		tmpName := filepath.Join(dir, tempname.For(name))
		file, err := os.OpenFile(tmpName, os.O_WRONLY|os.O_CREATE|os.O_EXCL, mode.Perm())
		if !errors.Is(err, fs.ErrExist) {
			return file, tmpName, err
		}
	}
}

// linkLocal is os.Link, a variable for tests needing a file system without hard links.
var linkLocal = os.Link

// renameLocal is os.Rename, a variable for tests needing a rename that fails once made.
var renameLocal = os.Rename

// place puts tmp, the full path of a complete temporary file next to local, in its place, as ratd places an upload
// (see placeFile in cmd/ratd): a file found there is replaced with overwrite only, keeping its nine
// permission bits, its mode as looked at then returned as found, special bits included, for the
// caller to tell the setuid or setgid bit lost, never put back; otherwise tmp is linked under its
// name, which fails if a file appeared since it was checked, looked at again then; on a file
// system without hard links (see linkUnsupported), renamed; a link or a rename failing with tmp
// found in place, as on NFS, told done. refused tells what is found there and not replaced.
func (t *transfers) place(tmp, local string, overwrite bool) (replaced bool, found fs.FileMode, refused string, err error) {
	q := transferRequest{tool: downloadTool, local: local, overwrite: overwrite}
	target := t.fullPath(local)
	tmpInfo, err := os.Lstat(tmp)
	if err != nil {
		return false, 0, "", err
	}
	for range 2 {
		info, err := os.Lstat(target)
		switch {
		case err == nil && os.SameFile(info, tmpInfo):
			// linked, the link failing all the same
			return false, 0, "", nil
		case err == nil:
			if refused := t.checkLocal(local, info, q); refused != "" {
				return false, 0, refused, nil
			}
			if err = os.Chmod(tmp, info.Mode().Perm()); err != nil {
				return false, 0, "", err
			}
			err = renameInPlace(tmp, target, tmpInfo)
			if err != nil && readOnlyKept && errors.Is(err, fs.ErrPermission) && info.Mode()&0o200 == 0 {
				return false, 0, fmt.Sprintf("%s is read-only, which Windows keeps from being replaced: clear its "+
					"read-only attribute, or download to another name.", local), nil
			}
			return true, info.Mode(), "", err
		case !errors.Is(err, fs.ErrNotExist):
			return false, 0, "", err
		}
		err = linkLocal(tmp, target)
		switch {
		case err == nil:
			return false, 0, "", nil
		case errors.Is(err, fs.ErrExist):
			continue
		case linkUnsupported(err):
			return false, 0, "", renameInPlace(tmp, target, tmpInfo)
		default:
			return false, 0, "", err
		}
	}
	return false, 0, fmt.Sprintf("%s exists: set overwrite to replace it.", local), nil
}

// renameInPlace renames tmp, looked at as tmpInfo, over target, both full paths, a rename failing
// with tmp found in place told done, as ratd's does.
func renameInPlace(tmp, target string, tmpInfo fs.FileInfo) error {
	err := renameLocal(tmp, target)
	if err != nil {
		if info, lookErr := os.Lstat(target); lookErr == nil && os.SameFile(info, tmpInfo) {
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
	if j.request.remotePipe {
		return portalProgressText(j, before)
	}
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

// portalProgressText tells the progress of the portal of j, still under way after a wait, before
// being the bytes it had moved as the wait began: that nothing came through yet, rat not knowing
// whether a command is at the pipe; or the bytes moved and the rate, a pipe having no size, nor
// time left.
func portalProgressText(j *job, before int64) string {
	moved := j.moved.Load()
	follow := fmt.Sprintf("Call %s again with the same paths to keep following it, or with cancel to close it.",
		j.request.tool)
	if j.request.upload() {
		return uploadPortalProgressText(j, before, follow)
	}
	switch {
	case !isReady(j):
		return fmt.Sprintf("Still opening the portal from %s to %s. %s", j.request.remote, j.request.local, follow)
	case moved == 0:
		return fmt.Sprintf("The portal from %s to %s is open, and nothing came through yet. %s", j.request.remote,
			j.request.local, follow)
	case moved == before:
		return fmt.Sprintf("Still copying what is written into %s to %s: %s, and nothing moved in the last %d seconds. "+
			"%s", j.request.remote, j.request.local, sizeText(moved), int(transferWait.Round(time.Second).Seconds()),
			follow)
	}
	return fmt.Sprintf("Still copying what is written into %s to %s: %s, %s/s. The copy goes on while you do "+
		"something else: call %s again with the same paths for the rest, or with cancel to close it.",
		j.request.remote, j.request.local, sizeText(moved), sizeText(int64(j.rate(time.Now()))), j.request.tool)
}

// uploadPortalProgressText tells the progress of the portal of j, an upload, as portalProgressText
// does, follow telling how to go on: what was read from the file and sent, rat not knowing what a
// command took of it, nor whether one reads (the connection holds part of the file before any
// does); and, once the whole file was sent, that the portal waits for the command reading to take
// it.
func uploadPortalProgressText(j *job, before int64, follow string) string {
	moved, size := j.moved.Load(), j.size.Load()
	switch {
	case !isReady(j):
		return fmt.Sprintf("Still opening the portal from %s to %s. %s", j.request.local, j.request.remote, follow)
	case moved >= size:
		return fmt.Sprintf("All of %s (%s) was sent into the portal at %s: it ends once the command reading %s took "+
			"it. %s", j.request.local, sizeText(size), j.request.remote, j.request.remote, follow)
	case moved == before:
		return fmt.Sprintf("The portal from %s to %s is open: %s sent so far, and nothing moved in the last %d "+
			"seconds, which a command reading %s slowly, or none reading it, would show. %s", j.request.local,
			j.request.remote, sentText(j), int(transferWait.Round(time.Second).Seconds()), j.request.remote, follow)
	}
	return fmt.Sprintf("Still sending %s into %s: %s sent so far, %s/s. The copy goes on while you do something "+
		"else: call %s again with the same paths for the rest, or with cancel to close it.", j.request.local,
		j.request.remote, sentText(j), sizeText(int64(j.rate(time.Now()))), j.request.tool)
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
