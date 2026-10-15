package runner

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
)

// PRSpec is what the runner opens or updates a PR with.
type PRSpec struct {
	Slug     string // owner/name
	Head     string // the item's branch
	Base     string
	Title    string
	BodyFile string // markdown, written by the runner (the agent's body plus the footer)
}

// PRs opens and updates PRs with the user's GitHub credentials, outside the agent's sandbox. The
// agent holds no GitHub credential (docs/decisions/2026-10-15-runner-pushes.md).
type PRs interface {
	// Find returns the open PR whose head is branch, or 0.
	Find(ctx context.Context, slug, branch string) (int, error)
	Create(ctx context.Context, s PRSpec) (int, error)
	Edit(ctx context.Context, pr int, s PRSpec) error
}

// GH is PRs through the gh CLI (the runner's own login).
type GH struct {
	// Env for gh; nil means the runner's own, minus loopd's and Slack's secrets.
	Env []string
}

func (g GH) run(ctx context.Context, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, "gh", args...)
	env := g.Env
	if env == nil {
		env = scrubEnv(nil)
	}
	cmd.Env = append(append([]string{}, env...), "GH_PROMPT_DISABLED=1", "NO_COLOR=1")
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("gh %s: %v: %s", args[0]+" "+args[1], err, clip(strings.TrimSpace(errb.String()), 500))
	}
	return strings.TrimSpace(out.String()), nil
}

// Find lists the open PRs with head branch.
func (g GH) Find(ctx context.Context, slug, branch string) (int, error) {
	out, err := g.run(ctx, "pr", "list", "--repo", slug, "--head", branch, "--state", "open", "--json", "number,headRefName")
	if err != nil {
		return 0, err
	}
	var prs []struct {
		Number      int    `json:"number"`
		HeadRefName string `json:"headRefName"`
	}
	if err := json.Unmarshal([]byte(out), &prs); err != nil {
		return 0, fmt.Errorf("gh pr list: %w", err)
	}
	for _, p := range prs {
		if p.HeadRefName == branch {
			return p.Number, nil
		}
	}
	return 0, nil
}

var prURLRe = regexp.MustCompile(`/pull/([0-9]+)\s*$`)

// Create opens the PR; gh prints its URL.
func (g GH) Create(ctx context.Context, s PRSpec) (int, error) {
	out, err := g.run(ctx, "pr", "create", "--repo", s.Slug, "--head", s.Head, "--base", s.Base, "--title", s.Title, "--body-file", s.BodyFile)
	if err != nil {
		return 0, err
	}
	m := prURLRe.FindStringSubmatch(out)
	if m == nil {
		return 0, fmt.Errorf("gh pr create printed no PR URL: %s", clip(out, 200))
	}
	return strconv.Atoi(m[1])
}

// Edit replaces the PR's title and body.
func (g GH) Edit(ctx context.Context, pr int, s PRSpec) error {
	_, err := g.run(ctx, "pr", "edit", strconv.Itoa(pr), "--repo", s.Slug, "--title", s.Title, "--body-file", s.BodyFile)
	return err
}

// DryRunPRs opens nothing: every branch gets "PR" 1 (the dry run, with a local remote).
type DryRunPRs struct{}

func (DryRunPRs) Find(context.Context, string, string) (int, error) { return 0, nil }
func (DryRunPRs) Create(context.Context, PRSpec) (int, error)       { return 1, nil }
func (DryRunPRs) Edit(context.Context, int, PRSpec) error           { return nil }

// RunStats are the numbers the PR's footer shows.
type RunStats struct {
	Turns     int
	CacheHit  float64 // 0..1
	Seconds   int
	CostUSD   float64
	HasResult bool
}

// PRBody is the agent's body plus the runner's Verification footer: the evidence files (uploaded
// to loopd with the item) and the run's turn and cache numbers.
func PRBody(agentBody string, item int64, evidence []EvidenceFile, labels map[string]string, st RunStats) string {
	var b strings.Builder
	b.WriteString(strings.TrimRight(agentBody, "\n"))
	b.WriteString("\n\n---\n\n### Verification\n\n")
	if len(evidence) == 0 {
		b.WriteString("No evidence files.\n")
	} else {
		fmt.Fprintf(&b, "Evidence files (on loop item #%d):\n\n", item)
		for _, e := range evidence {
			if l := labels[e.Name]; l != "" {
				fmt.Fprintf(&b, "- `%s`: %s\n", e.Name, oneLine(l))
			} else {
				fmt.Fprintf(&b, "- `%s`\n", e.Name)
			}
		}
	}
	if st.HasResult {
		fmt.Fprintf(&b, "\nRun: %d turns, prompt-cache hit %.0f%%, %s, about $%.2f (estimate).\n",
			st.Turns, st.CacheHit*100, duration(st.Seconds), st.CostUSD)
	} else {
		fmt.Fprintf(&b, "\nRun: %s (no usage numbers).\n", duration(st.Seconds))
	}
	b.WriteString("\n*Built by a jarvis-loop agent; pushed and opened by the runner.*\n")
	return b.String()
}

func oneLine(s string) string { return strings.Join(strings.Fields(s), " ") }

func duration(sec int) string {
	if sec < 60 {
		return fmt.Sprintf("%ds", sec)
	}
	return fmt.Sprintf("%dm %02ds", sec/60, sec%60)
}
