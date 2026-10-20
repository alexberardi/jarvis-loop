package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"maps"
	"time"

	"github.com/alexberardi/jarvis-loop/internal/pipeline"
)

// The control row and its hold rules. On jarvis-loop's hold list
// (docs/decisions/2026-10-18-narrow-loop-holds.md).

func (s *Store) loadControl(q interface {
	QueryRow(string, ...any) *sql.Row
}) (pipeline.Control, error) {
	var c pipeline.Control
	var stopped, nightly int
	var liftSet, liftUntil int64
	var limits, rules, budgets string
	err := q.QueryRow(`SELECT active_agents, stopped, max_run_minutes, limits, hold_rules, orchestrator_turns_per_day, runway_warn_hours, budgets,
		regression_nightly, regression_at, pace_normal_set, pace_normal_until FROM control WHERE id = 1`).
		Scan(&c.ActiveAgents, &stopped, &c.MaxRunMinutes, &limits, &rules, &c.OrchTurnsPerDay, &c.RunwayWarnHours, &budgets, &nightly, &c.RegressionAt,
			&liftSet, &liftUntil)
	if err != nil {
		return c, err
	}
	c.PaceNormalSet, c.PaceNormalUntil = nanoTime(liftSet), nanoTime(liftUntil)
	c.Stopped, c.RegressionNightly = stopped != 0, nightly != 0
	c.RegressionAt = c.RegressionTime()
	if err := json.Unmarshal([]byte(limits), &c.Limits); err != nil {
		return c, err
	}
	// '': the defaults (what got us limited last time); a budget the user set is stored.
	c.Budgets = pipeline.DefaultBudgets()
	if budgets != "" {
		c.Budgets = map[string]pipeline.Budget{}
		if err := json.Unmarshal([]byte(budgets), &c.Budgets); err != nil {
			return c, err
		}
	}
	if rules == "" {
		c.HoldRules = s.DefaultHoldRules()
		return c, nil
	}
	err = json.Unmarshal([]byte(rules), &c.HoldRules)
	return c, err
}

// saveControl writes the control row. builtInRules keeps hold_rules at ” (the live built-in
// defaults) instead of writing the list out: a change to another field must not freeze the
// defaults as if the user had set them.
func saveControl(tx *sql.Tx, c pipeline.Control, builtInRules bool) error {
	limits, err := json.Marshal(c.Limits)
	if err != nil {
		return err
	}
	if c.HoldRules == nil {
		c.HoldRules = []pipeline.HoldRule{}
	}
	rules, err := json.Marshal(c.HoldRules)
	if err != nil {
		return err
	}
	if builtInRules {
		rules = nil
	}
	stopped := 0
	if c.Stopped {
		stopped = 1
	}
	budgets, err := json.Marshal(c.Budgets)
	if err != nil {
		return err
	}
	if maps.EqualFunc(c.Budgets, pipeline.DefaultBudgets(), func(a, b pipeline.Budget) bool { return a == b }) {
		budgets = nil
	}
	nightly := 0
	if c.RegressionNightly {
		nightly = 1
	}
	_, err = tx.Exec(`UPDATE control SET active_agents=?, stopped=?, max_run_minutes=?, limits=?, hold_rules=?, orchestrator_turns_per_day=?, runway_warn_hours=?, budgets=?,
		regression_nightly=?, regression_at=?, pace_normal_set=?, pace_normal_until=? WHERE id = 1`,
		c.ActiveAgents, stopped, c.MaxRunMinutes, string(limits), string(rules), c.OrchTurnsPerDay, c.RunwayWarnHours, string(budgets), nightly, c.RegressionAt,
		timeNano(c.PaceNormalSet), timeNano(c.PaceNormalUntil))
	return err
}

// nanoTime and timeNano convert the control row's unix-ns times (0: none).
func nanoTime(ns int64) time.Time {
	if ns == 0 {
		return time.Time{}
	}
	return time.Unix(0, ns).UTC()
}

