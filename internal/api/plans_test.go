package api

import (
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/alexberardi/jarvis-loop/internal/pipeline"
	"github.com/alexberardi/jarvis-loop/internal/store"
)

const planBlueprint = "## Pieces\n### 1. Tables\n### 2. Store\n- depends on: 1\n"

type turnClaim struct {
	Kind  string            `json:"kind"`
	Plan  pipeline.Plan     `json:"plan"`
	Turn  pipeline.PlanTurn `json:"turn"`
	Lease struct {
		ID    string `json:"id"`
		Stage string `json:"stage"`
	} `json:"lease"`
}

func (e *env) claimTurn() turnClaim {
	e.t.Helper()
	var c turnClaim
	e.do(runnerTok, "POST", "/claim", map[string]any{"agent": "a1", "kinds": []string{"item", "plan"}}, http.StatusOK, &c)
	if c.Kind != "plan" || c.Lease.ID == "" || c.Lease.Stage != "plan" {
		e.t.Fatalf("claim %+v", c)
	}
	return c
}

func TestPlanRoutes(t *testing.T) {
	e := newEnv(t)
	e.store.SetRepos([]string{"alexberardi/jarvis-loop"})

	// Starting a plan is the user's; reading is both.
	e.refused(runnerTok, "POST", "/plans", map[string]any{"title": "x"}, http.StatusForbidden, "forbidden")
	e.refused(userTok, "POST", "/plans", map[string]any{"title": " "}, http.StatusBadRequest, pipeline.CodeMissingField)
	e.refused(userTok, "POST", "/plans", map[string]any{"title": "x", "nope": 1}, http.StatusBadRequest, "bad_request")
	var p pipeline.Plan
	e.do(userTok, "POST", "/plans", map[string]any{"title": "Pantry", "priority": "P2", "text": "a pantry list",
		"repos": []string{"alexberardi/jarvis-loop"}, "slack_channel": "C1", "slack_ts": "1.2"}, http.StatusCreated, &p)
	if p.ID != 1 || p.Stage != pipeline.PlanBrief || p.SlackTS != "1.2" {
		t.Fatalf("plan %+v", p)
	}
	var list struct{ Plans []store.PlanRow }
	e.do(runnerTok, "GET", "/plans?open=1", nil, http.StatusOK, &list)
	if len(list.Plans) != 1 || list.Plans[0].Turn != "agent" {
		t.Fatalf("plans %+v", list)
	}
	e.refused(userTok, "GET", "/plans/x", nil, http.StatusBadRequest, "bad_request")
	e.refused(userTok, "GET", "/plans/9", nil, http.StatusNotFound, store.CodeNotFound)

	// An item-only claim (no kinds) never gets the turn; bad kinds are refused.
	e.do(runnerTok, "POST", "/claim", map[string]any{"agent": "a1"}, http.StatusNoContent, nil)
	e.refused(runnerTok, "POST", "/claim", map[string]any{"agent": "a1", "kinds": []string{"chat"}}, http.StatusBadRequest, pipeline.CodeInvalidField)
	c := e.claimTurn()
	if c.Turn.Persona != "product" || len(c.Turn.Messages) != 1 || c.Turn.Messages[0] != "a pantry list" {
		t.Fatalf("turn %+v", c.Turn)
	}
	e.do(runnerTok, "POST", "/leases/"+c.Lease.ID+"/heartbeat", map[string]any{"note": "thinking"}, http.StatusOK, nil)
	e.refused(userTok, "POST", "/plans/1/messages", map[string]any{"text": " "}, http.StatusBadRequest, pipeline.CodeMissingField)
	e.refused(runnerTok, "POST", "/plans/1/messages", map[string]any{"text": "x"}, http.StatusForbidden, "forbidden")
	var queued pipeline.PlanTurn
	e.do(userTok, "POST", "/plans/P1/messages", map[string]any{"text": "and the fridge"}, http.StatusOK, &queued)
	if queued.Status != pipeline.TurnQueued || queued.ID == c.Turn.ID {
		t.Fatalf("queued %+v", queued)
	}

	// Results are the runner's, for the turn it holds.
	path := fmt.Sprintf("/plans/1/turns/%d/result", c.Turn.ID)
	e.refused(userTok, "POST", path, map[string]any{"reply": "x"}, http.StatusForbidden, "forbidden")
	e.refused(otherTok, "POST", path, map[string]any{"reply": "x"}, http.StatusForbidden, store.CodeNotYourLease)
	e.refused(runnerTok, "POST", fmt.Sprintf("/plans/1/turns/%d/result", queued.ID), map[string]any{"reply": "x"}, http.StatusConflict, store.CodeNoLease)
	var res struct {
		Plan pipeline.Plan     `json:"plan"`
		Turn pipeline.PlanTurn `json:"turn"`
	}
	e.do(runnerTok, "POST", path, map[string]any{"reply": "Here's a brief.", "brief": "v1", "ready": true, "session_id": "s1"}, http.StatusOK, &res)
	if res.Turn.Status != pipeline.TurnDone || res.Plan.ProductSession != "s1" {
		t.Fatalf("result %+v", res)
	}
	c = e.claimTurn()
	e.do(runnerTok, "POST", fmt.Sprintf("/plans/1/turns/%d/result", c.Turn.ID), map[string]any{"reply": "Updated.", "brief": "v2", "ready": true}, http.StatusOK, nil)

	var d pipeline.PlanDoc
	e.do(runnerTok, "GET", "/plans/1/docs/brief?v=1", nil, http.StatusOK, &d)
	if d.Version != 1 || d.Markdown != "v1" || d.SHA256 != pipeline.DocSHA256("v1") {
		t.Fatalf("doc %+v", d)
	}
	e.do(userTok, "GET", "/plans/1/docs/brief", nil, http.StatusOK, &d)
	if d.Version != 2 {
		t.Fatalf("latest %+v", d)
	}
	e.refused(userTok, "GET", "/plans/1/docs/blueprint", nil, http.StatusNotFound, store.CodeNotFound)
	e.refused(userTok, "GET", "/plans/1/docs/memo", nil, http.StatusBadRequest, pipeline.CodeInvalidField)

	// The stale button: refused with the newer version.
	var ae apiErr
	e.do(userTok, "POST", "/plans/1/approve", map[string]any{"kind": "brief", "sha256": pipeline.DocSHA256("v1")}, http.StatusConflict, &ae)
	if ae.Code != pipeline.CodeSHAMismatch || !strings.Contains(ae.Error, "approve v2 instead") {
		t.Fatalf("stale approve: %+v", ae)
	}
	e.refused(runnerTok, "POST", "/plans/1/approve", map[string]any{"kind": "brief", "sha256": d.SHA256}, http.StatusForbidden, "forbidden")
	var a store.PlanApproved
	e.do(userTok, "POST", "/plans/1/approve", map[string]any{"kind": "brief", "sha256": d.SHA256, "words": "yes"}, http.StatusOK, &a)
	if a.Plan.Stage != pipeline.PlanBlueprint {
		t.Fatalf("approved %+v", a)
	}

	c = e.claimTurn()
	if c.Turn.Persona != "engineering" {
		t.Fatalf("turn %+v", c.Turn)
	}
	e.do(runnerTok, "POST", fmt.Sprintf("/plans/1/turns/%d/result", c.Turn.ID), map[string]any{"reply": "Blueprint.", "blueprint": planBlueprint, "ready": true}, http.StatusOK, nil)
	e.do(userTok, "POST", "/plans/1/approve", map[string]any{"kind": "blueprint", "sha256": pipeline.DocSHA256(planBlueprint)}, http.StatusOK, &a)
	if a.Plan.Stage != pipeline.PlanSplit || len(a.Items) != 2 || a.Items[1].DependsOn[0] != a.Items[0].ID {
		t.Fatalf("hand-off %+v", a)
	}
	var detail store.PlanDetail
	e.do(runnerTok, "GET", "/plans/1", nil, http.StatusOK, &detail)
	if len(detail.Items) != 2 || len(detail.Turns) != 3 || len(detail.Docs) != 3 || detail.Docs[0].Markdown != "" {
		t.Fatalf("detail %+v", detail)
	}

	e.refused(runnerTok, "POST", "/plans/1/drop", nil, http.StatusForbidden, "forbidden")
	var dropped pipeline.Plan
	e.do(userTok, "POST", "/plans/1/drop", map[string]any{"words": "never mind"}, http.StatusOK, &dropped)
	if dropped.Stage != pipeline.PlanDropped {
		t.Fatalf("dropped %+v", dropped)
	}
	e.refused(userTok, "POST", "/plans/1/drop", nil, http.StatusConflict, pipeline.CodeWrongStage)
}
