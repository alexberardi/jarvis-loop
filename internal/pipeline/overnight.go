package pipeline

import (
	"fmt"
	"regexp"
	"slices"
	"strings"
	"time"
)

// The overnight regression (docs/decisions/2026-10-19-overnight-regression.md): the approver
// schedules one full run (`@jarvis-loop regression tonight`, the board's toggle, or every night by
// the control's regression_nightly). It starts when the queue is quiet (no agent lease live and
// nothing claimable for OvernightQuiet) and not before the set time (regression_at, default
// 01:00), whichever is later; while it runs it holds every claim but land's; its report waits for
// the morning.

// SuiteOvernight is the full run: scripts/e2e all, the voice lane and the pipeline scenarios on
// the real model, each failure rerun once (a pass on the rerun is flaky).
const SuiteOvernight = "overnight"

// OvernightQuiet is how long the queue must have been quiet before a scheduled run starts.
const OvernightQuiet = 10 * time.Minute

// OvernightMaxRun caps an overnight run (its lease is judged against it): three lanes, each failure
// rerun.
const OvernightMaxRun = 6 * time.Hour

// DefaultRegressionAt is the set time of a scheduled run when the control names none.
const DefaultRegressionAt = "01:00"

// MorningReportAt is when an overnight run's report reaches #loop when it ends at night, before
// the 09:00 standup; one that ends in the day (MorningReportAt to EveningAt) is posted at once.
const (
	MorningReportAt = "08:30"
	EveningAt       = "20:00"
	// UnstartedNoteAt: an overnight run still not started then (after its set time) gets a note in
	// #loop saying why (#317).
	UnstartedNoteAt = "07:00"
)

// RegressionMaxRunFor is the cap of a run of suite.
func RegressionMaxRunFor(suite string) time.Duration {
	if suite == SuiteOvernight {
		return OvernightMaxRun
	}
	return RegressionMaxRun
}

var clockRe = regexp.MustCompile(`^([01][0-9]|2[0-3]):([0-5][0-9])$`)

// ParseClock reads "HH:MM" (24-hour) into minutes after midnight.
func ParseClock(s string) (int, error) {
	m := clockRe.FindStringSubmatch(s)
	if m == nil {
		return 0, errf(CodeInvalidField, "regression_at must be HH:MM (24-hour), not %q", s)
	}
	var h, mm int
	fmt.Sscanf(m[1]+" "+m[2], "%d %d", &h, &mm)
	return h*60 + mm, nil
}

// NextAt is the first hhmm at or after now on loc's clock (hhmm valid; DefaultRegressionAt when
// it is not).
func NextAt(now time.Time, loc *time.Location, hhmm string) time.Time {
	mins, err := ParseClock(hhmm)
	if err != nil {
		mins, _ = ParseClock(DefaultRegressionAt)
	}
	t := now.In(loc)
	at := time.Date(t.Year(), t.Month(), t.Day(), mins/60, mins%60, 0, 0, loc)
	if at.Before(t) {
		at = time.Date(t.Year(), t.Month(), t.Day()+1, mins/60, mins%60, 0, 0, loc)
	}
	return at
}

// LastAt is the latest hhmm on loc's clock at or before now (NextAt's mirror).
func LastAt(now time.Time, loc *time.Location, hhmm string) time.Time {
	mins, err := ParseClock(hhmm)
	if err != nil {
		mins, _ = ParseClock(DefaultRegressionAt)
	}
	t := now.In(loc)
	at := time.Date(t.Year(), t.Month(), t.Day(), mins/60, mins%60, 0, 0, loc)
	if at.After(t) {
		at = time.Date(t.Year(), t.Month(), t.Day()-1, mins/60, mins%60, 0, 0, loc)
	}
	return at
}

