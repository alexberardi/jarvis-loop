// Package pipeline holds loopd's rules as pure functions: the stages, the outcomes that move an
// item between them, which waiting item a runner gets, and when a lease has run out. It does no
// I/O; the store calls it inside its transactions.
package pipeline

import (
	"fmt"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"
)

// Stage is where an item is in the pipeline.
type Stage string

// The stages (docs/phases/02-loopd-core.md, Stages and outcomes).
const (
	Triage   Stage = "triage"
	Build    Stage = "build"
	Review   Stage = "review"
	Land     Stage = "land"
	NeedsYou Stage = "needs_you"
	Held     Stage = "held"
	// QA is a merged item waiting for the user's own check (docs/decisions/2026-10-15-qa.md):
	// nothing claims it; the user's Verified or Reopen moves it to done.
	QA     Stage = "qa"
	Done   Stage = "done"
	Closed Stage = "closed"
)

// AllStages lists every stage, in pipeline order.
var AllStages = []Stage{Triage, Build, Review, Land, NeedsYou, Held, QA, Done, Closed}

// IsWork reports whether runners can claim items in this stage.
func (s Stage) IsWork() bool {
	return s == Triage || s == Build || s == Review || s == Land
}

// Valid reports whether s is a known stage.
func (s Stage) Valid() bool { return slices.Contains(AllStages, s) }

// Display is the name shown on the board: a work stage reads differently while it waits.
func Display(s Stage, claimed bool) string {
	switch s {
	case Triage:
		if claimed {
			return "triage"
		}
		return "intake"
	case Build:
		if claimed {
			return "build"
		}
		return "ready"
	case Review, Land:
		if claimed {
			return string(s)
		}
		return string(s) + " (queued)"
	case NeedsYou:
		return "needs-you"
	}
	return string(s)
}

// Outcome is how a work stage ended.
type Outcome string

// The outcomes a lease holder may report.
const (
	Reproduced Outcome = "reproduced"
	// Routed is triage naming the repo of an item that has none, without triaging it: the item
	// stays in triage, and the next run has that one repo checked out (docs/phases/06b-all-repos.md).
	Routed          Outcome = "routed"
	NotReproducible Outcome = "not_reproducible"
	Question        Outcome = "question"
	PROpened        Outcome = "pr_opened"
	// VerifyFailed is the runner's own verify step failing after a build: back to build, counted
	// like a review bounce.
	VerifyFailed Outcome = "verify_failed"
	Approved     Outcome = "approved"
	Bounce       Outcome = "bounce"
	Merged       Outcome = "merged"
	Hold         Outcome = "hold"
	BackToBuild  Outcome = "back_to_build"
	BackToReview Outcome = "back_to_review"
	UsageLimit   Outcome = "usage_limit"
	Failed       Outcome = "failed"
	// CIPending is land giving back its lease while the PR's CI is still pending (not failed): the
	// item stays in land and is claimed again after LandRequeueAfter (cipending.go).
	CIPending Outcome = "ci_pending"
)

// Risk values set by triage.
const (
	RiskNormal = "normal"
	RiskHold   = "hold"
)

// Resources a runner may offer and an item may need.
var Resources = []string{"gpu", "voice", "emulator", "macos"}

// Sources an item may come from.
var Sources = []string{"user", "finding", "nightly", "scout"}

// Sizes set by triage.
var Sizes = []string{"S", "M", "L"}

// Error codes. They are part of the API (the "code" in a JSON error).
const (
	CodeWrongStage       = "wrong_stage"
	CodeUnknownOutcome   = "unknown_outcome"
	CodeMissingField     = "missing_field"
	CodeInvalidField     = "invalid_field"
	CodeSHAMismatch      = "sha_mismatch"
	CodeEvidenceRequired = "evidence_required"
	CodeApprovalRequired = "approval_required"
	CodeSameAgent        = "same_agent"
)

// Error is a refused transition.
type Error struct {
	Code    string
	Message string
}

func (e *Error) Error() string { return e.Code + ": " + e.Message }

func errf(code, format string, args ...any) *Error {
	return &Error{Code: code, Message: fmt.Sprintf(format, args...)}
}

