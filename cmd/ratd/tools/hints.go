package tools

// Names of the tools rat offers next to those of ratd, with its file transfers (see cmd/rat).
// ratd names them only through the copy hints below.
const (
	UploadFile   = "upload_file"
	DownloadFile = "download_file"
)

// The copy hints are the sentences of the texts of ratd pointing to a copy by path, which keeps a
// file out of the context of the agent: in the descriptions of write_file and read_file, and in
// the instructions. ratd can not know whether its client offers such a copy: its hints are
// generic, naming no tool. rat, offering its file transfers, puts the direct sentence of each in
// its place (see [CopyHints]). Both sides are kept here: a test of ratd checks that its texts hold
// the generic sentences, and a rat and a ratd of different versions, whose sentences differ, leave
// the generic text, still true.
const (
	WriteFileCopyHint = "To copy a file from your filesystem without it entering your context, use a tool " +
		"copying files by path if you have one, otherwise have a subagent read it and write it, if you can start one."
	ReadFileCopyHint = "To copy a file to your filesystem without reading it, use a tool copying files by path if " +
		"you have one."
	InstructionsCopyHint = "To copy files without them entering your context, use a tool copying files by path if " +
		"you have one."
)

// CopyHint is a generic copy hint of the texts of ratd, and the direct sentence rat puts in its
// place.
type CopyHint struct {
	Generic, Direct string
}

// CopyHints returns the copy hints of the texts of ratd, each with the direct sentence naming the
// tools of rat. None of their characters is one JSON escapes: rat finds and replaces them in the
// answers of ratd as they are, without decoding them.
func CopyHints() []CopyHint {
	return []CopyHint{
		{WriteFileCopyHint, "To copy a file from your filesystem without it entering your context, use " + UploadFile + "."},
		{ReadFileCopyHint, "To copy a file to your filesystem without reading it, use " + DownloadFile + "."},
		{InstructionsCopyHint, "To copy files without them entering your context, use " + UploadFile + " and " +
			DownloadFile + "."},
	}
}
