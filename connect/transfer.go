package connect

import (
	"errors"
	"fmt"
	"io/fs"
	"net/url"
	"strconv"
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
	// opened (it changed while being read, a read failed), for the agent: a download is complete
	// only without it.
	HeaderError = "Rat-Error"
	// HeaderVersion is the version of ratd, in every answer of the transfer endpoints: a 404
	// without it comes from a ratd older than the transfers, one with it from a missing file.
	HeaderVersion = "Rat-Version"
)

// OctetStream is the media type of the files transferred: bytes copied exactly, never
// interpreted nor converted.
const OctetStream = "application/octet-stream"

// ErrHeader is the error of a transfer header that can not be read.
var ErrHeader = errors.New("invalid header")

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