// Item is one unit of work.
type Item struct {
	ID           int64     `json:"id"`
	Title        string    `json:"title"`
	Ask          string    `json:"ask"`
	Repo         string    `json:"repo"`
	Source       string    `json:"source"`
	Priority     string    `json:"priority"`
	Stage        Stage     `json:"stage"`
	ReturnStage  Stage     `json:"return_stage,omitempty"`
	Question     string    `json:"question,omitempty"`
	Area         string    `json:"area,omitempty"`
	Size         string    `json:"size,omitempty"`
	Risk         string    `json:"risk"`
	HoldReason   string    `json:"hold_reason,omitempty"`
	Resources    []string  `json:"resources"`
	BounceCount  int       `json:"bounce_count"`
	PR           int       `json:"pr,omitempty"`
	Branch       string    `json:"branch,omitempty"`
	HeadSHA      string    `json:"head_sha,omitempty"`
	BuilderAgent string    `json:"builder_agent,omitempty"`
	MergeSHA     string    `json:"merge_sha,omitempty"`
	Summary      string    `json:"summary,omitempty"`
	Approval     *Approval `json:"approval,omitempty"`
	// Trust is verify's refusal of a trusted-file change and the approver's OK for it (trust.go).
	Trust *Trust `json:"trust,omitempty"`
	// NeedsQA: once merged, the item waits in qa for the user's check instead of going to done.
	NeedsQA bool `json:"needs_qa"`
	// ReopenOf is the qa item whose Reopen filed this one.
	ReopenOf int64 `json:"reopen_of,omitempty"`
	// AwaitingDeploy: the item is in qa, but its repo's change runs only once deployed (the loop
	// itself): the user is asked to check after a runner reports a build that contains the merge
	// (docs/decisions/2026-10-16-qa-after-deploy.md).
	AwaitingDeploy bool `json:"awaiting_deploy,omitempty"`
	// Proof is how the loop can tell, once merged, that the change works (proof.go): the build's,
	// or the proof sweep's for an item that reached qa without one.
	Proof *Proof `json:"proof,omitempty"`
	// CIRedSHA is the head land sent back to build for red CI; a pr_opened with the same head
	// goes to the user, not review (the build changed nothing, so CI would stay red).
	CIRedSHA string `json:"ci_red_sha,omitempty"`
	// CIMainFixed is main's head when land, at that red CI, saw main red at the PR's base and green
	// now: the question for a build that changes nothing then recommends a rebuild (a rebase), since
	// a rerun tests the same merge again (docs/decisions/2026-10-17-needs-you-rebuild.md).
	CIMainFixed string `json:"ci_main_fixed,omitempty"`
	// DependsOn are the items this one waits for: claim skips it until they are all merged (qa or done).
	DependsOn []int64 `json:"depends_on"`
	// PlanID and PlanPiece link an item filed from a plan to its piece (docs/phases/07-planning.md).
	PlanID    int64 `json:"plan_id,omitempty"`
	PlanPiece int   `json:"plan_piece,omitempty"`
	// Builder is who builds it (BuilderQwen, BuilderClaude; "": not from a plan, claude): the
	// plan's piece says, and a qwen item's second strike flips it to claude for the rest
	// (docs/decisions/2026-10-16-qwen-builder.md).
	Builder string `json:"builder,omitempty"`
	// BuildSession is the OpenCode session of the item's last qwen build, which its next build
	// resumes ("" for none).
	BuildSession string `json:"build_session,omitempty"`
	// NotBefore is when a land item put back by ci_pending may be claimed again; CIPendingSince
	// and CIPendingSHA are when CI started pending on that head, across land runs (cipending.go).
	NotBefore      time.Time `json:"not_before,omitzero"`
	CIPendingSince time.Time `json:"ci_pending_since,omitzero"`
	CIPendingSHA   string    `json:"ci_pending_sha,omitempty"`
	CreatedAt      time.Time `json:"created_at"`
	UpdatedAt      time.Time `json:"updated_at"`
}

// ValidPriority reports whether p is P1–P5.
func ValidPriority(p string) bool {
	return len(p) == 2 && p[0] == 'P' && p[1] >= '1' && p[1] <= '5'
}

// PriorityRank orders priorities: P1 is 1, P5 is 5.
func PriorityRank(p string) int {
	if !ValidPriority(p) {
		return 9
	}
	return int(p[1] - '0')
}

