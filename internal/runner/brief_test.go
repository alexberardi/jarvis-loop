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
		"the runner pushes", "against `origin/main`", "alexberardi/jarvis-server", "not its merge step", "Keep the context small"} {
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
