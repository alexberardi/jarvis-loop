package runner

import (
	"strings"
	"testing"
)

func TestBuildBrief(t *testing.T) {
	it := Item{ID: 12, Title: "Fix the timer", Ask: "The timer says 5 when asked for 15.", Repo: "jarvis-server", Priority: "P2", Area: "cc", Size: "S", Risk: "normal"}
	b := BuildBrief(it, RunInfo{Slug: "alexberardi/jarvis-server", Base: "main", Branch: "loop/12-fix-the-timer",
		Worktree: "/w/12-build", Scratch: "/w/12-build.scratch", Sandbox: "loop-12"})
	for _, want := range []string{"- Item: #12: Fix the timer", "# Build stage", "never", "> The timer says 5 when asked for 15.",
		"Branch: `loop/12-fix-the-timer`", "`loop-12`", `"outcome": "ready_for_pr"`, `"outcome": "question"`, `"pr_title"`, `"pr_body"`,
		"the runner pushes", "against `origin/main`", "alexberardi/jarvis-server", "not its merge step", "Keep the context small",
		"scripts/loop-sandbox run", "transcript"} {
		if !strings.Contains(b, want) {
			t.Errorf("brief lacks %q", want)
		}
	}
	// Static first: the stage skill opens the prompt, the item comes at the end.
	if !strings.HasPrefix(b, "# Build stage") {
		t.Errorf("the brief must start with the static skill, got %q", b[:40])
	}
	if strings.Index(b, "Fix the timer") < strings.Index(b, "## When to stop and ask") {
		t.Error("the item belongs after the skill")
	}
	other := BuildBrief(Item{ID: 99, Title: "Other"}, RunInfo{Slug: "a/b", Base: "main", Branch: "loop/99-other", Worktree: "/x", Scratch: "/y", Sandbox: "loop-99"})
	skillEnd := strings.Index(b, "## The item")
	if skillEnd < 0 || b[:skillEnd] != other[:skillEnd] {
		t.Error("the part before the item must be the same for every item")
	}
	for _, bad := range []string{"git push origin", `"pr": 161`, "changed_paths", "already has"} {
		if strings.Contains(b, bad) {
			t.Errorf("the brief still says %q", bad)
		}
	}
	if strings.Contains(b, "name: build") {
		t.Error("the skill's frontmatter belongs to the file, not the brief")
	}
}

func TestBuildBriefResumeAndTriage(t *testing.T) {
	it := Item{ID: 12, Title: "Fix the timer"}
	info := RunInfo{Slug: "a/b", Base: "main", Branch: "loop/12-fix-the-timer", Worktree: "/w", Scratch: "/s", Sandbox: "loop-12",
		Resumed: 2, Triage: []TriageFile{{Name: "1-repro.txt", Label: "repro"}, {Name: "2-notes.md"}}}
	b := BuildBrief(it, info)
	for _, want := range []string{"this branch already has 2 commits from a previous run; check them (`git log origin/main..HEAD`) before redoing work",
		"`.loop/triage/1-repro.txt` (repro)", "`.loop/triage/2-notes.md`"} {
		if !strings.Contains(b, want) {
			t.Errorf("brief lacks %q", want)
		}
	}
	plain := BuildBrief(it, RunInfo{Slug: "a/b", Base: "main", Branch: "x"})
	cut := strings.Index(plain, "## The item")
	if b[:cut] != plain[:cut] {
		t.Error("resume and triage notes belong after the static part")
	}
	if e := BuildBrief(it, RunInfo{Slug: "a/b", Base: "main", Branch: "x", TriageErr: "loopd 500"}); !strings.Contains(e, "could not be copied (loopd 500)") {
		t.Error("a failed triage copy is said in the brief")
	}
}

