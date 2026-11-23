package connect

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"net/url"
	"strconv"
	"strings"
)

// The file transfers of rat copy files by path between the machine of a harness and the one of
// ratd, their bytes never going through MCP, whose messages reach a model. They travel over two
// endpoints of ratd next to [Path], in plain HTTP terms: the file is the body, headers describe
// it. The design, and the alternatives it rejected, are in the README of cmd/ratd (File
// transfers).
const (
	// UploadPath receives a file (POST), written at the path the request names.
	UploadPath = "/upload"
	// DownloadPath sends a file (GET), read at the path the request names.
	DownloadPath = "/download"
)

// The headers of the file transfers: prefixed Rat-, being rat's own. Standard headers keep their
// names and meaning (Content-Type, Transfer-Encoding, Trailer, Expect).
const (
	// HeaderPath names the file on the machine of ratd, absolute or starting with ~/, encoded by
	// [EncodePath].
	HeaderPath = "Rat-Path"
	// HeaderOverwrite tells whether an upload may replace an existing file: true or false, false
	// when absent, the safe side.
	HeaderOverwrite = "Rat-Overwrite"
	// HeaderMode holds permission bits in octal ([FormatMode]): of the file sent, in an upload and
	// in the answer to a download; of the file as written, in the answer to an upload.
	HeaderMode = "Rat-Mode"
	// HeaderSize is the size of the file a download sends, as it was opened: the body is chunked,
	// with no Content-Length.
	HeaderSize = "Rat-Size"
	// HeaderError is a trailer of a download, telling why the file was not sent whole as it was
	// opened (it changed while being read, a read failed, its size was not that of its content),
	// for the agent: a download is complete only without it. Its sentence travels encoded (see
	// [EncodeError]): it names a path, which may hold what a header can not carry.
	HeaderError = "Rat-Error"
	// HeaderVersion is the version of ratd, in every answer of the transfer endpoints: a 404
	// without it comes from a ratd older than the transfers, one with it from a missing file.
	HeaderVersion = "Rat-Version"
)

// OctetStream is the media type of the files transferred: bytes copied exactly, never
// interpreted nor converted.
const OctetStream = "application/octet-stream"

var (
	// ErrHeader is the error of a transfer header that can not be read.
	ErrHeader = errors.New("invalid header")
	// ErrRefused is the error of a transfer ratd refused or failed, wrapped by a [Refusal] telling
	// why.
	ErrRefused = errors.New("transfer refused")
	// ErrNoTransfers is the error of a transfer to a ratd older than the transfers.
	ErrNoTransfers = errors.New("ratd serves no file transfers: it is older than them")
	// ErrChanged is the error of an upload whose file changed while it was read: ratd wrote
	// nothing.
	ErrChanged = errors.New("the file changed while it was read")
	// ErrRead is the kind of a [TransferError] of an upload whose file failed to read (a disk
	// failing, a file system gone), or to be looked at once read, its reason that error: ratd wrote
	// nothing. A failure on the side of the caller, told apart from the connection lost or ratd
	// failing.
	ErrRead = errors.New("reading the file failed")
	// ErrCut is the kind of a [TransferError] of a transfer cut before its end once ratd answered:
	// a download whose body was cut short; an upload whose connection was lost once ratd gave its
	// go-ahead (100), before the end of the file, ratd then writing nothing of it. Lost before any
	// answer of ratd, a transfer fails with the error of net/http; once the file of an upload was
	// sent whole, with [ErrAnswerLost].
	ErrCut = errors.New("transfer cut")
	// ErrAnswerLost is the kind of a [TransferError] of an upload whose connection was lost once
	// its whole file was sent, before ratd answered: ratd may have written the file, which only its
	// answer would have told.
	ErrAnswerLost = errors.New("the answer of ratd lost once the file was sent whole")
)

// TransferError is a transfer failed in a way its caller tells apart with errors.Is, its Kind
// ([ErrCut], [ErrAnswerLost], [ErrRead]), along with the Reason it failed: what the system gave
// ("unexpected EOF"), or what came short. errors.As gives the reason apart, for a caller to tell
// it in its own words, which errors.Unwrap does not give of an error wrapping both.
type TransferError struct {
	Kind   error
	Reason error
}

func (e *TransferError) Error() string {
	return e.Kind.Error() + ": " + e.Reason.Error()
}

// Unwrap returns the kind and the reason, for errors.Is to find either.
func (e *TransferError) Unwrap() []error {
	return []error{e.Kind, e.Reason}
}

// innermost returns the innermost error of err, the reason the system gave ("broken pipe",
// "unexpected EOF"), rather than the request and the layers of net/http on the way ("net/http:
// HTTP/1.x transport connection broken", "write tcp A->B"), which tell an agent nothing more: the
// reason of a [TransferError].
func innermost(err error) error {
	for next := errors.Unwrap(err); next != nil; next = errors.Unwrap(err) {
		err = next
	}
	return err
}