// NewItem builds a new item at triage, applying the defaults (P3, source user; scout items P4).
func NewItem(title, ask, repo, priority, source string, now time.Time) (Item, error) {
	title = strings.TrimSpace(title)
	if title == "" {
		return Item{}, errf(CodeMissingField, "title is required")
	}
	if source == "" {
		source = "user"
	}
	if !slices.Contains(Sources, source) {
		return Item{}, errf(CodeInvalidField, "source must be one of %s", strings.Join(Sources, ", "))
	}
	if priority == "" {
		priority = "P3"
		if source == "scout" {
			priority = "P4"
		}
	}
	if !ValidPriority(priority) {
		return Item{}, errf(CodeInvalidField, "priority must be P1 to P5")
	}
	return Item{
		Title: title, Ask: strings.TrimSpace(ask), Repo: strings.TrimSpace(repo),
		Source: source, Priority: priority, Stage: Triage, Risk: RiskNormal,
		Resources: []string{}, DependsOn: []int64{}, CreatedAt: now, UpdatedAt: now,
	}, nil
}

// Transition is the result of a rule: the item as it should be stored, and what to record.
type Transition struct {
	Item    Item
	From    Stage
	To      Stage
	Outcome Outcome
	// Kind is the event kind to record.
	Kind string
	// Data is recorded on the event.
	Data map[string]any
	// UsagePause: the outcome is a usage limit: loopd records it and pauses claude claims until its
	// reset (usage.go); the active-agents number stays the user's.
	UsagePause bool
	// Also are more events to record after the transition's own (builder_fallback).
	Also []Note
}

// Note is an event a transition records besides its own.
type Note struct {
	Kind string
	Data map[string]any
}

// Advance is a lease holder finishing a stage.
type Advance struct {
	Outcome Outcome
	Fields  map[string]string
	// Evidence is how many files were attached under the lease being finished.
	Evidence int
	// Agent is the agent id on the lease.
	Agent string
	// ChangedPaths are the PR's changed files ("M path", "D path", "R old new"): required by
	// pr_opened, and by land's merged and hold.
	ChangedPaths []string
	// HoldRules are the rules hold risk is computed with (from control).
	HoldRules []HoldRule
	// Repos are the repo slugs (owner/name) loopd is configured with: the only values triage's
	// reproduced may set as the repo of an item that has none.
	Repos []string
}

var shaRe = regexp.MustCompile(`^[0-9a-f]{7,64}$`)