func TestRetryBrief(t *testing.T) {
	b := BuildBrief(Item{ID: 12, Title: "Fix the timer"}, RunInfo{Slug: "a/b", Base: "main", Branch: "loop/12-x", Worktree: "/w", Scratch: "/s", Sandbox: "loop-12"})
	r := RetryBrief(b, "evidence: evidence key.txt is not a regular file")
	if !strings.HasPrefix(r, strings.TrimRight(b, "\n")) {
		t.Error("the retry keeps the brief as it was, for the prompt cache")
	}
	tail := r[len(b)-1:]
	for _, want := range []string{RetryHeading, "> evidence: evidence key.txt is not a regular file", "`.loop/result.json` again", "one retry"} {
		if !strings.Contains(tail, want) {
			t.Errorf("the retry section lacks %q:\n%s", want, tail)
		}
	}
}

// A repo with a harness: the brief says how to use it through the runner, after the static part;
// a repo without one says nothing about it.
func TestBriefHarness(t *testing.T) {
	it := Item{ID: 12, Title: "Fix the timer"}
	h := &HarnessConfig{Up: "scripts/sandbox up --name $LOOP_SANDBOX", Down: "scripts/sandbox down --name $LOOP_SANDBOX --purge"}
	info := RunInfo{Slug: "a/b", Base: "main", Branch: "loop/12-x", Worktree: "/w", Scratch: "/s", Sandbox: "loop-12",
		Harness: h, HarnessBin: "/w/bin/loop-harness"}
	plain := BuildBrief(it, RunInfo{Slug: "a/b", Base: "main", Branch: "loop/12-x", Worktree: "/w", Scratch: "/s", Sandbox: "loop-12"})
	if strings.Contains(plain, "loop-harness") {
		t.Error("a repo without a harness gets no loop-harness note")
	}
	for _, stage := range []string{StageBuild, StageReview} {
		b := Brief(stage, it, info)
		for _, want := range []string{"## The harness, through the runner", "`loop-harness up`: runs `scripts/sandbox up --name $LOOP_SANDBOX`",
			"`loop-harness run '<command>'`", "`scripts/sandbox down --name $LOOP_SANDBOX --purge`", "`/w/bin/loop-harness`", "20m0s",
			"`$LOOP_SANDBOX` is `loop-12`", ".loop/scratch/harness/"} {
			if !strings.Contains(b, want) {
				t.Errorf("%s brief lacks %q", stage, want)
			}
		}
		if strings.Index(b, "loop-harness") < strings.Index(b, "## The item") {
			t.Errorf("%s: the harness note belongs after the static part", stage)
		}
		if stage == StageBuild && !strings.Contains(b, "verify steps still run after you") {
			t.Error("the build brief says the runner's verify still runs")
		}
	}
}

// Every claude stage's brief keeps its checks in the foreground: a one-shot run ends when the
// agent stops, and a background command never reports back (#208).
func TestBriefsSayForeground(t *testing.T) {
	for _, stage := range []string{StageBuild, StageReview, StageTriage} {
		b := Brief(stage, Item{ID: 1, Title: "T"}, RunInfo{Slug: "a/b", Base: "main", Branch: "loop/1-t", Worktree: "/x", Scratch: "/y", Sandbox: "loop-1"})
		for _, want := range []string{"## Run everything in the foreground", "never use `run_in_background`, Monitor or a\ntrailing `&`", "600000 ms"} {
			if !strings.Contains(b, want) {
				t.Errorf("the %s brief lacks %q", stage, want)
			}
		}
	}
}