// Refusal is a transfer ratd refused or failed: its HTTP status, 200 for a download failed by its
// trailer (see [HeaderError]), and the sentence ratd wrote for the agent. It wraps [ErrRefused].
type Refusal struct {
	Status int
	Text   string
}

func (r *Refusal) Error() string {
	return fmt.Sprintf("%v (%d): %s", ErrRefused, r.Status, r.Text)
}

func (r *Refusal) Unwrap() error {
	return ErrRefused
}

// textMax bounds what is read of a text ratd answers: a sentence, never a file.
const textMax = 64 << 10

// transferURL returns the URL of the transfer endpoint path on the ratd at server, HOST:PORT.
func transferURL(server, path string) string {
	return (&url.URL{Scheme: "https", Host: server, Path: path}).String()
}

// refusal returns the error of resp, an answer refusing or failing a transfer: [ErrNoTransfers]
// for a 404 without [HeaderVersion], from a ratd older than the transfers, a [Refusal] otherwise.
// A refusal of the client checks of ratd (a client revoked, 403; no valid CRL, 503) carries no
// version either, and is told as the others.
func refusal(resp *http.Response) error {
	if resp.StatusCode == http.StatusNotFound && resp.Header.Get(HeaderVersion) == "" {
		return ErrNoTransfers
	}
	text := readText(resp.Body)
	if text == "" {
		text = resp.Status
	}
	return &Refusal{Status: resp.StatusCode, Text: text}
}

// readText returns the text of body, a sentence for the agent, without the spaces around it,
// bounded by textMax.
func readText(body io.Reader) string {
	text, _ := io.ReadAll(io.LimitReader(body, textMax))
	return strings.TrimSpace(string(text))
}

// readErrors reads r, keeping the error it failed with, other than its end: a transfer tells its
// connection failing from its file failing.
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

// EncodePath returns path as [HeaderPath] carries it: percent-encoded as the path of a URL,
// slashes kept, readable in logs. Not as is: parsers strip the spaces around a header value, and
// refuse control characters, both legal in a file name.
func EncodePath(path string) string {
	return (&url.URL{Path: path}).EscapedPath()
}

// DecodePath returns the path [HeaderPath] carries in value.
func DecodePath(value string) (string, error) {
	path, err := url.PathUnescape(value)
	if err != nil {
		return "", fmt.Errorf("%w %s %q: not a percent-encoded path", ErrHeader, HeaderPath, value)
	}
	return path, nil
}

// NamesDirectory tells whether path, a destination, names a directory by its form: ending with a
// slash, or with . or .. as its last part, which cleaning it would turn into the name of a file,
// written (or, with overwrite, replaced) where the agent meant a directory, as cp copies into
// ~/backups/. Both sides refuse such a destination, a source being cleaned as given. Its separator
// is a slash: a local path goes through filepath.ToSlash first.
func NamesDirectory(path string) bool {
	return strings.HasSuffix(path, "/") || path == "." || path == ".." || strings.HasSuffix(path, "/.") ||
		strings.HasSuffix(path, "/..")
}

// EncodeError returns text as [HeaderError] carries it: percent-encoded where a header can carry
// nothing as is, control characters (a file name may hold an escape, a new line) and the percent
// sign, the rest kept as is, readable with curl. Sent as is, a control character makes Go's client
// refuse the trailer, the download then told cut, and a new line came as a space, the sentence
// naming another path. Rejected: the path quoted in the sentence (%q), which every text naming a
// path would then have to do.
func EncodeError(text string) string {
	var b strings.Builder
	for i := range len(text) {
		switch c := text[i]; {
		case c < ' ', c == 0x7f, c == '%':
			fmt.Fprintf(&b, "%%%02X", c)
		default:
			b.WriteByte(c)
		}
	}
	return b.String()
}

// DecodeError returns the text [HeaderError] carries in value (see [EncodeError]), or value as is
// when it is not percent-encoded.
func DecodeError(value string) string {
	if text, err := url.PathUnescape(value); err == nil {
		return text
	}
	return value
}

// FormatMode returns the permission bits of mode as [HeaderMode] carries them: four octal digits,
// as chmod reads them (0644). The setuid, setgid and sticky bits never travel.
func FormatMode(mode fs.FileMode) string {
	return fmt.Sprintf("%04o", uint32(mode.Perm()))
}

// ParseMode returns the permission bits [HeaderMode] carries in value: octal, 0777 at most.
func ParseMode(value string) (fs.FileMode, error) {
	bits, err := strconv.ParseUint(value, 8, 32)
	if err != nil || bits > 0o777 {
		return 0, fmt.Errorf("%w %s %q: expected permission bits in octal, such as 0644", ErrHeader, HeaderMode, value)
	}
	return fs.FileMode(bits), nil
}

// ParseOverwrite returns what [HeaderOverwrite] carries in value: true or false, false when
// absent.
func ParseOverwrite(value string) (bool, error) {
	switch value {
	case "", "false":
		return false, nil
	case "true":
		return true, nil
	}
	return false, fmt.Errorf("%w %s %q: expected true or false", ErrHeader, HeaderOverwrite, value)
}
