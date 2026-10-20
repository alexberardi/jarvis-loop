package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/alexberardi/jarvis-loop/internal/pipeline"
)

// Orchestrate turns (docs/phases/08-orchestrator.md, piece 3;
// docs/decisions/2026-10-17-orchestrate-turns.md): loopd queues a turn from its triggers and the
// 15-minute sweep, a runner that asks for kind orchestrate takes it, and reports how it went.

// Turn statuses.
const (
	OrchQueued  = "queued"
	OrchRunning = "running"
	OrchDone    = "done"
	OrchFailed  = "failed"
)

// Event kinds of the turns themselves (no item). They, and the orchestrator's own actions, do not
// count as a change for the sweep.
const (
	KindOrchClaimed = "orchestrate_claimed"
	KindOrchDone    = "orchestrate_done"
	KindOrchFailed  = "orchestrate_failed"
	KindOrchExpired = "orchestrate_expired"
	KindOrchStopped = "orchestrate_stopped"
	KindOrchCapped  = "orchestrator.capped"
)

// OrchDayZone is the zone the orchestrator's day (its cap, its session) is counted in: the user's,
// as the digest's.
const OrchDayZone = "America/New_York"

// OrchActionResult is how one action of a turn's result went, as the runner reports it: the next
// turn's snapshot shows it (a refusal is not fatal: the orchestrator learns from it).
type OrchActionResult struct {
	Action  string `json:"action"`
	ItemID  int64  `json:"item_id,omitempty"`
	OK      bool   `json:"ok"`
	Status  int    `json:"status,omitempty"`
	Code    string `json:"code,omitempty"`
	Message string `json:"message,omitempty"`
}

// OrchTurn is one orchestrate turn.
type OrchTurn struct {
	ID        int64              `json:"id"`
	Status    string             `json:"status"`
	Triggers  []pipeline.Trigger `json:"triggers"`
	Day       string             `json:"day,omitempty"`
	Resume    string             `json:"resume,omitempty"`
	Session   string             `json:"session,omitempty"`
	Actions   []OrchActionResult `json:"actions,omitempty"`
	Error     string             `json:"error,omitempty"`
	Runner    string             `json:"runner,omitempty"`
	Agent     string             `json:"agent,omitempty"`
	ClaimedAt *time.Time         `json:"claimed_at,omitempty"`
	CreatedAt time.Time          `json:"created_at"`
	UpdatedAt time.Time          `json:"updated_at"`

	fingerprint string
	lease       Lease
}

// OrchTurnResult is a runner finishing the turn it holds: the session it ended with, how each
// action went, or why it failed.
type OrchTurnResult struct {
	SessionID string             `json:"session_id"`
	Error     string             `json:"error"`
	Actions   []OrchActionResult `json:"actions"`
	// The turn's spend as claude reported it (0 when it did not): cost, and tokens in (cache
	// reads and writes included) and out.
	CostUSD      float64 `json:"total_cost_usd,omitempty"`
	InputTokens  int64   `json:"usage_input,omitempty"`
	OutputTokens int64   `json:"usage_output,omitempty"`
}

// MaxOrchActions caps the actions a result reports.
const MaxOrchActions = 50

const orchTurnCols = `id, status, triggers, day, resume, session, fingerprint, actions, error, runner, agent,
	lease_id, claimed_at, expires_at, note, activity, created_at, updated_at`

func scanOrchTurn(row scanner) (OrchTurn, error) {
	var (
		t                         OrchTurn
		triggers, actions, act    string
		claimed, expires, cr, upd int64
	)
	err := row.Scan(&t.ID, &t.Status, &triggers, &t.Day, &t.Resume, &t.Session, &t.fingerprint, &actions, &t.Error,
		&t.Runner, &t.Agent, &t.lease.ID, &claimed, &expires, &t.lease.Note, &act, &cr, &upd)
	if err != nil {
		return t, err
	}
	if err := json.Unmarshal([]byte(triggers), &t.Triggers); err != nil {
		return t, err
	}
	if err := json.Unmarshal([]byte(actions), &t.Actions); err != nil {
		return t, err
	}
	t.CreatedAt, t.UpdatedAt = fromNanos(cr), fromNanos(upd)
	if claimed != 0 {
		c := fromNanos(claimed)
		t.ClaimedAt = &c
		t.lease.ClaimedAt, t.lease.ExpiresAt = c, fromNanos(expires)
	}
	t.lease.Stage, t.lease.Runner, t.lease.Agent, t.lease.Activity = pipeline.OrchestrateStage, t.Runner, t.Agent, parseActivity(act)
	return t, nil
}

