package store

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"time"

	"github.com/alexberardi/jarvis-loop/internal/pipeline"
)

// The night shift (docs/decisions/2026-10-20-night-shift.md): loopd acts on the orchestrator's
// recommendation for an escalation of a night class once it has waited pipeline.NightGrace while
// the night shift is on, unless the user answered first. pipeline.DecideNight decides; this file
// finds the escalations, takes the class's action and records it.

// Event kinds of the night shift.
const (
	// KindOrchNightAction is an action the night shift took: data class, action, reason, undo,
	// escalation, head_sha (and filed: the item a scope choice filed).
	KindOrchNightAction = "orchestrator.night_action"
	// KindOrchNightStayed is an escalation of a night class the night shift leaves to the user
	// (data escalation, class, why): recorded once.
	KindOrchNightStayed = "orchestrator.night_stayed"
	// KindOrchNightOverride is the user's "night" or "morning" (data override, on, until).
	KindOrchNightOverride = "orchestrator.night_override"
)

// NightActor is the night shift's actor: an orchestrator's, so its actions show where the
// orchestrator's do (the board's panel, the standup, the item's thread).
const NightActor = orchPrefix + "night-shift"

// nightScan is how far back the sweep looks for open night escalations.
const nightScan = 48 * time.Hour

func (s *Store) nightSetting(tx interface {
	QueryRow(string, ...any) *sql.Row
}) (pipeline.NightSetting, error) {
	ctl, err := s.loadControl(tx)
	if err != nil {
		return pipeline.NightSetting{}, err
	}
	n := pipeline.NightSetting{Mode: ctl.NightShift, ClassesOff: ctl.NightClassesOff}
	var at int64
	if err := tx.QueryRow(`SELECT night_override, night_override_at FROM orch_state WHERE id = 1`).Scan(&n.Override, &at); err != nil {
		return n, err
	}
	if at != 0 {
		n.OverrideAt = fromNanos(at)
	}
	return n, nil
}

// NightState is the night shift as the board and Slack show it.
type NightState struct {
	pipeline.NightSetting
	// On: the night shift is on now.
	On bool `json:"on"`
	// Until is when auto mode next changes (the next 23:00 or 08:00); zero when it is off or on.
	Until time.Time `json:"until"`
}

func (s *Store) nightState(set pipeline.NightSetting, now time.Time) NightState {
	return NightState{NightSetting: set, On: set.On(now, s.orchLoc()), Until: set.Until(now, s.orchLoc())}
}

// NightShift is the night shift's state now.
func (s *Store) NightShift(ctx context.Context) (NightState, error) {
	set, err := s.nightSetting(s.r)
	if err != nil {
		return NightState{}, err
	}
	return s.nightState(set, s.clock.Now()), nil
}

// SetNightOverride records the user's "night" or "morning": in auto mode it turns the night shift
// on or off until the next 23:00 or 08:00. Off and on modes are not changed by it.
// channel and ts name the message that said it ("" when none).
func (s *Store) SetNightOverride(ctx context.Context, actor, override, channel, ts string) (NightState, error) {
	var out NightState
	if override != pipeline.OverrideNight && override != pipeline.OverrideMorning {
		return out, errf(pipeline.CodeInvalidField, "override must be %s or %s", pipeline.OverrideNight, pipeline.OverrideMorning)
	}
	err := s.tx(ctx, func(tx *sql.Tx) error {
		now := s.clock.Now()
		if _, err := tx.Exec(`UPDATE orch_state SET night_override = ?, night_override_at = ? WHERE id = 1`, override, now.UnixNano()); err != nil {
			return err
		}
		set, err := s.nightSetting(tx)
		if err != nil {
			return err
		}
		out = s.nightState(set, now)
		data := map[string]any{"override": override, "mode": set.Mode, "on": out.On}
		if ts != "" {
			data["channel"], data["ts"] = channel, ts
		}
		if !out.Until.IsZero() {
			data["until"] = out.Until
		}
		return s.event(tx, Event{Actor: actor, Kind: KindOrchNightOverride, Data: data})
	})
	return out, err
}

