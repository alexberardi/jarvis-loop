package runner

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"regexp"
	"sort"
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
	// View is one PR's title, body and head, for a review run's context.
	View(ctx context.Context, slug string, pr int) (PRView, error)
	// Comment posts a comment on the PR (a review's verdict; never a GitHub approval).
	Comment(ctx context.Context, slug string, pr int, bodyFile string) error
	// Comments are the PR's conversation comments, reviews and inline review comments, oldest
	// first, for a build run after a bounce.
	Comments(ctx context.Context, slug string, pr int) ([]PRComment, error)
	// MergedPRs are the repo's last n merged PRs, newest first, for triage's dedupe.
	MergedPRs(ctx context.Context, slug string, n int) ([]MergedPR, error)
}

// PRView is what a review run is told about its PR.
type PRView struct {
	Number      int    `json:"number"`
	Title       string `json:"title"`
	Body        string `json:"body"`
	HeadRefName string `json:"headRefName"`
	HeadRefOid  string `json:"headRefOid"`
	BaseRefName string `json:"baseRefName"`
	State       string `json:"state"`
}

// PRComment is one comment on a PR: Path and Line are set for an inline review comment, State for
// a review.
type PRComment struct {
	Author string `json:"author"`
	At     string `json:"at"`
	Body   string `json:"body"`
	Path   string `json:"path,omitempty"`
	Line   int    `json:"line,omitempty"`
	State  string `json:"state,omitempty"`
}

// MergedPR is one recently merged PR.
type MergedPR struct {
	Number   int      `json:"number"`
	Title    string   `json:"title"`
	MergedAt string   `json:"merged_at"`
	Files    []string `json:"files"`
}

// GH is PRs through the gh CLI (the runner's own login).
type GH struct {
	// Env for gh; nil means the runner's own, minus loopd's and Slack's secrets.
	Env []string
	// Dir is where gh runs; set it to a directory that is no git checkout, so gh never acts on a
	// local repository (every call names --repo anyway). Empty: the runner's own directory.
	Dir string
}

func (g GH) run(ctx context.Context, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, "gh", args...)
	cmd.Dir = g.Dir
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

// View reads the PR (gh pr view).
func (g GH) View(ctx context.Context, slug string, pr int) (PRView, error) {
	out, err := g.run(ctx, "pr", "view", strconv.Itoa(pr), "--repo", slug, "--json", "number,title,body,headRefName,headRefOid,baseRefName,state")
	if err != nil {
		return PRView{}, err
	}
	var v PRView
	if err := json.Unmarshal([]byte(out), &v); err != nil {
		return PRView{}, fmt.Errorf("gh pr view: %w", err)
	}
	return v, nil
}

// Comment posts a comment (gh pr comment): a review's verdict, never an approval.
func (g GH) Comment(ctx context.Context, slug string, pr int, bodyFile string) error {
	_, err := g.run(ctx, "pr", "comment", strconv.Itoa(pr), "--repo", slug, "--body-file", bodyFile)
	return err
}

// Comments reads the PR's comments and reviews (gh pr view) and its inline review comments
// (gh api, read only).
func (g GH) Comments(ctx context.Context, slug string, pr int) ([]PRComment, error) {
	out, err := g.run(ctx, "pr", "view", strconv.Itoa(pr), "--repo", slug, "--json", "comments,reviews")
	if err != nil {
		return nil, err
	}
	var v struct {
		Comments []struct {
			Author struct {
				Login string `json:"login"`
			} `json:"author"`
			Body      string `json:"body"`
			CreatedAt string `json:"createdAt"`
		} `json:"comments"`
		Reviews []struct {
			Author struct {
				Login string `json:"login"`
			} `json:"author"`
			Body        string `json:"body"`
			State       string `json:"state"`
			SubmittedAt string `json:"submittedAt"`
		} `json:"reviews"`
	}
	if err := json.Unmarshal([]byte(out), &v); err != nil {
		return nil, fmt.Errorf("gh pr view: %w", err)
	}
	var cs []PRComment
	for _, c := range v.Comments {
		cs = append(cs, PRComment{Author: c.Author.Login, At: c.CreatedAt, Body: c.Body})
	}
	for _, r := range v.Reviews {
		if strings.TrimSpace(r.Body) != "" || r.State == "CHANGES_REQUESTED" {
			cs = append(cs, PRComment{Author: r.Author.Login, At: r.SubmittedAt, Body: r.Body, State: r.State})
		}
	}
	// One page of 100 (a --paginate'd array answer is several JSON arrays back to back).
	out, err = g.run(ctx, "api", "--method", "GET", fmt.Sprintf("repos/%s/pulls/%d/comments?per_page=100", slug, pr))
	if err != nil {
		return nil, err
	}
	var inline []struct {
		User struct {
			Login string `json:"login"`
		} `json:"user"`
		Body      string `json:"body"`
		Path      string `json:"path"`
		Line      int    `json:"line"`
		CreatedAt string `json:"created_at"`
	}
	if err := json.Unmarshal([]byte(out), &inline); err != nil {
		return nil, fmt.Errorf("gh api pulls comments: %w", err)
	}
	for _, c := range inline {
		cs = append(cs, PRComment{Author: c.User.Login, At: c.CreatedAt, Body: c.Body, Path: c.Path, Line: c.Line})
	}
	sort.SliceStable(cs, func(i, j int) bool { return cs[i].At < cs[j].At })
	return cs, nil
}