func saveOrchTurn(tx *sql.Tx, t OrchTurn) error {
	if t.Triggers == nil {
		t.Triggers = []pipeline.Trigger{}
	}
	if t.Actions == nil {
		t.Actions = []OrchActionResult{}
	}
	triggers, err := json.Marshal(t.Triggers)
	if err != nil {
		return err
	}
	actions, err := json.Marshal(t.Actions)
	if err != nil {
		return err
	}
	var claimed int64
	if t.ClaimedAt != nil {
		claimed = t.ClaimedAt.UnixNano()
	}
	var expires int64
	if !t.lease.ExpiresAt.IsZero() {
		expires = t.lease.ExpiresAt.UnixNano()
	}
	_, err = tx.Exec(`UPDATE orch_turns SET status=?, triggers=?, day=?, resume=?, session=?, fingerprint=?, actions=?, error=?,
		runner=?, agent=?, lease_id=?, claimed_at=?, expires_at=?, note=?, activity=?, updated_at=? WHERE id = ?`,
		t.Status, string(triggers), t.Day, t.Resume, t.Session, t.fingerprint, string(actions), t.Error, t.Runner, t.Agent,
		t.lease.ID, claimed, expires, t.lease.Note, encodedActivity(t.lease.Activity), t.UpdatedAt.UnixNano(), t.ID)
	return err
}

// orchTurnWith is the turn with that status (queued or running: there is at most one), or nil.
func orchTurnWith(q interface {
	QueryRow(string, ...any) *sql.Row
}, status string) (*OrchTurn, error) {
	t, err := scanOrchTurn(q.QueryRow(`SELECT `+orchTurnCols+` FROM orch_turns WHERE status = ?`, status))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &t, nil
}

// SetLoopRepo is the loop's own repo (default pipeline.LoopRepo): a merge to it starts a turn.
func (s *Store) SetLoopRepo(slug string) {
	s.reposMu.Lock()
	defer s.reposMu.Unlock()
	s.loopRepo = slug
}

func (s *Store) loopRepoSlug() string {
	s.reposMu.RLock()
	defer s.reposMu.RUnlock()
	if s.loopRepo == "" {
		return pipeline.LoopRepo
	}
	return s.loopRepo
}

func (s *Store) orchDay(t time.Time) string { return pipeline.OrchDay(t, s.orchLoc()) }

// orchLoc is OrchDayZone's location (UTC when the zone database lacks it).
func (s *Store) orchLoc() *time.Location {
	loc, err := time.LoadLocation(OrchDayZone)
	if err != nil {
		return time.UTC
	}
	return loc
}

// queueOrch adds a trigger to the queued turn, queueing one when there is none (a turn that is
// running does not take it: it joins the next).
func (s *Store) queueOrch(tx *sql.Tx, tr pipeline.Trigger) error {
	now := s.clock.Now()
	if tr.At.IsZero() {
		tr.At = now
	}
	q, err := orchTurnWith(tx, OrchQueued)
	if err != nil {
		return err
	}
	if q == nil {
		b, _ := json.Marshal([]pipeline.Trigger{tr})
		_, err := tx.Exec(`INSERT INTO orch_turns (status, triggers, created_at, updated_at) VALUES (?,?,?,?)`,
			OrchQueued, string(b), now.UnixNano(), now.UnixNano())
		return err
	}
	q.Triggers = pipeline.AddTrigger(q.Triggers, tr)
	q.UpdatedAt = now
	return saveOrchTurn(tx, *q)
}

// QueueOrchestrate starts (or joins) an orchestrate turn for reason: the user's message to the
// orchestrator (pipeline.TriggerMessage, with its words) is the one loopd's Slack layer sends
// (piece 2); items are the items it names.
func (s *Store) QueueOrchestrate(ctx context.Context, reason, text string, items ...int64) error {
	return s.tx(ctx, func(tx *sql.Tx) error {
		if len(items) == 0 {
			return s.queueOrch(tx, pipeline.Trigger{Reason: reason, Text: clipText(text, 2000)})
		}
		for _, id := range items {
			if err := s.queueOrch(tx, pipeline.Trigger{Reason: reason, ItemID: id, Text: clipText(text, 2000)}); err != nil {
				return err
			}
		}
		return nil
	})
}

