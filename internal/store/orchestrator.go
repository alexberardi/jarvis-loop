package store

import (
	"context"
	"database/sql"
	"errors"
	"strconv"
	"strings"
	"time"

	"github.com/alexberardi/jarvis-loop/internal/pipeline"
)

// The orchestrator's actions (docs/phases/08-orchestrator.md, "What it may do"). Each is checked
// against its limit here, in the transaction that records it, and each event carries its reason.
// actor is the orchestrator's ("orchestrator:<name>").

// Event kinds of the orchestrator's own actions without a transition.
const (
	KindOrchEscalated = "orchestrator.escalated"
	KindOrchNote      = "orchestrator.note"
	KindOrchAdded     = "orchestrator.added"
	KindOrchSeenAgain = "orchestrator.seen_again"
	KindLinked        = "linked"
)

const orchPrefix = "orchestrator:"

type viaKey struct{}

// WithVia marks the writes made with ctx as done through someone else (an orchestrator acting for
// the user): every event they record carries data.via.
func WithVia(ctx context.Context, via string) context.Context {
	return context.WithValue(ctx, viaKey{}, via)
}

// ViaFrom is who ctx's writes are done through (WithVia), or "".
func ViaFrom(ctx context.Context) string {
	v, _ := ctx.Value(viaKey{}).(string)
	return v
}

// Refused records a refused action on an item (an API route the principal may not use), then
// returns err.
func (s *Store) Refused(ctx context.Context, actor string, id int64, action string, err error) error {
	return s.refuse(ctx, actor, id, action, err)
}

func itemEvents(tx *sql.Tx, id int64) ([]Event, error) {
	rows, err := tx.Query(`SELECT `+eventCols+` FROM events WHERE item_id = ? ORDER BY id`, id)
	if err != nil {
		return nil, err
	}
	return scanEvents(rows)
}

func str(m map[string]any, k string) string {
	v, _ := m[k].(string)
	return v
}

// orch applies an orchestrator action that moves or changes an item: rule sees the item and its
// events, and its transition is saved and recorded like a human one.
func (s *Store) orch(ctx context.Context, actor string, id int64, action, reason string,
	rule func(tx *sql.Tx, it pipeline.Item, evs []Event) (pipeline.Transition, error)) (pipeline.Item, error) {
	var out pipeline.Item
	err := s.tx(ctx, func(tx *sql.Tx) error {
		r, err := pipeline.NeedReason(reason)
		if err != nil {
			return err
		}
		it, err := loadItem(tx, id)
		if err != nil {
			return err
		}
		evs, err := itemEvents(tx, id)
		if err != nil {
			return err
		}
		tr, err := rule(tx, it, evs)
		if err != nil {
			return err
		}
		if tr.Data == nil {
			tr.Data = map[string]any{}
		}
		tr.Data["reason"] = r
		tr.Item.UpdatedAt = s.clock.Now()
		if err := saveItem(tx, tr.Item); err != nil {
			return err
		}
		out = tr.Item
		if err := s.event(tx, transitionEvent(id, actor, tr)); err != nil {
			return err
		}
		return s.planItemSettled(tx, tr.Item)
	})
	if err != nil {
		return out, s.refuse(ctx, actor, id, action, err)
	}
	return out, nil
}

// lastNeedsYou is the event that put the item in needs_you last.
func lastNeedsYou(evs []Event) (Event, bool) {
	for i := len(evs) - 1; i >= 0; i-- {
		if evs[i].To == string(pipeline.NeedsYou) && evs[i].From != string(pipeline.NeedsYou) {
			return evs[i], true
		}
	}
	return Event{}, false
}

