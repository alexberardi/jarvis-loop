package sandbox

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"sync"

	"github.com/alexberardi/jarvis-loop/internal/runner"
)

// PR is a pull request as the fake GitHub recorded it.
type PR struct {
	Number  int    `json:"number"`
	Repo    string `json:"repo"`
	Head    string `json:"head"`
	Base    string `json:"base"`
	Title   string `json:"title"`
	Body    string `json:"body"`
	HeadSHA string `json:"head_sha"` // the branch's head in the remote when the PR was opened or edited
	State   string `json:"state"`
	Edits   int    `json:"edits,omitempty"`
}

// GitHub is the fake GitHub: bare repositories on disk are the remotes the runner pushes to, and
// the runner's PR client (runner.PRs) and PR check (Runner.VerifyPR) are answered here instead of
// by gh. It records each PR (number, head branch and sha, title, body); numbers start at 101.
type GitHub struct {
	mu    sync.Mutex
	prs   []PR
	next  int
	repos map[string]string // slug -> bare repo path
}

var _ runner.PRs = (*GitHub)(nil)

// NewGitHub returns a fake over these remotes (owner/name -> bare repo).
func NewGitHub(repos map[string]string) *GitHub {
	return &GitHub{next: 101, repos: repos}
}

// PRs returns the recorded PRs.
func (g *GitHub) PRs() []PR {
	g.mu.Lock()
	defer g.mu.Unlock()
	return append([]PR(nil), g.prs...)
}

// RemoteHead is a branch's head in the remote ("" when it does not exist).
func (g *GitHub) RemoteHead(slug, branch string) string {
	repo := g.repos[slug]
	if repo == "" {
		return ""
	}
	out, err := exec.Command("git", "-C", repo, "rev-parse", "--verify", "-q", "refs/heads/"+branch).Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

// Branches lists a remote's branches.
func (g *GitHub) Branches(slug string) []string {
	out, err := exec.Command("git", "-C", g.repos[slug], "for-each-ref", "--format=%(refname:short)", "refs/heads/").Output()
	if err != nil {
		return nil
	}
	return strings.Fields(string(out))
}

// Subject is a commit's subject line in a remote.
func (g *GitHub) Subject(slug, rev string) string {
	out, _ := exec.Command("git", "-C", g.repos[slug], "log", "-1", "--format=%s", rev).Output()
	return strings.TrimSpace(string(out))
}

// Files lists the files a commit changed in a remote.
func (g *GitHub) Files(slug, rev string) []string {
	out, _ := exec.Command("git", "-C", g.repos[slug], "show", "--name-only", "--format=", rev).Output()
	return strings.Fields(string(out))
}

// Find returns the open PR whose head is branch, or 0 (gh pr list --head).
func (g *GitHub) Find(_ context.Context, slug, branch string) (int, error) {
	if _, ok := g.repos[slug]; !ok {
		return 0, fmt.Errorf("no repository %s", slug)
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	for _, p := range g.prs {
		if p.Repo == slug && p.Head == branch && p.State == "OPEN" {
			return p.Number, nil
		}
	}
	return 0, nil
}

func (g *GitHub) spec(s runner.PRSpec) (string, string, error) {
	if _, ok := g.repos[s.Slug]; !ok {
		return "", "", fmt.Errorf("no repository %s", s.Slug)
	}
	head := g.RemoteHead(s.Slug, s.Head)
	if head == "" {
		return "", "", fmt.Errorf("head ref %s must be a branch that exists on the remote", s.Head)
	}
	body, err := os.ReadFile(s.BodyFile)
	if err != nil {
		return "", "", fmt.Errorf("body file: %w", err)
	}
	return head, string(body), nil
}

// Create opens a PR (gh pr create): the head branch must already be on the remote.
func (g *GitHub) Create(_ context.Context, s runner.PRSpec) (int, error) {
	head, body, err := g.spec(s)
	if err != nil {
		return 0, err
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	for _, p := range g.prs {
		if p.Repo == s.Slug && p.Head == s.Head && p.State == "OPEN" {
			return 0, fmt.Errorf("a pull request for branch %s already exists: #%d", s.Head, p.Number)
		}
	}
	pr := PR{Number: g.next, Repo: s.Slug, Head: s.Head, Base: s.Base, Title: s.Title, Body: body, HeadSHA: head, State: "OPEN"}
	g.next++
	g.prs = append(g.prs, pr)
	return pr.Number, nil
}

// Edit replaces a PR's title and body (gh pr edit).
func (g *GitHub) Edit(_ context.Context, number int, s runner.PRSpec) error {
	head, body, err := g.spec(s)
	if err != nil {
		return err
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	for i, p := range g.prs {
		if p.Repo == s.Slug && p.Number == number {
			g.prs[i].Title, g.prs[i].Body, g.prs[i].HeadSHA = s.Title, body, head
			g.prs[i].Edits++
			return nil
		}
	}
	return fmt.Errorf("no pull request %s#%d", s.Slug, number)
}

// Verify is the runner's PR check against the fake: the PR's head branch, the branch's current
// head in the remote, and its state (gh pr view).
func (g *GitHub) Verify(_ context.Context, slug string, number int) (runner.PRInfo, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	for _, p := range g.prs {
		if p.Repo == slug && p.Number == number {
			return runner.PRInfo{HeadRefName: p.Head, HeadRefOid: g.RemoteHead(slug, p.Head), State: p.State}, nil
		}
	}
	return runner.PRInfo{}, fmt.Errorf("no pull request %s#%d", slug, number)
}