// ApplyAdvance applies an outcome to an item in a work stage.
func ApplyAdvance(it Item, a Advance, now time.Time) (Transition, error) {
	from := it.Stage
	if !from.IsWork() {
		return Transition{}, errf(CodeWrongStage, "%s is not a work stage", from)
	}
	f := fields(a.Fields)
	stages, known := outcomeStages[a.Outcome]
	if !known {
		return Transition{}, errf(CodeUnknownOutcome, "unknown outcome %q", a.Outcome)
	}
	if !slices.Contains(stages, from) {
		return Transition{}, errf(CodeWrongStage, "%s is not an outcome of %s", a.Outcome, from)
	}

	data := map[string]any{}
	for k, v := range a.Fields {
		data[k] = v
	}
	if a.Agent != "" {
		data["agent"] = a.Agent
	}
	tr := Transition{From: from, Outcome: a.Outcome, Kind: "advanced", Data: data}
	// A qwen build's OpenCode session, which the item's next build resumes.
	if from == Build && it.Builder == BuilderQwen && strings.TrimSpace(a.Fields["builder"]) == BuilderQwen {
		if sess := strings.TrimSpace(a.Fields["session_id"]); sess != "" {
			it.BuildSession = sess
		}
	}
	builder := it.Builder
	needPaths := func() ([]ChangedPath, error) {
		if len(a.ChangedPaths) == 0 {
			return nil, errf(CodeMissingField, "changed_paths is required for %s", a.Outcome)
		}
		paths, err := ParseChangedPaths(a.ChangedPaths)
		if err != nil {
			return nil, err
		}
		data["changed_paths"] = a.ChangedPaths
		return paths, nil
	}
	// The rules that apply are the item's repo's; pr_opened reports the repo (the item may have
	// none yet).
	ruleRepo := it.Repo
	if a.Outcome == PROpened {
		ruleRepo = strings.TrimSpace(a.Fields["repo"])
	}
	// holdReasons computes the rule hits and records them on the event.
	holdReasons := func(paths []ChangedPath, extra ...string) []string {
		matches := MatchHoldRules(a.HoldRules, ruleRepo, paths)
		reasons := []string{}
		for _, m := range matches {
			reasons = append(reasons, m.String())
		}
		if len(matches) > 0 {
			data["hold_matches"] = matches
		}
		return append(reasons, extra...)
	}
	needEvidence := func() error {
		if a.Evidence < 1 {
			return errf(CodeEvidenceRequired, "%s needs at least one evidence file attached under this lease", a.Outcome)
		}
		return nil
	}

	trustNotes, err := applyTrustFields(&it, from, a.Outcome, f, data)
	if err != nil {
		return Transition{}, err
	}
	tr.Also = append(tr.Also, trustNotes...)

	if a.Outcome != CIPending {
		it.NotBefore, it.CIPendingSince, it.CIPendingSHA = time.Time{}, time.Time{}, ""
	}

	switch a.Outcome {
	case Reproduced:
		area, err := f.need("area")
		if err != nil {
			return Transition{}, err
		}
		size, err := f.oneOf("size", Sizes)
		if err != nil {
			return Transition{}, err
		}
		risk, err := f.oneOf("risk", []string{RiskNormal, RiskHold})
		if err != nil {
			return Transition{}, err
		}
		res, err := f.resources("resources")
		if err != nil {
			return Transition{}, err
		}
		repo := strings.TrimSpace(a.Fields["repo"])
		if repo != "" {
			if err := checkTriageRepo(it, repo, a.Repos); err != nil {
				return Transition{}, err
			}
		}
		if err := needEvidence(); err != nil {
			return Transition{}, err
		}
		if repo != "" {
			it.Repo = repo
			data["repo"] = repo
		}
		it.Area, it.Size, it.Resources = area, size, res
		qa, err := f.needsQA()
		if err != nil {
			return Transition{}, err
		}
		if risk == RiskHold {
			raiseRisk(&it, []string{"triage: risk hold"})
		}
		it.NeedsQA = it.NeedsQA || qa
		it.Stage = Build
	case Routed:
		repo, err := f.need("repo")
		if err != nil {
			return Transition{}, err
		}
		why, err := f.need("why")
		if err != nil {
			return Transition{}, err
		}
		if err := checkTriageRepo(it, repo, a.Repos); err != nil {
			return Transition{}, err
		}
		it.Repo = repo
		data["repo"], data["why"] = repo, why
		it.Stage = Triage
	case NotReproducible:
		summary, err := f.need("summary")
		if err != nil {
			return Transition{}, err
		}
		if err := needEvidence(); err != nil {
			return Transition{}, err
		}
		it.Summary = summary
		it.Stage = Closed
	case Question:
		q, err := f.need("question")
		if err != nil {
			return Transition{}, err
		}
		it = toNeedsYou(it, from, q)
	case PROpened:
		repo, err := f.need("repo")
		if err != nil {
			return Transition{}, err
		}
		prs, err := f.need("pr")
		if err != nil {
			return Transition{}, err
		}
		pr, perr := strconv.Atoi(prs)
		if perr != nil || pr <= 0 {
			return Transition{}, errf(CodeInvalidField, "pr must be a positive number")
		}
		branch, err := f.need("branch")
		if err != nil {
			return Transition{}, err
		}
		sha, err := f.sha("head_sha")
		if err != nil {
			return Transition{}, err
		}
		paths, err := needPaths()
		if err != nil {
			return Transition{}, err
		}
		var extra []string
		nr, err := needsReview(f)
		if err != nil {
			return Transition{}, err
		}
		if nr {
			extra = append(extra, "marked needs review")
		}
		if err := needEvidence(); err != nil {
			return Transition{}, err
		}
		// The build's proof; a rebuild that states none keeps the one it had.
		if raw := strings.TrimSpace(a.Fields["proof"]); raw != "" {
			p, err := ParseProof(raw, ProofByBuild)
			if err != nil {
				return Transition{}, err
			}
			it.Proof = p
		}
		raiseRisk(&it, holdReasons(paths, extra...))
		voidApproval(&it, data, sha)
		it.Repo, it.PR, it.Branch, it.HeadSHA = repo, pr, branch, sha
		it.BuilderAgent = a.Agent
		if it.CIRedSHA != "" && it.CIRedSHA == sha {
			// Another review and land would see the same red CI: the user decides. An answer
			// returns to land, which reads CI again (a rerun may have passed).
			q := fmt.Sprintf("CI failed on %s and the build changed nothing: rerun CI, fix main, or drop", sha[:7])
			if it.CIMainFixed != "" {
				q = fmt.Sprintf("CI failed on %s and the build changed nothing, but main was red at this PR's base and is green now (`%s`): "+
					"rebuild (rebase on main), not a rerun, which tests the same merge again. Reply \"rebuild\" or use the Rebuild button, or drop.",
					sha[:7], it.CIMainFixed[:7])
			}
			it = toNeedsYou(it, Land, q)
			break
		}
		it.CIRedSHA, it.CIMainFixed = "", ""
		it.Stage = Review
	case Approved:
		summary, err := f.need("summary")
		if err != nil {
			return Transition{}, err
		}
		if err := reviewApproves(&it, a, f); err != nil {
			return Transition{}, err
		}
		nr, _ := needsReview(f) // checked by reviewApproves
		if n := carryApproval(&it, f, nr, func(p []ChangedPath) []HoldMatch { return MatchHoldRules(a.HoldRules, ruleRepo, p) }); n != nil {
			tr.Also = append(tr.Also, *n)
		}
		qa, err := f.needsQA()
		if err != nil {
			return Transition{}, err
		}
		it.NeedsQA = it.NeedsQA || qa
		it.Summary = summary
		it.Stage = Land
	case Bounce:
		notes, err := f.need("notes")
		if err != nil {
			return Transition{}, err
		}
		it = bounce(it, data, notes)
	case VerifyFailed:
		// The runner's verify failed on the build's commit: nothing was pushed; the next build run
		// gets the log. It counts like a review bounce.
		reason, err := f.need("reason")
		if err != nil {
			return Transition{}, err
		}
		if err := needEvidence(); err != nil {
			return Transition{}, err
		}
		it = bounce(it, data, reason)
	case Merged:
		sha, err := f.sha("merge_sha")
		if err != nil {
			return Transition{}, err
		}
		head, err := f.sha("head_sha")
		if err != nil {
			return Transition{}, err
		}
		paths, err := needPaths()
		if err != nil {
			return Transition{}, err
		}
		// The land script sends needs_qa for a repo whose config has qa: true, and
		// qa_after_deploy (which implies it) for the loop's own repo.
		qa, err := f.needsQA()
		if err != nil {
			return Transition{}, err
		}
		afterDeploy, err := f.boolean("qa_after_deploy")
		if err != nil {
			return Transition{}, err
		}
		if err := checkMerge(&it, head, holdReasons(paths)); err != nil {
			return Transition{}, err
		}
		it.HeadSHA = head
		it.MergeSHA = sha
		it.NeedsQA = it.NeedsQA || qa || afterDeploy
		it.Stage = Done
		if it.NeedsQA {
			it.Stage = QA
		}
		if afterDeploy {
			it.AwaitingDeploy = true
			data["awaiting_deploy"] = true
		}
	case Hold:
		reason, err := f.need("reason")
		if err != nil {
			return Transition{}, err
		}
		head, err := f.sha("head_sha")
		if err != nil {
			return Transition{}, err
		}
		paths, err := needPaths()
		if err != nil {
			return Transition{}, err
		}
		// The held message must show the commit the user would approve: an approval of another
		// one is dropped, not kept for review to carry (the user approves this head).
		voidApproval(&it, data, head)
		if it.Approval != nil && it.Approval.HeadSHA != head {
			it.Approval = nil
		}
		it.HeadSHA = head
		raiseRisk(&it, holdReasons(paths, "land: "+reason))
		it.Stage = Held
	case BackToBuild:
		reason, err := f.need("reason")
		if err != nil {
			return Transition{}, err
		}
		// The land script sends the paths it computed; risk is recomputed on them.
		if len(a.ChangedPaths) > 0 {
			paths, err := needPaths()
			if err != nil {
				return Transition{}, err
			}
			raiseRisk(&it, holdReasons(paths))
		}
		// Red CI names the head it failed on and counts as a bounce; a conflict or missing
		// evidence does not.
		if _, ok := a.Fields["ci_head_sha"]; ok {
			head, err := f.sha("ci_head_sha")
			if err != nil {
				return Transition{}, err
			}
			it.CIRedSHA, it.CIMainFixed = head, ""
			if _, ok := a.Fields["main_fixed_sha"]; ok {
				if it.CIMainFixed, err = f.sha("main_fixed_sha"); err != nil {
					return Transition{}, err
				}
			}
			it = bounce(it, data, reason)
			break
		}
		it.Stage = Build
	case BackToReview:
		// The PR's head moved since review (or land merged the base in): the new head gets a
		// fresh review by an agent other than the builder.
		if _, err := f.need("notes"); err != nil {
			return Transition{}, err
		}
		head, err := f.sha("head_sha")
		if err != nil {
			return Transition{}, err
		}
		paths, err := needPaths()
		if err != nil {
			return Transition{}, err
		}
		voidApproval(&it, data, head)
		it.HeadSHA = head
		raiseRisk(&it, holdReasons(paths))
		it.Stage = Review
	case UsageLimit:
		if _, err := f.need("error"); err != nil {
			return Transition{}, err
		}
		tr.Kind = "usage_limit"
		tr.UsagePause = true
	case Failed:
		msg, err := f.need("error")
		if err != nil {
			return Transition{}, err
		}
		it = toNeedsYou(it, from, msg)
	case CIPending:
		if err := applyCIPending(&it, f, data, now); err != nil {
			return Transition{}, err
		}
	}
	if builder == BuilderQwen && it.Builder == BuilderClaude {
		tr.Also = append(tr.Also, Note{Kind: "builder_fallback", Data: map[string]any{
			"from": BuilderQwen, "to": BuilderClaude, "bounce_count": it.BounceCount, "outcome": string(a.Outcome)}})
	}
	it.UpdatedAt = now
	tr.Item = it
	tr.To = it.Stage
	return tr, nil
}

