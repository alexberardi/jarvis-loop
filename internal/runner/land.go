package runner

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
)

// Land is plain code, no agent (docs/phases/06-land.md): no claude process, no model, nothing an
// agent wrote is read as an instruction. Every gate is re-derived from GitHub and git on every
// pass, never from an agent's words.

// StageLand is the land stage, which a runner claims when its config's stages name it.
const StageLand = "land"

// RunnerStages are every stage a runner may claim: the agent stages and land.
var RunnerStages = []string{StageTriage, StageBuild, StageReview, StageLand}

// The runner's Verification footer on every PR it opens (PRBody). Land requires the heading and
// the body to end with the trailer: the runner appends the footer after the agent's text, so an
// agent quoting these lines in its own body does not pass for it.
const (
	VerificationHeading = "### Verification"
	VerificationTrailer = "*Built by a jarvis-loop agent; pushed and opened by the runner.*"
)

// Check buckets, as gh pr checks names them.
const (
	BucketPass     = "pass"
	BucketFail     = "fail"
	BucketCancel   = "cancel"
	BucketPending  = "pending"
	BucketSkipping = "skipping"
)

// Check is one CI check on a PR's head commit.
type Check struct {
	Name   string `json:"name"`
	Bucket string `json:"bucket"`
	URL    string `json:"url,omitempty"`
}

// LandPR is what GitHub says about a PR, everything the gates need in one read.
type LandPR struct {
	Number           int
	State            string // OPEN, CLOSED, MERGED
	IsDraft          bool
	HeadRefName      string
	HeadRefOid       string
	BaseRefName      string
	Mergeable        string // MERGEABLE, CONFLICTING, UNKNOWN
	MergeStateStatus string // CLEAN, BEHIND, BLOCKED, DIRTY, ...
	Body             string
	MergeCommit      string // set once merged
	Checks           []Check
}

// WorkflowRun is one Actions run on a commit (main's CI after a merge).
type WorkflowRun struct {
	Name       string `json:"name"`
	Workflow   string `json:"workflowName"`
	Status     string `json:"status"`
	Conclusion string `json:"conclusion"`
	URL        string `json:"url"`
}

// LandGitHub is what the land script asks GitHub, with the runner's own login (gh). The agent
// never holds it.
type LandGitHub interface {
	// LandView reads a PR's state, head, base, mergeability, body and the checks on its head.
	LandView(ctx context.Context, slug string, pr int) (LandPR, error)
	// Merge squash-merges the PR only if its head is still head (--match-head-commit), and
	// deletes its branch. Never --auto, never --admin.
	Merge(ctx context.Context, slug string, pr int, head string) error
	// CommitRuns lists the Actions runs on a commit.
	CommitRuns(ctx context.Context, slug, sha string) ([]WorkflowRun, error)
}

// LandFacts are the gates' inputs, re-derived on every pass.
type LandFacts struct {
	PR LandPR
	// Branch is the item's branch and Base the repo's configured base.
	Branch, Base string
	// ReviewedHead is the head_sha of the item's last review approval ("" when there is none).
	ReviewedHead string
	// Paths are the PR's changed files, computed by the runner from git (never reported to it).
	Paths []string
	// BuildEvidence is how many evidence files the item's last pr_opened run attached.
	BuildEvidence int
	// Hold: loopd's risk is hold, or loopd's hold rules match Paths.
	Hold bool
	// ApprovedHead is the head the user approved a held merge for ("" when none).
	ApprovedHead string
}

// Action is what a pass of the gates decided.
type Action int

// The actions.
const (
	// ActAdvance reports Outcome with Fields to loopd; the land run ends.
	ActAdvance Action = iota
	// ActWait waits (CI pending, mergeability unknown) and runs the gates again.
	ActWait
	// ActConflict merges the base into the branch, if git can, and sends the new head to review.
	ActConflict
	// ActMerge squash-merges the PR at the head the gates checked.
	ActMerge
)

func (a Action) String() string {
	return [...]string{"advance", "wait", "conflict", "merge"}[a]
}

// Decision is one pass of the gates.
type Decision struct {
	Action  Action
	Outcome string
	Fields  map[string]string
	// Wait names what is pending, for the wait's heartbeat note and its time-out message.
	Wait string
}

func advance(outcome string, fields map[string]string) Decision {
	return Decision{Action: ActAdvance, Outcome: outcome, Fields: fields}
}

func failed(format string, args ...any) Decision {
	return advance("failed", map[string]string{"error": fmt.Sprintf(format, args...)})
}

