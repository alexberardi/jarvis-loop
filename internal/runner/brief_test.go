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
		"`git push origin loop/12-fix-the-timer`", "`loop-12`", `"outcome": "pr_opened"`, `"outcome": "question"`,
		"changed_paths are against `origin/main`", "alexberardi/jarvis-server", "not its merge step", "Keep the context small"} {
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
	if strings.Contains(b, "name: build") {
		t.Error("the skill's frontmatter belongs to the file, not the brief")
	}
}
