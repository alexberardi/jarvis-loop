package api

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/alexberardi/jarvis-loop/internal/slack"
	"github.com/alexberardi/jarvis-loop/internal/store"
)

// doAs is do with an X-Loop-On-Behalf header.
func (e *env) doAs(tok, onBehalf, method, path string, body any, wantStatus int) apiErr {
	e.t.Helper()
	b, _ := json.Marshal(body)
	return e.doRawAs(tok, onBehalf, method, path, string(b), wantStatus)
}

// doRawAs is doAs with the body sent as is.
func (e *env) doRawAs(tok, onBehalf, method, path, body string, wantStatus int) apiErr {
	e.t.Helper()
	req, _ := http.NewRequest(method, e.srv.URL+path, strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+tok)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set(OnBehalfHeader, onBehalf)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		e.t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != wantStatus {
		e.t.Fatalf("%s %s as %s: status %d, want %d: %s", method, path, onBehalf, resp.StatusCode, wantStatus, raw)
	}
	var ae apiErr
	json.Unmarshal(raw, &ae)
	return ae
}

func (e *env) newest(id int64) (kind, actor string, data map[string]any) {
	e.t.Helper()
	d := e.get(id)
	ev := d.Events[len(d.Events)-1]
	return ev.Kind, ev.Actor, ev.Data
}

func (e *env) failed(id int64) {
	e.t.Helper()
	e.mustClaim("t", id, "triage")
	e.advance(id, "failed", map[string]string{"error": "fatal: index.lock exists"}, 200)
}

const orchActor = "orchestrator:loop"

// The orchestrator never approves, drops, OKs, changes config or works a lease: every user and
// runner write route refuses its token, and a refusal on an item is recorded on the item.
func TestOrchestratorRefusedRoutes(t *testing.T) {
	e := newEnv(t)
	id := e.addItem("x")
	for _, r := range []struct{ method, path string }{
		{"POST", "/items"}, {"PATCH", "/items/1"}, {"POST", "/claim"}, {"POST", "/leases/x/heartbeat"},
		{"POST", "/items/1/advance"}, {"POST", "/items/1/evidence"}, {"POST", "/items/1/events"},
		{"POST", "/items/1/answer"}, {"POST", "/items/1/approve"}, {"POST", "/items/1/hold"},
		{"POST", "/items/1/rebuild"}, {"POST", "/items/1/drop"}, {"POST", "/items/1/qa"},
		{"POST", "/items/1/verified"}, {"POST", "/items/1/reopen"}, {"POST", "/plans"},
		{"POST", "/plans/1/messages"}, {"POST", "/plans/1/approve"}, {"POST", "/plans/1/drop"},
		{"POST", "/plans/1/turns/1/result"}, {"PUT", "/control"},
	} {
		e.refused(orchTok, r.method, r.path, map[string]any{}, 403, "forbidden")
	}
	refused := map[string]bool{}
	for _, ev := range e.get(id).Events {
		if ev.Kind == "refused" {
			if ev.Actor != orchActor || ev.Data["code"] != "forbidden" {
				t.Fatalf("refused event %+v", ev)
			}
			refused[ev.Data["action"].(string)] = true
		}
	}
	for _, a := range []string{"approve", "drop", "patch", "answer", "hold", "advance"} {
		if !refused[a] {
			t.Fatalf("no refused event for %s: %v", a, refused)
		}
	}
	// It reads; the others do not use its routes.
	e.do(orchTok, "GET", "/items", nil, 200, nil)
	e.do(orchTok, "GET", fmt.Sprintf("/items/%d", id), nil, 200, nil)
	e.do(orchTok, "GET", "/control", nil, 200, nil)
	for _, tok := range []string{userTok, runnerTok} {
		e.refused(tok, "POST", "/orchestrator/items/1/retry", map[string]any{"reason": "r"}, 403, "forbidden")
		e.refused(tok, "POST", "/orchestrator/agents", map[string]any{"active_agents": 0, "reason": "r"}, 403, "forbidden")
	}
}