// Decide runs the gates in the spec's order (docs/phases/06-land.md): the PR, review approval
// for this head, the changed paths, CI, harness evidence, hold, mergeable. The first gate that
// does not pass decides.
func Decide(f LandFacts) Decision {
	pr := f.PR
	// 1. The PR.
	switch pr.State {
	case "MERGED":
		if pr.MergeCommit == "" {
			return failed("PR #%d is merged, but GitHub gave no merge commit", pr.Number)
		}
		// Merged by hand: record it (loopd still requires an approval for a held item).
		return advance("merged", map[string]string{"merge_sha": pr.MergeCommit, "head_sha": pr.HeadRefOid, "merged_outside": "true"})
	case "OPEN":
	case "CLOSED":
		return failed("PR #%d was closed without merging", pr.Number)
	default:
		return failed("PR #%d is %q, not open", pr.Number, pr.State)
	}
	if pr.IsDraft {
		return failed("PR #%d is a draft", pr.Number)
	}
	if pr.BaseRefName != f.Base {
		return failed("PR #%d targets %s, not the configured base %s", pr.Number, pr.BaseRefName, f.Base)
	}
	if pr.HeadRefName != f.Branch {
		return failed("PR #%d's head is branch %s, not the item's %s", pr.Number, pr.HeadRefName, f.Branch)
	}
	// 2. Review approval for this head.
	if f.ReviewedHead != pr.HeadRefOid {
		notes := fmt.Sprintf("The PR's head is `%s`, but the review approved `%s`: the new head needs its own review (it moved since review).",
			short7(pr.HeadRefOid), short7(f.ReviewedHead))
		if f.ReviewedHead == "" {
			notes = fmt.Sprintf("The item has no review approval on record for `%s`: it needs a review.", short7(pr.HeadRefOid))
		}
		return advance("back_to_review", map[string]string{"notes": notes, "head_sha": pr.HeadRefOid})
	}
	// 3. The changed paths (computed by the caller; loopd recomputes risk on them).
	if len(f.Paths) == 0 {
		return failed("PR #%d changes no files against %s", pr.Number, f.Base)
	}
	// 4. CI on this head.
	var fails, pending []string
	for _, c := range pr.Checks {
		switch c.Bucket {
		case BucketPass, BucketSkipping:
		case BucketPending:
			pending = append(pending, c.Name)
		default: // fail, cancel, and anything gh may add
			s := c.Name
			if c.URL != "" {
				s += " (" + c.URL + ")"
			}
			fails = append(fails, s)
		}
	}
	if len(fails) > 0 {
		return advance("back_to_build", map[string]string{"reason": fmt.Sprintf("CI failed on `%s`: %s", short7(pr.HeadRefOid), strings.Join(fails, ", "))})
	}
	if len(pr.Checks) == 0 {
		// A repo with no CI configured is a configuration error, not a pass.
		return Decision{Action: ActWait, Wait: "CI (no checks reported yet)"}
	}
	if len(pending) > 0 {
		return Decision{Action: ActWait, Wait: "CI (" + strings.Join(pending, ", ") + ")"}
	}
	// 5. Harness evidence.
	if f.BuildEvidence < 1 {
		return advance("back_to_build", map[string]string{"reason": "No harness evidence from build: the last pr_opened run attached no evidence file."})
	}
	if !strings.Contains(pr.Body, VerificationHeading) || !strings.HasSuffix(strings.TrimSpace(pr.Body), VerificationTrailer) {
		return advance("back_to_build", map[string]string{"reason": "The PR body has no runner Verification footer (the evidence list the runner writes)."})
	}
	// 6. Hold.
	if f.Hold && f.ApprovedHead != pr.HeadRefOid {
		return advance("hold", map[string]string{"reason": "on the hold list", "head_sha": pr.HeadRefOid})
	}
	// 7. Mergeable.
	switch pr.Mergeable {
	case "MERGEABLE":
		return Decision{Action: ActMerge} // behind but clean merges as is; main's CI catches the rest
	case "CONFLICTING":
		return Decision{Action: ActConflict}
	}
	return Decision{Action: ActWait, Wait: "GitHub's mergeability check"}
}

// outsideDocs returns the paths not under docs/.
func outsideDocs(paths []string) []string {
	var out []string
	for _, p := range paths {
		if !strings.HasPrefix(p, "docs/") {
			out = append(out, p)
		}
	}
	return out
}

// landViewFields are the PR fields LandView asks gh for.
const landViewFields = "number,state,isDraft,headRefName,headRefOid,baseRefName,mergeable,mergeStateStatus,body,mergeCommit,statusCheckRollup"