func clipText(s string, n int) string {
	s = strings.TrimSpace(s)
	if r := []rune(s); len(r) > n {
		return string(r[:n]) + "…"
	}
	return s
}

// orchHook queues a trigger for an item event that is one: an entry into needs_you, an expired
// lease, a merge to the loop's own repo. s.event calls it for every event it writes.
func (s *Store) orchHook(tx *sql.Tx, e Event) error {
	if e.ItemID == 0 {
		return nil
	}
	reason := pipeline.OrchTriggerFor(e.Kind, pipeline.Stage(e.From), pipeline.Stage(e.To), pipeline.Outcome(e.Outcome))
	if reason == "" && e.Outcome == string(pipeline.Merged) {
		var repo string
		if err := tx.QueryRow(`SELECT repo FROM items WHERE id = ?`, e.ItemID).Scan(&repo); err != nil {
			return err
		}
		if repo == s.loopRepoSlug() {
			reason = pipeline.TriggerLoopMerged
		}
	}
	if reason == "" {
		return nil
	}
	return s.queueOrch(tx, pipeline.Trigger{Reason: reason, ItemID: e.ItemID})
}

// orchFingerprint is what the sweep compares: the newest event the orchestrator did not cause
// (its own actions and its turns' bookkeeping are left out, so a turn's notes do not start the
// next one; so are the regression runs', which change no item).
func orchFingerprint(q interface {
	QueryRow(string, ...any) *sql.Row
}) (string, error) {
	var n int64
	err := q.QueryRow(`SELECT COALESCE(MAX(id), 0) FROM events WHERE actor NOT LIKE 'orchestrator:%'
		AND kind NOT LIKE 'orchestrat%' AND kind NOT LIKE 'regression%'`).Scan(&n)
	return strconv.FormatInt(n, 10), err
}

func (s *Store) orchTurnsOn(tx *sql.Tx, day string) (int, error) {
	var n int
	err := tx.QueryRow(`SELECT COUNT(*) FROM orch_turns WHERE day = ? AND status != ?`, day, OrchQueued).Scan(&n)
	return n, err
}

// orchCandidate is the queued turn a claim may start, or nil: none queued, one running, or the
// day's cap reached (recorded once a day).
func (s *Store) orchCandidate(tx *sql.Tx, ctl pipeline.Control) (*pipeline.OrchCandidate, error) {
	q, err := orchTurnWith(tx, OrchQueued)
	if err != nil || q == nil {
		return nil, err
	}
	if r, err := orchTurnWith(tx, OrchRunning); err != nil || r != nil {
		return nil, err
	}
	day := s.orchDay(s.clock.Now())
	n, err := s.orchTurnsOn(tx, day)
	if err != nil {
		return nil, err
	}
	if !pipeline.OrchUnderCap(ctl.OrchTurnsPerDay, n) {
		var capped string
		if err := tx.QueryRow(`SELECT capped_day FROM orch_state WHERE id = 1`).Scan(&capped); err != nil {
			return nil, err
		}
		if capped == day {
			return nil, nil
		}
		if _, err := tx.Exec(`UPDATE orch_state SET capped_day = ? WHERE id = 1`, day); err != nil {
			return nil, err
		}
		return nil, s.event(tx, Event{Actor: "loopd", Kind: KindOrchCapped,
			Data: map[string]any{"day": day, "turns": n, "limit": ctl.OrchTurnsPerDay, "turn": q.ID}})
	}
	return &pipeline.OrchCandidate{TurnID: q.ID, CreatedAt: q.CreatedAt}, nil
}

