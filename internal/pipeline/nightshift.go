package pipeline

import (
	"fmt"
	"regexp"
	"slices"
	"strings"
	"time"
)

// The night shift (docs/decisions/2026-10-20-night-shift.md): while the user is away, loopd acts on
// the orchestrator's own recommendation for a few low-risk classes of escalation, after a grace
// period in which the user's answer wins. The orchestrator names the class; loopd checks it, the
// limits and the never-list, and takes the class's one action itself.

// Night shift modes (control's night_shift).
const (
	NightOff    = "off"
	NightAlways = "on"
	// NightAuto is on from NightStartHour to NightEndHour on the loop's clock, or as the user's
	// "night" / "morning" says until the next of those edges.
	NightAuto = "auto"
)

// NightModes are the night_shift values.
var NightModes = []string{NightOff, NightAlways, NightAuto}

// The user's overrides of the auto hours ("@orchestrator night", "@orchestrator morning").
const (
	OverrideNight   = "night"
	OverrideMorning = "morning"
)

// The night shift's classes. The first three are escalations it acts on after NightGrace;
// ClassTicketLimit is the orchestrator's daily ticket cap, raised at night for process tickets.
const (
	// ClassVerifyFlake: a verify regression the change cannot reach, or a verify harness error.
	// Action: rebuild with notes to re-run verify, once per head.
	ClassVerifyFlake = "verify_flake"
	// ClassBounceLimit: the bounce that sent the item to the user, with concrete must-fix notes.
	// Action: one more build with those notes, once per item per night.
	ClassBounceLimit = "bounce_limit"
	// ClassScopeChoice: a scope question where the recommendation is the narrower option. Action:
	// answer with it, and file the deferred part as a P3 item.
	ClassScopeChoice = "scope_choice"
	// ClassTicketLimit: the ticket cap is OrchAddsPerNight at night, for process tickets.
	ClassTicketLimit = "ticket_limit"
)

// NightClasses are the classes the user can turn off one by one.
var NightClasses = []string{ClassVerifyFlake, ClassBounceLimit, ClassScopeChoice, ClassTicketLimit}

// EscalationClasses are the classes an escalation may carry: the ones acted on after the grace.
var EscalationClasses = []string{ClassVerifyFlake, ClassBounceLimit, ClassScopeChoice}

const (
	// NightGrace is how long a night escalation waits for the user before loopd acts on it.
	NightGrace = 15 * time.Minute
	// NightStartHour and NightEndHour are the auto hours, on the loop's clock (New York).
	NightStartHour = 23
	NightEndHour   = 8
	// OrchAddsPerNight is the ticket cap at night, for process tickets (ClassTicketLimit).
	OrchAddsPerNight = 10
	// DeferredPriority is the priority of the item a scope choice files for the deferred part.
	DeferredPriority = "P3"
)

// InNightHours reports whether now is within the auto hours, on loc's clock.
func InNightHours(now time.Time, loc *time.Location) bool {
	h := now.In(loc).Hour()
	return h >= NightStartHour || h < NightEndHour
}

func atHour(t time.Time, dayOffset, hour int, loc *time.Location) time.Time {
	return time.Date(t.Year(), t.Month(), t.Day()+dayOffset, hour, 0, 0, 0, loc)
}

// NightStart is the NightStartHour that began the night now is in, or the last night's in the day.
func NightStart(now time.Time, loc *time.Location) time.Time {
	t := now.In(loc)
	if s := atHour(t, 0, NightStartHour, loc); !t.Before(s) {
		return s
	}
	return atHour(t, -1, NightStartHour, loc)
}

// lastNightEdge is the latest start or end of the auto hours at or before now.
func lastNightEdge(now time.Time, loc *time.Location) time.Time {
	t := now.In(loc)
	if e := atHour(t, 0, NightEndHour, loc); !t.Before(e) && t.Before(atHour(t, 0, NightStartHour, loc)) {
		return e
	}
	return NightStart(now, loc)
}