func timeNano(t time.Time) int64 {
	if t.IsZero() {
		return 0
	}
	return t.UnixNano()
}

// builtInRules reports whether control's hold rules are the built-in defaults (stored as ”).
func builtInRules(tx *sql.Tx) (bool, error) {
	var raw string
	err := tx.QueryRow(`SELECT hold_rules FROM control WHERE id = 1`).Scan(&raw)
	return raw == "", err
}

// resetBuiltInCopy puts the hold rules back on the live defaults when the stored list is a copy of
// a built-in list (pipeline.IsBuiltInHoldRuleSet): a control change before phase 6b wrote the
// defaults out, which froze them. A list the user changed is theirs and stays.
func (s *Store) resetBuiltInCopy() error {
	return s.tx(context.Background(), func(tx *sql.Tx) error {
		var raw string
		if err := tx.QueryRow(`SELECT hold_rules FROM control WHERE id = 1`).Scan(&raw); err != nil || raw == "" {
			return err
		}
		var rules []pipeline.HoldRule
		if json.Unmarshal([]byte(raw), &rules) != nil || !pipeline.IsBuiltInHoldRuleSet(rules) {
			return nil
		}
		_, err := tx.Exec(`UPDATE control SET hold_rules = '' WHERE id = 1`)
		return err
	})
}

// Control returns the current control row.
func (s *Store) Control(ctx context.Context) (pipeline.Control, error) {
	return s.loadControl(s.r)
}

// ControlPatch changes some of the control row; Limits are merged key by key, HoldRules
// replace the whole list.
type ControlPatch struct {
	ActiveAgents  *int                 `json:"active_agents"`
	Stopped       *bool                `json:"stopped"`
	MaxRunMinutes *int                 `json:"max_run_minutes"`
	Limits        map[string]int       `json:"limits"`
	HoldRules     *[]pipeline.HoldRule `json:"hold_rules"`
	// OrchTurnsPerDay is the orchestrator's daily cap of turns.
	OrchTurnsPerDay *int `json:"orchestrator_turns_per_day"`
	// RunwayWarnHours is the runway below which loopd warns the user when away (0: off).
	RunwayWarnHours *int `json:"runway_warn_hours"`
	// Budgets replace the budget of each provider they name (usage.go).
	Budgets map[string]pipeline.Budget `json:"budgets"`
	// RegressionNightly schedules the overnight regression every night, at RegressionAt (HH:MM).
	RegressionNightly *bool   `json:"regression_nightly"`
	RegressionAt      *string `json:"regression_at"`
	// PaceNormalUntil lifts claude's pacing (loop pace normal, #316) until a time (RFC 3339), or
	// until the next limit hit (PaceUntilNextHit); "" puts pacing back on. The lift starts now.
	PaceNormalUntil *string `json:"pace_normal_until"`
}

// PaceUntilNextHit is ControlPatch.PaceNormalUntil's "until the next limit hit".
const PaceUntilNextHit = "reset"