// claimOrch starts the queued turn: it resumes the day's session, and records the board it sees.
func (s *Store) claimOrch(tx *sql.Tx, runnerName, agent string, c pipeline.OrchCandidate) (*Claim, error) {
	now := s.clock.Now()
	t, err := scanOrchTurn(tx.QueryRow(`SELECT `+orchTurnCols+` FROM orch_turns WHERE id = ?`, c.TurnID))
	if err != nil {
		return nil, err
	}
	var session, sessionDay string
	if err := tx.QueryRow(`SELECT session, session_day FROM orch_state WHERE id = 1`).Scan(&session, &sessionDay); err != nil {
		return nil, err
	}
	id, err := newLeaseID()
	if err != nil {
		return nil, err
	}
	if t.fingerprint, err = orchFingerprint(tx); err != nil {
		return nil, err
	}
	t.Status, t.Runner, t.Agent, t.UpdatedAt, t.ClaimedAt = OrchRunning, runnerName, agent, now, &now
	t.Day = s.orchDay(now)
	t.Resume = pipeline.OrchResume(session, sessionDay, t.Day)
	t.lease = Lease{ID: id, Stage: pipeline.OrchestrateStage, Runner: runnerName, Agent: agent, ClaimedAt: now, ExpiresAt: now.Add(LeaseTTL)}
	if err := saveOrchTurn(tx, t); err != nil {
		return nil, err
	}
	if err := s.event(tx, Event{Actor: "runner:" + runnerName, Kind: KindOrchClaimed, Data: map[string]any{
		"turn": t.ID, "triggers": len(t.Triggers), "resume": t.Resume, "runner": runnerName, "agent": agent, "lease": id}}); err != nil {
		return nil, err
	}
	return &Claim{Kind: pipeline.WorkOrchestrate, Stage: pipeline.OrchestrateStage, Lease: t.lease, Orch: &t}, nil
}

// requeueOrch puts a turn whose run ended early back: its triggers join the queued turn (or it is
// queued again), so nothing it was for is lost.
func (s *Store) requeueOrch(tx *sql.Tx, t OrchTurn, why string) error {
	now := s.clock.Now()
	q, err := orchTurnWith(tx, OrchQueued)
	if err != nil {
		return err
	}
	t.UpdatedAt = now
	if q != nil {
		for _, tr := range t.Triggers {
			q.Triggers = pipeline.AddTrigger(q.Triggers, tr)
		}
		q.UpdatedAt = now
		t.Status, t.Error = OrchFailed, fmt.Sprintf("%s; its triggers joined turn %d", why, q.ID)
		if err := saveOrchTurn(tx, t); err != nil {
			return err
		}
		return saveOrchTurn(tx, *q)
	}
	t.Status, t.Day, t.Runner, t.Agent, t.ClaimedAt, t.lease = OrchQueued, "", "", "", nil, Lease{}
	return saveOrchTurn(tx, t)
}

// sweepOrch ends the running turn's lease when it expired (the turn waits again) or ran past the
// max run time (it fails).
func (s *Store) sweepOrch(tx *sql.Tx, now time.Time, maxRun time.Duration) error {
	t, err := orchTurnWith(tx, OrchRunning)
	if err != nil || t == nil {
		return err
	}
	data := map[string]any{"turn": t.ID, "lease": t.lease.ID, "runner": t.Runner, "agent": t.Agent, "note": t.lease.Note}
	switch pipeline.JudgeLease(t.lease.ClaimedAt, t.lease.ExpiresAt, now, maxRun) {
	case pipeline.LeaseExpired:
		if err := s.requeueOrch(tx, *t, "its lease expired"); err != nil {
			return err
		}
		return s.event(tx, Event{Actor: "loopd", Kind: KindOrchExpired, Data: data})
	case pipeline.LeaseOvertime:
		t.Status, t.Error, t.UpdatedAt = OrchFailed, fmt.Sprintf("the turn ran past the max run time (%s)", maxRun), now
		if err := saveOrchTurn(tx, *t); err != nil {
			return err
		}
		data["error"] = t.Error
		return s.event(tx, Event{Actor: "loopd", Kind: KindOrchFailed, Data: data})
	}
	return nil
}