// NextNightEdge is the next start or end of the auto hours after now: when an override ends.
func NextNightEdge(now time.Time, loc *time.Location) time.Time {
	t := now.In(loc)
	for _, e := range []time.Time{atHour(t, 0, NightEndHour, loc), atHour(t, 0, NightStartHour, loc), atHour(t, 1, NightEndHour, loc)} {
		if e.After(t) {
			return e
		}
	}
	return atHour(t, 1, NightStartHour, loc)
}

// NightSetting is the night shift's control: the mode, the classes the user turned off, and the
// user's last override of the auto hours.
type NightSetting struct {
	Mode       string    `json:"mode"`
	ClassesOff []string  `json:"classes_off"`
	Override   string    `json:"override,omitempty"`
	OverrideAt time.Time `json:"override_at,omitempty"`
}

// On reports whether the night shift is on at now. An override counts in auto mode until the next
// edge of the auto hours after it was given.
func (n NightSetting) On(now time.Time, loc *time.Location) bool {
	switch n.Mode {
	case NightAlways:
		return true
	case NightAuto:
		if n.Override != "" && !n.OverrideAt.IsZero() && !n.OverrideAt.Before(lastNightEdge(now, loc)) {
			return n.Override == OverrideNight
		}
		return InNightHours(now, loc)
	}
	return false
}

// Until is when, in auto mode, the night shift next turns on or off after now (an override that
// reaches the auto hours runs on with them); zero in off and on modes.
func (n NightSetting) Until(now time.Time, loc *time.Location) time.Time {
	if n.Mode != NightAuto {
		return time.Time{}
	}
	on := n.On(now, loc)
	e := NextNightEdge(now, loc)
	for i := 0; i < 3 && n.On(e, loc) == on; i++ {
		e = NextNightEdge(e, loc)
	}
	return e
}

// ClassOn reports whether the user left class on.
func (n NightSetting) ClassOn(class string) bool { return !slices.Contains(n.ClassesOff, class) }

// CheckNightSetting refuses a night_shift mode or a class to turn off that does not exist.
func CheckNightSetting(mode string, classesOff []string) error {
	if mode != "" && !slices.Contains(NightModes, mode) {
		return errf(CodeInvalidField, "night_shift must be one of %s", strings.Join(NightModes, ", "))
	}
	for _, c := range classesOff {
		if !slices.Contains(NightClasses, c) {
			return errf(CodeInvalidField, "no night class %q (have %s)", c, strings.Join(NightClasses, ", "))
		}
	}
	return nil
}

// CheckEscalationClass refuses an escalation's night class that loopd could not act on: an unknown
// one, ticket_limit (a cap, not an escalation), one without a recommendation, a scope choice
// without the deferred part, or a deferred part on another class.
func CheckEscalationClass(class, recommendation, deferred string) error {
	if class == "" {
		if strings.TrimSpace(deferred) != "" {
			return errf(CodeInvalidField, "deferred goes with class %s", ClassScopeChoice)
		}
		return nil
	}
	if !slices.Contains(EscalationClasses, class) {
		return errf(CodeInvalidField, "class must be one of %s (%s is a cap, not an escalation)",
			strings.Join(EscalationClasses, ", "), ClassTicketLimit)
	}
	if strings.TrimSpace(recommendation) == "" {
		return errf(CodeMissingField, "a night class needs your recommendation: it is what the night shift does")
	}
	hasDeferred := strings.TrimSpace(deferred) != ""
	if class == ClassScopeChoice && !hasDeferred {
		return errf(CodeMissingField, "a scope choice needs deferred: the part it leaves for a %s item", DeferredPriority)
	}
	if class != ClassScopeChoice && hasDeferred {
		return errf(CodeInvalidField, "deferred goes with class %s", ClassScopeChoice)
	}
	return nil
}