// checkTriageRepo checks a repo triage gives an item: only one that has none, and only a slug
// loopd is configured with.
func checkTriageRepo(it Item, repo string, repos []string) error {
	switch {
	case it.Repo != "":
		return errf(CodeInvalidField, "the item already has repo %q; triage sets one only when it has none", it.Repo)
	case len(repos) == 0:
		return errf(CodeInvalidField, "loopd has no configured repos (--repos), so triage cannot set one")
	case !slices.Contains(repos, repo):
		return errf(CodeInvalidField, "repo must be a configured repo (%s), not %q", strings.Join(repos, ", "), repo)
	}
	return nil
}

// QwenStrikes is the bounce at which a qwen item's builder becomes claude for the rest: the first
// strike's build resumes the qwen session, the second goes to claude.
const QwenStrikes = 2

// bounce sends an item back to build, or to the user at the second bounce (a review's or the
// runner's verify, counted together). A qwen item's QwenStrikes-th bounce flips it to claude.
func bounce(it Item, data map[string]any, why string) Item {
	it.BounceCount++
	data["bounce_count"] = it.BounceCount
	if it.Builder == BuilderQwen && it.BounceCount >= QwenStrikes {
		it.Builder, it.BuildSession = BuilderClaude, ""
		data["builder_fallback"] = true
	}
	if it.BounceCount >= 2 {
		return toNeedsYou(it, Build, why)
	}
	it.Stage = Build
	return it
}

