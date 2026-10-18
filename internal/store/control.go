package store

import (
	"context"
	"database/sql"
	"encoding/json"

	"github.com/alexberardi/jarvis-loop/internal/pipeline"
)

// The control row and its hold rules. On jarvis-loop's hold list
// (docs/decisions/2026-10-18-narrow-loop-holds.md).

func (s *Store) loadControl(q interface {
	QueryRow(string, ...any) *sql.Row
}) (pipeline.Control, error) {
	var c pipeline.Control
	var stopped int
	var limits, rules string
	err := q.QueryRow(`SELECT active_agents, stopped, max_run_minutes, limits, hold_rules, orchestrator_turns_per_day FROM control WHERE id = 1`).
		Scan(&c.ActiveAgents, &stopped, &c.MaxRunMinutes, &limits, &rules, &c.OrchTurnsPerDay)
	if err != nil {
		return c, err
	}
	c.Stopped = stopped != 0
	if err := json.Unmarshal([]byte(limits), &c.Limits); err != nil {
		return c, err
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
	_, err = tx.Exec(`UPDATE control SET active_agents=?, stopped=?, max_run_minutes=?, limits=?, hold_rules=?, orchestrator_turns_per_day=? WHERE id = 1`,
		c.ActiveAgents, stopped, c.MaxRunMinutes, string(limits), string(rules), c.OrchTurnsPerDay)
	return err
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
}

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
		for k, v := range p.Limits {
			c.Limits[k] = v
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
		return s.event(tx, Event{Actor: actor, Kind: "control", Data: map[string]any{"from": old, "to": c}})
	})
	return out, err
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