// MergedPRs lists the last n merged PRs with their files (gh pr list).
func (g GH) MergedPRs(ctx context.Context, slug string, n int) ([]MergedPR, error) {
	out, err := g.run(ctx, "pr", "list", "--repo", slug, "--state", "merged", "--limit", strconv.Itoa(n), "--json", "number,title,mergedAt,files")
	if err != nil {
		return nil, err
	}
	var prs []struct {
		Number   int    `json:"number"`
		Title    string `json:"title"`
		MergedAt string `json:"mergedAt"`
		Files    []struct {
			Path string `json:"path"`
		} `json:"files"`
	}
	if err := json.Unmarshal([]byte(out), &prs); err != nil {
		return nil, fmt.Errorf("gh pr list: %w", err)
	}
	var res []MergedPR
	for _, p := range prs {
		m := MergedPR{Number: p.Number, Title: p.Title, MergedAt: p.MergedAt}
		for _, f := range p.Files {
			m.Files = append(m.Files, f.Path)
		}
		res = append(res, m)
	}
	return res, nil
}

// DryRunPRs opens nothing: every branch gets "PR" 1 (the dry run, with a local remote).
type DryRunPRs struct{}

func (DryRunPRs) Find(context.Context, string, string) (int, error) { return 0, nil }
func (DryRunPRs) Create(context.Context, PRSpec) (int, error)       { return 1, nil }
func (DryRunPRs) Edit(context.Context, int, PRSpec) error           { return nil }
func (DryRunPRs) View(_ context.Context, _ string, pr int) (PRView, error) {
	return PRView{Number: pr, State: "OPEN"}, nil
}
func (DryRunPRs) Comment(context.Context, string, int, string) error { return nil }
func (DryRunPRs) Comments(context.Context, string, int) ([]PRComment, error) {
	return nil, nil
}
func (DryRunPRs) MergedPRs(context.Context, string, int) ([]MergedPR, error) { return nil, nil }

// RunStats are the numbers the PR's footer shows.
type RunStats struct {
	Turns     int
	CacheHit  float64 // 0..1
	Seconds   int
	CostUSD   float64
	HasResult bool
}

// PRBody is the agent's body plus the runner's Verification footer: the runner's own verify steps
// and their results (apart from the agent's claims), the agent's evidence files (uploaded to loopd
// with the item) and the run's turn and cache numbers.
func PRBody(agentBody string, item int64, evidence []EvidenceFile, labels map[string]string, st RunStats, verify []VerifyResult) string {
	var b strings.Builder
	b.WriteString(strings.TrimRight(agentBody, "\n"))
	b.WriteString("\n\n---\n\n" + VerificationHeading + "\n\n")
	if len(verify) > 0 {
		b.WriteString("**Runner verify** (run by the runner after the agent, outside its sandbox, from the runner's config):\n\n")
		for _, v := range verify {
			fmt.Fprintf(&b, "- %s: %s", oneLine(v.Name), v.Status())
			if v.Skipped == "" {
				names := []string{"`" + v.LogFile.Name + "`"}
				for _, e := range v.Evidence {
					names = append(names, "`"+e.Name+"`")
				}
				fmt.Fprintf(&b, "; %s", strings.Join(names, ", "))
			}
			b.WriteString("\n")
		}
		b.WriteString("\n")
	}
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
	b.WriteString("\n" + VerificationTrailer + "\n")
	return b.String()
}

func oneLine(s string) string { return strings.Join(strings.Fields(s), " ") }

func duration(sec int) string {
	if sec < 60 {
		return fmt.Sprintf("%ds", sec)
	}
	return fmt.Sprintf("%dm %02ds", sec/60, sec%60)
}