// OrchAnswer is the orchestrator answering an item's question with an answer of one of the kinds it
// may give (pipeline.CheckOrchestratorAnswer). A question that is a run failure is not answered:
// that is OrchRetry, with its backoff.
func (s *Store) OrchAnswer(ctx context.Context, actor string, id int64, kind, text, reason string) (pipeline.Item, error) {
	return s.orch(ctx, actor, id, "answer", reason, func(_ *sql.Tx, it pipeline.Item, evs []Event) (pipeline.Transition, error) {
		if it.Stage != pipeline.NeedsYou {
			return pipeline.Transition{}, errf(pipeline.CodeWrongStage, "item is %s, not needs_you", it.Stage)
		}
		if e, ok := lastNeedsYou(evs); ok && pipeline.IsRunFailure(e.Kind, pipeline.Outcome(e.Outcome)) {
			return pipeline.Transition{}, errf(pipeline.CodeLimit, "the item is stuck on a run failure: retry it (with its backoff), do not answer it")
		}
		var prior []pipeline.OrchAnswer
		for _, e := range evs {
			if e.Kind == "answered" && strings.HasPrefix(e.Actor, orchPrefix) {
				prior = append(prior, pipeline.OrchAnswer{Kind: str(e.Data, "answer_kind"), Text: str(e.Data, "answer"), HeadSHA: str(e.Data, "head_sha")})
			}
		}
		if err := pipeline.CheckOrchestratorAnswer(kind, text, it.HeadSHA, prior); err != nil {
			return pipeline.Transition{}, err
		}
		tr, err := pipeline.ApplyAnswer(it, text)
		if err != nil {
			return tr, err
		}
		tr.Data["answer_kind"], tr.Data["head_sha"] = kind, it.HeadSHA
		return tr, nil
	})
}

// OrchRetry sends an item stuck on a run failure (a failed run or one past the time limit) back to
// its stage, after the backoff for its failure on this head; the third failure on a head is
// refused and escalated instead.
func (s *Store) OrchRetry(ctx context.Context, actor string, id int64, reason string) (pipeline.Item, error) {
	var escalate map[string]any
	var due time.Time
	it, err := s.orch(ctx, actor, id, "retry", reason, func(_ *sql.Tx, it pipeline.Item, evs []Event) (pipeline.Transition, error) {
		if it.Stage != pipeline.NeedsYou {
			return pipeline.Transition{}, errf(pipeline.CodeWrongStage, "item is %s, not needs_you", it.Stage)
		}
		failed, ok := lastNeedsYou(evs)
		if !ok || !pipeline.IsRunFailure(failed.Kind, pipeline.Outcome(failed.Outcome)) {
			return pipeline.Transition{}, errf(pipeline.CodeWrongStage, "the item is not stuck on a run failure; only those are retried")
		}
		failures := 1
		for _, e := range evs {
			if e.Kind == "retried" && str(e.Data, "head_sha") == it.HeadSHA {
				failures++
			}
		}
		notBefore, ask := pipeline.RetryWait(failures, failed.At)
		if ask {
			escalate = map[string]any{"text": "failure " + strconv.Itoa(failures) + " on the same head: " + it.Question,
				"reason": "the third failure on one head is the user's", "failure": failures, "head_sha": it.HeadSHA, "auto": true}
			return pipeline.Transition{}, errf(pipeline.CodeLimit, "failure %d on this head: escalated to the user, not retried", failures)
		}
		if now := s.clock.Now(); now.Before(notBefore) {
			due = notBefore
			return pipeline.Transition{}, errf(pipeline.CodeTooSoon, "failure %d: retry after %s", failures, notBefore.Format(time.RFC3339))
		}
		return pipeline.ApplyRetry(it, failures, reason)
	})
	if !due.IsZero() {
		// The orchestrator's next turn starts when the backoff ends, not at the next sweep.
		if e := s.OrchDue(ctx, due); e != nil {
			return it, e
		}
	}
	if escalate != nil {
		// Recorded apart from the refused retry (whose transaction rolled back), and not rate
		// limited: loopd escalates this one itself, once per item and head (a repeated retry
		// must not ping the user again).
		if e := s.tx(ctx, func(tx *sql.Tx) error {
			var n int
			if err := tx.QueryRow(`SELECT COUNT(*) FROM events WHERE item_id = ? AND kind = ? AND actor = 'loopd'
				AND json_extract(data, '$.auto') AND json_extract(data, '$.head_sha') = ?`,
				id, KindOrchEscalated, escalate["head_sha"]).Scan(&n); err != nil || n > 0 {
				return err
			}
			return s.event(tx, Event{ItemID: id, Actor: "loopd", Kind: KindOrchEscalated, Data: escalate})
		}); e != nil {
			return it, e
		}
	}
	return it, err
}

// OrchPriority sets an item's priority, any of P1 to P5 (the user's decision 3).
func (s *Store) OrchPriority(ctx context.Context, actor string, id int64, priority, reason string) (pipeline.Item, error) {
	return s.orch(ctx, actor, id, "priority", reason, func(_ *sql.Tx, it pipeline.Item, _ []Event) (pipeline.Transition, error) {
		return pipeline.ApplyPatch(it, &priority, nil)
	})
}