// Every action refuses a missing reason, and records the reason when it acts.
func TestOrchestratorActionsNeedAndRecordReasons(t *testing.T) {
	e := newEnv(t)
	a, b := e.addItem("a"), e.addItem("b")
	e.failed(a)
	e.clock.Advance(2 * time.Minute)
	actions := []struct {
		method, path string
		body         map[string]any
		status       int
		item         int64
		kind         string
	}{
		{"POST", fmt.Sprintf("/orchestrator/items/%d/retry", a), map[string]any{}, 200, a, "retried"},
		{"POST", fmt.Sprintf("/orchestrator/items/%d/priority", b), map[string]any{"priority": "P1"}, 200, b, "patched"},
		{"POST", fmt.Sprintf("/orchestrator/items/%d/link", b), map[string]any{"after": a}, 200, b, "linked"},
		{"POST", "/orchestrator/escalations", map[string]any{"item_id": b, "text": "product call", "recommendation": "do A"}, 201, b, "orchestrator.escalated"},
		{"POST", "/orchestrator/items", map[string]any{"title": "git lock", "repo": "jarvis-server", "priority": "P1", "signature": "index.lock exists"}, 201, 0, ""},
		{"POST", "/orchestrator/agents", map[string]any{"active_agents": 1}, 200, 0, ""},
		{"POST", "/orchestrator/notes", map[string]any{"text": "watching #1"}, 201, 0, ""},
	}
	for _, x := range actions {
		e.refused(orchTok, x.method, x.path, x.body, 400, "missing_field")
		x.body["reason"] = "because " + x.path
		var out map[string]any
		e.do(orchTok, x.method, x.path, x.body, x.status, &out)
		if x.item == 0 {
			continue
		}
		kind, actor, data := e.newest(x.item)
		if kind != x.kind || actor != orchActor || data["reason"] != "because "+x.path {
			t.Fatalf("%s: event %s by %s %v", x.path, kind, actor, data)
		}
	}
	var nb struct {
		Notes []struct {
			Actor string         `json:"actor"`
			Data  map[string]any `json:"data"`
		} `json:"notes"`
	}
	e.do(userTok, "GET", "/orchestrator/notes", nil, 200, &nb)
	if len(nb.Notes) != 1 || nb.Notes[0].Data["text"] != "watching #1" || nb.Notes[0].Actor != orchActor {
		t.Fatalf("notebook %+v", nb)
	}
	var c struct {
		ActiveAgents int `json:"active_agents"`
	}
	e.do(userTok, "GET", "/control", nil, 200, &c)
	if c.ActiveAgents != 1 {
		t.Fatalf("active agents %d", c.ActiveAgents)
	}
	e.refused(orchTok, "POST", "/orchestrator/agents", map[string]any{"active_agents": 2, "reason": "r"}, 409, "limit")
	// The same signature again is the open item, not a new one.
	var dup struct {
		Duplicate bool `json:"duplicate"`
		Item      struct{ ID int64 }
	}
	e.do(orchTok, "POST", "/orchestrator/items", map[string]any{"title": "git lock again", "signature": "Index.lock exists ", "reason": "r"}, 200, &dup)
	if !dup.Duplicate || dup.Item.ID != 3 {
		t.Fatalf("dedupe %+v", dup)
	}
}

func TestOrchestratorAnswerAndRetryLimits(t *testing.T) {
	e := newEnv(t)
	id := e.addItem("x")
	e.failed(id)
	path := fmt.Sprintf("/orchestrator/items/%d/", id)
	e.refused(orchTok, "POST", path+"answer", map[string]any{"kind": "loop", "text": "go on", "reason": "r"}, 409, "limit")
	e.refused(orchTok, "POST", path+"retry", map[string]any{"reason": "git lock"}, 429, "too_soon")
	e.clock.Advance(2 * time.Minute)
	e.do(orchTok, "POST", path+"retry", map[string]any{"reason": "git lock"}, 200, nil)
	e.mustClaim("t", id, "triage")
	e.advance(id, "question", map[string]string{"question": "no emulator: skip the shots?"}, 200)
	e.refused(orchTok, "POST", path+"answer", map[string]any{"kind": "product", "text": "ship it", "reason": "r"}, 400, "invalid_field")
	e.do(orchTok, "POST", path+"answer", map[string]any{"kind": "harness", "text": "list it under not verified", "reason": "standing answer"}, 200, nil)
	kind, actor, data := e.newest(id)
	if kind != "answered" || actor != orchActor || data["answer_kind"] != "harness" {
		t.Fatalf("answer event %s %s %v", kind, actor, data)
	}
}

