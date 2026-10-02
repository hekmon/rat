package main

import (
	"testing"
	"time"

	"github.com/hekmon/rat/tmux"
)

// TestPipelineTexts guards what wait_window and list_windows tell of a pipeline: the commands before
// the last one that failed come first, by position, then the status of the pipeline, explained,
// with every status; 141 (SIGPIPE) told as usually normal; nothing more for a single command, a
// pipeline where only the last command failed, nor where the terminals do not record pipelines.
func TestPipelineTexts(t *testing.T) {
	now := time.Unix(1790000000, 0)
	at := now.Add(-3 * time.Second)
	noPipelines := terminalsHold
	noPipelines.Pipelines = false
	for _, tc := range []struct {
		name      string
		prompt    tmux.Prompt
		terminals tmux.TerminalsCheck
		wait      string
		list      string
	}{
		{"tee", tmux.Prompt{Time: at, Status: 0, Pipeline: []int{1, 0}}, terminalsHold,
			"build: the command finished 3s ago: command 1 of its pipeline of 2 exited with status 1. The pipeline " +
				"exits with the status of its last command, 0, as bash reports it (statuses, left to right: 1 0).",
			", last command exited with status 0, command 1 of its pipeline of 2 with 1 (statuses: 1 0) (3s ago)"},
		{"several", tmux.Prompt{Time: at, Status: 0, Pipeline: []int{2, 1, 0}}, terminalsHold,
			"build: the command finished 3s ago: commands 1 and 2 of its pipeline of 3 exited with statuses 2 and 1. " +
				"The pipeline exits with the status of its last command, 0, as bash reports it (statuses, left to " +
				"right: 2 1 0).",
			", last command exited with status 0, commands 1 and 2 of its pipeline of 3 with 2 and 1 (statuses: 2 1 0) " +
				"(3s ago)"},
		{"sigpipe", tmux.Prompt{Time: at, Status: 0, Pipeline: []int{141, 0}}, terminalsHold,
			"build: the command finished 3s ago: command 1 of its pipeline of 2 exited with status 141 (141: stopped " +
				"by the end of the pipe, usually normal). The pipeline exits with the status of its last command, 0, as " +
				"bash reports it (statuses, left to right: 141 0).",
			", last command exited with status 0, command 1 of its pipeline of 2 with 141 (141: stopped by the end of " +
				"the pipe, usually normal) (statuses: 141 0) (3s ago)"},
		{"last failed", tmux.Prompt{Time: at, Status: 1, Pipeline: []int{0, 1}}, terminalsHold,
			"build: the command finished 3s ago, exit status 1.", ", last command exited with status 1 (3s ago)"},
		{"single", tmux.Prompt{Time: at, Status: 2}, terminalsHold,
			"build: the command finished 3s ago, exit status 2.", ", last command exited with status 2 (3s ago)"},
		{"not recorded", tmux.Prompt{Time: at, Status: 0, Pipeline: []int{1, 0}}, noPipelines,
			"build: the command finished 3s ago, exit status 0.", ", last command exited with status 0 (3s ago)"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w := tmux.Window{Name: "build", Command: "bash", Path: "/src", Activity: at, Prompt: tc.prompt, Hooked: true}
			d := &daemon{terminals: tc.terminals}
			if text := d.finishedText("build", w, now); text != tc.wait {
				t.Errorf("wait_window: expected\n%s\ngot\n%s", tc.wait, text)
			}
			if text, expected := describeWindow(w, now, tc.terminals), "build: bash in /src, active 3s ago"+tc.list; text != expected {
				t.Errorf("list_windows: expected\n%s\ngot\n%s", expected, text)
			}
		})
	}
}