// OrchLink makes an open item wait for another open item (loop add --after, after the fact).
func (s *Store) OrchLink(ctx context.Context, actor string, id, after int64, reason string) (pipeline.Item, error) {
	return s.orch(ctx, actor, id, "link", reason, func(tx *sql.Tx, it pipeline.Item, _ []Event) (pipeline.Transition, error) {
		it, err := linkAfter(tx, it, after)
		if err != nil {
			return pipeline.Transition{}, err
		}
		return pipeline.Transition{Item: it, From: it.Stage, To: it.Stage, Kind: KindLinked,
			Data: map[string]any{"after": after, "depends_on": it.DependsOn}}, nil
	})
}

// linkAfter makes an open item wait for another open item, and saves it: checkDependencies reads
// the graph from the table to refuse a cycle.
func linkAfter(tx *sql.Tx, it pipeline.Item, after int64) (pipeline.Item, error) {
	if !isOpen(it.Stage) {
		return it, errf(pipeline.CodeWrongStage, "item is %s", it.Stage)
	}
	if after == it.ID {
		return it, errf(pipeline.CodeInvalidField, "an item cannot wait for itself")
	}
	var stage pipeline.Stage
	err := tx.QueryRow(`SELECT stage FROM items WHERE id = ?`, after).Scan(&stage)
	if errors.Is(err, sql.ErrNoRows) {
		return it, errf(pipeline.CodeInvalidField, "after: no item #%d", after)
	}
	if err != nil {
		return it, err
	}
	if !isOpen(stage) {
		return it, errf(pipeline.CodeInvalidField, "after: #%d is %s; only an open item can be waited for", after, stage)
	}
	for _, d := range it.DependsOn {
		if d == after {
			return it, errf(pipeline.CodeInvalidField, "#%d already waits for #%d", it.ID, after)
		}
	}
	it.DependsOn = append(append([]int64{}, it.DependsOn...), after)
	if err := saveItem(tx, it); err != nil {
		return it, err
	}
	return it, checkDependencies(tx, it)
}

func isOpen(st pipeline.Stage) bool { return st != pipeline.Done && st != pipeline.Closed }

// OrchAdd files an item (a process ticket or a follow-up) with a failure signature. An open item
// filed with the same normalised signature is returned instead (dup), with the new reason recorded
// on it; otherwise at most pipeline.OrchAddsPerDay a day.
func (s *Store) OrchAdd(ctx context.Context, actor string, n NewItem, signature, reason string) (it pipeline.Item, dup bool, err error) {
	sig := pipeline.NormaliseSignature(signature)
	err = s.tx(ctx, func(tx *sql.Tx) error {
		r, err := pipeline.NeedReason(reason)
		if err != nil {
			return err
		}
		if sig == "" {
			return errf(pipeline.CodeMissingField, "signature is required: it deduplicates the orchestrator's items")
		}
		var existing int64
		err = tx.QueryRow(`SELECT e.item_id FROM events e JOIN items i ON i.id = e.item_id
			WHERE e.kind = ? AND json_extract(e.data, '$.signature') = ? AND i.stage NOT IN (?, ?) ORDER BY e.id LIMIT 1`,
			KindOrchAdded, sig, string(pipeline.Done), string(pipeline.Closed)).Scan(&existing)
		switch {
		case errors.Is(err, sql.ErrNoRows):
		case err != nil:
			return err
		default:
			if it, err = loadItem(tx, existing); err != nil {
				return err
			}
			dup = true
			return s.event(tx, Event{ItemID: existing, Actor: actor, Kind: KindOrchSeenAgain,
				Data: map[string]any{"signature": sig, "raw_signature": signature, "reason": r, "title": n.Title}})
		}
		var added int
		if err := tx.QueryRow(`SELECT COUNT(*) FROM events WHERE kind = ? AND actor LIKE ? AND at > ?`,
			KindOrchAdded, orchPrefix+"%", s.clock.Now().Add(-24*time.Hour).UnixNano()).Scan(&added); err != nil {
			return err
		}
		if added >= pipeline.OrchAddsPerDay {
			return errf(pipeline.CodeLimit, "already %d items filed in the last 24 hours; recommend the next one to the user", added)
		}
		if n.Source == "" {
			n.Source = "finding"
		}
		if n.Repo, err = s.ResolveRepo(n.Repo); err != nil {
			return err
		}
		if it, err = pipeline.NewItem(n.Title, n.Ask, n.Repo, n.Priority, n.Source, s.clock.Now()); err != nil {
			return err
		}
		it.DependsOn = dedupeIDs(n.DependsOn)
		if err := s.insertItem(tx, actor, &it); err != nil {
			return err
		}
		if err := checkDependencies(tx, it); err != nil {
			return err
		}
		return s.event(tx, Event{ItemID: it.ID, Actor: actor, Kind: KindOrchAdded,
			Data: map[string]any{"signature": sig, "raw_signature": signature, "reason": r}})
	})
	if err != nil {
		return it, false, s.refuse(ctx, actor, 0, "add", err)
	}
	return it, dup, nil
}