// ParseLandView reads gh pr view's JSON (landViewFields). The checks come from the status
// rollup of the head commit: check runs and commit statuses, one per name (a re-run replaces
// the earlier run), bucketed as gh pr checks does.
func ParseLandView(raw []byte) (LandPR, error) {
	var v struct {
		Number           int    `json:"number"`
		State            string `json:"state"`
		IsDraft          bool   `json:"isDraft"`
		HeadRefName      string `json:"headRefName"`
		HeadRefOid       string `json:"headRefOid"`
		BaseRefName      string `json:"baseRefName"`
		Mergeable        string `json:"mergeable"`
		MergeStateStatus string `json:"mergeStateStatus"`
		Body             string `json:"body"`
		MergeCommit      *struct {
			Oid string `json:"oid"`
		} `json:"mergeCommit"`
		Rollup []struct {
			Typename     string `json:"__typename"`
			Name         string `json:"name"`
			WorkflowName string `json:"workflowName"`
			Status       string `json:"status"`
			Conclusion   string `json:"conclusion"`
			DetailsURL   string `json:"detailsUrl"`
			StartedAt    string `json:"startedAt"`
			Context      string `json:"context"`
			State        string `json:"state"`
			TargetURL    string `json:"targetUrl"`
		} `json:"statusCheckRollup"`
	}
	if err := json.Unmarshal(raw, &v); err != nil {
		return LandPR{}, fmt.Errorf("gh pr view: %w", err)
	}
	if v.Number <= 0 || v.State == "" {
		return LandPR{}, fmt.Errorf("gh pr view: no PR in the answer")
	}
	pr := LandPR{Number: v.Number, State: v.State, IsDraft: v.IsDraft, HeadRefName: v.HeadRefName, HeadRefOid: v.HeadRefOid,
		BaseRefName: v.BaseRefName, Mergeable: v.Mergeable, MergeStateStatus: v.MergeStateStatus, Body: v.Body}
	if v.MergeCommit != nil {
		pr.MergeCommit = v.MergeCommit.Oid
	}
	type latest struct {
		c       Check
		started string
		order   int
	}
	byName := map[string]latest{}
	for i, r := range v.Rollup {
		name, url := r.Name, r.DetailsURL
		if r.Typename == "StatusContext" {
			name, url = r.Context, r.TargetURL
		} else if r.WorkflowName != "" {
			name = r.WorkflowName + "/" + r.Name
		}
		c := Check{Name: name, Bucket: checkBucket(r.Typename, r.Status, r.Conclusion, r.State), URL: url}
		if old, ok := byName[name]; ok && old.started > r.StartedAt {
			continue
		}
		byName[name] = latest{c: c, started: r.StartedAt, order: i}
	}
	all := make([]latest, 0, len(byName))
	for _, l := range byName {
		all = append(all, l)
	}
	sort.Slice(all, func(i, j int) bool { return all[i].order < all[j].order })
	for _, l := range all {
		pr.Checks = append(pr.Checks, l.c)
	}
	return pr, nil
}

// checkBucket buckets one rollup entry as gh pr checks does. Anything gh might add later that is
// not known to pass is a failure: an unknown conclusion never merges.
func checkBucket(typename, status, conclusion, state string) string {
	if typename == "StatusContext" {
		switch state {
		case "SUCCESS":
			return BucketPass
		case "PENDING", "EXPECTED":
			return BucketPending
		}
		return BucketFail
	}
	if status != "COMPLETED" {
		return BucketPending
	}
	switch conclusion {
	case "SUCCESS", "NEUTRAL":
		return BucketPass
	case "SKIPPED":
		return BucketSkipping
	case "CANCELLED":
		return BucketCancel
	}
	return BucketFail
}

// ParseRuns reads gh run list's JSON.
func ParseRuns(raw []byte) ([]WorkflowRun, error) {
	var runs []WorkflowRun
	if err := json.Unmarshal(raw, &runs); err != nil {
		return nil, fmt.Errorf("gh run list: %w", err)
	}
	return runs, nil
}

// MainResult is main's CI on a merge commit: "" while there are no runs yet or any is still going,
// "green" when every run passed (success, neutral, skipped), else "red" with the failing runs'
// names.
func MainResult(runs []WorkflowRun) (string, []string) {
	if len(runs) == 0 {
		return "", nil
	}
	var failing []string
	for _, r := range runs {
		if r.Status != "completed" {
			return "", nil
		}
		switch r.Conclusion {
		case "success", "neutral", "skipped":
		default:
			name := r.Name
			if name == "" {
				name = r.Workflow
			}
			failing = append(failing, name)
		}
	}
	if len(failing) > 0 {
		return "red", failing
	}
	return "green", nil
}