// OrchAddCap is the orchestrator's ticket cap for the last 24 hours at now: OrchAddsPerNight for a
// process ticket while the night shift is on (and ticket_limit is), else OrchAddsPerDay.
func OrchAddCap(set NightSetting, now time.Time, loc *time.Location, process bool) int {
	if process && set.On(now, loc) && set.ClassOn(ClassTicketLimit) {
		return OrchAddsPerNight
	}
	return OrchAddsPerDay
}

// NightEscalation is an escalation as the night shift sees it.
type NightEscalation struct {
	Class, Recommendation, Deferred string
	// Step is the hand step the user must do before the recommendation can be the answer: the
	// night shift never acts on one.
	Step string
	At   time.Time
	// Approval: about an item held for the user's approval.
	Approval bool
	// Night: made while the night shift was on. Only those are acted on: one from the day was
	// the user's to answer.
	Night bool
}

// NightPrior is an earlier night action on the item.
type NightPrior struct {
	Class, HeadSHA string
	At             time.Time
}

// NightCase is an open escalation and its item, for DecideNight.
type NightCase struct {
	Esc  NightEscalation
	Item Item
	// NeedsYouOutcome is the outcome of the event that put the item in needs_you.
	NeedsYouOutcome Outcome
	// NewQuestion: the item asks something new since the escalation.
	NewQuestion bool
	// UserSince: the user did something about the item since the escalation (spoke to the
	// orchestrator in its thread).
	UserSince bool
	Prior     []NightPrior
}

// NightVerdict is what the night shift does with an escalation now.
type NightVerdict int

const (
	// NightWait: not now (night off, the class off, inside the grace, tonight's allowance used).
	NightWait NightVerdict = iota
	// NightAct: take the class's action.
	NightAct
	// NightStay: never at night; it stays escalated (recorded once).
	NightStay
	// NightGone: the escalation is settled (answered, moved on); nothing to do.
	NightGone
)

func (v NightVerdict) String() string {
	return [...]string{"wait", "act", "stay", "gone"}[v]
}

// neverAtNight is the never-list as words in what the night shift would act on: a recommendation,
// a deferred part, or the review notes a rebuild would carry. Loose on purpose: a false hit only
// leaves the escalation to the user.
var neverAtNight = []struct {
	re   *regexp.Regexp
	what string
}{
	{regexp.MustCompile(`(?i)\bmerg(e|es|ed|ing)\b|\bapprov`), "merges and approvals"},
	{regexp.MustCompile(`(?i)\b(credentials?|tokens?|secrets?|passwords?|api[- ]keys?|ssh[- ]keys?)\b`), "credentials"},
	{regexp.MustCompile(`(?i)\b(auth|authn|authz|oauth|login|permissions?)\b`), "auth and access"},
	{regexp.MustCompile(`(?i)\b(signing|signed|sign|keystore|certificates?)\b`), "signing"},
	{regexp.MustCompile(`(?i)\b(releases?|tags?|tagging)\b`), "releases and tags"},
	{regexp.MustCompile(`(?i)\b(prod|production|deploy\w*)\b`), "prod"},
	{regexp.MustCompile(`(?i)\b(delete|deleting|deletes|discard\w*|force[- ]push\w*|reset --hard)\b`), "deleting work"},
	{regexp.MustCompile(`(?i)\b(limits?|budgets?|caps?|quotas?)\b`), "limits and budgets"},
	{regexp.MustCompile(`(?i)\.github/|\bworkflows?\b|\bci\b`), "CI and workflow files"},
	{regexp.MustCompile(`(?i)\b(skip|disable|weaken|remove|delete|loosen|relax)\w*\s+(\w+\s+){0,2}(tests?|scenarios?|checks?|assertions?)\b`), "test-weakening"},
	{regexp.MustCompile(`(?i)\bmirrors?\b|\bby hand\b|\bhand[- ]fix`), "hand fixes on a runner or a mirror"},
}

// NeverAtNight names the never-list entry texts touch, or "".
func NeverAtNight(texts ...string) string {
	for _, t := range texts {
		for _, n := range neverAtNight {
			if n.re.MatchString(t) {
				return n.what
			}
		}
	}
	return ""
}

