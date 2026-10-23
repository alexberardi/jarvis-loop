package runner

import (
	"strings"
	"testing"

	"github.com/alexberardi/jarvis-loop/internal/pipeline"
)

// The runner reports the waited-on PRs of its repos that merged or closed: a merge that was the
// last thing an item waited on sends it back to its stage; a close sends it to the user. An open
// PR, and a PR of a repo the runner does not have, are left alone.
func TestCheckWaitPRs(t *testing.T) {
	g := newRig(t, "")
	gh := &fakeCloser{state: map[int]string{44: "MERGED", 45: "CLOSED", 46: "OPEN"}}
	g.r.Close = gh
	merged, closed, open := g.toBuild("merged"), g.toBuild("closed"), g.toBuild("open")
	for id, pr := range map[int64][]string{merged: {"alex/demo#44", "other/repo#44"}, closed: {"alex/demo#45"}, open: {"alex/demo#46"}} {
		if _, err := g.st.Wait(g.ctx, "user:alex", id, nil, pr, "main's fix"); err != nil {
			t.Fatal(err)
		}
	}
	if err := g.r.CheckWaitPRs(g.ctx); err != nil {
		t.Fatal(err)
	}
	stage := func(id int64) pipeline.Item {
		d, err := g.st.GetItem(g.ctx, id)
		if err != nil {
			t.Fatal(err)
		}
		return d.Item
	}
	// other/repo#44 is not this runner's: the item still waits on it.
	if it := stage(merged); it.Stage != pipeline.Waiting || len(pipeline.OpenWaitPRs(it)) != 1 || pipeline.OpenWaitPRs(it)[0] != "other/repo#44" {
		t.Fatalf("merged: %+v", it)
	}
	if ev := g.lastEvent(merged, "wait_pr_merged"); ev == nil || ev.Actor != "runner:devbox" || ev.Data["pr"] != "alex/demo#44" {
		t.Fatalf("wait_pr_merged: %+v", ev)
	}
	if it := stage(closed); it.Stage != pipeline.NeedsYou || it.ReturnStage != pipeline.Build || !strings.Contains(it.Question, "closed without merging") {
		t.Fatalf("closed: %+v", it)
	}
	if it := stage(open); it.Stage != pipeline.Waiting {
		t.Fatalf("open: %+v", it)
	}
	// Once the other PR merges too (a runner with that repo reports it), the item goes back.
	if _, err := g.st.WaitPR(g.ctx, "runner:other", merged, "other/repo#44", "MERGED"); err != nil {
		t.Fatal(err)
	}
	if it := stage(merged); it.Stage != pipeline.Build {
		t.Fatalf("after both: %+v", it)
	}
}

func TestValidateWaitResult(t *testing.T) {
	ok := `{"outcome": "wait", "on": [472], "prs": ["o/n#444"], "reason": "main's test fix first"}`
	v, err := ValidateReview([]byte(ok), strings.Repeat("a", 40))
	if err != nil || v.Outcome != OutcomeWait {
		t.Fatalf("review wait: %+v %v", v, err)
	}
	if f := WaitFields(v); f["on"] != "472" || f["prs"] != "o/n#444" || f["reason"] != "main's test fix first" {
		t.Fatalf("fields %v", f)
	}
	if _, err := ValidateResult([]byte(ok), Expect{Stage: StageBuild}, GitFacts{}, t.TempDir()); err != nil {
		t.Fatalf("build wait: %v", err)
	}
	for raw, want := range map[string]string{
		`{"outcome": "wait", "reason": "x"}`:                          "names nothing",
		`{"outcome": "wait", "on": [1]}`:                              "reason must be",
		`{"outcome": "wait", "on": [0], "reason": "x"}`:               "not an item id",
		`{"outcome": "wait", "prs": ["a b"], "reason": "x"}`:          "not a PR",
		`{"outcome": "wait", "on": [1], "reason": "x", "notes": "y"}`: "not part of outcome wait",
	} {
		if _, err := ValidateReview([]byte(raw), strings.Repeat("a", 40)); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%s: %v, want %q", raw, err, want)
		}
	}
	if _, err := ValidateTriage([]byte(ok), TriageExpect{}); err == nil {
		t.Fatal("triage may not wait")
	}
}