func toNeedsYou(it Item, ret Stage, question string) Item {
	it.Stage = NeedsYou
	it.ReturnStage = ret
	it.Question = question
	return it
}

// ApplyAnswer records the user's answer: the item returns to the stage that asked, waiting.
func ApplyAnswer(it Item, text string) (Transition, error) {
	if it.Stage != NeedsYou {
		return Transition{}, errf(CodeWrongStage, "item is %s, not needs_you", it.Stage)
	}
	text = strings.TrimSpace(text)
	if text == "" {
		return Transition{}, errf(CodeMissingField, "text is required")
	}
	ret := it.ReturnStage
	if !ret.IsWork() {
		ret = Triage
	}
	data := map[string]any{"question": it.Question, "answer": text}
	it.Stage, it.ReturnStage, it.Question = ret, "", ""
	return Transition{Item: it, From: NeedsYou, To: ret, Kind: "answered", Data: data}, nil
}

// ApplyRebuild sends a held or needs_you item back to build with notes for the builder: the
// user's reply in the held thread that is not an approval, a rebuild reply or the Rebuild button
// on a held or needs_you item, or the runner finding the held PR conflicting. The PR, branch and
// hold stay; a question is dropped (the notes replace its answer). It is not a bounce. The user's
// rebuild drops any approval (they asked for a change); the runner's conflict rebuild (conflict)
// keeps one for review to carry if the build only merges the base (carryApproval).
func ApplyRebuild(it Item, notes string, conflict bool) (Transition, error) {
	if it.Stage != Held && it.Stage != NeedsYou {
		return Transition{}, errf(CodeWrongStage, "item is %s, not held or needs_you", it.Stage)
	}
	if it.Stage == NeedsYou && !Rebuildable(it) {
		return Transition{}, errf(CodeWrongStage, "item waits on triage's question (no build yet): answer it")
	}
	notes = strings.TrimSpace(notes)
	if notes == "" {
		return Transition{}, errf(CodeMissingField, "notes are required: they tell the builder what to do")
	}
	data := map[string]any{"notes": notes}
	if it.Approval != nil && !conflict {
		it.Approval = nil
		data["approval_voided"] = true
	}
	if it.Stage == NeedsYou {
		data["question"] = it.Question
	}
	from := it.Stage
	it.Stage, it.ReturnStage, it.Question = Build, "", ""
	return Transition{Item: it, From: from, To: Build, Kind: "rebuild", Data: data}, nil
}