// MorningReport is when an overnight run that finished at finished is reported: at once in the day
// (MorningReportAt to EveningAt on loc's clock), else at the next MorningReportAt.
func MorningReport(finished time.Time, loc *time.Location) time.Time {
	morning := NextAt(finished, loc, MorningReportAt)
	t := finished.In(loc)
	mins := t.Hour()*60 + t.Minute()
	from, _ := ParseClock(MorningReportAt)
	to, _ := ParseClock(EveningAt)
	if mins >= from && mins < to {
		return finished
	}
	return morning
}

// HoldsClaims says a live lease holds every claim but land's: a running overnight run (it needs
// the emulator, the sandbox ports and the CPU to itself).
func HoldsClaims(live []LiveLease) bool {
	return slices.ContainsFunc(live, func(l LiveLease) bool { return l.Holds })
}

// QueueBusy says why the queue is not quiet ("" when it is): an agent lease is live (any but land,
// which needs no emulator and keeps landing through the night, and a regression's own), or an
// agent could start something: an item waiting in triage, build or review whose dependencies are
// merged, not held by a frozen repo, that some runner offers the resources for and that the claude
// pace lets through (with a slow pace's starved fallback, as the board's paced label says); a queued
// plan turn or the queued orchestrate turn (orchQueued) the pace lets through. Work the pace holds
// back cannot be claimed, so it leaves the queue quiet for the overnight run, whose lanes are no
// claude runs (#317).
func QueueBusy(c Control, pace Pace, items []Item, deps map[int64]Stage, live []LiveLease, turns []TurnCandidate, orchQueued bool, frozen Frozen, runners []RunnerSeen, now time.Time) string {
	for _, l := range live {
		switch {
		case l.Stage == Land || l.Stage == RegressionStage:
		case l.ItemID != 0:
			return fmt.Sprintf("the %s of #%d runs", l.Stage, l.ItemID)
		case l.Stage == OrchestrateStage:
			return "the orchestrate turn runs"
		default:
			return fmt.Sprintf("a %s turn runs", l.Stage)
		}
	}
	p := newPacing(c, pace, live, RunnerBuilders(runners, now))
	for _, t := range turns {
		if p.claudeOK && (pace.Level == PaceNormal || t.Priority == "P1") {
			return fmt.Sprintf("a turn of plan %d is queued", t.PlanID)
		}
	}
	if orchQueued && p.claudeOK {
		return "the orchestrate turn is queued"
	}
	claimed := map[int64]bool{}
	for _, l := range live {
		claimed[l.ItemID] = true
	}
	for _, it := range items {
		if it.Stage == Land || !it.Stage.IsWork() || claimed[it.ID] {
			continue
		}
		if len(WaitingOn(it, deps)) > 0 || frozen.Blocks(it) || len(Unservable(it.Needs(), it.Repo, runners, now)) > 0 || !p.fallback(it) {
			continue
		}
		return fmt.Sprintf("#%d waits in %s", it.ID, it.Stage)
	}
	return ""
}

// OvernightReady says a scheduled run may start: its set time (notBefore) has come and the queue
// has been quiet since quietSince (zero: it is not) for OvernightQuiet.
func OvernightReady(notBefore, quietSince, now time.Time) bool {
	return !now.Before(notBefore) && !quietSince.IsZero() && now.Sub(quietSince) >= OvernightQuiet
}

var tonightRe = regexp.MustCompile(`(?i)^regression\s+tonight(?:\s+(off|cancel))?[.!]?$`)

// ParseRegressionTonight reads "regression tonight" (schedule the overnight run: on true) and
// "regression tonight off|cancel" (cancel it: on false); ok is false for anything else.
func ParseRegressionTonight(text string) (on, ok bool) {
	m := tonightRe.FindStringSubmatch(strings.TrimSpace(text))
	if m == nil {
		return false, false
	}
	return m[1] == "", true
}

// Lanes of an overnight run's tests (the suites of scripts/e2e plus the pipeline scenarios).
var Lanes = []string{SuiteWeb, SuiteMobile, "pipeline", "voice"}