// OrchEscalate records the orchestrator asking the user (the Slack ping is the Slack layer's): text
// is what it needs, recommendation what it recommends. At most one per pipeline.OrchEscalateEvery,
// except about an approval (an item held for one). id may be 0 (not about one item).
func (s *Store) OrchEscalate(ctx context.Context, actor string, id int64, text, recommendation, reason string) (Event, error) {
	var out Event
	err := s.tx(ctx, func(tx *sql.Tx) error {
		r, err := pipeline.NeedReason(reason)
		if err != nil {
			return err
		}
		if strings.TrimSpace(text) == "" {
			return errf(pipeline.CodeMissingField, "text is required: what the user is asked")
		}
		approval := false
		if id != 0 {
			it, err := loadItem(tx, id)
			if err != nil {
				return err
			}
			approval = it.Stage == pipeline.Held
		}
		now := s.clock.Now()
		if !approval {
			var last sql.NullInt64
			if err := tx.QueryRow(`SELECT MAX(at) FROM events WHERE kind = ? AND COALESCE(json_extract(data, '$.approval'), 0) = 0`,
				KindOrchEscalated).Scan(&last); err != nil {
				return err
			}
			if last.Valid {
				if next := fromNanos(last.Int64).Add(pipeline.OrchEscalateEvery); now.Before(next) {
					return errf(pipeline.CodeTooSoon, "one escalation per %d minutes: escalate after %s, or batch it with the next",
						int(pipeline.OrchEscalateEvery.Minutes()), next.Format(time.RFC3339))
				}
			}
		}
		return s.recordEvent(tx, &out, Event{ItemID: id, Actor: actor, Kind: KindOrchEscalated, Data: map[string]any{
			"text": strings.TrimSpace(text), "recommendation": strings.TrimSpace(recommendation), "reason": r, "approval": approval}})
	})
	if err != nil {
		return out, s.refuse(ctx, actor, id, "escalate", err)
	}
	return out, nil
}

// OrchAgentsDown lowers the active-agents number (never raises it, so never above the user's). The
// event (kind control) says ping: the Slack layer tells the user every time.
func (s *Store) OrchAgentsDown(ctx context.Context, actor string, want int, reason string) (pipeline.Control, error) {
	var out pipeline.Control
	err := s.tx(ctx, func(tx *sql.Tx) error {
		r, err := pipeline.NeedReason(reason)
		if err != nil {
			return err
		}
		old, err := s.loadControl(tx)
		if err != nil {
			return err
		}
		if err := pipeline.CheckAgentsDown(old.ActiveAgents, want); err != nil {
			return err
		}
		c := old
		c.ActiveAgents = want
		builtIn, err := builtInRules(tx)
		if err != nil {
			return err
		}
		if err := saveControl(tx, c, builtIn); err != nil {
			return err
		}
		out = c
		return s.event(tx, Event{Actor: actor, Kind: "control", Data: map[string]any{"from": old, "to": c, "reason": r, "ping": true}})
	})
	if err != nil {
		return out, s.refuse(ctx, actor, 0, "agents", err)
	}
	return out, nil
}

// OrchNote appends to the orchestrator's notebook.
func (s *Store) OrchNote(ctx context.Context, actor, text, reason string) (Event, error) {
	var out Event
	err := s.tx(ctx, func(tx *sql.Tx) error {
		r, err := pipeline.NeedReason(reason)
		if err != nil {
			return err
		}
		if strings.TrimSpace(text) == "" {
			return errf(pipeline.CodeMissingField, "text is required")
		}
		return s.recordEvent(tx, &out, Event{Actor: actor, Kind: KindOrchNote, Data: map[string]any{"text": strings.TrimSpace(text), "reason": r}})
	})
	return out, err
}