// Rebuildable reports whether a needs_you item may go back to build with the user's notes: its
// question came after triage (a build, review or land question), so the item has its repo.
func Rebuildable(it Item) bool {
	return it.ReturnStage == Build || it.ReturnStage == Review || it.ReturnStage == Land
}

// DropReason is the reason a dropped item's event records.
const DropReason = "dropped by the user"

// ApplyDrop closes an open item at the user's word, from any stage but done and closed.
func ApplyDrop(it Item, words string) (Transition, error) {
	if it.Stage == Done || it.Stage == Closed {
		return Transition{}, errf(CodeWrongStage, "item is %s", it.Stage)
	}
	from := it.Stage
	it.Stage, it.ReturnStage, it.Question = Closed, "", ""
	return Transition{Item: it, From: from, To: Closed, Kind: "dropped",
		Data: map[string]any{"reason": DropReason, "words": strings.TrimSpace(words)}}, nil
}

// ApplySetQA marks an item needs_qa at the user's word, before it lands: once merged it waits in
// qa for the user's check.
func ApplySetQA(it Item) (Transition, error) {
	switch it.Stage {
	case QA, Done, Closed:
		return Transition{}, errf(CodeWrongStage, "item is %s; mark it before it lands", it.Stage)
	}
	it.NeedsQA = true
	return Transition{Item: it, From: it.Stage, To: it.Stage, Kind: "qa_marked", Data: map[string]any{"needs_qa": true}}, nil
}

// ApplyVerified is the user's check of a merged item passing: qa → done.
func ApplyVerified(it Item, words string) (Transition, error) {
	if it.Stage != QA {
		return Transition{}, errf(CodeWrongStage, "item is %s, not qa", it.Stage)
	}
	it.Stage, it.AwaitingDeploy = Done, false
	return Transition{Item: it, From: QA, To: Done, Kind: "verified", Data: map[string]any{"words": strings.TrimSpace(words)}}, nil
}

// ApplyReopen is the user's check of a merged item failing: qa → done, and the caller files a new
// item (NewReopen) with the user's words as its ask. The merged work is not undone.
func ApplyReopen(it Item, words string) (Transition, error) {
	if it.Stage != QA {
		return Transition{}, errf(CodeWrongStage, "item is %s, not qa", it.Stage)
	}
	words = strings.TrimSpace(words)
	if words == "" {
		return Transition{}, errf(CodeMissingField, "say what is wrong: the words are the new item's ask")
	}
	it.Stage, it.AwaitingDeploy = Done, false
	return Transition{Item: it, From: QA, To: Done, Kind: "reopened", Data: map[string]any{"words": words}}, nil
}

// ApplyDeployed is a runner reporting that a loop build containing the item's merge commit is
// running (revision is that build's commit): the wait ends and the user is asked to check.
func ApplyDeployed(it Item, revision string) (Transition, error) {
	if !shaRe.MatchString(revision) {
		return Transition{}, errf(CodeInvalidField, "revision must be a lowercase hex commit sha")
	}
	if it.Stage != QA || !it.AwaitingDeploy {
		return Transition{}, errf(CodeWrongStage, "item is %s and not waiting for a deploy", it.Stage)
	}
	it.AwaitingDeploy = false
	return Transition{Item: it, From: QA, To: QA, Kind: "deployed",
		Data: map[string]any{"revision": revision, "merge_sha": it.MergeSHA}}, nil
}

// ReopenPriority is a reopened item's priority: something merged is broken.
const ReopenPriority = "P2"