// An item that needs the emulator is told so, and told its route to one: the harness through the
// runner, the runner's verify steps after the build, or nothing (#546). A triage run is told it
// has none.
func TestBriefSaysWhatTheItemNeeds(t *testing.T) {
	it := Item{ID: 12, Title: "Verify the Maestro flows", Resources: []string{"emulator", "gpu"}}
	base := RunInfo{Slug: "a/b", Base: "main", Branch: "loop/12-x", Worktree: "/w", Scratch: "/s", Sandbox: "loop-12"}
	withHarness := base
	withHarness.Harness, withHarness.HarnessBin = &HarnessConfig{Up: "scripts/sandbox up", Down: "scripts/sandbox down"}, "/bin/loop-harness"
	withVerify := base
	withVerify.Verify = []string{"mobile flows"}
	conditional := base
	conditional.Verify, conditional.VerifyConditional = []string{"mobile flows (only when a path you change matches app/**)"}, true
	for _, c := range []struct {
		what  string
		brief string
		want  string
	}{
		{"with a harness", BuildBrief(it, withHarness), "`loop-harness` (just below)"},
		{"with verify steps", BuildBrief(it, withVerify), "the runner's verify steps (above) run in that fence after you"},
		{"with a when_changed verify step", BuildBrief(it, conditional), "it is skipped and nothing runs on the emulator this time"},
		{"with neither", BuildBrief(it, base), "Say that in `not_verified`"},
		{"a triage run", Brief(StageTriage, it, base), "A triage run has no way to it at all"},
		{"a review run", Brief(StageReview, it, withVerify), "a verify step runs after the build, not after you"},
	} {
		if !strings.Contains(c.brief, "- What this item needs from a runner (triage set it; this runner was claimed for it): `emulator`, `gpu`") {
			t.Errorf("%s: the brief does not name the item's resources", c.what)
		}
		for _, want := range []string{"**The emulator**: this item needs it", "no `/dev/kvm`", c.want} {
			if !strings.Contains(c.brief, want) {
				t.Errorf("%s: the brief lacks %q", c.what, want)
			}
		}
	}
	// A review run is not told the repo has no verify step just because Verify is the build's.
	if rev := Brief(StageReview, it, withVerify); strings.Contains(rev, "this repo has no harness through the runner and no verify step") {
		t.Error("a review run is told the repo has no verify step")
	}
	// A step that always runs is a route without that caveat.
	if strings.Contains(BuildBrief(it, withVerify), "is no route on its own") {
		t.Error("an unconditional verify step is described as skippable")
	}
	// Nothing for an item that needs nothing, and the note stays out of the cacheable prefix.
	plain := BuildBrief(Item{ID: 12, Title: "Fix the timer"}, base)
	if strings.Contains(plain, "The emulator") || strings.Contains(plain, "needs from a runner") {
		t.Error("an item with no resources is told about them anyway")
	}
	cut := strings.Index(plain, "## The item")
	if BuildBrief(it, withHarness)[:cut] != plain[:cut] {
		t.Error("the resources note belongs after the static part")
	}
}

// A harness that lives in another repo (harness.repo) says so: where its commands run, and that
// the agent's own worktree is writable beside it, in $LOOP_WORKTREE.
func TestBriefHarnessInAnotherRepo(t *testing.T) {
	info := RunInfo{Slug: "a/mobile", Base: "main", Branch: "loop/12-x", Worktree: "/w", Scratch: "/s", Sandbox: "loop-12",
		Harness: &HarnessConfig{Up: "scripts/sandbox up", Down: "scripts/sandbox down"}, HarnessBin: "/bin/loop-harness",
		HarnessDir: "/work/12-harness-jarvis-server", HarnessRepo: "alex/jarvis-server"}
	b := BuildBrief(Item{ID: 12, Title: "Flows"}, info)
	for _, want := range []string{"the harness scripts are in `alex/jarvis-server`", "(`/work/12-harness-jarvis-server`, also `$LOOP_HARNESS_DIR`)",
		"its path is `$LOOP_WORKTREE`", "one command in that checkout",
		// What a command writes outlives the run only under $LOOP_EVIDENCE_DIR.
		"files it writes under `$LOOP_EVIDENCE_DIR` (screenshots) you can Read, and that is where screenshots have to go"} {
		if !strings.Contains(b, want) {
			t.Errorf("brief lacks %q", want)
		}
	}
	// A harness in the worktree is unchanged: files are simply here, and no checkout is mentioned.
	info.HarnessDir, info.HarnessRepo = "", ""
	own := BuildBrief(Item{ID: 12, Title: "Flows"}, info)
	if !strings.Contains(own, "one command in this worktree") || strings.Contains(own, "$LOOP_HARNESS_DIR") {
		t.Error("a harness in the worktree is described as one")
	}
	if !strings.Contains(own, "files it writes here (screenshots) you can Read") || strings.Contains(own, "$LOOP_EVIDENCE_DIR") {
		t.Error("a harness in the worktree is told to write evidence somewhere else")
	}
}