// orchHeartbeat is Heartbeat for the running turn's lease. found is false when no turn holds it.
func (s *Store) orchHeartbeat(tx *sql.Tx, runnerName, leaseID, note, actJSON string) (stop, found bool, err error) {
	t, err := scanOrchTurn(tx.QueryRow(`SELECT `+orchTurnCols+` FROM orch_turns WHERE lease_id = ?`, leaseID))
	if errors.Is(err, sql.ErrNoRows) {
		return false, false, nil
	}
	if err != nil {
		return false, true, err
	}
	if t.Runner != runnerName {
		return false, true, errf(CodeNotYourLease, "lease %s is held by another runner", leaseID)
	}
	now := s.clock.Now()
	if _, err := tx.Exec(`UPDATE runners SET last_seen = ? WHERE name = ?`, now.UnixNano(), runnerName); err != nil {
		return false, true, err
	}
	if t.Status != OrchRunning {
		return true, true, nil
	}
	ctl, err := s.loadControl(tx)
	if err != nil {
		return false, true, err
	}
	if ctl.Stopped {
		if err := s.requeueOrch(tx, t, "the loop was stopped"); err != nil {
			return true, true, err
		}
		return true, true, s.event(tx, Event{Actor: "loopd", Kind: KindOrchStopped,
			Data: map[string]any{"turn": t.ID, "lease": leaseID, "runner": t.Runner, "agent": t.Agent, "note": note}})
	}
	if note != "" {
		t.lease.Note = note
	}
	if actJSON != "" {
		t.lease.Activity = parseActivity(actJSON)
	}
	t.lease.ExpiresAt = now.Add(LeaseTTL)
	return false, true, saveOrchTurn(tx, t)
}

// OrchestrateResult is a runner finishing the orchestrate turn it holds the lease on: done (with
// how its actions went) or failed; the session is the orchestrator's for the rest of the day.
func (s *Store) OrchestrateResult(ctx context.Context, runnerName string, turnID int64, r OrchTurnResult) (OrchTurn, error) {
	var out OrchTurn
	err := s.tx(ctx, func(tx *sql.Tx) error {
		if err := s.sweep(tx); err != nil {
			return err
		}
		t, err := scanOrchTurn(tx.QueryRow(`SELECT `+orchTurnCols+` FROM orch_turns WHERE id = ?`, turnID))
		if errors.Is(err, sql.ErrNoRows) {
			return errf(CodeNotFound, "no orchestrate turn %d", turnID)
		}
		if err != nil {
			return err
		}
		if t.Status != OrchRunning {
			return errf(CodeNoLease, "orchestrate turn %d has no live lease (it is %s)", turnID, t.Status)
		}
		if t.Runner != runnerName {
			return errf(CodeNotYourLease, "orchestrate turn %d is leased by another runner", turnID)
		}
		if len(r.Actions) > MaxOrchActions {
			return errf(pipeline.CodeInvalidField, "at most %d actions, got %d", MaxOrchActions, len(r.Actions))
		}
		for i := range r.Actions {
			r.Actions[i].Message = clipText(r.Actions[i].Message, 500)
		}
		now := s.clock.Now()
		t.Actions, t.Session, t.UpdatedAt = r.Actions, strings.TrimSpace(r.SessionID), now
		kind := KindOrchDone
		t.Status = OrchDone
		if why := strings.TrimSpace(r.Error); why != "" {
			t.Status, t.Error, kind = OrchFailed, clipText(why, 1500), KindOrchFailed
		}
		if err := saveOrchTurn(tx, t); err != nil {
			return err
		}
		if t.Session != "" {
			// The session of the day the turn started on: a turn that resumed it keeps it, a new
			// one (the first of a day, or one whose session was gone) replaces it.
			if _, err := tx.Exec(`UPDATE orch_state SET session = ?, session_day = ? WHERE id = 1`, t.Session, t.Day); err != nil {
				return err
			}
		}
		refused := 0
		for _, a := range t.Actions {
			if !a.OK {
				refused++
			}
		}
		data := map[string]any{"turn": t.ID, "lease": t.lease.ID, "actions": len(t.Actions), "refused": refused, "session": t.Session}
		if t.Error != "" {
			data["error"] = t.Error
		}
		if r.CostUSD > 0 || r.InputTokens > 0 || r.OutputTokens > 0 {
			data["total_cost_usd"], data["usage_input"], data["usage_output"] = r.CostUSD, r.InputTokens, r.OutputTokens
		}
		out = t
		return s.event(tx, Event{Actor: "runner:" + runnerName, Kind: kind, Data: data})
	})
	return out, err
}

// OrchDue tells loopd a wait the orchestrator met ends at: a turn starts then (a retry's backoff),
// without waiting for the sweep.
func (s *Store) OrchDue(ctx context.Context, at time.Time) error {
	return s.tx(ctx, func(tx *sql.Tx) error {
		_, err := tx.Exec(`UPDATE orch_state SET due_at = CASE WHEN due_at = 0 OR due_at > ?1 THEN ?1 ELSE due_at END WHERE id = 1`, at.UnixNano())
		return err
	})
}