// isProcessTicket reports whether an item filed against repo is a process ticket: one against the
// loop's own repo (by its bare name: the harness's is sandbox/jarvis-loop).
func isProcessTicket(repo string) bool {
	_, name, _ := strings.Cut(repo, "/")
	return name == "jarvis-loop" || repo == "jarvis-loop"
}

// nightSweep acts on the open night escalations: the ones the night shift has not acted on or
// left to the user yet. OrchTick runs it.
func (s *Store) nightSweep(tx *sql.Tx, now time.Time) error {
	set, err := s.nightSetting(tx)
	if err != nil {
		return err
	}
	loc := s.orchLoc()
	if !set.On(now, loc) {
		return nil
	}
	escs, err := scanEventRows(tx.Query(`SELECT `+eventCols+` FROM events e WHERE kind = ? AND COALESCE(item_id, 0) != 0 AND at > ?
		AND COALESCE(json_extract(data, '$.class'), '') != ''
		AND NOT EXISTS (SELECT 1 FROM events n WHERE n.item_id = e.item_id AND n.kind IN (?, ?) AND json_extract(n.data, '$.escalation') = e.id)
		ORDER BY id`, KindOrchEscalated, now.Add(-nightScan).UnixNano(), KindOrchNightAction, KindOrchNightStayed))
	if err != nil {
		return err
	}
	for _, esc := range escs {
		it, c, err := nightCase(tx, esc)
		if err != nil {
			return err
		}
		verdict, why := pipeline.DecideNight(c, set, now, loc)
		switch verdict {
		case pipeline.NightStay:
			if err := s.event(tx, Event{ItemID: it.ID, Actor: NightActor, Kind: KindOrchNightStayed,
				Data: map[string]any{"escalation": esc.ID, "class": c.Esc.Class, "why": why, "reason": "never at night: " + why}}); err != nil {
				return err
			}
		case pipeline.NightAct:
			if err := s.nightAct(tx, esc, it, c.Esc, now); err != nil {
				return err
			}
		}
	}
	return nil
}

// nightCase is an escalation and its item as pipeline.DecideNight sees them.
func nightCase(tx *sql.Tx, esc Event) (pipeline.Item, pipeline.NightCase, error) {
	it, err := loadItem(tx, esc.ItemID)
	if err != nil {
		return it, pipeline.NightCase{}, err
	}
	evs, err := itemEvents(tx, esc.ItemID)
	if err != nil {
		return it, pipeline.NightCase{}, err
	}
	c := pipeline.NightCase{Item: it, Esc: pipeline.NightEscalation{
		Class: str(esc.Data, "class"), Recommendation: str(esc.Data, "recommendation"), Deferred: str(esc.Data, "deferred"),
		At: esc.At, Approval: esc.Data["approval"] == true, Night: esc.Data["night"] == true}}
	if last, ok := lastNeedsYou(evs); ok {
		c.NeedsYouOutcome = pipeline.Outcome(last.Outcome)
		c.NewQuestion = last.ID > esc.ID
	}
	for _, e := range evs {
		if e.ID > esc.ID && isUserActor(e.Actor) && e.Kind != "refused" {
			c.UserSince = true
		}
		if e.Kind == KindOrchNightAction {
			c.Prior = append(c.Prior, pipeline.NightPrior{Class: str(e.Data, "class"), HeadSHA: str(e.Data, "head_sha"), At: e.At})
		}
	}
	if !c.UserSince {
		var n int
		if err := tx.QueryRow(`SELECT COUNT(*) FROM events WHERE kind = ? AND id > ? AND json_extract(data, '$.item_id') = ?
			AND json_extract(data, '$.approver') = 1`, KindOrchMessage, esc.ID, esc.ItemID).Scan(&n); err != nil {
			return it, c, err
		}
		c.UserSince = n > 0
	}
	return it, c, nil
}

