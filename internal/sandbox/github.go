package sandbox

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
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
	// Comments are what the runner posted on the PR (the review stage's verdicts), oldest first.
	Comments []string `json:"comments,omitempty"`
	// MergeCommit is the squash commit on main once the land script merged it.
	MergeCommit string `json:"merge_commit,omitempty"`
	// MergeCalls are the land script's merge calls: each head it asked to merge at, and GitHub's
	// answer ("merged" or the refusal).
	MergeCalls []MergeCall `json:"merge_calls,omitempty"`
}

// MergeCall is one gh pr merge.
type MergeCall struct {
	Head   string `json:"head"`
	Result string `json:"result"`
}

// GitHub is the fake GitHub: bare repositories on disk are the remotes the runner pushes to, and
// the runner's PR client (runner.PRs) and PR check (Runner.VerifyPR) are answered here instead of
// by gh. It records each PR (number, head branch and sha, title, body); numbers start at 101.
type GitHub struct {
	mu     sync.Mutex
	prs    []PR
	next   int
	repos  map[string]string // slug -> bare repo path
	merged map[string][]runner.MergedPR

	// checks are the CI checks on a commit ("slug@sha" -> name -> pass, fail, pending, cancel,
	// skip), set by scenario steps; mainCI is how every workflow run on a commit of main goes,
	// and mainCIAt how they went on one commit of main ("slug@sha"), whether merged or pushed.
	checks   map[string]map[string]string
	mainCI   map[string]string
	mainCIAt map[string]map[string]string
	// mainRerun is how a main job goes once re-run (gh run rerun --failed); reran the jobs re-run
	// on a commit of main ("slug@sha" -> job), and Reruns how many re-runs the runner asked for.
	mainRerun map[string]string
	reran     map[string]map[string]bool
	reruns    int
	// mainHead is where each repo's main should be: where it started, then each merge or
	// scripted push. Anything else moving main is a failure.
	mainHead map[string]string
	runIDs   int
	// runs are the Actions runs the fake has shown (a PR check's or a main run's id -> its job
	// name and how it went), for gh run view --log-failed.
	runs map[int64]fakeRun
	// refuse is how many of the next merges GitHub refuses anyway (as when the base moved).
	refuse int
}

var (
	_ runner.PRs        = (*GitHub)(nil)
	_ runner.LandGitHub = (*GitHub)(nil)
)

// NewGitHub returns a fake over these remotes (owner/name -> bare repo).
func NewGitHub(repos map[string]string) *GitHub {
	g := &GitHub{next: 101, repos: repos, checks: map[string]map[string]string{}, mainCI: map[string]string{}, mainCIAt: map[string]map[string]string{}, mainRerun: map[string]string{}, reran: map[string]map[string]bool{}, mainHead: map[string]string{}, runIDs: 9000,
		runs: map[int64]fakeRun{}}
	for slug := range repos {
		g.mainHead[slug] = g.RemoteHead(slug, "main")
	}
	return g
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

// SetMerged sets a repo's recently merged PRs (newest first), what triage sees for dedupe.
func (g *GitHub) SetMerged(slug string, prs []runner.MergedPR) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.merged == nil {
		g.merged = map[string][]runner.MergedPR{}
	}
	g.merged[slug] = prs
}