// The approver's message makes the orchestrator act as them (via the orchestrator); approvals stay
// on their buttons; forged, someone else's, reused and old messages are refused.
func TestActForUser(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	a, b := e.addItem("a"), e.addItem("b")
	if err := e.store.RecordSlackMessage(ctx, "C0LOOP", "100.1", slack.Approver, "drop #1 and #2"); err != nil {
		t.Fatal(err)
	}
	if err := e.store.RecordSlackMessage(ctx, "C0LOOP", "100.2", "U0SOMEONE", "drop #1"); err != nil {
		t.Fatal(err)
	}
	drop := func(id int64) string { return fmt.Sprintf("/items/%d/drop", id) }

	if ae := e.doAs(orchTok, "C0LOOP/100.9", "POST", drop(a), map[string]any{}, 403); ae.Code != "not_approver" {
		t.Fatalf("forged: %+v", ae)
	}
	if ae := e.doAs(orchTok, "C0LOOP/100.2", "POST", drop(a), map[string]any{}, 403); ae.Code != "not_approver" {
		t.Fatalf("someone else's: %+v", ae)
	}
	if ae := e.doAs(userTok, "C0LOOP/100.1", "POST", drop(a), map[string]any{}, 403); ae.Code != "forbidden" {
		t.Fatalf("a user token naming a message: %+v", ae)
	}
	if ae := e.doAs(orchTok, "100.1", "POST", drop(a), map[string]any{}, 400); ae.Code != "invalid_field" {
		t.Fatalf("malformed: %+v", ae)
	}
	if ae := e.doAs(orchTok, "C0LOOP/100.1", "POST", fmt.Sprintf("/items/%d/approve", a), map[string]any{"head_sha": shaA}, 403); ae.Code != "not_on_behalf" {
		t.Fatalf("approve for the user: %+v", ae)
	}
	if ae := e.doAs(orchTok, "C0LOOP/100.1", "POST", "/plans/1/approve", map[string]any{}, 403); ae.Code != "not_on_behalf" {
		t.Fatalf("plan approve for the user: %+v", ae)
	}
	kind, actor, data := e.newest(a)
	if kind != "refused" || actor != orchActor || data["code"] != "not_on_behalf" || data["action"] != "approve" {
		t.Fatalf("refused approve: %s %s %v", kind, actor, data)
	}

	e.doAs(orchTok, "C0LOOP/100.1", "POST", drop(a), map[string]any{"words": "drop #1 and #2"}, 200)
	e.doAs(orchTok, "C0LOOP/100.1", "POST", drop(b), map[string]any{"words": "drop #1 and #2"}, 200)
	for _, id := range []int64{a, b} {
		d := e.get(id)
		ev := d.Events[len(d.Events)-1]
		if d.Item.Stage != "closed" || ev.Kind != "dropped" || ev.Actor != "slack:"+slack.Approver || ev.Data["via"] != orchActor {
			t.Fatalf("#%d: %s %+v", id, d.Item.Stage, ev)
		}
	}
	if ae := e.doAs(orchTok, "C0LOOP/100.1", "POST", drop(a), map[string]any{}, 409); ae.Code != "message_used" {
		t.Fatalf("reused: %+v", ae)
	}

	// For the user, PUT /control only pauses the loop: the hold rules, limits and run time stay theirs.
	// The check is on the patch the handler decodes, so a body the decoder reads leniently (a
	// trailing } or ]) is refused too. A refused call spends its message, so each gets its own.
	before, err := e.store.Control(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for i, body := range []string{
		`{"hold_rules": [], "max_run_minutes": 600}`,
		`{"stopped": true, "hold_rules": []}`,
		`{"limits": {"bounces": 9}}`,
		`{"max_run_minutes": 600}`,
		`{"hold_rules":[]}}`,
		`{"hold_rules":[],"max_run_minutes":600}]`,
		`{"orchestrator_turns_per_day": 2000}`,
		`{"stopped": true, "orchestrator_turns_per_day": 2000}`,
		`{"runway_warn_hours": 0}`,
		`{"stopped": true, "runway_warn_hours": 0}`,
	} {
		ts := "100.3" + strconv.Itoa(i)
		if err := e.store.RecordSlackMessage(ctx, "C0LOOP", ts, slack.Approver, "what's up with #3?"); err != nil {
			t.Fatal(err)
		}
		if ae := e.doRawAs(orchTok, "C0LOOP/"+ts, "PUT", "/control", body, 403); ae.Code != "not_on_behalf" {
			t.Fatalf("control %s for the user: %+v", body, ae)
		}
	}
	after, err := e.store.Control(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(after.HoldRules) == 0 || len(after.HoldRules) != len(before.HoldRules) || after.MaxRunMinutes != before.MaxRunMinutes || after.Stopped ||
		after.OrchTurnsPerDay != before.OrchTurnsPerDay || after.RunwayWarnHours != before.RunwayWarnHours {
		t.Fatalf("control changed: %+v -> %+v", before, after)
	}
	if err := e.store.RecordSlackMessage(ctx, "C0LOOP", "100.3", slack.Approver, "pause the loop"); err != nil {
		t.Fatal(err)
	}
	evs, err := e.store.EventsBetween(ctx, time.Time{}, e.clock.Now().Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	last := evs[len(evs)-1]
	if last.Kind != "refused" || last.Actor != orchActor || last.ItemID != 0 || last.Data["code"] != "not_on_behalf" || last.Data["action"] != "control" {
		t.Fatalf("refused control: %+v", last)
	}
	e.doAs(orchTok, "C0LOOP/100.3", "PUT", "/control", map[string]any{"stopped": true}, 200)
	evs, err = e.store.EventsBetween(ctx, time.Time{}, e.clock.Now().Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	last = evs[len(evs)-1]
	if last.Kind != "control" || last.Actor != "slack:"+slack.Approver || last.Data["via"] != orchActor {
		t.Fatalf("pause for the user: %+v", last)
	}
	if c, _ := e.store.Control(ctx); !c.Stopped || len(c.HoldRules) != len(before.HoldRules) {
		t.Fatalf("after pause: %+v", c)
	}

	e.clock.Advance(11 * time.Minute)
	if ae := e.doAs(orchTok, "C0LOOP/100.1", "PUT", "/control", map[string]any{"stopped": true}, 403); ae.Code != "not_approver" {
		t.Fatalf("old message: %+v", ae)
	}
}

func TestOrchestratorTokenKind(t *testing.T) {
	toks, err := ParseTokens(bytes.NewReader([]byte(orchTok + "\torchestrator\tloop\n")))
	if err != nil {
		t.Fatal(err)
	}
	if p, ok := toks.Lookup(orchTok); !ok || p.Kind != KindOrchestrator || p.Actor() != orchActor {
		t.Fatalf("%+v %v", p, ok)
	}
}

// Piece 2's hand-off (docs/decisions/2026-10-17-orchestrator-slack.md): the messages people sent
// the orchestrator, its replies, and the approval message it has posted instead of approving.
func TestOrchestratorSlackRoutes(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	for _, ts := range []string{"1.1", "1.2"} {
		if _, _, err := e.store.OrchMessage(ctx, store.OrchestratorMessage{Channel: "D1", TS: ts, User: slack.Approver, Text: "what's stuck?", Approver: true}); err != nil {
			t.Fatal(err)
		}
	}
	var got struct {
		Messages []store.Event `json:"messages"`
	}
	e.do(orchTok, "GET", "/orchestrator/messages", nil, 200, &got)
	if len(got.Messages) != 2 || got.Messages[0].Data["text"] != "what's stuck?" {
		t.Fatalf("messages %+v", got.Messages)
	}
	e.do(orchTok, "GET", fmt.Sprintf("/orchestrator/messages?since=%d", got.Messages[0].ID), nil, 200, &got)
	if len(got.Messages) != 1 {
		t.Fatalf("since: %+v", got.Messages)
	}
	e.refused(orchTok, "GET", "/orchestrator/messages?since=x", nil, 400, "bad_request")

	var ev store.Event
	e.do(orchTok, "POST", "/orchestrator/replies", map[string]any{"channel": "D1", "thread_ts": "1.1", "text": "nothing is stuck", "reason": "you asked"}, 201, &ev)
	if ev.Kind != store.KindOrchReply || ev.Data["text"] != "nothing is stuck" || ev.Data["reason"] != "you asked" {
		t.Fatalf("reply %+v", ev)
	}
	e.do(orchTok, "POST", "/orchestrator/replies", map[string]any{"channel": slack.ChannelLoop, "thread_ts": "1.1", "text": "ok"}, 201, nil)
	e.refused(orchTok, "POST", "/orchestrator/replies", map[string]any{"channel": "C0GENERAL", "text": "hi"}, 400, "bad_request")
	e.refused(userTok, "POST", "/orchestrator/replies", map[string]any{"channel": "D1", "text": "hi"}, 403, "forbidden")

	id := e.addItem("x")
	e.refused(orchTok, "POST", fmt.Sprintf("/orchestrator/items/%d/approval", id), map[string]any{"reason": "asked to approve"}, 409, "wrong_stage")

	// Waiting on a trusted-file OK, the item's trust message is posted again; nothing is trusted.
	e.mustClaim("triager", id, "triage")
	e.upload(runnerTok, id, "repro.log", []byte("x\n"), 201)
	e.advance(id, "reproduced", map[string]string{"area": "harness", "size": "S", "risk": "normal", "resources": ""}, 200)
	e.mustClaim("builder", id, "build")
	e.advance(id, "failed", map[string]string{"error": "the change touches tools/pipeline", "trust_sha": shaA,
		"trusted_paths": "tools/pipeline", "trusted_diffstat": "tools/pipeline | 2 +-"}, 200)
	e.do(orchTok, "POST", fmt.Sprintf("/orchestrator/items/%d/approval", id), map[string]any{"reason": "asked to OK the trusted files"}, 201, &ev)
	if ev.Kind != store.KindOrchApprovalAsked || ev.Data["trust"] != true || ev.Data["trust_sha"] != shaA {
		t.Fatalf("trust asked %+v", ev)
	}
	if d := e.get(id); d.Item.Stage != "needs_you" {
		t.Fatalf("trusted: %+v", d.Item)
	}
}
