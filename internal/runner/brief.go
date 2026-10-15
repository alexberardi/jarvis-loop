package runner

import (
	"fmt"
	"strings"

	"github.com/alexberardi/jarvis-loop/skills"
)

// Item is the part of a loopd item the runner uses.
type Item struct {
	ID          int64    `json:"id"`
	Title       string   `json:"title"`
	Ask         string   `json:"ask"`
	Repo        string   `json:"repo"`
	Priority    string   `json:"priority"`
	Stage       string   `json:"stage"`
	Area        string   `json:"area"`
	Size        string   `json:"size"`
	Risk        string   `json:"risk"`
	HoldReason  string   `json:"hold_reason"`
	Resources   []string `json:"resources"`
	BounceCount int      `json:"bounce_count"`
	PR          int      `json:"pr"`
	Branch      string   `json:"branch"`
	Summary     string   `json:"summary"`
}

// RunInfo is where one run happens.
type RunInfo struct {
	Slug     string
	Base     string
	Branch   string
	Worktree string
	Scratch  string
	Sandbox  string
	// Resumed is how many commits the branch already has over the base (an earlier run's).
	Resumed int
	// Triage are the item's evidence files copied into .loop/triage/; TriageErr says why the copy
	// failed.
	Triage    []TriageFile
	TriageErr string
}

// TriageFile is one file in .loop/triage/.
type TriageFile struct {
	Name  string
	Label string
}

// BuildBrief is .loop/brief.md for a build run: the stage skill, the item, and the result
// contract. It is also the prompt.
func BuildBrief(it Item, r RunInfo) string {
	var b strings.Builder
	skill := skills.Build
	if strings.HasPrefix(skill, "---\n") {
		if i := strings.Index(skill[4:], "\n---\n"); i >= 0 {
			skill = skill[4+i+5:]
		}
	}
	// Static first (the stage skill and the contract), the item last: the prompt is the first
	// user message, and its common start is what a cache can share across runs.
	b.WriteString(strings.TrimSpace(skill))
	b.WriteString("\n\n## The result contract\n\nCommit your work on the item's branch (the runner pushes it and opens the PR; you never push), then write `.loop/result.json`, exactly one of:\n\n```json\n")
	b.WriteString(`{"outcome": "ready_for_pr", "pr_title": "one line, at most 256 characters",
 "pr_body": "markdown, at most 60,000 characters: what changed, the tests, what was not verified; name evidence files by file name",
 "summary": "one paragraph", "not_verified": ["…"], "evidence": [{"file": "evidence/after.png", "label": "after, phone"}],
 "needs_review": false}`)
	b.WriteString("\n```\n\n```json\n{\"outcome\": \"question\", \"question\": \"…\"}\n```\n\n")
	b.WriteString("`ready_for_pr` means everything is committed: the runner refuses a worktree with uncommitted changes, and reports git's own HEAD and changed files. It adds a Verification footer (the evidence files, the run's numbers) to your `pr_body`. Set `needs_review` to true when the change touches something the user should look at before it merges. No other fields.\n")
	fmt.Fprintf(&b, "\n## The item\n\nThis brief is also saved as `.loop/brief.md`.\n\n- Item: #%d: %s\n", it.ID, it.Title)
	fmt.Fprintf(&b, "- Priority: %s\n- Repository: `%s` (base branch `%s`; the PR's changes are against `origin/%s`)\n", orDash(it.Priority), r.Slug, r.Base, r.Base)
	if it.Area != "" || it.Size != "" {
		fmt.Fprintf(&b, "- Triage: area %s, size %s, risk %s\n", orDash(it.Area), orDash(it.Size), orDash(it.Risk))
	}
	if it.HoldReason != "" {
		fmt.Fprintf(&b, "- On the hold list: %s (the user approves the merge; build it as asked)\n", it.HoldReason)
	}
	if it.BounceCount > 0 {
		fmt.Fprintf(&b, "- Review bounced it %d time(s); the PR #%d and its review comments say why. Fix those on the same branch.\n", it.BounceCount, it.PR)
	}
	if it.Summary != "" {
		fmt.Fprintf(&b, "- Notes so far: %s\n", it.Summary)
	}
	if len(r.Triage) > 0 {
		b.WriteString("- Triage notes and evidence so far (read them first):")
		for _, f := range r.Triage {
			if f.Label != "" {
				fmt.Fprintf(&b, " `.loop/triage/%s` (%s);", f.Name, strings.Join(strings.Fields(f.Label), " "))
			} else {
				fmt.Fprintf(&b, " `.loop/triage/%s`;", f.Name)
			}
		}
		b.WriteString("\n")
	}
	if r.TriageErr != "" {
		fmt.Fprintf(&b, "- The item's triage evidence could not be copied (%s).\n", clip(r.TriageErr, 200))
	}
	ask := strings.TrimSpace(it.Ask)
	if ask == "" {
		ask = it.Title
	}
	fmt.Fprintf(&b, "\nThe ask:\n\n> %s\n", strings.ReplaceAll(ask, "\n", "\n> "))
	b.WriteString("\n## This run\n\n")
	fmt.Fprintf(&b, "- Worktree (your working directory): `%s`\n- Branch: `%s` (checked out; commit on it, the runner pushes it)\n", r.Worktree, r.Branch)
	if r.Resumed > 0 {
		s := "s"
		if r.Resumed == 1 {
			s = ""
		}
		fmt.Fprintf(&b, "- Resumed: this branch already has %d commit%s from a previous run; check them (`git log origin/%s..HEAD`) before redoing work.\n", r.Resumed, s, r.Base)
	}
	fmt.Fprintf(&b, "- Scratch dir (temp files, GOTMPDIR): `%s`\n- Sandbox name for the harness: `%s`\n", r.Scratch, r.Sandbox)
	return b.String()
}

func orDash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}