// NewReopen is the item a Reopen files: P2, the same repo, the user's words as the ask, linked to
// the qa item.
func NewReopen(it Item, words string, now time.Time) (Item, error) {
	words = strings.TrimSpace(words)
	title := fmt.Sprintf("Reopen #%d: %s", it.ID, strings.Join(strings.Fields(words), " "))
	if r := []rune(title); len(r) > 100 {
		title = string(r[:99]) + "…"
	}
	n, err := NewItem(title, words, it.Repo, ReopenPriority, "user", now)
	if err != nil {
		return n, err
	}
	n.ReopenOf = it.ID
	return n, nil
}

// ApplyPatch changes an item's priority or title.
func ApplyPatch(it Item, priority, title *string) (Transition, error) {
	if it.Stage == Done || it.Stage == Closed {
		return Transition{}, errf(CodeWrongStage, "item is %s", it.Stage)
	}
	if priority == nil && title == nil {
		return Transition{}, errf(CodeMissingField, "nothing to change: give priority or title")
	}
	data := map[string]any{}
	if priority != nil {
		if !ValidPriority(*priority) {
			return Transition{}, errf(CodeInvalidField, "priority must be P1 to P5")
		}
		data["priority"] = map[string]string{"from": it.Priority, "to": *priority}
		it.Priority = *priority
	}
	if title != nil {
		t := strings.TrimSpace(*title)
		if t == "" {
			return Transition{}, errf(CodeInvalidField, "title cannot be empty")
		}
		data["title"] = map[string]string{"from": it.Title, "to": t}
		it.Title = t
	}
	return Transition{Item: it, From: it.Stage, To: it.Stage, Kind: "patched", Data: data}, nil
}

// Overtime sends an item whose run passed the max run time to needs_you.
func Overtime(it Item, note string, maxRun time.Duration) Transition {
	from := it.Stage
	q := fmt.Sprintf("run exceeded %d minutes", int(maxRun.Minutes()))
	if note != "" {
		q += "; last note: " + note
	}
	it = toNeedsYou(it, from, q)
	return Transition{Item: it, From: from, To: NeedsYou, Kind: "run_overtime", Data: map[string]any{"note": note}}
}

type fields map[string]string

func (f fields) need(k string) (string, error) {
	v := strings.TrimSpace(f[k])
	if v == "" {
		return "", errf(CodeMissingField, "%s is required", k)
	}
	return v, nil
}

// needsQA reads an optional needs_qa: "" and "false" are false.
func (f fields) needsQA() (bool, error) { return f.boolean("needs_qa") }

// boolean reads an optional true/false field: "" and "false" are false.
func (f fields) boolean(k string) (bool, error) {
	switch strings.TrimSpace(f[k]) {
	case "", "false":
		return false, nil
	case "true":
		return true, nil
	}
	return false, errf(CodeInvalidField, "%s must be true or false", k)
}

func (f fields) oneOf(k string, allowed []string) (string, error) {
	v, err := f.need(k)
	if err != nil {
		return "", err
	}
	if !slices.Contains(allowed, v) {
		return "", errf(CodeInvalidField, "%s must be one of %s", k, strings.Join(allowed, ", "))
	}
	return v, nil
}

func (f fields) sha(k string) (string, error) {
	v, err := f.need(k)
	if err != nil {
		return "", err
	}
	if !shaRe.MatchString(v) {
		return "", errf(CodeInvalidField, "%s must be a lowercase hex commit sha", k)
	}
	return v, nil
}

// resources requires the key to be present; an empty value (or "none") means no resources.
func (f fields) resources(k string) ([]string, error) {
	raw, ok := f[k]
	if !ok {
		return nil, errf(CodeMissingField, "%s is required (empty for none)", k)
	}
	if strings.TrimSpace(raw) == "none" {
		return []string{}, nil
	}
	return ParseResources(raw)
}

// ParseResources parses a comma-separated resource list into a sorted, deduplicated slice.
func ParseResources(raw string) ([]string, error) {
	out := []string{}
	for _, r := range strings.Split(raw, ",") {
		r = strings.TrimSpace(r)
		if r == "" {
			continue
		}
		if !slices.Contains(Resources, r) {
			return nil, errf(CodeInvalidField, "unknown resource %q (have %s)", r, strings.Join(Resources, ", "))
		}
		if !slices.Contains(out, r) {
			out = append(out, r)
		}
	}
	slices.Sort(out)
	return out, nil
}