// DecideNight is what the night shift does with an open escalation at now, and why.
func DecideNight(c NightCase, set NightSetting, now time.Time, loc *time.Location) (NightVerdict, string) {
	it, e := c.Item, c.Esc
	if it.Stage != NeedsYou || c.NewQuestion {
		return NightGone, "the item moved on"
	}
	if c.UserSince {
		return NightGone, "you spoke about it"
	}
	if !set.On(now, loc) || !set.ClassOn(e.Class) {
		return NightWait, ""
	}
	if !e.Night {
		return NightStay, "escalated while the night shift was off: yours to answer"
	}
	if e.Approval {
		return NightStay, "approvals are yours"
	}
	if e.Step != "" {
		return NightStay, "its recommendation needs your hand first (" + e.Step + ")"
	}
	if it.Trust != nil && it.Trust.By == "" {
		return NightStay, "it waits on your OK of its trusted files"
	}
	if it.Risk == RiskHold {
		why := "the item is on the hold list"
		if it.HoldReason != "" {
			why += " (" + it.HoldReason + ")"
		}
		return NightStay, why
	}
	texts := []string{e.Recommendation, e.Deferred}
	if e.Class == ClassBounceLimit {
		texts = append(texts, it.Question)
	}
	if what := NeverAtNight(texts...); what != "" {
		return NightStay, what + " are never acted on at night"
	}
	switch e.Class {
	case ClassVerifyFlake:
		if c.NeedsYouOutcome != VerifyFailed {
			return NightStay, "the item's question is not a verify failure"
		}
	case ClassBounceLimit:
		if c.NeedsYouOutcome != Bounce {
			return NightStay, "the item's question is not a review bounce"
		}
	}
	if (e.Class == ClassVerifyFlake || e.Class == ClassBounceLimit) && !Rebuildable(it) {
		return NightStay, "the item has no build to send back"
	}
	start := NightStart(now, loc)
	for _, p := range c.Prior {
		switch {
		case e.Class == ClassVerifyFlake && p.Class == ClassVerifyFlake && p.HeadSHA == it.HeadSHA:
			head := "this head"
			if it.HeadSHA != "" {
				head += " (" + shortSHA(it.HeadSHA) + ")"
			}
			return NightStay, "verify was already re-run once on " + head + ": once per head"
		case e.Class == ClassBounceLimit && p.Class == ClassBounceLimit && !p.At.Before(start):
			return NightWait, "" // once per item per night: the next night may
		}
	}
	if now.Before(e.At.Add(NightGrace)) {
		return NightWait, ""
	}
	return NightAct, ""
}

func shortSHA(s string) string {
	if len(s) > 7 {
		return s[:7]
	}
	return s
}

// The night shift's actions on an item, as its event and its thread say them.

// NightVerifyNotes are a verify_flake rebuild's notes for the builder.
func NightVerifyNotes(reason string) string {
	return "Night shift: re-run verify. The orchestrator judged the verify failure unrelated to this change (" +
		strings.TrimSpace(reason) + "): do not change code for it; rebuild the same change so the runner's verify runs again, " +
		"and fix only what the new verify output shows is this change's."
}

// NightBounceNotes are a bounce_limit rebuild's notes: the latest review notes.
func NightBounceNotes(reviewNotes string) string {
	return "Night shift: one more build with the reviewer's notes. Fix each must-fix point:\n\n" + strings.TrimSpace(reviewNotes)
}

// DeferredTitle is the title of the item a scope choice files for the part it defers.
func DeferredTitle(from Item, deferred string) string {
	line := strings.TrimSpace(strings.SplitN(strings.TrimSpace(deferred), "\n", 2)[0])
	if r := []rune(line); len(r) > 100 {
		line = string(r[:99]) + "…"
	}
	return fmt.Sprintf("Deferred from #%d: %s", from.ID, line)
}
