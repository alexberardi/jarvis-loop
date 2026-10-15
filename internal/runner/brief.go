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
	b.WriteString("\n\n## The result contract\n\nWrite `.loop/result.json`, exactly one of:\n\n```json\n")
	b.WriteString(`{"outcome": "pr_opened", "repo": "<owner/name>", "pr": 161, "branch": "<the item's branch>",
 "head_sha": "<git rev-parse HEAD, 40 hex>", "changed_paths": ["M internal/x.go", "A internal/x_test.go"],
 "summary": "one paragraph", "not_verified": ["…"], "evidence": [{"file": "evidence/after.png", "label": "after, phone"}],
 "needs_review": false}`)
	b.WriteString("\n```\n\n```json\n{\"outcome\": \"question\", \"question\": \"…\"}\n```\n\n")
	b.WriteString("`changed_paths` is `git diff --name-status origin/<base>...HEAD`, one entry per line of it. Set `needs_review` to true when the change touches something the user should look at before it merges. No other fields.\n")
	fmt.Fprintf(&b, "\n## The item\n\nThis brief is also saved as `.loop/brief.md`.\n\n- Item: #%d: %s\n", it.ID, it.Title)
	fmt.Fprintf(&b, "- Priority: %s\n- Repository: `%s` (base branch `%s`; changed_paths are against `origin/%s`)\n", orDash(it.Priority), r.Slug, r.Base, r.Base)
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
	ask := strings.TrimSpace(it.Ask)
	if ask == "" {
		ask = it.Title
	}
	fmt.Fprintf(&b, "\nThe ask:\n\n> %s\n", strings.ReplaceAll(ask, "\n", "\n> "))
	b.WriteString("\n## This run\n\n")
	fmt.Fprintf(&b, "- Worktree (your working directory): `%s`\n- Branch: `%s`; push it with exactly `git push origin %s`\n", r.Worktree, r.Branch, r.Branch)
	fmt.Fprintf(&b, "- Scratch dir (temp files, GOTMPDIR): `%s`\n- Sandbox name for the harness: `%s`\n", r.Scratch, r.Sandbox)
	return b.String()
}

func orDash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}