// OrchTick runs the orchestrator's timed triggers: items idle past their stage's limit, the
// runway warning (runwayCheck), a wait that ended, and the 15-minute sweep (a turn only when the board changed since the last one).
// Sweep calls it at most once a minute of the store's clock.
func (s *Store) OrchTick(ctx context.Context) error {
	return s.tx(ctx, func(tx *sql.Tx) error {
		now := s.clock.Now()
		if err := s.orchIdle(tx, now); err != nil {
			return err
		}
		if err := s.runwayCheck(tx, now); err != nil {
			return err
		}
		if err := s.proofSweep(tx); err != nil {
			return err
		}
		if err := s.doneSweep(tx, now); err != nil {
			return err
		}
		if err := s.nightSweep(tx, now); err != nil {
			return err
		}
		var lastSweep, dueAt int64
		if err := tx.QueryRow(`SELECT last_sweep, due_at FROM orch_state WHERE id = 1`).Scan(&lastSweep, &dueAt); err != nil {
			return err
		}
		if dueAt != 0 && now.UnixNano() >= dueAt {
			if _, err := tx.Exec(`UPDATE orch_state SET due_at = 0 WHERE id = 1`); err != nil {
				return err
			}
			if err := s.queueOrch(tx, pipeline.Trigger{Reason: pipeline.TriggerDue, At: fromNanos(dueAt)}); err != nil {
				return err
			}
		}
		var last time.Time
		if lastSweep != 0 {
			last = fromNanos(lastSweep)
		}
		var open int
		if err := tx.QueryRow(`SELECT COUNT(*) FROM items WHERE stage NOT IN ('done','closed')`).Scan(&open); err != nil {
			return err
		}
		fp, err := orchFingerprint(tx)
		if err != nil {
			return err
		}
		var lastFP string
		err = tx.QueryRow(`SELECT fingerprint FROM orch_turns WHERE status != ? ORDER BY id DESC LIMIT 1`, OrchQueued).Scan(&lastFP)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		due, run := pipeline.OrchSweep(last, now, open, fp, lastFP)
		if !due {
			return nil
		}
		if _, err := tx.Exec(`UPDATE orch_state SET last_sweep = ? WHERE id = 1`, now.UnixNano()); err != nil {
			return err
		}
		if q, err := orchTurnWith(tx, OrchQueued); err != nil || q != nil || !run {
			return err // a queued turn sees the board anyway
		}
		return s.queueOrch(tx, pipeline.Trigger{Reason: pipeline.TriggerSweep})
	})
}

// orchIdle queues an idle trigger for each item waiting (unclaimed, its dependencies merged) in a
// work stage longer than its stage allows, once per wait. An item flagged unservable is skipped:
// loopd has told the user already.
func (s *Store) orchIdle(tx *sql.Tx, now time.Time) error {
	waiting, err := waitingItems(tx)
	if err != nil {
		return err
	}
	flagged, err := unservableMissing(tx)
	if err != nil {
		return err
	}
	deps, err := dependencyStages(tx, waiting)
	if err != nil {
		return err
	}
	for _, it := range waiting {
		if len(waitingOn(it, deps)) > 0 || !pipeline.IdlePast(it.Stage, it.UpdatedAt, now) || flagged[it.ID] != nil {
			continue
		}
		var seen int64
		err := tx.QueryRow(`SELECT updated_at FROM orch_idle WHERE item_id = ?`, it.ID).Scan(&seen)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		if seen == it.UpdatedAt.UnixNano() {
			continue
		}
		if _, err := tx.Exec(`INSERT INTO orch_idle (item_id, updated_at) VALUES (?,?) ON CONFLICT (item_id) DO UPDATE SET updated_at = excluded.updated_at`,
			it.ID, it.UpdatedAt.UnixNano()); err != nil {
			return err
		}
		if err := s.queueOrch(tx, pipeline.Trigger{Reason: pipeline.TriggerIdle, ItemID: it.ID, Text: string(it.Stage)}); err != nil {
			return err
		}
	}
	return nil
}

// OrchTurns are the orchestrate turns, newest first (at most limit).
func (s *Store) OrchTurns(ctx context.Context, limit int) ([]OrchTurn, error) {
	rows, err := s.r.QueryContext(ctx, `SELECT `+orchTurnCols+` FROM orch_turns ORDER BY id DESC LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []OrchTurn{}
	for rows.Next() {
		t, err := scanOrchTurn(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}