// isUserActor reports whether actor is a person's: the Slack user's, the CLI's or the board's.
func isUserActor(actor string) bool {
	for _, p := range []string{"slack:", "user:", "board:"} {
		if strings.HasPrefix(actor, p) {
			return true
		}
	}
	return false
}

// nightAct takes an escalation's class action on its item and records it.
func (s *Store) nightAct(tx *sql.Tx, esc Event, it pipeline.Item, e pipeline.NightEscalation, now time.Time) error {
	reason := clipText(str(esc.Data, "reason"), 500)
	var (
		tr           pipeline.Transition
		err          error
		action, undo string
		filed        int64
	)
	switch e.Class {
	case pipeline.ClassVerifyFlake:
		tr, err = pipeline.ApplyRebuild(it, pipeline.NightVerifyNotes(reason), false)
		action = "sent it back to build to re-run verify"
		if it.HeadSHA != "" {
			action += fmt.Sprintf(" on `%s`", shortHead(it.HeadSHA))
		}
		undo = fmt.Sprintf("loop rebuild %d --notes \"…\" (your notes replace mine)", it.ID)
	case pipeline.ClassBounceLimit:
		tr, err = pipeline.ApplyRebuild(it, pipeline.NightBounceNotes(it.Question), false)
		action = "sent it back to build once more with the reviewer's notes"
		undo = fmt.Sprintf("loop rebuild %d --notes \"…\" (your notes replace mine)", it.ID)
	case pipeline.ClassScopeChoice:
		d, nerr := pipeline.NewItem(pipeline.DeferredTitle(it, e.Deferred),
			fmt.Sprintf("%s\n\nDeferred by the night shift from #%d, whose scope question it answered with: %s",
				strings.TrimSpace(e.Deferred), it.ID, strings.TrimSpace(e.Recommendation)),
			it.Repo, pipeline.DeferredPriority, "finding", now)
		if nerr != nil {
			return nerr
		}
		d.Feature = it.Feature
		if err := s.insertItem(tx, NightActor, &d); err != nil {
			return err
		}
		filed = d.ID
		tr, err = pipeline.ApplyAnswer(it, fmt.Sprintf("%s\n\n(The night shift filed the deferred part as #%d.)", strings.TrimSpace(e.Recommendation), filed))
		action = fmt.Sprintf("answered with my recommendation (%s) and filed #%d (%s) for the rest", clipText(e.Recommendation, 200), filed, pipeline.DeferredPriority)
		undo = fmt.Sprintf("loop drop %d, then loop rebuild %d --notes \"…\" with the scope you want", filed, it.ID)
	default:
		return errf(pipeline.CodeInvalidField, "no night action for class %q", e.Class)
	}
	if err != nil {
		return err
	}
	tr.Data["night_shift"] = e.Class
	tr.Data["escalation"] = esc.ID
	tr.Data["reason"] = reason
	tr.Item.UpdatedAt = now
	if err := saveItem(tx, tr.Item); err != nil {
		return err
	}
	if err := s.event(tx, transitionEvent(it.ID, NightActor, tr)); err != nil {
		return err
	}
	data := map[string]any{"escalation": esc.ID, "class": e.Class, "action": action + " → " + string(tr.To), "reason": reason,
		"undo": undo, "head_sha": it.HeadSHA}
	if filed != 0 {
		data["filed"] = filed
	}
	return s.event(tx, Event{ItemID: it.ID, Actor: NightActor, Kind: KindOrchNightAction, Data: data})
}

func shortHead(sha string) string {
	if len(sha) > 7 {
		return sha[:7]
	}
	return sha
}

// NightActions are the night shift's actions recorded in [from, to), oldest first.
func (s *Store) NightActions(ctx context.Context, from, to time.Time) ([]Event, error) {
	return scanEventRows(s.r.QueryContext(ctx, `SELECT `+eventCols+` FROM events WHERE kind = ? AND at >= ? AND at < ? ORDER BY id`,
		KindOrchNightAction, from.UnixNano(), to.UnixNano()))
}