// SetControl applies a patch to the control row and records it.
func (s *Store) SetControl(ctx context.Context, actor string, p ControlPatch) (pipeline.Control, error) {
	var out pipeline.Control
	err := s.tx(ctx, func(tx *sql.Tx) error {
		old, err := s.loadControl(tx)
		if err != nil {
			return err
		}
		c := old
		c.Limits = map[string]int{}
		for k, v := range old.Limits {
			c.Limits[k] = v
		}
		if p.ActiveAgents != nil {
			c.ActiveAgents = *p.ActiveAgents
		}
		if p.Stopped != nil {
			c.Stopped = *p.Stopped
		}
		if p.MaxRunMinutes != nil {
			c.MaxRunMinutes = *p.MaxRunMinutes
		}
		if p.OrchTurnsPerDay != nil {
			c.OrchTurnsPerDay = *p.OrchTurnsPerDay
		}
		if p.RunwayWarnHours != nil {
			c.RunwayWarnHours = *p.RunwayWarnHours
		}
		if p.RegressionNightly != nil {
			c.RegressionNightly = *p.RegressionNightly
		}
		if p.RegressionAt != nil {
			c.RegressionAt = *p.RegressionAt
		}
		var liftNote string
		if p.PaceNormalUntil != nil {
			if liftNote, err = s.liftPace(&c, *p.PaceNormalUntil); err != nil {
				return err
			}
		}
		for k, v := range p.Limits {
			c.Limits[k] = v
		}
		if p.Budgets != nil {
			c.Budgets = maps.Clone(old.Budgets)
			if c.Budgets == nil {
				c.Budgets = map[string]pipeline.Budget{}
			}
			maps.Copy(c.Budgets, p.Budgets)
		}
		builtIn, err := builtInRules(tx)
		if err != nil {
			return err
		}
		if p.HoldRules != nil {
			c.HoldRules = *p.HoldRules
			builtIn = false
		}
		if err := c.Validate(); err != nil {
			return err
		}
		if err := saveControl(tx, c, builtIn); err != nil {
			return err
		}
		out = c
		if err := s.event(tx, Event{Actor: actor, Kind: "control", Data: map[string]any{"from": old, "to": c}}); err != nil {
			return err
		}
		if liftNote == "" {
			return nil
		}
		// The orchestrator reads its notebook at every turn: the user's call on usage goes in it,
		// so it does not pace (or report pacing) against it.
		return s.event(tx, Event{Actor: actor, Kind: KindOrchNote, Data: map[string]any{"text": liftNote, "reason": "the user set claude's pace"}})
	})
	return out, err
}

// liftPace applies a PaceNormalUntil patch to c and says it for the orchestrator's notebook.
func (s *Store) liftPace(c *pipeline.Control, v string) (string, error) {
	now := s.clock.Now()
	switch v {
	case "":
		c.PaceNormalSet, c.PaceNormalUntil = time.Time{}, time.Time{}
		return "The user put claude's usage pacing back on: budgets and the CLI's warnings pace claims again.", nil
	case PaceUntilNextHit:
		c.PaceNormalSet, c.PaceNormalUntil = now, time.Time{}
		return "The user lifted claude's pacing until the next limit hit: claim at the normal pace whatever the budget or the CLI's warnings say. A limit hit still pauses claims and ends the lift.", nil
	}
	t, err := time.Parse(time.RFC3339, v)
	if err != nil {
		return "", errf(pipeline.CodeInvalidField, "pace_normal_until is a time (RFC 3339), %q or \"\" (pacing back on)", PaceUntilNextHit)
	}
	if !t.After(now) {
		return "", errf(pipeline.CodeInvalidField, "pace_normal_until must be in the future")
	}
	c.PaceNormalSet, c.PaceNormalUntil = now, t
	return "The user lifted claude's pacing until " + t.In(usageZone()).Format("Mon 2 Jan 15:04") +
		": claim at the normal pace whatever the budget or the CLI's warnings say. A limit hit still pauses claims and ends the lift.", nil
}

// SetDefaultHoldRules replaces the built-in hold rules (what control's hold_rules means while the
// user has set none). Nothing is recorded: like SetRepos, it is configuration, not a change. The
// loop's harness uses it to point the defaults at its scratch repos.
func (s *Store) SetDefaultHoldRules(rules []pipeline.HoldRule) {
	s.reposMu.Lock()
	defer s.reposMu.Unlock()
	s.rules = append([]pipeline.HoldRule(nil), rules...)
}

// DefaultHoldRules are the built-in hold rules.
func (s *Store) DefaultHoldRules() []pipeline.HoldRule {
	s.reposMu.RLock()
	defer s.reposMu.RUnlock()
	if s.rules == nil {
		return pipeline.DefaultHoldRules()
	}
	return append([]pipeline.HoldRule(nil), s.rules...)
}