// Notebook is the orchestrator's notes, oldest first (the newest limit, or all when limit is 0).
func (s *Store) Notebook(ctx context.Context, limit int) ([]Event, error) {
	q := `SELECT ` + eventCols + ` FROM (SELECT * FROM events WHERE kind = ? ORDER BY id DESC`
	args := []any{KindOrchNote}
	if limit > 0 {
		q += ` LIMIT ?`
		args = append(args, limit)
	}
	rows, err := s.r.QueryContext(ctx, q+`) ORDER BY id`, args...)
	if err != nil {
		return nil, err
	}
	return scanEvents(rows)
}

// OrchBoard is what the board's Orchestrator panel shows (docs/phases/08-orchestrator.md, "Talking
// to it").
type OrchBoard struct {
	// Actions is its recent events, newest first: its own (refusals too), loopd's escalations,
	// and what the user did through it; its notes are in Notes.
	Actions []Event
	// Escalations is the open ones, newest first: about an item until the item changes stage (or
	// is done or closed), about none for EscalationShown.
	Escalations []Event
	// Tickets is the items it filed, newest first.
	Tickets []int64
	// Notes is its notebook's newest, oldest first.
	Notes []Event
}

// EscalationShown is how long an escalation about no item stays open on the board.
const EscalationShown = 24 * time.Hour

// OrchestratorBoard is the board's Orchestrator panel, at most limit of each list.
func (s *Store) OrchestratorBoard(ctx context.Context, limit int) (OrchBoard, error) {
	var out OrchBoard
	// An item it files is created, then orchestrator.added (with the reason): the second says it.
	rows, err := s.r.QueryContext(ctx, `SELECT `+eventCols+` FROM events WHERE kind != ? AND NOT (kind = 'created' AND actor LIKE ?) AND
		(actor LIKE ? OR kind = ? OR json_extract(data, '$.via') LIKE ?) ORDER BY id DESC LIMIT ?`,
		KindOrchNote, orchPrefix+"%", orchPrefix+"%", KindOrchEscalated, orchPrefix+"%", limit)
	if err != nil {
		return out, err
	}
	if out.Actions, err = scanEvents(rows); err != nil {
		return out, err
	}
	rows, err = s.r.QueryContext(ctx, `SELECT `+eventCols+` FROM events e WHERE kind = ? AND (
		(COALESCE(item_id, 0) = 0 AND at > ?) OR
		(EXISTS (SELECT 1 FROM items i WHERE i.id = e.item_id AND i.stage NOT IN (?, ?)) AND
		 NOT EXISTS (SELECT 1 FROM events l WHERE l.item_id = e.item_id AND l.id > e.id AND l.to_stage != '' AND l.to_stage != l.from_stage)))
		ORDER BY id DESC LIMIT ?`,
		KindOrchEscalated, s.clock.Now().Add(-EscalationShown).UnixNano(), string(pipeline.Done), string(pipeline.Closed), limit)
	if err != nil {
		return out, err
	}
	if out.Escalations, err = scanEvents(rows); err != nil {
		return out, err
	}
	rows, err = s.r.QueryContext(ctx, `SELECT item_id FROM events WHERE kind = ? ORDER BY id DESC LIMIT ?`, KindOrchAdded, limit)
	if err != nil {
		return out, err
	}
	defer rows.Close()
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return out, err
		}
		out.Tickets = append(out.Tickets, id)
	}
	if err := rows.Err(); err != nil {
		return out, err
	}
	out.Notes, err = s.Notebook(ctx, limit)
	return out, err
}

// ---- acting for the user ----

// SlackMessage is a message loopd's Slack layer saw.
type SlackMessage struct {
	Channel, TS, User, Text string
	At                      time.Time
}

// RecordSlackMessage keeps a message the Slack layer saw, so an action that names it can be
// checked. Recording one twice keeps the first.
func (s *Store) RecordSlackMessage(ctx context.Context, channel, ts, user, text string) error {
	return s.tx(ctx, func(tx *sql.Tx) error {
		_, err := tx.Exec(`INSERT INTO slack_messages (channel, ts, user, text, at) VALUES (?,?,?,?,?) ON CONFLICT DO NOTHING`,
			channel, ts, user, text, s.clock.Now().UnixNano())
		return err
	})
}
