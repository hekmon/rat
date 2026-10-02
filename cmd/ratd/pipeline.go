package main

import (
	"fmt"
	"slices"
	"strconv"
	"strings"

	"github.com/hekmon/rat/tmux"
)

// pipelineSentence tells, in the descriptions of the tools telling how commands exited, what they
// tell of a pipeline: ratd itself recommends command 2>&1 | tee log, whose status is tee's. Told
// before the agent meets it, as two statuses for one command can be misread.
const pipelineSentence = "A pipeline (command | tee log) exits with the status of its last command, as in bash: " +
	"when a command before it failed, the status of each is told."

// sigpipeStatus is the status of a command killed by SIGPIPE: the command after it in the pipeline
// stopped reading (yes | head), usually normal.
const sigpipeStatus = 141

// pipelineFailure is what the status of a pipeline hides: its commands, before the last one, that
// exited with a status other than 0.
type pipelineFailure struct {
	// commands names them by position, from 1, within the pipeline ("command 1 of its pipeline of
	// 2", "commands 1 and 2 of its pipeline of 3")
	commands string
	// statuses tells theirs, prefixed with status or statuses ("status 1", "statuses 2 and 1")
	statuses string
	// bare is statuses without the prefix ("1", "2 and 1")
	bare string
	// note explains a status misleading there, empty otherwise: 141, SIGPIPE
	note string
	// all lists every status of the pipeline, left to right ("2 1 0")
	all string
}

// failedPipeline returns what the pipeline prompt p follows hides (see pipelineFailure), false when
// no command but the last failed, a single command included: nothing is hidden, nothing is told.
func failedPipeline(p tmux.Prompt) (pipelineFailure, bool) {
	if len(p.Pipeline) < 2 {
		return pipelineFailure{}, false
	}
	var positions, failed []string
	for i, status := range p.Pipeline[:len(p.Pipeline)-1] {
		if status != 0 {
			positions, failed = append(positions, strconv.Itoa(i+1)), append(failed, strconv.Itoa(status))
		}
	}
	if len(failed) == 0 {
		return pipelineFailure{}, false
	}
	f := pipelineFailure{bare: enumerate(failed)}
	if len(failed) == 1 {
		f.commands = fmt.Sprintf("command %s of its pipeline of %d", positions[0], len(p.Pipeline))
		f.statuses = "status " + f.bare
	} else {
		f.commands = fmt.Sprintf("commands %s of its pipeline of %d", enumerate(positions), len(p.Pipeline))
		f.statuses = "statuses " + f.bare
	}
	if slices.Contains(p.Pipeline[:len(p.Pipeline)-1], sigpipeStatus) {
		f.note = fmt.Sprintf(" (%d: stopped by the end of the pipe, usually normal)", sigpipeStatus)
	}
	all := make([]string, len(p.Pipeline))
	for i, status := range p.Pipeline {
		all[i] = strconv.Itoa(status)
	}
	f.all = strings.Join(all, " ")
	return f, true
}

// enumerate joins items as a sentence does: "1", "1 and 2", "1, 2 and 3".
func enumerate(items []string) string {
	if len(items) == 1 {
		return items[0]
	}
	return strings.Join(items[:len(items)-1], ", ") + " and " + items[len(items)-1]
}
