package sandbox

import (
	"fmt"
	"slices"
	"strings"

	"github.com/alexberardi/jarvis-loop/internal/pipeline"
	"github.com/alexberardi/jarvis-loop/internal/store"
)

// Plans in the harness (docs/phases/07-planning.md, "Harness"): driven through loopd's user and
// runner API. The runner side of a turn is a fake here (the hand runner claims the turn and
// reports a scripted result); the real runner's turns and #plan in Slack come with later pieces.

// PlanMatch checks a plan.
type PlanMatch struct {
	Stage string `json:"stage,omitempty"`
	// Items is how many items its hand-off filed.
	Items *int `json:"items,omitempty"`
	// Turn is whose turn it is: "you", "agent", or "none" (not in a conversation).
	Turn string `json:"turn,omitempty"`
	// Docs: kind -> the latest version (0: none).
	Docs map[string]int `json:"docs,omitempty"`
	// Turns: how many turns it has had, and the newest one's status and error (a regexp).
	Turns     *int   `json:"turns,omitempty"`
	LastTurn  string `json:"last_turn,omitempty"`
	LastError string `json:"last_error,omitempty"`
}

// HandPlanner is the agent id of the harness's fake plan-turn runner.
const HandPlanner = "hand-planner"

func (p *player) planRef(alias string) (int64, error) {
	id, ok := p.plans[alias]
	if !ok {
		return 0, fmt.Errorf("no plan %q in this scenario (start it first)", alias)
	}
	return id, nil
}

func (p *player) planStep(st Step) error {
	if st.Plan == "" {
		return fmt.Errorf("%s needs plan (an alias)", st.Do)
	}
	user := p.s.user
	if st.Do == "plan" {
		if _, dup := p.plans[st.Plan]; dup {
			return fmt.Errorf("plan %q started twice", st.Plan)
		}
		var pl pipeline.Plan
		if err := user.do("POST", "/plans", store.NewPlan{Title: st.Title, Priority: st.Priority, Text: st.Text, Repos: []string{RepoSlug}}, &pl); err != nil {
			return err
		}
		p.plans[st.Plan] = pl.ID
		return nil
	}
	id, err := p.planRef(st.Plan)
	if err != nil {
		return err
	}
	switch st.Do {
	case "plan_say":
		return user.do("POST", fmt.Sprintf("/plans/%d/messages", id), map[string]string{"text": st.Text}, nil)
	case "plan_drop":
		return user.do("POST", fmt.Sprintf("/plans/%d/drop", id), map[string]string{"words": st.Text}, nil)
	case "plan_turn":
		return p.planTurn(st, id)
	case "plan_approve":
		var doc pipeline.PlanDoc
		path := fmt.Sprintf("/plans/%d/docs/%s", id, st.Doc)
		if st.Version > 0 {
			path += fmt.Sprintf("?v=%d", st.Version)
		}
		if err := user.do("GET", path, nil, &doc); err != nil {
			return err
		}
		err := user.do("POST", fmt.Sprintf("/plans/%d/approve", id), map[string]string{"kind": st.Doc, "sha256": doc.SHA256, "words": st.Text}, nil)
		if st.Refused == "" {
			return err
		}
		if err == nil {
			return fmt.Errorf("approving %s v%d went through, want it refused (%s)", st.Doc, doc.Version, st.Refused)
		}
		if ok, merr := matches(st.Refused, err.Error()); merr != nil || !ok {
			return fmt.Errorf("refused with %v, want %q (%v)", err, st.Refused, merr)
		}
		return nil
	case "plan_items":
		var d store.PlanDetail
		if err := user.do("GET", fmt.Sprintf("/plans/%d", id), nil, &d); err != nil {
			return err
		}
		if len(d.Items) != len(st.Pieces) {
			return fmt.Errorf("plan %s filed %d items, want %d", st.Plan, len(d.Items), len(st.Pieces))
		}
		for i, alias := range st.Pieces {
			if _, dup := p.items[alias]; dup {
				return fmt.Errorf("item %q named twice", alias)
			}
			it := d.Items[i]
			if it.Piece != i+1 {
				return fmt.Errorf("plan %s's item #%d is piece %d, want %d", st.Plan, it.ID, it.Piece, i+1)
			}
			p.items[alias] = itemRef{id: it.ID, title: it.Title}
		}
		return nil
	}
	return fmt.Errorf("unknown plan step %q", st.Do)
}

