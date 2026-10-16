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

// Approval is the user's approval of a held merge, bound to the commit they saw.
type Approval struct {
	By      string    `json:"by"`
	Words   string    `json:"words"`
	HeadSHA string    `json:"head_sha"`
	At      time.Time `json:"at"`
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
	// NeedsQA: once merged, the item waits in qa for the user's check instead of going to done.
	NeedsQA bool `json:"needs_qa"`
	// ReopenOf is the qa item whose Reopen filed this one.
	ReopenOf int64 `json:"reopen_of,omitempty"`
	// CIRedSHA is the head land sent back to build for red CI; a pr_opened with the same head
	// goes to the user, not review (the build changed nothing, so CI would stay red).
	CIRedSHA string `json:"ci_red_sha,omitempty"`
	// DependsOn are the items this one waits for: claim skips it until they are all done.
	DependsOn []int64 `json:"depends_on"`
	// PlanID and PlanPiece link an item filed from a plan to its piece (docs/phases/07-planning.md).
	PlanID    int64 `json:"plan_id,omitempty"`
	PlanPiece int   `json:"plan_piece,omitempty"`
	// Builder is who the plan's piece says should build it (BuilderQwen, BuilderClaude; "": not
	// from a plan). Nothing reads it yet beyond /loop show and the board.
	Builder   string    `json:"builder,omitempty"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
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
	// ActiveAgentsZero: the outcome turns the active-agents number to 0 (usage_limit).
	ActiveAgentsZero bool
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
	allowed := map[Outcome][]Stage{
		Reproduced: {Triage}, Routed: {Triage}, NotReproducible: {Triage}, Question: {Triage, Build},
		PROpened: {Build}, VerifyFailed: {Build}, Approved: {Review}, Bounce: {Review},
		Merged: {Land}, Hold: {Land}, BackToBuild: {Land}, BackToReview: {Land},
		UsageLimit: {Triage, Build, Review, Land}, Failed: {Triage, Build, Review, Land},
	}
	stages, known := allowed[a.Outcome]
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
		switch strings.TrimSpace(a.Fields["needs_review"]) {
		case "", "false":
		case "true":
			extra = append(extra, "marked needs review")
		default:
			return Transition{}, errf(CodeInvalidField, "needs_review must be true or false")
		}
		if err := needEvidence(); err != nil {
			return Transition{}, err
		}
		raiseRisk(&it, holdReasons(paths, extra...))
		if it.Approval != nil && it.Approval.HeadSHA != sha {
			it.Approval = nil // an approval is for the commit the user saw
			data["approval_voided"] = true
		}
		it.Repo, it.PR, it.Branch, it.HeadSHA = repo, pr, branch, sha
		it.BuilderAgent = a.Agent
		if it.CIRedSHA != "" && it.CIRedSHA == sha {
			// Another review and land would see the same red CI: the user decides. An answer
			// returns to land, which reads CI again (a rerun may have passed).
			it = toNeedsYou(it, Land, fmt.Sprintf("CI failed on %s and the build changed nothing: rerun CI, fix main, or drop", sha[:7]))
			break
		}
		it.CIRedSHA = ""
		it.Stage = Review
	case Approved:
		summary, err := f.need("summary")
		if err != nil {
			return Transition{}, err
		}
		sha, err := f.sha("head_sha")
		if err != nil {
			return Transition{}, err
		}
		if sha != it.HeadSHA {
			return Transition{}, errf(CodeSHAMismatch, "reviewed %s but the PR head is %s", sha, it.HeadSHA)
		}
		if a.Agent != "" && a.Agent == it.BuilderAgent {
			return Transition{}, errf(CodeSameAgent, "the builder cannot approve its own work")
		}
		// The reviewer holds what a path rule cannot see: a curl | sh pin edited in a docs page, a
		// new secret used in a package.
		switch strings.TrimSpace(a.Fields["needs_review"]) {
		case "", "false":
		case "true":
			raiseRisk(&it, []string{"review: marked needs review"})
		default:
			return Transition{}, errf(CodeInvalidField, "needs_review must be true or false")
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
		// The land script sends needs_qa for a repo whose config has qa: true.
		qa, err := f.needsQA()
		if err != nil {
			return Transition{}, err
		}
		// Risk is recomputed on what was actually merged; the approval must be for that head.
		raiseRisk(&it, holdReasons(paths))
		if it.Risk == RiskHold && (it.Approval == nil || it.Approval.HeadSHA != head) {
			return Transition{}, errf(CodeApprovalRequired, "on the hold list (%s): merged head %s needs the user's approval for that commit",
				it.HoldReason, head)
		}
		it.HeadSHA = head
		it.MergeSHA = sha
		it.NeedsQA = it.NeedsQA || qa
		it.Stage = Done
		if it.NeedsQA {
			it.Stage = QA
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
		// The held message must show the commit the user would approve.
		if it.Approval != nil && it.Approval.HeadSHA != head {
			it.Approval = nil
			data["approval_voided"] = true
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
			it.CIRedSHA = head
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
		if it.Approval != nil && it.Approval.HeadSHA != head {
			it.Approval = nil // an approval is for the commit the user saw
			data["approval_voided"] = true
		}
		it.HeadSHA = head
		raiseRisk(&it, holdReasons(paths))
		it.Stage = Review
	case UsageLimit:
		if _, err := f.need("error"); err != nil {
			return Transition{}, err
		}
		tr.Kind = "usage_limit"
		tr.ActiveAgentsZero = true
	case Failed:
		msg, err := f.need("error")
		if err != nil {
			return Transition{}, err
		}
		it = toNeedsYou(it, from, msg)
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

// bounce sends an item back to build, or to the user at the second bounce (a review's or the
// runner's verify, counted together).
func bounce(it Item, data map[string]any, why string) Item {
	it.BounceCount++
	data["bounce_count"] = it.BounceCount
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

// ApplyApprove records the user's approval of a held merge for the head they saw.
func ApplyApprove(it Item, by, headSHA, words string, now time.Time) (Transition, error) {
	if it.Stage != Held {
		return Transition{}, errf(CodeWrongStage, "item is %s, not held", it.Stage)
	}
	headSHA = strings.TrimSpace(headSHA)
	if headSHA == "" {
		return Transition{}, errf(CodeMissingField, "head_sha is required")
	}
	if headSHA != it.HeadSHA {
		return Transition{}, errf(CodeSHAMismatch, "approval is for %s but the PR head is now %s", headSHA, it.HeadSHA)
	}
	it.Approval = &Approval{By: by, Words: words, HeadSHA: headSHA, At: now}
	it.Stage = Land
	it.UpdatedAt = now
	return Transition{Item: it, From: Held, To: Land, Kind: "approved",
		Data: map[string]any{"by": by, "words": words, "head_sha": headSHA}}, nil
}

// ApplyHold records the user keeping a held merge held.
func ApplyHold(it Item, words string) (Transition, error) {
	if it.Stage != Held {
		return Transition{}, errf(CodeWrongStage, "item is %s, not held", it.Stage)
	}
	return Transition{Item: it, From: Held, To: Held, Kind: "held", Data: map[string]any{"words": words}}, nil
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
	it.Stage = Done
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
	it.Stage = Done
	return Transition{Item: it, From: QA, To: Done, Kind: "reopened", Data: map[string]any{"words": words}}, nil
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
func (f fields) needsQA() (bool, error) {
	switch strings.TrimSpace(f["needs_qa"]) {
	case "", "false":
		return false, nil
	case "true":
		return true, nil
	}
	return false, errf(CodeInvalidField, "needs_qa must be true or false")
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
