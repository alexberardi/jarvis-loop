package pipeline

import (
	"regexp"
	"slices"
	"strings"
	"time"
)

// The orchestrator's authority (docs/phases/08-orchestrator.md, "What it may do"). loopd enforces
// these limits; the orchestrator's own skill only knows them.

// Error codes of the orchestrator's refused actions.
const (
	// CodeLimit is an action past one of its limits (answers per head, adds per day, the third
	// failure on a head, agents up).
	CodeLimit = "limit"
	// CodeTooSoon is an action inside its waiting time (a retry's backoff, the escalation rate);
	// the message says when it may be tried again.
	CodeTooSoon = "too_soon"
)

// The kinds of answer the orchestrator may give ("Answers it may give"). A runner or environment
// error is not one of them: that is the retry action, with its backoff.
const (
	AnswerHistory = "history" // the item's history already answers the question
	AnswerHarness = "harness" // the standing answer: the harness can't run in the agent's sandbox
	AnswerBounce  = "bounce"  // review bounce limit: go on with the reviewer's notes (once per item)
	AnswerRedCI   = "red_ci"  // same-head red CI: re-run once, then back to build with the log
	AnswerLoop    = "loop"    // how the loop works
)

// AnswerKinds are the kinds of answer the orchestrator may give.
var AnswerKinds = []string{AnswerHistory, AnswerHarness, AnswerBounce, AnswerRedCI, AnswerLoop}

// Limits of the orchestrator's actions.
const (
	OrchAnswersPerHead = 2
	OrchAddsPerDay     = 5
	OrchFailuresToAsk  = 3 // the failure on one head that is escalated instead of retried
	OrchEscalateEvery  = 10 * time.Minute
)

// OrchRetryBackoff is how long after the n-th failure on a head (index n-1) a retry may run.
// The spec lists 2, 10 and 30 minutes, and escalates the third failure, so the third step is never
// waited for: OrchFailuresToAsk stops first (docs/decisions/2026-10-16-orchestrator-token.md).
var OrchRetryBackoff = []time.Duration{2 * time.Minute, 10 * time.Minute, 30 * time.Minute}

// OrchAnswer is one of the orchestrator's earlier answers on an item.
type OrchAnswer struct {
	Kind    string
	Text    string
	HeadSHA string
}

// CheckOrchestratorAnswer refuses an answer the orchestrator may not give: a kind not in
// AnswerKinds, a third answer on the same head, the same answer as its previous one, or a second
// bounce answer on the item. prior are its earlier answers on the item, oldest first.
func CheckOrchestratorAnswer(kind, text, head string, prior []OrchAnswer) error {
	if !slices.Contains(AnswerKinds, kind) {
		return errf(CodeInvalidField, "kind must be one of %s (a runner or environment error is a retry, not an answer)",
			strings.Join(AnswerKinds, ", "))
	}
	if strings.TrimSpace(text) == "" {
		return errf(CodeMissingField, "text is required")
	}
	onHead := 0
	for _, a := range prior {
		if a.HeadSHA == head {
			onHead++
		}
		if kind == AnswerBounce && a.Kind == AnswerBounce {
			return errf(CodeLimit, "the bounce answer is given once per item; this one is the user's")
		}
	}
	if onHead >= OrchAnswersPerHead {
		return errf(CodeLimit, "already %d answers on this head; the next one is the user's", onHead)
	}
	if n := len(prior); n > 0 && sameText(prior[n-1].Text, text) {
		return errf(CodeLimit, "the same answer as the previous one; ask the user instead")
	}
	return nil
}

func sameText(a, b string) bool {
	return strings.EqualFold(strings.Join(strings.Fields(a), " "), strings.Join(strings.Fields(b), " "))
}

// RetryWait is when a retry after the failures-th failure on the item's head may run (failed at
// failedAt), or ask=true when that failure is the one to escalate instead.
func RetryWait(failures int, failedAt time.Time) (notBefore time.Time, ask bool) {
	if failures >= OrchFailuresToAsk {
		return time.Time{}, true
	}
	if failures < 1 {
		failures = 1
	}
	return failedAt.Add(OrchRetryBackoff[failures-1]), false
}

// IsRunFailure reports whether an event put the item in needs_you because its run failed on the
// runner or the environment: a failed outcome or a run past the time limit.
func IsRunFailure(kind string, outcome Outcome) bool {
	return (kind == "advanced" && outcome == Failed) || kind == "run_overtime"
}

// CheckAgentsDown refuses an active-agents number that is not lower than the current one: the
// orchestrator only lowers it, so it never goes above the user's number.
func CheckAgentsDown(current, want int) error {
	if want < 0 {
		return errf(CodeInvalidField, "active_agents cannot be negative")
	}
	if want >= current {
		return errf(CodeLimit, "active agents only go down (now %d); raising them is the user's", current)
	}
	return nil
}

var (
	sigURL   = regexp.MustCompile(`[a-z]+://\S+`)
	sigPath  = regexp.MustCompile(`(?:[\w.\-~]*/)+[\w.\-]*`)
	sigSHA   = regexp.MustCompile(`\b[0-9a-f]{7,64}\b`)
	sigNum   = regexp.MustCompile(`\d+`)
	sigSpace = regexp.MustCompile(`\s+`)
)

// NormaliseSignature is a failure signature with what differs between occurrences removed
// (urls, paths, shas, numbers), lower case and with single spaces, so two items failing the same
// way have the same signature.
func NormaliseSignature(s string) string {
	s = strings.ToLower(s)
	s = sigURL.ReplaceAllString(s, " ")
	s = sigPath.ReplaceAllString(s, " ")
	s = sigSHA.ReplaceAllString(s, " ")
	s = sigNum.ReplaceAllString(s, " ")
	return strings.TrimSpace(sigSpace.ReplaceAllString(s, " "))
}

// NeedReason refuses an orchestrator action without a reason: every one is recorded with it.
func NeedReason(reason string) (string, error) {
	r := strings.TrimSpace(reason)
	if r == "" {
		return "", errf(CodeMissingField, "reason is required: every orchestrator action records why")
	}
	return r, nil
}

// ApplyRetry sends an item stuck on a run failure back to the stage that failed, for another run:
// the failure-th on its head.
func ApplyRetry(it Item, failure int, reason string) (Transition, error) {
	if it.Stage != NeedsYou {
		return Transition{}, errf(CodeWrongStage, "item is %s, not needs_you", it.Stage)
	}
	ret := it.ReturnStage
	if !ret.IsWork() {
		ret = Triage
	}
	data := map[string]any{"question": it.Question, "reason": reason, "failure": failure, "head_sha": it.HeadSHA}
	it.Stage, it.ReturnStage, it.Question = ret, "", ""
	return Transition{Item: it, From: NeedsYou, To: ret, Kind: "retried", Data: data}, nil
}
