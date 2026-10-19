package pipeline

import (
	"time"
)

// Orchestrate turns (docs/phases/08-orchestrator.md, piece 3): loopd queues one turn at a time from
// its triggers and a 15-minute sweep; a runner that asks for them (kind orchestrate) takes it in
// the next free slot, ahead of every item and plan turn
// (docs/decisions/2026-10-17-orchestrate-turns.md).

// WorkOrchestrate is the claim kind of an orchestrate turn.
const WorkOrchestrate = "orchestrate"

// OrchestrateStage is the stage a running orchestrate turn counts as among the live leases.
const OrchestrateStage Stage = "orchestrate"

// DefaultOrchTurnsPerDay is the daily cap of orchestrate turns (the spec's "Cost");
// MaxOrchTurnsPerDay the most the control accepts. 0 turns the orchestrator off.
const (
	DefaultOrchTurnsPerDay = 150
	MaxOrchTurnsPerDay     = 2000
)

// OrchSweepEvery is how often loopd looks at the board for a turn when no trigger came.
const OrchSweepEvery = 15 * time.Minute

// Why a turn was queued.
const (
	TriggerNeedsYou     = "needs_you"     // an item entered needs_you (a question)
	TriggerRunFailed    = "run_failed"    // ... because its run failed or ran overtime
	TriggerLeaseExpired = "lease_expired" // a run's lease expired (the runner went away)
	TriggerIdle         = "idle"          // an item waited in its stage past IdleLimits
	TriggerLoopMerged   = "loop_merged"   // a merge to the loop's own repo
	TriggerMessage      = "message"       // the user wrote to the orchestrator
	TriggerSweep        = "sweep"         // the 15-minute sweep, with something changed
	TriggerDue          = "due"           // a wait the orchestrator was told about ended (a retry's backoff)
	TriggerProof        = "proof_needed"  // an item is in qa without a proof: write one (the proof sweep)
)

// MaxTriggers caps the reasons one turn collects: past it, the snapshot has the rest.
const MaxTriggers = 50

// Trigger is one reason for a turn.
type Trigger struct {
	Reason string `json:"reason"`
	ItemID int64  `json:"item_id,omitempty"`
	Text   string `json:"text,omitempty"` // a message's words
	// Where a message came from, so the turn's reply lands in its thread: the channel (a DM's
	// too), its ts and the thread it was in ("" when it started none), and whether the approver
	// wrote it (only the approver's words may make the orchestrator act).
	Channel  string    `json:"channel,omitempty"`
	TS       string    `json:"ts,omitempty"`
	ThreadTS string    `json:"thread_ts,omitempty"`
	Approver bool      `json:"approver,omitempty"`
	At       time.Time `json:"at"`
}

// ReplyThread is the thread a reply to a message trigger goes in: the thread the message was in,
// or the thread it starts.
func (t Trigger) ReplyThread() string {
	if t.ThreadTS != "" {
		return t.ThreadTS
	}
	return t.TS
}

// AddTrigger adds t to a turn's reasons: the same reason on the same item joins the first (a
// message always counts on its own), and at most MaxTriggers are kept.
func AddTrigger(ts []Trigger, t Trigger) []Trigger {
	if t.Reason != TriggerMessage {
		for _, o := range ts {
			if o.Reason == t.Reason && o.ItemID == t.ItemID {
				return ts
			}
		}
	}
	if len(ts) >= MaxTriggers {
		return ts
	}
	return append(ts, t)
}

// OrchTriggerFor is the trigger an item event is, or "": an entry into needs_you (a run failure
// or a question) or an expired lease.
func OrchTriggerFor(kind string, from, to Stage, outcome Outcome) string {
	switch {
	case kind == "lease_expired":
		return TriggerLeaseExpired
	case to == NeedsYou && from != NeedsYou && IsRunFailure(kind, outcome):
		return TriggerRunFailed
	case to == NeedsYou && from != NeedsYou:
		return TriggerNeedsYou
	}
	return ""
}

// IdleLimits is how long an item may wait in a work stage, unclaimed and unchanged, before the
// orchestrator looks at it. The other stages wait for the user, which needs_you and the
// escalations already cover.
var IdleLimits = map[Stage]time.Duration{
	Triage: time.Hour,
	Build:  2 * time.Hour,
	Review: time.Hour,
	Land:   time.Hour,
}

// IdlePast says an item in stage, unchanged since since, has waited longer than its stage allows.
func IdlePast(stage Stage, since, now time.Time) bool {
	limit, ok := IdleLimits[stage]
	return ok && !now.Before(since.Add(limit))
}

// OrchSweep is the 15-minute sweep: due when OrchSweepEvery passed since the last one, and run (a
// turn queued) only when something is open and the board changed since the last turn (fp, a
// fingerprint of what the orchestrator did not do itself, against the last turn's).
func OrchSweep(last, now time.Time, open int, fp, lastFP string) (due, run bool) {
	if !last.IsZero() && now.Before(last.Add(OrchSweepEvery)) {
		return false, false
	}
	return true, open > 0 && fp != lastFP
}

// OrchUnderCap says another turn may start today: fewer than limit started (0 is off).
func OrchUnderCap(limit, today int) bool { return today < limit }

// OrchDay is the day a turn counts on (the cap, the session's rotation) in loc.
func OrchDay(t time.Time, loc *time.Location) string { return t.In(loc).Format(time.DateOnly) }

// OrchResume is the session a turn starting on day resumes: the orchestrator's session of that
// day, or "" (a new one: the session rotates daily so its context stays small).
func OrchResume(session, sessionDay, day string) string {
	if sessionDay != day {
		return ""
	}
	return session
}

// OrchCandidate is the queued orchestrate turn a runner could take.
type OrchCandidate struct {
	TurnID    int64
	CreatedAt time.Time
}
