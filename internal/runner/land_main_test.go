package runner

import (
	"errors"
	"slices"
	"testing"
	"time"

	"github.com/alexberardi/jarvis-loop/internal/fakeclaude"
)

// watchMain on a red merge: the commit before it (main's head when land last passed the gates) is
// named the last green one only when its own runs are green; the failed jobs are re-run once
// first, and a refused re-run leaves the red standing at once.
func TestWatchMainRedNamesTheLastGreen(t *testing.T) {
	const merge = "cccccccccccccccccccccccccccccccccccccccc" // fakeLand's merge commit
	red := []WorkflowRun{{ID: 400, Name: "ci", Status: "completed", Conclusion: "failure", URL: "https://github.com/alex/demo/actions/runs/400"}}
	green := []WorkflowRun{{ID: 399, Name: "ci", Status: "completed", Conclusion: "success"}}
	for _, c := range []struct {
		name      string
		prev      []WorkflowRun
		rerunErr  error
		lastGreen bool
	}{
		{name: "previous commit green", prev: green, lastGreen: true},
		{name: "previous commit red", prev: red},
		{name: "previous commit has no runs", prev: nil},
		{name: "re-run refused", prev: green, rerunErr: errors.New("HTTP 403"), lastGreen: true},
	} {
		t.Run(c.name, func(t *testing.T) {
			g := newRig(t, fakeclaude.Good)
			id, gh := g.toLand("Add a feature")
			prev := gitT(t, g.remote, "rev-parse", "main")
			gh.mu.Lock()
			gh.commits = map[string][]WorkflowRun{merge: red}
			if c.prev != nil {
				gh.commits[prev] = c.prev
			}
			gh.rerunErr = c.rerunErr
			gh.mu.Unlock()
			g.stages(StageLand)
			if !g.runOnce() {
				t.Fatal("nothing claimed")
			}
			g.waitMerged(gh, id)
			if c.rerunErr == nil {
				waiting := make(chan struct{})
				go func() { g.clk.BlockUntil(2); close(waiting) }() // the watch's new deadline and its poll
				select {
				case <-waiting:
				case <-time.After(10 * time.Second):
					t.Fatalf("the watch never waited on a re-run; reruns %v; log:\n%s", gh.reruns, g.logs.String())
				}
				g.clk.Advance(LandPollEvery)
			}
			g.r.Wait()
			evs := g.events(id, "main_ci")
			if len(evs) != 1 {
				t.Fatalf("main_ci events %+v; log:\n%s", evs, g.logs.String())
			}
			data := evs[0].Data
			if data["result"] != "red" || data["merge_sha"] != merge {
				t.Fatalf("main_ci %+v", data)
			}
			if lg, ok := data["last_green"]; c.lastGreen != ok || (ok && lg != prev) {
				t.Fatalf("last_green %v, want it %v (%s)", lg, c.lastGreen, prev)
			}
			_, rerun := data["rerun"]
			if c.rerunErr == nil && (!rerun || !slices.Equal(gh.reruns, []int64{400})) {
				t.Fatalf("want one re-run of run 400: %+v, reruns %v", data, gh.reruns)
			}
			if c.rerunErr != nil && (rerun || len(gh.reruns) != 0) {
				t.Fatalf("re-run refused, yet rerun %+v %v", data, gh.reruns)
			}
		})
	}
}