// planTurn has the fake runner claim the plan's next turn and report the step's result.
func (p *player) planTurn(st Step, id int64) error {
	var c struct {
		Kind string            `json:"kind"`
		Turn pipeline.PlanTurn `json:"turn"`
	}
	if err := p.s.handAPI.do("POST", "/claim", map[string]any{"agent": HandPlanner, "kinds": []string{pipeline.WorkPlan}}, &c); err != nil {
		return err
	}
	if c.Kind != pipeline.WorkPlan {
		return fmt.Errorf("no plan turn to claim (want one of %s)", st.Plan)
	}
	if c.Turn.PlanID != id {
		return fmt.Errorf("claimed a turn of plan P%d, want %s (P%d)", c.Turn.PlanID, st.Plan, id)
	}
	reply := st.Text
	if reply == "" {
		reply = "Done."
	}
	var res struct {
		Turn pipeline.PlanTurn `json:"turn"`
	}
	r := pipeline.TurnResult{Reply: reply, Brief: st.Brief, Blueprint: st.Blueprint, Ready: st.Ready, BackToBrief: st.BackToBrief,
		SessionID: "fake-" + c.Turn.Persona}
	if err := p.s.handAPI.do("POST", fmt.Sprintf("/plans/%d/turns/%d/result", id, c.Turn.ID), r, &res); err != nil {
		return err
	}
	switch {
	case st.Fails == "" && res.Turn.Status != pipeline.TurnDone:
		return fmt.Errorf("turn %d is %s (%s), want done", res.Turn.ID, res.Turn.Status, res.Turn.Error)
	case st.Fails != "":
		if res.Turn.Status != pipeline.TurnFailed {
			return fmt.Errorf("turn %d is %s, want failed (%s)", res.Turn.ID, res.Turn.Status, st.Fails)
		}
		if ok, err := matches(st.Fails, res.Turn.Error); err != nil || !ok {
			return fmt.Errorf("turn %d failed with %q, want %q (%v)", res.Turn.ID, res.Turn.Error, st.Fails, err)
		}
	}
	return nil
}

func (p *player) planProblems(e Expect) []string {
	var out []string
	add := func(format string, args ...any) { out = append(out, fmt.Sprintf(format, args...)) }
	for _, alias := range sortedKeys(e.Plans) {
		m := e.Plans[alias]
		id, err := p.planRef(alias)
		if err != nil {
			add("%v", err)
			continue
		}
		d, err := p.s.Store.GetPlan(p.ctx, id)
		if err != nil {
			add("plan %s: %v", alias, err)
			continue
		}
		if m.Stage != "" && string(d.Plan.Stage) != m.Stage {
			add("plan %s is %s, want %s", alias, d.Plan.Stage, m.Stage)
		}
		if m.Items != nil && len(d.Items) != *m.Items {
			add("plan %s filed %d items, want %d", alias, len(d.Items), *m.Items)
		}
		if m.Turn != "" {
			got := d.Turn
			if got == "" {
				got = "none"
			}
			if got != m.Turn {
				add("plan %s: it is %s's turn, want %s's", alias, got, m.Turn)
			}
		}
		for _, kind := range sortedKeys(m.Docs) {
			v := 0
			for _, doc := range d.Docs {
				if doc.Kind == kind {
					v = max(v, doc.Version)
				}
			}
			if v != m.Docs[kind] {
				add("plan %s's latest %s is v%d, want v%d", alias, kind, v, m.Docs[kind])
			}
		}
		if m.Turns != nil && len(d.Turns) != *m.Turns {
			add("plan %s has had %d turns, want %d", alias, len(d.Turns), *m.Turns)
		}
		if m.LastTurn != "" || m.LastError != "" {
			if len(d.Turns) == 0 {
				add("plan %s has no turns", alias)
				continue
			}
			last := d.Turns[len(d.Turns)-1]
			if m.LastTurn != "" && string(last.Status) != m.LastTurn {
				add("plan %s's last turn is %s, want %s", alias, last.Status, m.LastTurn)
			}
			if m.LastError != "" {
				if ok, err := matches(m.LastError, last.Error); err != nil || !ok {
					add("plan %s's last turn error %q, want %q", alias, last.Error, m.LastError)
				}
			}
		}
	}
	for _, alias := range sortedKeys(e.DependsOn) {
		r, err := p.ref(alias)
		if err != nil {
			add("%v", err)
			continue
		}
		var want []int64
		for _, a := range e.DependsOn[alias] {
			dr, err := p.ref(a)
			if err != nil {
				add("%v", err)
				continue
			}
			want = append(want, dr.id)
		}
		d, err := p.s.Store.GetItem(p.ctx, r.id)
		if err != nil {
			add("%s: %v", alias, err)
			continue
		}
		got := slices.Clone(d.Item.DependsOn)
		slices.Sort(got)
		slices.Sort(want)
		if !slices.Equal(got, want) && !(len(got) == 0 && len(want) == 0) {
			add("%s depends on %s, want %s", alias, ids(got), ids(want))
		}
	}
	return out
}

func ids(xs []int64) string {
	if len(xs) == 0 {
		return "nothing"
	}
	var parts []string
	for _, x := range xs {
		parts = append(parts, fmt.Sprintf("#%d", x))
	}
	return strings.Join(parts, ", ")
}