// MergedPRs answers gh pr list --state merged.
func (g *GitHub) MergedPRs(_ context.Context, slug string, n int) ([]runner.MergedPR, error) {
	if _, ok := g.repos[slug]; !ok {
		return nil, fmt.Errorf("no repository %s", slug)
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	prs := g.merged[slug]
	if len(prs) > n {
		prs = prs[:n]
	}
	return append([]runner.MergedPR(nil), prs...), nil
}

func (g *GitHub) find(slug string, number int) (int, bool) {
	for i, p := range g.prs {
		if p.Repo == slug && p.Number == number {
			return i, true
		}
	}
	return 0, false
}

// View answers gh pr view.
func (g *GitHub) View(_ context.Context, slug string, number int) (runner.PRView, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	i, ok := g.find(slug, number)
	if !ok {
		return runner.PRView{}, fmt.Errorf("no pull request %s#%d", slug, number)
	}
	p := g.prs[i]
	v := runner.PRView{Number: p.Number, Title: p.Title, Body: p.Body, HeadRefName: p.Head, HeadRefOid: g.RemoteHead(slug, p.Head),
		BaseRefName: p.Base, State: p.State}
	v.Author.Login, _, _ = strings.Cut(slug, "/") // the runner opens PRs as the user, the repo's owner
	return v, nil
}

// Comment records a comment (gh pr comment).
func (g *GitHub) Comment(_ context.Context, slug string, number int, bodyFile string) error {
	body, err := os.ReadFile(bodyFile)
	if err != nil {
		return fmt.Errorf("body file: %w", err)
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	i, ok := g.find(slug, number)
	if !ok {
		return fmt.Errorf("no pull request %s#%d", slug, number)
	}
	g.prs[i].Comments = append(g.prs[i].Comments, string(body))
	return nil
}

// Comments answers the runner's read of a PR's comments: the ones it posted, as the user's
// account (gh's login) would show them.
func (g *GitHub) Comments(_ context.Context, slug string, number int) ([]runner.PRComment, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	i, ok := g.find(slug, number)
	if !ok {
		return nil, fmt.Errorf("no pull request %s#%d", slug, number)
	}
	var out []runner.PRComment
	for n, c := range g.prs[i].Comments {
		out = append(out, runner.PRComment{Author: "sandbox", At: fmt.Sprintf("comment %d", n+1), Body: c})
	}
	return out, nil
}

// SetChecks sets the CI checks on a repo's commit (name -> pass, fail, pending, cancel, skip).
func (g *GitHub) SetChecks(slug, sha string, checks map[string]string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	m := map[string]string{}
	for k, v := range checks {
		m[k] = v
	}
	g.checks[slug+"@"+sha] = m
}

// SetMainCI sets how the workflow runs on main's commits go (name -> pass, fail, pending, cancel); empty:
// no runs at all.
func (g *GitHub) SetMainCI(runs map[string]string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.mainCI = map[string]string{}
	for k, v := range runs {
		g.mainCI[k] = v
	}
}

// SetMainCIAt sets how the workflow runs on one commit of main went (name -> pass, fail, pending,
// cancel), for that commit only: it wins over SetMainCI's.
func (g *GitHub) SetMainCIAt(slug, sha string, runs map[string]string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	m := map[string]string{}
	for k, v := range runs {
		m[k] = v
	}
	g.mainCIAt[slug+"@"+sha] = m
}

// SetMainRerun sets how main's jobs go once re-run (name -> pass, fail, pending); a job not named
// goes as it went the first time.
func (g *GitHub) SetMainRerun(runs map[string]string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.mainRerun = map[string]string{}
	for k, v := range runs {
		g.mainRerun[k] = v
	}
}

// RerunFailed answers gh run rerun --failed on a run of main the fake has shown.
func (g *GitHub) RerunFailed(_ context.Context, slug string, id int64) error {
	g.mu.Lock()
	defer g.mu.Unlock()
	r, ok := g.runs[id]
	if !ok || r.slug != slug || r.sha == "" {
		return fmt.Errorf("could not find any workflow run with ID %d", id)
	}
	if !r.failed {
		return fmt.Errorf("run %d has no failed jobs to re-run", id)
	}
	k := slug + "@" + r.sha
	if g.reran[k] == nil {
		g.reran[k] = map[string]bool{}
	}
	g.reran[k][r.job] = true
	g.reruns++
	return nil
}

// Reruns is how many re-runs of main's failed jobs the runner asked for.
func (g *GitHub) Reruns() int {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.reruns
}

// MainHead is where a repo's main should be.
func (g *GitHub) MainHead(slug string) string {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.mainHead[slug]
}

var checkBuckets = map[string]string{"pass": runner.BucketPass, "fail": runner.BucketFail, "pending": runner.BucketPending,
	"cancel": runner.BucketCancel, "skip": runner.BucketSkipping}

// mergeTree says whether head merges into the base cleanly (git merge-tree), and the tree.
func (g *GitHub) mergeTree(slug, base, head string) (string, bool) {
	cmd := exec.Command("git", "-C", g.repos[slug], "merge-tree", "--write-tree", "refs/heads/"+base, head)
	out, err := cmd.Output()
	first, _, _ := strings.Cut(strings.TrimSpace(string(out)), "\n")
	return first, err == nil
}

// LandView answers the land script's gh pr view: the PR, its branch's head on the remote, the
// checks scripted for that head, and its mergeability, computed with git as GitHub would.
func (g *GitHub) LandView(_ context.Context, slug string, number int) (runner.LandPR, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	i, ok := g.find(slug, number)
	if !ok {
		return runner.LandPR{}, fmt.Errorf("no pull request %s#%d", slug, number)
	}
	p := g.prs[i]
	pr := runner.LandPR{Number: p.Number, State: p.State, HeadRefName: p.Head, HeadRefOid: p.HeadSHA, BaseRefName: p.Base,
		Body: p.Body, MergeCommit: p.MergeCommit, MergeStateStatus: "UNKNOWN", Mergeable: "UNKNOWN"}
	if p.State == "OPEN" {
		pr.HeadRefOid = g.RemoteHead(slug, p.Head)
		if _, clean := g.mergeTree(slug, p.Base, pr.HeadRefOid); clean {
			pr.Mergeable, pr.MergeStateStatus = "MERGEABLE", "CLEAN"
		} else {
			pr.Mergeable, pr.MergeStateStatus = "CONFLICTING", "DIRTY"
		}
	}
	checks := g.checks[slug+"@"+pr.HeadRefOid]
	names := make([]string, 0, len(checks))
	for n := range checks {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		b := checkBuckets[checks[n]]
		if b == "" {
			b = runner.BucketFail
		}
		id := g.runID(slug, pr.HeadRefOid, n, b == runner.BucketFail)
		pr.Checks = append(pr.Checks, runner.Check{Name: n, Bucket: b, URL: fmt.Sprintf("https://github.com/%s/actions/runs/%d/job/%d", slug, id, id+1)})
	}
	return pr, nil
}

// Merge is gh pr merge --squash --delete-branch --match-head-commit: it refuses a PR that is not
// open, a head that moved, or a conflict; otherwise it squashes the head onto main as one commit,
// moves main and deletes the branch.
func (g *GitHub) Merge(_ context.Context, slug string, number int, head string) error {
	g.mu.Lock()
	defer g.mu.Unlock()
	i, ok := g.find(slug, number)
	if !ok {
		return fmt.Errorf("no pull request %s#%d", slug, number)
	}
	p := &g.prs[i]
	refuse := func(msg string) error {
		p.MergeCalls = append(p.MergeCalls, MergeCall{Head: head, Result: msg})
		return fmt.Errorf("gh pr merge: %s", msg)
	}
	if p.State != "OPEN" {
		return refuse("Pull request " + slug + "#" + strconv.Itoa(number) + " is not open")
	}
	if cur := g.RemoteHead(slug, p.Head); cur != head {
		return refuse("Head branch was modified. Review and try the merge again.")
	}
	tree, clean := g.mergeTree(slug, p.Base, head)
	if !clean {
		return refuse("Pull request is not mergeable: the merge commit cannot be cleanly created.")
	}
	if g.refuse > 0 {
		g.refuse--
		return refuse("Base branch was modified. Review and try the merge again.")
	}
	repo := g.repos[slug]
	old := g.RemoteHead(slug, p.Base)
	commit, err := git(repo, "commit-tree", tree, "-p", old, "-m", fmt.Sprintf("%s (#%d)", p.Title, p.Number))
	if err != nil {
		return refuse(err.Error())
	}
	if _, err := git(repo, "update-ref", "refs/heads/"+p.Base, commit, old); err != nil {
		return refuse(err.Error())
	}
	if _, err := git(repo, "update-ref", "-d", "refs/heads/"+p.Head); err != nil {
		return refuse(err.Error())
	}
	p.State, p.MergeCommit, p.HeadSHA = "MERGED", commit, head
	p.MergeCalls = append(p.MergeCalls, MergeCall{Head: head, Result: "merged"})
	if p.Base == "main" {
		g.mainHead[slug] = commit
	}
	return nil
}

// CommitRuns answers gh run list --commit: on a commit of main, one run per scripted main CI job.
func (g *GitHub) CommitRuns(_ context.Context, slug, sha string) ([]runner.WorkflowRun, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if _, ok := g.repos[slug]; !ok {
		return nil, fmt.Errorf("no repository %s", slug)
	}
	ci, onMain := g.mainCIAt[slug+"@"+sha]
	if !onMain {
		ci = g.mainCI
	}
	for _, p := range g.prs {
		onMain = onMain || p.Repo == slug && p.MergeCommit == sha
	}
	if !onMain {
		return nil, nil
	}
	names := make([]string, 0, len(ci))
	for n := range ci {
		names = append(names, n)
	}
	sort.Strings(names)
	var runs []runner.WorkflowRun
	for i, n := range names {
		id := int64(g.runIDs + i)
		r := runner.WorkflowRun{ID: id, Name: n, Workflow: n, URL: fmt.Sprintf("https://github.com/%s/actions/runs/%d", slug, id)}
		res := ci[n]
		if g.reran[slug+"@"+sha][n] {
			if v, ok := g.mainRerun[n]; ok {
				res = v
			}
		}
		g.runs[id] = fakeRun{slug: slug, sha: sha, job: n, failed: res == "fail"}
		switch res {
		case "pass":
			r.Status, r.Conclusion = "completed", "success"
		case "fail":
			r.Status, r.Conclusion = "completed", "failure"
		case "cancel":
			r.Status, r.Conclusion = "completed", "cancelled"
		default:
			r.Status = "in_progress"
		}
		runs = append(runs, r)
	}
	return runs, nil
}

// fakeRun is an Actions run the fake has shown.
type fakeRun struct {
	slug, sha, job string
	failed         bool
}

// runID is the id of a PR check's run: the same for the same repo, commit and check (ids from
// 7000, below main's).
func (g *GitHub) runID(slug, sha, name string, failed bool) int64 {
	for id, r := range g.runs {
		if r.slug == slug && r.sha == sha && r.job == name {
			r.failed = failed
			g.runs[id] = r
			return id
		}
	}
	id := int64(7000 + 2*len(g.runs))
	g.runs[id] = fakeRun{slug: slug, sha: sha, job: name, failed: failed}
	return id
}

// FailedLog answers gh run view --log-failed: for a failed run, a short Go test failure in gh's
// format (job, step, timestamp), with colour codes and a token-like string the runner must strip.
func (g *GitHub) FailedLog(_ context.Context, slug string, id int64) (string, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	r, ok := g.runs[id]
	if !ok || r.slug != slug {
		return "", fmt.Errorf("could not find any workflow run with ID %d", id)
	}
	if !r.failed {
		return "", nil
	}
	var b strings.Builder
	for i, l := range []string{
		"ok  \tgithub.com/sandbox/demo/internal/fine\t0.1s",
		"\x1b[31m--- FAIL: TestDigest (0.02s)\x1b[0m",
		"    digest_test.go:12: got \"old wording\", want \"new wording\" (GH_TOKEN=ghp_sandboxfaketoken0123456789)",
		"FAIL",
		"FAIL\tgithub.com/sandbox/demo/internal/digest\t0.3s",
		"##[error]Process completed with exit code 1.",
	} {
		fmt.Fprintf(&b, "%s\ttest\t2026-10-16T12:00:%02d.0000000Z %s\n", r.job, i, l)
	}
	return b.String(), nil
}

// Push commits content to a file on a branch of the remote, as someone else would (a teammate's
// commit on the PR's branch, or a change landing on main meanwhile).
func (g *GitHub) Push(slug, branch, file, content, msg string) (string, error) {
	return g.PushFile(slug, branch, file, content, msg, 0o644)
}

// PushFile is Push with the file's mode (0o755: a script).
func (g *GitHub) PushFile(slug, branch, file, content, msg string, mode os.FileMode) (string, error) {
	repo := g.repos[slug]
	if repo == "" {
		return "", fmt.Errorf("no repository %s", slug)
	}
	tmp, err := os.MkdirTemp("", "fake-gh-push-")
	if err != nil {
		return "", err
	}
	defer os.RemoveAll(tmp)
	if _, err := git(tmp, "clone", "-q", "--branch", branch, repo, "."); err != nil {
		return "", err
	}
	if err := os.MkdirAll(filepath.Dir(filepath.Join(tmp, file)), 0o755); err != nil {
		return "", err
	}
	if err := os.WriteFile(filepath.Join(tmp, file), []byte(content), mode); err != nil {
		return "", err
	}
	if err := os.Chmod(filepath.Join(tmp, file), mode); err != nil {
		return "", err
	}
	for _, args := range [][]string{{"add", "-A"}, {"commit", "-q", "-m", msg}, {"push", "-q", "origin", "HEAD:refs/heads/" + branch}} {
		if _, err := git(tmp, args...); err != nil {
			return "", err
		}
	}
	head := g.RemoteHead(slug, branch)
	if branch == "main" {
		g.mu.Lock()
		g.mainHead[slug] = head
		g.mu.Unlock()
	}
	return head, nil
}

// RefuseMerges makes GitHub refuse the next n merges.
func (g *GitHub) RefuseMerges(n int) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.refuse = n
}

// MergeByHand merges an item's PR as someone on GitHub would, outside the loop.
func (g *GitHub) MergeByHand(slug, branch string) error {
	g.mu.Lock()
	n := 0
	for _, p := range g.prs {
		if p.Repo == slug && p.Head == branch && p.State == "OPEN" {
			n = p.Number
		}
	}
	g.mu.Unlock()
	if n == 0 {
		return fmt.Errorf("no open PR for %s", branch)
	}
	return g.Merge(context.Background(), slug, n, g.RemoteHead(slug, branch))
}

// PRState answers gh pr view --json state.
func (g *GitHub) PRState(_ context.Context, slug string, number int) (string, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	i, ok := g.find(slug, number)
	if !ok {
		return "", fmt.Errorf("no pull request %s#%d", slug, number)
	}
	return g.prs[i].State, nil
}

// ClosePR closes an open PR with a comment, as gh pr close does; the branch stays. Closing a
// closed or merged PR is GitHub's refusal, which the runner must never cause (it reads the state
// first).
func (g *GitHub) ClosePR(_ context.Context, slug string, number int, comment string) error {
	g.mu.Lock()
	defer g.mu.Unlock()
	i, ok := g.find(slug, number)
	if !ok {
		return fmt.Errorf("no pull request %s#%d", slug, number)
	}
	if g.prs[i].State != "OPEN" {
		return fmt.Errorf("pull request %s#%d is already %s", slug, number, strings.ToLower(g.prs[i].State))
	}
	g.prs[i].State = "CLOSED"
	g.prs[i].Comments = append(g.prs[i].Comments, comment)
	return nil
}

// CloseByHand closes an item's open PR as someone on GitHub would, with no comment.
func (g *GitHub) CloseByHand(slug, branch string) error {
	g.mu.Lock()
	defer g.mu.Unlock()
	for i, p := range g.prs {
		if p.Repo == slug && p.Head == branch && p.State == "OPEN" {
			g.prs[i].State = "CLOSED"
			return nil
		}
	}
	return fmt.Errorf("no open PR for %s", branch)
}
