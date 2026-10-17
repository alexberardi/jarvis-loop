package api

import (
	"context"
	"strings"
	"testing"

	"github.com/alexberardi/jarvis-loop/internal/pipeline"
)

func TestHeartbeatActivity(t *testing.T) {
	e := newEnv(t)
	id := e.addItem("x")
	c := e.mustClaim("a", id, "triage")
	path := "/leases/" + c.Lease.ID + "/heartbeat"
	act := map[string]any{"phase": "triage agent", "elapsed_s": 12, "tool_calls": 4,
		"todos":  []map[string]string{{"text": "read <the> code", "status": "in_progress"}},
		"recent": []string{"Bash: run the tests"}, "message": "Looking."}
	e.do(runnerTok, "POST", path, map[string]any{"note": "agent running", "activity": act}, 200, nil)
	d, _ := e.store.GetItem(context.Background(), id)
	if d.Lease == nil || d.Lease.Activity == nil || d.Lease.Activity.Todos[0].Text != "read <the> code" || d.Lease.Activity.ToolCalls != 4 {
		t.Fatalf("lease: %+v", d.Lease)
	}

	// Another runner's lease, or a token that is not a runner's: refused, as the note is.
	e.refused(otherTok, "POST", path, map[string]any{"activity": act}, 403, "not_your_lease")
	e.refused(userTok, "POST", path, map[string]any{"activity": act}, 403, "forbidden")

	// Past the limits: refused, and the stored activity stays as it was.
	long := strings.Repeat("x", pipeline.MaxActivityMessage+1)
	many := make([]map[string]string, pipeline.MaxActivityTodos+1)
	for i := range many {
		many[i] = map[string]string{"text": "t", "status": "pending"}
	}
	for name, bad := range map[string]map[string]any{
		"message":     {"phase": "x", "message": long},
		"todo text":   {"phase": "x", "todos": []map[string]string{{"text": strings.Repeat("é", pipeline.MaxActivityTodo+1), "status": "pending"}}},
		"todo status": {"phase": "x", "todos": []map[string]string{{"text": "t", "status": "done"}}},
		"todos":       {"phase": "x", "todos": many},
		"recent":      {"phase": "x", "recent": []string{"a", "b", "c", "d", "e", "f"}},
		"phase":       {"phase": strings.Repeat("p", pipeline.MaxActivityName+1)},
		"negative":    {"phase": "x", "elapsed_s": -1},
		"unknown":     {"phase": "x", "command": "rm -rf /"},
	} {
		t.Run(name, func(t *testing.T) {
			e.refused(runnerTok, "POST", path, map[string]any{"activity": bad}, 400, "bad_request")
		})
	}
	if d, _ := e.store.GetItem(context.Background(), id); d.Lease.Activity == nil || d.Lease.Activity.Message != "Looking." {
		t.Fatalf("a refused activity replaced the stored one: %+v", d.Lease.Activity)
	}
}
