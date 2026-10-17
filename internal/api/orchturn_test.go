package api

import (
	"strconv"
	"testing"

	"github.com/alexberardi/jarvis-loop/internal/pipeline"
	"github.com/alexberardi/jarvis-loop/internal/store"
)

func TestOrchestrateTurnRoutes(t *testing.T) {
	e := newEnv(t)
	id := e.addItem("x")
	e.failed(id)
	// Only a runner token claims it, and only when it asks for the kind.
	var cl struct {
		Kind  string          `json:"kind"`
		Stage string          `json:"stage"`
		Orch  *store.OrchTurn `json:"orchestrate"`
		Lease struct {
			ID string `json:"id"`
		} `json:"lease"`
	}
	e.refused(runnerTok, "POST", "/claim", map[string]any{"agent": "o", "kinds": []string{"orchestrator"}}, 400, pipeline.CodeInvalidField)
	e.refused(orchTok, "POST", "/claim", map[string]any{"agent": "o", "kinds": []string{"orchestrate"}}, 403, "forbidden")
	e.do(runnerTok, "POST", "/claim", map[string]any{"agent": "o", "kinds": []string{"orchestrate"}}, 200, &cl)
	if cl.Kind != pipeline.WorkOrchestrate || cl.Stage != "orchestrate" || cl.Orch == nil || cl.Lease.ID == "" ||
		len(cl.Orch.Triggers) != 1 || cl.Orch.Triggers[0].ItemID != id {
		t.Fatalf("claim: %+v", cl)
	}
	e.do(runnerTok, "POST", "/leases/"+cl.Lease.ID+"/heartbeat", map[string]any{"note": "agent running"}, 200, nil)

	var snap store.OrchSnapshot
	e.do(orchTok, "GET", "/orchestrator/snapshot", nil, 200, &snap)
	if snap.Turn == nil || snap.Turn.ID != cl.Orch.ID || len(snap.Items) != 1 || len(snap.Triggered) != 1 ||
		snap.Triggered[0].Item.ID != id || snap.TurnsToday != 1 || snap.TurnsPerDay != pipeline.DefaultOrchTurnsPerDay {
		t.Fatalf("snapshot: %+v", snap)
	}

	path := "/orchestrate/turns/" + strconv.FormatInt(cl.Orch.ID, 10) + "/result"
	res := map[string]any{"session_id": "sess", "actions": []map[string]any{{"action": "retry", "item_id": id, "ok": false, "status": 429, "code": "too_soon"}}}
	e.refused(orchTok, "POST", path, res, 403, "forbidden")
	e.refused(otherTok, "POST", path, res, 403, store.CodeNotYourLease)
	var out struct {
		Turn store.OrchTurn `json:"turn"`
	}
	e.do(runnerTok, "POST", path, res, 200, &out)
	if out.Turn.Status != store.OrchDone || out.Turn.Session != "sess" || len(out.Turn.Actions) != 1 {
		t.Fatalf("result: %+v", out.Turn)
	}
	snap = store.OrchSnapshot{}
	e.do(orchTok, "GET", "/orchestrator/snapshot", nil, 200, &snap)
	if snap.Turn != nil || snap.LastTurn == nil || snap.LastTurn.Actions[0].Code != "too_soon" {
		t.Fatalf("the next snapshot shows the last turn's refusal: %+v", snap.LastTurn)
	}
	// The cap is the user's: not on behalf of them.
	e.do(userTok, "PUT", "/control", map[string]any{"orchestrator_turns_per_day": 10}, 200, nil)
	e.refused(userTok, "PUT", "/control", map[string]any{"orchestrator_turns_per_day": -1}, 400, pipeline.CodeInvalidField)
}
