package api

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/alexberardi/jarvis-loop/internal/clock"
	"github.com/alexberardi/jarvis-loop/internal/pipeline"
	"github.com/alexberardi/jarvis-loop/internal/store"
)

const (
	userTok   = "user-token-0123456789abcdef"
	runnerTok = "runner-token-0123456789abcdef"
	otherTok  = "other-runner-token-0123456789"
	orchTok   = "orchestrator-token-0123456789"
	shaA      = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	shaB      = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	shaM      = "cccccccccccccccccccccccccccccccccccccccc"
)

var t0 = time.Date(2026, 10, 14, 12, 0, 0, 0, time.UTC)

type env struct {
	t     *testing.T
	srv   *httptest.Server
	clock *clock.Fake
	store *store.Store
	logs  *bytes.Buffer
}

func newEnv(t *testing.T) *env {
	t.Helper()
	clk := clock.NewFake(t0)
	st, err := store.Open(t.TempDir(), clk)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	toks, err := ParseTokens(strings.NewReader(fmt.Sprintf("# comment\n%s\tuser\talex\n%s\trunner\tdevbox\n\n%s\trunner\tmac\n%s\torchestrator\tloop\n", userTok, runnerTok, otherTok, orchTok)))
	if err != nil {
		t.Fatal(err)
	}
	logs := &bytes.Buffer{}
	srv := httptest.NewServer(New(st, toks, slog.New(slog.NewJSONHandler(logs, nil))))
	t.Cleanup(srv.Close)
	return &env{t: t, srv: srv, clock: clk, store: st, logs: logs}
}

// do sends a JSON request and decodes a JSON answer into out (when not nil).
func (e *env) do(tok, method, path string, body any, wantStatus int, out any) {
	e.t.Helper()
	var rd io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rd = bytes.NewReader(b)
	}
	req, _ := http.NewRequest(method, e.srv.URL+path, rd)
	if tok != "" {
		req.Header.Set("Authorization", "Bearer "+tok)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		e.t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != wantStatus {
		e.t.Fatalf("%s %s: status %d, want %d: %s", method, path, resp.StatusCode, wantStatus, raw)
	}
	if out != nil {
		if err := json.Unmarshal(raw, out); err != nil {
			e.t.Fatalf("%s %s: decode %s: %v", method, path, raw, err)
		}
	}
}

type apiErr struct {
	Error string `json:"error"`
	Code  string `json:"code"`
}

func (e *env) refused(tok, method, path string, body any, status int, code string) {
	e.t.Helper()
	var ae apiErr
	e.do(tok, method, path, body, status, &ae)
	if ae.Code != code || ae.Error == "" {
		e.t.Fatalf("%s %s: error %+v, want code %q", method, path, ae, code)
	}
}

type claimResp struct {
	Item  struct{ ID int64 } `json:"item"`
	Stage string             `json:"stage"`
	Lease struct {
		ID        string    `json:"id"`
		ExpiresAt time.Time `json:"expires_at"`
	} `json:"lease"`
	Brief struct {
		Stage string `json:"stage"`
	} `json:"brief"`
}

func (e *env) claim(tok, agent string, caps ...string) *claimResp {
	e.t.Helper()
	b, _ := json.Marshal(map[string]any{"agent": agent, "capabilities": caps})
	req, _ := http.NewRequest("POST", e.srv.URL+"/claim", bytes.NewReader(b))
	req.Header.Set("Authorization", "Bearer "+tok)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		e.t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNoContent {
		return nil
	}
	if resp.StatusCode != 200 {
		raw, _ := io.ReadAll(resp.Body)
		e.t.Fatalf("claim: %d %s", resp.StatusCode, raw)
	}
	var c claimResp
	if err := json.NewDecoder(resp.Body).Decode(&c); err != nil {
		e.t.Fatal(err)
	}
	return &c
}

func (e *env) mustClaim(agent string, wantItem int64, wantStage string, caps ...string) *claimResp {
	e.t.Helper()
	c := e.claim(runnerTok, agent, caps...)
	if c == nil || c.Item.ID != wantItem || c.Stage != wantStage || c.Brief.Stage != wantStage || c.Lease.ID == "" {
		e.t.Fatalf("claim by %s: %+v, want item %d at %s", agent, c, wantItem, wantStage)
	}
	return c
}

func (e *env) upload(tok string, id int64, name string, content []byte, wantStatus int) map[string]any {
	e.t.Helper()
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	mw.WriteField("label", "before/after")
	fw, _ := mw.CreateFormFile("file", name)
	fw.Write(content)
	mw.Close()
	req, _ := http.NewRequest("POST", fmt.Sprintf("%s/items/%d/evidence", e.srv.URL, id), &buf)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	req.Header.Set("Authorization", "Bearer "+tok)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		e.t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != wantStatus {
		e.t.Fatalf("upload: %d, want %d: %s", resp.StatusCode, wantStatus, raw)
	}
	out := map[string]any{}
	json.Unmarshal(raw, &out)
	return out
}

var cleanPaths = []string{"M internal/modules/cc/turn.go", "A internal/modules/cc/turn_test.go"}

// advance reports an outcome with a PR diff that hits no hold rule.
func (e *env) advance(id int64, outcome string, fields map[string]string, wantStatus int) map[string]any {
	e.t.Helper()
	return e.advanceWith(id, outcome, fields, cleanPaths, wantStatus)
}

func (e *env) advanceWith(id int64, outcome string, fields map[string]string, paths []string, wantStatus int) map[string]any {
	e.t.Helper()
	out := map[string]any{}
	e.do(runnerTok, "POST", fmt.Sprintf("/items/%d/advance", id), map[string]any{"outcome": outcome, "fields": fields, "changed_paths": paths}, wantStatus, &out)
	return out
}

type detail struct {
	Item struct {
		ID          int64  `json:"id"`
		Stage       string `json:"stage"`
		ReturnStage string `json:"return_stage"`
		Question    string `json:"question"`
		HeadSHA     string `json:"head_sha"`
		PR          int    `json:"pr"`
		Risk        string `json:"risk"`
		HoldReason  string `json:"hold_reason"`
		BounceCount int    `json:"bounce_count"`
		Approval    *struct {
			By      string `json:"by"`
			Words   string `json:"words"`
			HeadSHA string `json:"head_sha"`
		} `json:"approval"`
	} `json:"item"`
	Display string           `json:"display"`
	Lease   *json.RawMessage `json:"lease"`
	Events  []struct {
		Kind    string         `json:"kind"`
		Actor   string         `json:"actor"`
		Outcome string         `json:"outcome"`
		From    string         `json:"from"`
		To      string         `json:"to"`
		Data    map[string]any `json:"data"`
	} `json:"events"`
	Evidence []struct {
		N int `json:"n"`
	} `json:"evidence"`
}

func (e *env) get(id int64) detail {
	e.t.Helper()
	var d detail
	e.do(userTok, "GET", fmt.Sprintf("/items/%d", id), nil, 200, &d)
	return d
}

func (e *env) addItem(title string) int64 {
	e.t.Helper()
	var it struct{ ID int64 }
	e.do(userTok, "POST", "/items", map[string]any{"title": title, "ask": "fix it", "repo": "jarvis-server"}, 201, &it)
	return it.ID
}

func TestAuth(t *testing.T) {
	e := newEnv(t)
	e.refused("", "GET", "/items", nil, 401, "unauthorized")
	e.refused("not-a-token", "GET", "/items", nil, 401, "unauthorized")
	e.refused(userTok+"x", "GET", "/items", nil, 401, "unauthorized")
	// Runner-only.
	e.refused(userTok, "POST", "/claim", map[string]any{"agent": "a"}, 403, "forbidden")
	e.refused(userTok, "POST", "/leases/x/heartbeat", map[string]any{}, 403, "forbidden")
	e.refused(userTok, "POST", "/items/1/advance", map[string]any{"outcome": "failed"}, 403, "forbidden")
	// User-only.
	e.refused(runnerTok, "PUT", "/control", map[string]any{"active_agents": 6}, 403, "forbidden")
	e.refused(runnerTok, "POST", "/items/1/answer", map[string]any{"text": "x"}, 403, "forbidden")
	e.refused(runnerTok, "POST", "/items/1/approve", map[string]any{"head_sha": shaA}, 403, "forbidden")
	e.refused(runnerTok, "POST", "/items/1/hold", map[string]any{}, 403, "forbidden")
	e.refused(runnerTok, "POST", "/items/1/drop", map[string]any{"words": "x"}, 403, "forbidden")
	e.refused(runnerTok, "POST", "/items/1/qa", map[string]any{}, 403, "forbidden")
	e.refused(runnerTok, "POST", "/items/1/verified", map[string]any{}, 403, "forbidden")
	e.refused(runnerTok, "POST", "/items/1/reopen", map[string]any{"words": "x"}, 403, "forbidden")
	// Both.
	id := e.addItem("from the user")
	var it struct{ ID int64 }
	e.do(runnerTok, "POST", "/items", map[string]any{"title": "a finding", "source": "finding"}, 201, &it)
	e.do(runnerTok, "GET", "/items", nil, 200, nil)
	e.do(runnerTok, "PATCH", fmt.Sprintf("/items/%d", id), map[string]any{"priority": "P2"}, 200, nil)
	e.do(runnerTok, "GET", "/control", nil, 200, nil)
	// A claim may not name another runner.
	e.refused(runnerTok, "POST", "/claim", map[string]any{"runner": "mac", "agent": "a"}, 403, "runner_mismatch")

	if strings.Contains(e.logs.String(), userTok) || strings.Contains(e.logs.String(), runnerTok) {
		t.Fatal("a token reached the log")
	}
	if !strings.Contains(e.logs.String(), `"path":"/items"`) {
		t.Fatalf("requests are not logged: %s", e.logs.String())
	}
}

func TestBadRequests(t *testing.T) {
	e := newEnv(t)
	e.refused(userTok, "POST", "/items", map[string]any{"title": ""}, 400, "missing_field")
	e.refused(userTok, "POST", "/items", map[string]any{"title": "x", "priority": "P0"}, 400, "invalid_field")
	e.refused(userTok, "POST", "/items", map[string]any{"title": "x", "bogus": 1}, 400, "bad_request")
	e.refused(userTok, "GET", "/items/abc", nil, 400, "bad_request")
	e.refused(userTok, "GET", "/items/42", nil, 404, "not_found")
	e.refused(userTok, "GET", "/items?stage=nope", nil, 400, "invalid_field")
	e.refused(runnerTok, "POST", "/claim", map[string]any{"agent": ""}, 400, "missing_field")
	e.refused(runnerTok, "POST", "/claim", map[string]any{"agent": "a", "capabilities": []string{"tpu"}}, 400, "invalid_field")
	e.refused(userTok, "PUT", "/control", map[string]any{"active_agents": 7}, 400, "invalid_field")
}

// A bare repo name is stored as its configured slug; a repo loopd is not configured with is
// refused (400), so loop add shows why (#127).
func TestAddItemRepo(t *testing.T) {
	e := newEnv(t)
	e.store.SetRepos([]string{"alexberardi/jarvis-server"})
	var it struct{ Repo string }
	e.do(userTok, "POST", "/items", map[string]any{"title": "x", "repo": "jarvis-server"}, 201, &it)
	if it.Repo != "alexberardi/jarvis-server" {
		t.Fatalf("repo: %q", it.Repo)
	}
	e.refused(userTok, "POST", "/items", map[string]any{"title": "y", "repo": "nope"}, 400, "invalid_field")
}

// depends_on: unknown ids and cycles are refused (400); the item shows what it waits on.
func TestAddItemDependsOn(t *testing.T) {
	e := newEnv(t)
	a := e.addItem("a")
	e.refused(userTok, "POST", "/items", map[string]any{"title": "b", "depends_on": []int64{a, 42}}, 400, "invalid_field")
	e.refused(userTok, "POST", "/items", map[string]any{"title": "b", "depends_on": []int64{a + 1}}, 400, "invalid_field")
	var it struct {
		ID        int64
		DependsOn []int64 `json:"depends_on"`
	}
	e.do(userTok, "POST", "/items", map[string]any{"title": "b", "depends_on": []int64{a}}, 201, &it)
	if len(it.DependsOn) != 1 || it.DependsOn[0] != a {
		t.Fatalf("created: %+v", it)
	}
	var d struct {
		WaitingOn []int64 `json:"waiting_on"`
	}
	e.do(userTok, "GET", fmt.Sprintf("/items/%d", it.ID), nil, 200, &d)
	if len(d.WaitingOn) != 1 || d.WaitingOn[0] != a {
		t.Fatalf("waiting on: %+v", d)
	}
	// A closed item never becomes done: adding after it is refused.
	e.do(userTok, "POST", fmt.Sprintf("/items/%d/drop", a), map[string]any{"words": "x"}, 200, nil)
	e.refused(userTok, "POST", "/items", map[string]any{"title": "c", "depends_on": []int64{a}}, 400, "invalid_field")
}

// The spec's full walk: every stage, review independence, a bounce, the review's head-sha check,
// the hold, a wrong-sha approval refused, the approval for the right head, and the merge.
func TestFullWalk(t *testing.T) {
	e := newEnv(t)
	id := e.addItem("wake word misses after resume")
	if d := e.get(id); d.Item.Stage != "triage" || d.Display != "intake" {
		t.Fatalf("new item: %+v", d)
	}

	// Triage.
	e.mustClaim("triager", id, "triage")
	if d := e.get(id); d.Display != "triage" {
		t.Fatalf("claimed triage displays %q", d.Display)
	}
	e.advance(id, "reproduced", map[string]string{"area": "voice", "size": "M", "risk": "normal", "resources": "gpu"}, 409) // no evidence yet
	e.upload(runnerTok, id, "repro.log", []byte("missed 3/10\n"), 201)
	e.advance(id, "reproduced", map[string]string{"area": "voice", "size": "M", "risk": "normal", "resources": "gpu"}, 200)

	// Build: needs gpu.
	if c := e.claim(runnerTok, "builder"); c != nil {
		t.Fatalf("a runner without gpu got a gpu item: %+v", c)
	}
	e.mustClaim("builder", id, "build", "gpu", "voice")
	e.upload(runnerTok, id, "before.png", []byte("\x89PNG\r\n\x1a\n...."), 201)
	e.advance(id, "pr_opened", map[string]string{"repo": "jarvis-server", "pr": "142", "branch": "fix/wake", "head_sha": shaA}, 200)

	// Review independence: the builder is not given its own review.
	if c := e.claim(runnerTok, "builder", "gpu"); c != nil {
		t.Fatalf("the builder got its own review: %+v", c)
	}
	e.mustClaim("reviewer", id, "review", "gpu")
	e.advance(id, "bounce", map[string]string{"notes": "no test for the timeout"}, 200)
	if d := e.get(id); d.Item.Stage != "build" || d.Item.BounceCount != 1 {
		t.Fatalf("after bounce: %+v", d.Item)
	}

	// Build again, a new head.
	e.mustClaim("builder", id, "build", "gpu")
	e.upload(runnerTok, id, "after.png", []byte("\x89PNG\r\n\x1a\n...."), 201)
	e.advance(id, "pr_opened", map[string]string{"repo": "jarvis-server", "pr": "142", "branch": "fix/wake", "head_sha": shaB}, 200)

	// Review: approving a stale head is refused; the current head lands.
	e.mustClaim("reviewer", id, "review", "gpu")
	var ae apiErr
	e.do(runnerTok, "POST", fmt.Sprintf("/items/%d/advance", id), map[string]any{"outcome": "approved", "fields": map[string]string{"summary": "ok", "head_sha": shaA}}, 409, &ae)
	if ae.Code != "sha_mismatch" {
		t.Fatalf("stale review: %+v", ae)
	}
	e.advance(id, "approved", map[string]string{"summary": "tests cover the timeout", "head_sha": shaB}, 200)

	// Land: held.
	e.mustClaim("lander", id, "land", "gpu")
	e.advance(id, "hold", map[string]string{"reason": "weakens a test", "head_sha": shaB}, 200)
	if d := e.get(id); d.Item.Stage != "held" || d.Display != "held" {
		t.Fatalf("after hold: %+v", d)
	}
	if c := e.claim(runnerTok, "lander", "gpu"); c != nil {
		t.Fatal("a held item was claimed")
	}
	e.do(userTok, "POST", fmt.Sprintf("/items/%d/hold", id), map[string]any{"words": "not yet"}, 200, nil)
	e.refused(userTok, "POST", fmt.Sprintf("/items/%d/approve", id), map[string]any{"head_sha": shaA, "words": "yes"}, 409, "sha_mismatch")
	e.do(userTok, "POST", fmt.Sprintf("/items/%d/approve", id), map[string]any{"head_sha": shaB, "words": "yea, removing that old test is fine"}, 200, nil)
	d := e.get(id)
	if d.Item.Stage != "land" || d.Item.Approval == nil || d.Item.Approval.By != "user:alex" ||
		d.Item.Approval.HeadSHA != shaB || d.Item.Approval.Words != "yea, removing that old test is fine" {
		t.Fatalf("after approve: %+v", d.Item)
	}

	// Land: merged.
	e.mustClaim("lander", id, "land", "gpu")
	e.advance(id, "merged", map[string]string{"merge_sha": shaM, "head_sha": shaB}, 200)
	d = e.get(id)
	if d.Item.Stage != "done" || d.Display != "done" || d.Lease != nil || len(d.Evidence) != 3 {
		t.Fatalf("done: %+v", d)
	}
	e.refused(userTok, "PATCH", fmt.Sprintf("/items/%d", id), map[string]any{"priority": "P1"}, 409, "wrong_stage")

	var outcomes []string
	refused := 0
	for _, ev := range d.Events {
		if ev.Kind == "advanced" {
			outcomes = append(outcomes, ev.Outcome)
		}
		if ev.Kind == "refused" {
			refused++
		}
	}
	want := "reproduced,pr_opened,bounce,pr_opened,approved,hold,merged"
	if strings.Join(outcomes, ",") != want {
		t.Fatalf("advance history %v, want %s", outcomes, want)
	}
	if refused != 3 { // no evidence, stale review, wrong-sha approval
		t.Fatalf("refused events: %d, want 3", refused)
	}
}

func TestSecondBounceGoesToNeedsYou(t *testing.T) {
	e := newEnv(t)
	id := e.addItem("x")
	e.mustClaim("t", id, "triage")
	e.upload(runnerTok, id, "e.txt", []byte("e"), 201)
	e.advance(id, "reproduced", map[string]string{"area": "a", "size": "S", "risk": "normal", "resources": ""}, 200)
	for i := 1; i <= 2; i++ {
		e.mustClaim("builder", id, "build")
		e.upload(runnerTok, id, "e.txt", []byte("e"), 201)
		e.advance(id, "pr_opened", map[string]string{"repo": "r", "pr": "1", "branch": "b", "head_sha": shaA}, 200)
		e.mustClaim("reviewer", id, "review")
		e.advance(id, "bounce", map[string]string{"notes": fmt.Sprintf("bounce %d", i)}, 200)
	}
	d := e.get(id)
	if d.Item.Stage != "needs_you" || d.Item.Question != "bounce 2" || d.Item.ReturnStage != "build" || d.Display != "needs-you" {
		t.Fatalf("after 2nd bounce: %+v", d.Item)
	}
	if c := e.claim(runnerTok, "any"); c != nil {
		t.Fatal("a needs_you item was claimed")
	}
	e.do(userTok, "POST", fmt.Sprintf("/items/%d/answer", id), map[string]any{"text": "add the test the reviewer asked for"}, 200, nil)
	if d := e.get(id); d.Item.Stage != "build" {
		t.Fatalf("after answer: %+v", d.Item)
	}
}

func TestLeaseExpiry(t *testing.T) {
	e := newEnv(t)
	id := e.addItem("x")
	c := e.mustClaim("a", id, "triage")
	if !c.Lease.ExpiresAt.Equal(t0.Add(5 * time.Minute)) {
		t.Fatalf("lease expires %v", c.Lease.ExpiresAt)
	}
	e.clock.Advance(5 * time.Minute)
	// Expiry is evaluated on every request: the next read already sees it.
	d := e.get(id)
	if d.Lease != nil || d.Item.Stage != "triage" || d.Display != "intake" || d.Events[len(d.Events)-1].Kind != "lease_expired" {
		t.Fatalf("after expiry: %+v", d)
	}
	var hb struct{ Stop bool }
	e.do(runnerTok, "POST", "/leases/"+c.Lease.ID+"/heartbeat", map[string]any{}, 200, &hb)
	if !hb.Stop {
		t.Fatal("heartbeat on an expired lease must say stop")
	}
	e.refused(runnerTok, "POST", fmt.Sprintf("/items/%d/advance", id), map[string]any{"outcome": "failed", "fields": map[string]string{"error": "x"}}, 409, "no_lease")
	// It is back in the queue.
	e.mustClaim("b", id, "triage")
	e.refused(otherTok, "POST", fmt.Sprintf("/items/%d/advance", id), map[string]any{"outcome": "failed", "fields": map[string]string{"error": "x"}}, 403, "not_your_lease")
}

func TestMaxRunTime(t *testing.T) {
	e := newEnv(t)
	min30 := 30
	e.do(userTok, "PUT", "/control", map[string]any{"max_run_minutes": min30}, 200, nil)
	id := e.addItem("x")
	c := e.mustClaim("a", id, "triage")
	var hb struct{ Stop bool }
	for range 7 { // 28 minutes
		e.clock.Advance(4 * time.Minute)
		e.do(runnerTok, "POST", "/leases/"+c.Lease.ID+"/heartbeat", map[string]any{"note": "bisecting"}, 200, &hb)
		if hb.Stop {
			t.Fatal("stop before the max run time")
		}
	}
	e.clock.Advance(2 * time.Minute)
	e.do(runnerTok, "POST", "/leases/"+c.Lease.ID+"/heartbeat", map[string]any{}, 200, &hb)
	if !hb.Stop {
		t.Fatal("no stop after the max run time")
	}
	d := e.get(id)
	if d.Item.Stage != "needs_you" || d.Item.Question != "run exceeded 30 minutes; last note: bisecting" || d.Item.ReturnStage != "triage" {
		t.Fatalf("overtime: %+v", d.Item)
	}
}

func TestStopViaHeartbeatAndUsageLimit(t *testing.T) {
	e := newEnv(t)
	id := e.addItem("x")
	c := e.mustClaim("a", id, "triage")
	e.do(userTok, "PUT", "/control", map[string]any{"stopped": true}, 200, nil)
	var hb struct{ Stop bool }
	e.do(runnerTok, "POST", "/leases/"+c.Lease.ID+"/heartbeat", map[string]any{}, 200, &hb)
	if !hb.Stop {
		t.Fatal("stopped, but heartbeat says go on")
	}
	if d := e.get(id); d.Lease != nil || d.Item.Stage != "triage" {
		t.Fatalf("stop must return the item to its queue: %+v", d)
	}
	if c := e.claim(runnerTok, "a"); c != nil {
		t.Fatal("claim while stopped")
	}
	e.do(userTok, "PUT", "/control", map[string]any{"stopped": false}, 200, nil)
	e.mustClaim("a", id, "triage")

	e.advance(id, "usage_limit", map[string]string{"error": "rate_limit_error", "reset": "resets 17:00"}, 200)
	var ctl struct {
		ActiveAgents int  `json:"active_agents"`
		Stopped      bool `json:"stopped"`
	}
	e.do(userTok, "GET", "/control", nil, 200, &ctl)
	if ctl.ActiveAgents != 0 {
		t.Fatalf("usage_limit: active agents %d, want 0", ctl.ActiveAgents)
	}
	if c := e.claim(runnerTok, "a"); c != nil {
		t.Fatal("claim at active agents 0")
	}
	d := e.get(id)
	if d.Item.Stage != "triage" || d.Events[len(d.Events)-1].Kind != "usage_limit" {
		t.Fatalf("usage_limit item: %+v", d)
	}
	e.do(userTok, "PUT", "/control", map[string]any{"active_agents": 2}, 200, nil)
	e.mustClaim("a", id, "triage")
}

func TestEvidenceUpAndDown(t *testing.T) {
	e := newEnv(t)
	id := e.addItem("x")
	e.upload(runnerTok, id, "a.txt", []byte("x"), 409) // no lease
	e.mustClaim("a", id, "triage")
	e.upload(userTok, id, "a.txt", []byte("x"), 403)
	e.upload(otherTok, id, "a.txt", []byte("x"), 403)

	png := append([]byte("\x89PNG\r\n\x1a\n"), bytes.Repeat([]byte{1}, 1000)...)
	ev := e.upload(runnerTok, id, "../shot.png", png, 201)
	sum := sha256.Sum256(png)
	if ev["n"] != float64(1) || ev["name"] != "shot.png" || ev["sha256"] != hex.EncodeToString(sum[:]) || ev["content_type"] != "image/png" {
		t.Fatalf("uploaded: %+v", ev)
	}
	html := e.upload(runnerTok, id, "page.html", []byte("<html><script>alert(1)</script></html>"), 201)

	for _, tok := range []string{userTok, runnerTok} {
		req, _ := http.NewRequest("GET", fmt.Sprintf("%s/items/%d/evidence/1", e.srv.URL, id), nil)
		req.Header.Set("Authorization", "Bearer "+tok)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode != 200 || !bytes.Equal(body, png) || resp.Header.Get("Content-Type") != "image/png" ||
			resp.Header.Get("X-Content-Type-Options") != "nosniff" {
			t.Fatalf("download: %d %v", resp.StatusCode, resp.Header)
		}
	}
	// Anything that is not a plain image or text is served as a download, never rendered.
	req, _ := http.NewRequest("GET", fmt.Sprintf("%s/items/%d/evidence/%v", e.srv.URL, id, html["n"]), nil)
	req.Header.Set("Authorization", "Bearer "+userTok)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.Header.Get("Content-Type") != "application/octet-stream" || !strings.HasPrefix(resp.Header.Get("Content-Disposition"), "attachment") ||
		!strings.Contains(resp.Header.Get("Content-Security-Policy"), "sandbox") {
		t.Fatalf("html evidence served as %v", resp.Header)
	}
	e.refused(userTok, "GET", fmt.Sprintf("/items/%d/evidence/9", id), nil, 404, "not_found")
	e.refused("", "GET", fmt.Sprintf("/items/%d/evidence/1", id), nil, 401, "unauthorized")

	big := make([]byte, store.MaxEvidenceBytes+1)
	e.upload(runnerTok, id, "big.bin", big, 413)
}

func TestTokensFile(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "tokens")
	os.WriteFile(p, []byte(userTok+"\tuser\talex\n"), 0o644)
	if _, err := LoadTokens(p); err == nil {
		t.Fatal("a tokens file readable by others must be refused")
	}
	os.Chmod(p, 0o600)
	toks, err := LoadTokens(p)
	if err != nil {
		t.Fatal(err)
	}
	if pr, ok := toks.Lookup(userTok); !ok || pr.Kind != "user" || pr.Name != "alex" {
		t.Fatalf("lookup: %+v %v", pr, ok)
	}
	if _, ok := toks.Lookup(userTok[:len(userTok)-1]); ok {
		t.Fatal("a prefix matched")
	}
	for _, bad := range []string{
		"short\tuser\talex\n",
		userTok + "\tadmin\talex\n",
		userTok + "\tuser\n",
		userTok + "\tuser\talex\n" + userTok + "\trunner\tdevbox\n",
		"",
	} {
		if _, err := ParseTokens(strings.NewReader(bad)); err == nil {
			t.Errorf("accepted a bad tokens file: %q", bad)
		}
	}
}

// Walks an item to land with the given triage risk and PR diff; returns its id.
func (e *env) toLand(risk string, paths []string) int64 {
	e.t.Helper()
	id := e.addItem("x")
	e.mustClaim("t", id, "triage")
	e.upload(runnerTok, id, "e.txt", []byte("e"), 201)
	e.advance(id, "reproduced", map[string]string{"area": "a", "size": "S", "risk": risk, "resources": ""}, 200)
	e.mustClaim("builder", id, "build")
	e.upload(runnerTok, id, "e.txt", []byte("e"), 201)
	e.advanceWith(id, "pr_opened", map[string]string{"repo": "alexberardi/jarvis-server", "pr": "1", "branch": "b", "head_sha": shaA}, paths, 200)
	e.mustClaim("reviewer", id, "review")
	e.advance(id, "approved", map[string]string{"summary": "ok", "head_sha": shaA}, 200)
	return id
}

// Hold risk comes from loopd's rules on the PR's changed paths, not from triage's label.
func TestHoldComputedFromChangedPaths(t *testing.T) {
	cases := []struct {
		name   string
		paths  []string
		reason string
	}{
		{"auth change labelled normal", []string{"M internal/modules/cc/turn.go", "M internal/modules/auth/login.go"}, "auth: M internal/modules/auth/login.go"},
		{"deleted test", []string{"D internal/x_test.go"}, "deleted-test: D internal/x_test.go"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			e := newEnv(t)
			id := e.toLand("normal", c.paths)
			d := e.get(id)
			if d.Item.Risk != "hold" || d.Item.HoldReason != c.reason {
				t.Fatalf("risk %q reason %q, want hold %q", d.Item.Risk, d.Item.HoldReason, c.reason)
			}
			var matched bool
			for _, ev := range d.Events {
				if ev.Outcome == "pr_opened" && ev.Data["hold_matches"] != nil {
					matched = true
				}
			}
			if !matched {
				t.Fatal("the pr_opened event does not say which rule matched")
			}
			e.mustClaim("lander", id, "land")
			e.refused(runnerTok, "POST", fmt.Sprintf("/items/%d/advance", id), map[string]any{"outcome": "merged",
				"fields": map[string]string{"merge_sha": shaM, "head_sha": shaA}, "changed_paths": c.paths}, 409, "approval_required")
			e.advanceWith(id, "hold", map[string]string{"reason": "on the hold list", "head_sha": shaA}, c.paths, 200)
			if d := e.get(id); d.Item.Stage != "held" || d.Item.HeadSHA != shaA {
				t.Fatalf("held: %+v", d.Item)
			}
		})
	}
	t.Run("needs_review", func(t *testing.T) {
		e := newEnv(t)
		id := e.addItem("x")
		e.mustClaim("t", id, "triage")
		e.upload(runnerTok, id, "e.txt", []byte("e"), 201)
		e.advance(id, "reproduced", map[string]string{"area": "a", "size": "S", "risk": "normal", "resources": ""}, 200)
		e.mustClaim("builder", id, "build")
		e.upload(runnerTok, id, "e.txt", []byte("e"), 201)
		e.advance(id, "pr_opened", map[string]string{"repo": "r", "pr": "1", "branch": "b", "head_sha": shaA, "needs_review": "true"}, 200)
		if d := e.get(id); d.Item.Risk != "hold" || d.Item.HoldReason != "marked needs review" {
			t.Fatalf("needs_review: %+v", d.Item)
		}
	})
	t.Run("changed_paths required", func(t *testing.T) {
		e := newEnv(t)
		id := e.addItem("x")
		e.mustClaim("t", id, "triage")
		e.upload(runnerTok, id, "e.txt", []byte("e"), 201)
		e.advance(id, "reproduced", map[string]string{"area": "a", "size": "S", "risk": "normal", "resources": ""}, 200)
		e.mustClaim("builder", id, "build")
		e.upload(runnerTok, id, "e.txt", []byte("e"), 201)
		e.refused(runnerTok, "POST", fmt.Sprintf("/items/%d/advance", id), map[string]any{"outcome": "pr_opened",
			"fields": map[string]string{"repo": "r", "pr": "1", "branch": "b", "head_sha": shaA}}, 400, "missing_field")
	})
}

// Approval for A; land pushes B (e.g. merged main in). merged with B is refused and the item
// stays in land; hold with B shows B and voids A; approve B; merged with B lands.
func TestMergedHeadMustMatchApproval(t *testing.T) {
	e := newEnv(t)
	id := e.toLand("hold", cleanPaths)
	e.mustClaim("lander", id, "land")
	e.advanceWith(id, "hold", map[string]string{"reason": "triage said hold", "head_sha": shaA}, cleanPaths, 200)
	e.do(userTok, "POST", fmt.Sprintf("/items/%d/approve", id), map[string]any{"head_sha": shaA, "words": "A is fine"}, 200, nil)

	e.mustClaim("lander", id, "land")
	e.refused(runnerTok, "POST", fmt.Sprintf("/items/%d/advance", id), map[string]any{"outcome": "merged",
		"fields": map[string]string{"merge_sha": shaM, "head_sha": shaB}, "changed_paths": cleanPaths}, 409, "approval_required")
	if d := e.get(id); d.Item.Stage != "land" || d.Lease == nil {
		t.Fatalf("a refused merge must leave the item in land under its lease: %+v", d)
	}

	e.advanceWith(id, "hold", map[string]string{"reason": "head moved after approval", "head_sha": shaB}, cleanPaths, 200)
	d := e.get(id)
	if d.Item.Stage != "held" || d.Item.HeadSHA != shaB || d.Item.Approval != nil {
		t.Fatalf("hold with B: %+v", d.Item)
	}
	e.refused(userTok, "POST", fmt.Sprintf("/items/%d/approve", id), map[string]any{"head_sha": shaA}, 409, "sha_mismatch")
	e.do(userTok, "POST", fmt.Sprintf("/items/%d/approve", id), map[string]any{"head_sha": shaB, "words": "B too"}, 200, nil)
	e.mustClaim("lander", id, "land")
	e.advanceWith(id, "merged", map[string]string{"merge_sha": shaM, "head_sha": shaB}, cleanPaths, 200)
	if d := e.get(id); d.Item.Stage != "done" || d.Item.HeadSHA != shaB {
		t.Fatalf("merged B: %+v", d.Item)
	}
}

func TestHoldRulesInControl(t *testing.T) {
	e := newEnv(t)
	var ctl struct {
		HoldRules []struct {
			Name     string   `json:"name"`
			Glob     string   `json:"glob"`
			Statuses []string `json:"statuses"`
			Repo     string   `json:"repo"`
		} `json:"hold_rules"`
	}
	e.do(runnerTok, "GET", "/control", nil, 200, &ctl)
	if len(ctl.HoldRules) != len(pipeline.DefaultHoldRules()) || ctl.HoldRules[0].Glob != "internal/modules/auth/**" ||
		ctl.HoldRules[0].Repo != "alexberardi/jarvis-server" {
		t.Fatalf("default hold rules: %+v", ctl.HoldRules)
	}
	e.refused(userTok, "PUT", "/control", map[string]any{"hold_rules": []map[string]any{{"name": "x", "glob": "/abs"}}}, 400, "invalid_field")
	e.do(userTok, "PUT", "/control", map[string]any{"hold_rules": []map[string]any{{"name": "docs", "glob": "docs/**"}}}, 200, &ctl)
	if len(ctl.HoldRules) != 1 || ctl.HoldRules[0].Name != "docs" {
		t.Fatalf("hold rules after PUT: %+v", ctl.HoldRules)
	}
	id := e.toLand("normal", []string{"M docs/STATUS.md"})
	if d := e.get(id); d.Item.HoldReason != "docs: M docs/STATUS.md" {
		t.Fatalf("custom rule: %+v", d.Item)
	}
}

func TestClaimStagesAndRunnerEvents(t *testing.T) {
	e := newEnv(t)
	tri := e.addItem("still in triage")
	e.refused(runnerTok, "POST", "/claim", map[string]any{"agent": "a1", "stages": []string{"held"}}, 400, "invalid_field")
	// A build-only runner does not get the triage item.
	e.do(runnerTok, "POST", "/claim", map[string]any{"agent": "a1", "stages": []string{"build"}}, 204, nil)
	c := e.mustClaim("a1", tri, "triage")

	ev := map[string]any{"kind": "guard_refused", "data": map[string]any{"rule": "git_push", "tool": "Bash", "reason": "only loop/1-x"}}
	e.refused(otherTok, "POST", fmt.Sprintf("/items/%d/events", tri), ev, 403, "not_your_lease")
	e.refused(runnerTok, "POST", fmt.Sprintf("/items/%d/events", tri), map[string]any{"kind": "advanced"}, 400, "invalid_field")
	e.refused(userTok, "POST", fmt.Sprintf("/items/%d/events", tri), ev, 403, "forbidden")
	e.do(runnerTok, "POST", fmt.Sprintf("/items/%d/events", tri), ev, 201, nil)
	d := e.get(tri)
	last := d.Events[len(d.Events)-1]
	if last.Kind != "guard_refused" || last.Actor != "runner:devbox" || last.Data["rule"] != "git_push" || last.From != "triage" {
		t.Fatalf("event %+v", last)
	}
	// Without a live lease, nothing is recorded.
	e.do(runnerTok, "POST", "/leases/"+c.Lease.ID+"/heartbeat", map[string]any{}, 200, nil)
	e.clock.Advance(10 * time.Minute)
	e.refused(runnerTok, "POST", fmt.Sprintf("/items/%d/events", tri), ev, 409, "no_lease")
}

// GET /items?open=1 is the runner's snapshot for triage: open items only, with repo and PR.
func TestOpenItemsList(t *testing.T) {
	e := newEnv(t)
	b := e.addItem("will close")
	a := e.addItem("still open")
	for _, id := range []int64{b, a} {
		e.mustClaim("triager", id, "triage")
		e.upload(runnerTok, id, "repro.txt", []byte("seen"), 201)
		if id == b {
			e.advance(id, "not_reproducible", map[string]string{"summary": "duplicate of #1"}, 200)
		} else {
			e.advance(id, "reproduced", map[string]string{"area": "x", "size": "S", "risk": "normal", "resources": ""}, 200)
		}
	}
	var out struct {
		Items []struct {
			ID    int64  `json:"id"`
			Stage string `json:"stage"`
			Repo  string `json:"repo"`
		} `json:"items"`
	}
	e.do(runnerTok, "GET", "/items?open=1", nil, 200, &out)
	if len(out.Items) != 1 || out.Items[0].ID != a || out.Items[0].Repo != "jarvis-server" {
		t.Fatalf("open items %+v", out.Items)
	}
	e.do(userTok, "GET", "/items", nil, 200, &out)
	if len(out.Items) != 2 {
		t.Fatalf("all items %+v", out.Items)
	}
	e.refused(userTok, "GET", "/items?open=maybe", nil, 400, "invalid_field")
}

// Land's back_to_review: a new head goes to review, the approval for the old one is void.
func TestBackToReviewFromLand(t *testing.T) {
	e := newEnv(t)
	id := e.toLand("normal", []string{"M internal/x.go"})
	e.mustClaim("lander", id, "land")
	e.advanceWith(id, "back_to_review", map[string]string{"notes": "the PR head moved since review", "head_sha": shaB}, nil, 400)
	e.advanceWith(id, "back_to_review", map[string]string{"notes": "the PR head moved since review", "head_sha": shaB}, []string{"M internal/x.go"}, 200)
	d := e.get(id)
	if d.Item.Stage != "review" || d.Item.HeadSHA != shaB {
		t.Fatalf("after back_to_review: %+v", d.Item)
	}
	// The builder still cannot review the new head.
	if c := e.claim(runnerTok, "builder"); c != nil {
		t.Fatalf("the builder got the review of the new head: %+v", c)
	}
}

// main_ci: the runner that merged the item records main's CI once the item is done.
func TestMainCIEventRoute(t *testing.T) {
	e := newEnv(t)
	id := e.toLand("normal", []string{"M internal/x.go"})
	ev := map[string]any{"kind": "main_ci", "data": map[string]any{"result": "green", "urls": []string{"https://github.com/o/r/actions/runs/1"}}}
	path := fmt.Sprintf("/items/%d/events", id)
	e.mustClaim("lander", id, "land")
	e.refused(runnerTok, "POST", path, ev, 409, "wrong_stage") // not merged yet
	e.advanceWith(id, "merged", map[string]string{"merge_sha": shaM, "head_sha": shaA}, []string{"M internal/x.go"}, 200)
	e.refused(userTok, "POST", path, ev, 403, "forbidden")
	e.refused(otherTok, "POST", path, ev, 403, "not_your_merge")
	e.refused(runnerTok, "POST", path, map[string]any{"kind": "main_ci", "data": map[string]any{"result": "maybe"}}, 400, "invalid_field")
	e.do(runnerTok, "POST", path, ev, 201, nil)
	e.refused(runnerTok, "POST", path, ev, 409, "already_recorded")
	d := e.get(id)
	last := d.Events[len(d.Events)-1]
	if last.Kind != "main_ci" || last.Actor != "runner:devbox" || last.Data["result"] != "green" {
		t.Fatalf("event %+v", last)
	}
}

// A board token reads the web board only (docs/decisions/2026-10-15-board.md): the API refuses it
// on every route, reads included.
func TestBoardTokenRefusedByAPI(t *testing.T) {
	const boardTok = "board-token-0123456789abcdef"
	st, err := store.Open(t.TempDir(), clock.NewFake(t0))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	toks, err := ParseTokens(strings.NewReader(userTok + "\tuser\talex\n" + boardTok + "\tboard\tphone\n"))
	if err != nil {
		t.Fatal(err)
	}
	if pr, ok := toks.Lookup(boardTok); !ok || pr.Kind != KindBoard || pr.Actor() != "board:phone" {
		t.Fatalf("lookup: %+v %v", pr, ok)
	}
	srv := httptest.NewServer(New(st, toks, slog.New(slog.DiscardHandler)))
	t.Cleanup(srv.Close)
	e := &env{t: t, srv: srv, store: st}
	var it struct{ ID int64 }
	e.do(userTok, "POST", "/items", map[string]string{"title": "x"}, 201, &it)
	for _, r := range []struct{ method, path string }{
		{"POST", "/items"}, {"GET", "/items"}, {"GET", "/items/1"}, {"PATCH", "/items/1"},
		{"POST", "/claim"}, {"POST", "/leases/x/heartbeat"}, {"POST", "/items/1/advance"},
		{"POST", "/items/1/evidence"}, {"POST", "/items/1/events"}, {"GET", "/items/1/evidence/1"},
		{"POST", "/items/1/answer"}, {"POST", "/items/1/approve"}, {"POST", "/items/1/hold"}, {"POST", "/items/1/drop"},
		{"GET", "/control"}, {"PUT", "/control"},
	} {
		e.refused(boardTok, r.method, r.path, nil, 403, "forbidden")
	}
}

// drop: the user closes an item from any open stage; a live lease ends. The runner then lists the
// dropped items whose PR it has to close and records pr_closed once.
func TestDropRoute(t *testing.T) {
	e := newEnv(t)
	id := e.toLand("normal", []string{"M internal/x.go"})
	e.mustClaim("lander", id, "land")
	toClose := func() []int64 {
		e.t.Helper()
		var out struct{ Items []struct{ ID int64 } }
		e.do(runnerTok, "GET", "/items?pr_to_close=1", nil, 200, &out)
		var ids []int64
		for _, it := range out.Items {
			ids = append(ids, it.ID)
		}
		return ids
	}
	if ids := toClose(); len(ids) != 0 {
		t.Fatalf("before the drop: %v", ids)
	}
	path := fmt.Sprintf("/items/%d/drop", id)
	e.refused(runnerTok, "POST", path, map[string]any{"words": "no"}, 403, "forbidden")
	e.do(userTok, "POST", path, map[string]any{"words": "no hardware"}, 200, nil)
	d := e.get(id)
	last := d.Events[len(d.Events)-1]
	if d.Item.Stage != "closed" || d.Lease != nil || last.Kind != "dropped" || last.Actor != "user:alex" ||
		last.From != "land" || last.Data["words"] != "no hardware" || last.Data["reason"] != "dropped by the user" {
		t.Fatalf("after drop: stage %s lease %v event %+v", d.Item.Stage, d.Lease, last)
	}
	e.refused(userTok, "POST", path, map[string]any{}, 409, "wrong_stage")
	e.refused(runnerTok, "GET", "/items?pr_to_close=maybe", nil, 400, "invalid_field")
	if ids := toClose(); len(ids) != 1 || ids[0] != id {
		t.Fatalf("to close: %v", ids)
	}
	ev := map[string]any{"kind": "pr_closed", "data": map[string]any{"pr": 1, "state": "closed"}}
	ev["data"].(map[string]any)["pr"] = d.Item.PR
	events := fmt.Sprintf("/items/%d/events", id)
	e.refused(userTok, "POST", events, ev, 403, "forbidden")
	e.refused(runnerTok, "POST", events, map[string]any{"kind": "pr_closed", "data": map[string]any{"pr": 3, "state": "gone"}}, 400, "invalid_field")
	e.do(otherTok, "POST", events, ev, 201, nil)
	e.refused(runnerTok, "POST", events, ev, 409, "already_recorded")
	if ids := toClose(); len(ids) != 0 {
		t.Fatalf("after pr_closed: %v", ids)
	}
}

// The QA lane: the user marks an item needs_qa before it lands; merged puts it in qa; Verified moves
// it to done, Reopen files a linked P2 item (docs/decisions/2026-10-15-qa.md).
func TestQARoutes(t *testing.T) {
	e := newEnv(t)
	merge := func(id int64) {
		e.t.Helper()
		e.mustClaim("lander", id, "land")
		e.advance(id, "merged", map[string]string{"merge_sha": shaB, "head_sha": shaA}, 200)
	}
	a := e.toLand("normal", []string{"M internal/x.go"})
	e.do(userTok, "POST", fmt.Sprintf("/items/%d/qa", a), map[string]any{}, 200, nil)
	merge(a)
	if d := e.get(a); d.Item.Stage != "qa" {
		t.Fatalf("a after merged: %s", d.Item.Stage)
	}
	e.refused(userTok, "POST", fmt.Sprintf("/items/%d/qa", a), map[string]any{}, 409, "wrong_stage")
	// b, from a repo with qa: true (the land script's needs_qa).
	b := e.toLand("normal", []string{"M internal/x.go"})
	e.mustClaim("lander", b, "land")
	e.advance(b, "merged", map[string]string{"merge_sha": shaB, "head_sha": shaA, "needs_qa": "true"}, 200)
	e.refused(userTok, "POST", fmt.Sprintf("/items/%d/reopen", a), map[string]any{"words": " "}, 400, "missing_field")
	var out struct {
		Item struct {
			ID    int64
			Stage string
		}
		Filed struct {
			ID       int64  `json:"id"`
			Priority string `json:"priority"`
			ReopenOf int64  `json:"reopen_of"`
			Ask      string `json:"ask"`
		}
	}
	e.do(userTok, "POST", fmt.Sprintf("/items/%d/reopen", a), map[string]any{"words": "still broken"}, 200, &out)
	if out.Item.Stage != "done" || out.Filed.ReopenOf != a || out.Filed.Priority != "P2" || out.Filed.Ask != "still broken" {
		t.Fatalf("reopen: %+v", out)
	}

	e.do(userTok, "POST", fmt.Sprintf("/items/%d/verified", b), map[string]any{"words": "ok on the phone"}, 200, nil)
	d := e.get(b)
	last := d.Events[len(d.Events)-1]
	if d.Item.Stage != "done" || last.Kind != "verified" || last.Data["words"] != "ok on the phone" {
		t.Fatalf("verified: %s %+v", d.Item.Stage, last)
	}
	e.refused(userTok, "POST", fmt.Sprintf("/items/%d/verified", b), map[string]any{}, 409, "wrong_stage")
}

// QA after deploy: the land script's qa_after_deploy puts the item in qa awaiting a deploy; the
// runners list it (awaiting_deploy=1) and one records deployed with its build's revision
// (docs/decisions/2026-10-16-qa-after-deploy.md).
func TestQAAfterDeployRoutes(t *testing.T) {
	e := newEnv(t)
	id := e.toLand("normal", []string{"M internal/x.go"})
	e.mustClaim("lander", id, "land")
	e.advance(id, "merged", map[string]string{"merge_sha": shaB, "head_sha": shaA, "qa_after_deploy": "true"}, 200)
	awaiting := func() []int64 {
		e.t.Helper()
		var out struct{ Items []struct{ ID int64 } }
		e.do(runnerTok, "GET", "/items?awaiting_deploy=1", nil, 200, &out)
		var ids []int64
		for _, it := range out.Items {
			ids = append(ids, it.ID)
		}
		return ids
	}
	if ids := awaiting(); len(ids) != 1 || ids[0] != id {
		t.Fatalf("awaiting deploy: %v", ids)
	}
	e.refused(runnerTok, "GET", "/items?awaiting_deploy=maybe", nil, 400, "invalid_field")
	events := fmt.Sprintf("/items/%d/events", id)
	ev := map[string]any{"kind": "deployed", "data": map[string]any{"revision": shaA}}
	e.refused(userTok, "POST", events, ev, 403, "forbidden")
	e.refused(runnerTok, "POST", events, map[string]any{"kind": "deployed", "data": map[string]any{}}, 400, "invalid_field")
	e.do(otherTok, "POST", events, ev, 201, nil)
	e.refused(runnerTok, "POST", events, ev, 409, "wrong_stage")
	if d := e.get(id); d.Item.Stage != "qa" || d.Events[len(d.Events)-1].Kind != "deployed" {
		t.Fatalf("after deployed: %s %+v", d.Item.Stage, d.Events[len(d.Events)-1])
	}
	if ids := awaiting(); len(ids) != 0 {
		t.Fatalf("after deployed: %v", ids)
	}
}

// A held item goes back to build with notes: the user's (a Slack reply that is not an approval)
// or a runner's (the held PR turned conflicting). Neither holds a lease; only held items move.
func TestRebuildHeld(t *testing.T) {
	for _, c := range []struct{ tok, actor, notes string }{
		{userTok, "user:alex", "rebase and fix the typo"},
		{runnerTok, "runner:devbox", "PR #1 conflicts with main"},
	} {
		e := newEnv(t)
		id := e.toLand("hold", []string{"M a.go"})
		e.refused(c.tok, "POST", fmt.Sprintf("/items/%d/rebuild", id), map[string]any{"notes": c.notes}, 409, "wrong_stage")
		e.mustClaim("lander", id, "land")
		e.advance(id, "hold", map[string]string{"reason": "on the hold list", "head_sha": shaA}, 200)
		e.refused(c.tok, "POST", fmt.Sprintf("/items/%d/rebuild", id), map[string]any{"notes": " "}, 400, "missing_field")
		e.do(c.tok, "POST", fmt.Sprintf("/items/%d/rebuild", id), map[string]any{"notes": c.notes}, 200, nil)
		d := e.get(id)
		last := d.Events[len(d.Events)-1]
		if d.Item.Stage != "build" || d.Item.BounceCount != 0 || d.Item.PR != 1 || last.Kind != "rebuild" ||
			last.Actor != c.actor || last.Data["notes"] != c.notes || last.From != "held" || last.To != "build" {
			t.Fatalf("%s: %+v, last %+v", c.actor, d.Item, last)
		}
	}
}

// A needs_you item goes back to build with the user's notes (a rebuild reply or the Rebuild
// button), dropping its question; a runner's rebuild (the held watch) moves only held items.
func TestRebuildNeedsYou(t *testing.T) {
	e := newEnv(t)
	id := e.addItem("x")
	e.mustClaim("t", id, "triage")
	e.upload(runnerTok, id, "e.txt", []byte("e"), 201)
	e.advance(id, "question", map[string]string{"question": "which repo?"}, 200)
	path := fmt.Sprintf("/items/%d/rebuild", id)
	e.refused(userTok, "POST", path, map[string]any{"notes": "rebuild"}, 409, "wrong_stage") // triage's question
	e.do(userTok, "POST", fmt.Sprintf("/items/%d/answer", id), map[string]any{"text": "jarvis-server"}, 200, nil)
	e.mustClaim("t", id, "triage")
	e.upload(runnerTok, id, "e.txt", []byte("e"), 201)
	e.advance(id, "reproduced", map[string]string{"area": "a", "size": "S", "risk": "normal", "resources": ""}, 200)
	e.mustClaim("builder", id, "build")
	e.advance(id, "question", map[string]string{"question": "which plug?"}, 200)
	e.refused(runnerTok, "POST", path, map[string]any{"notes": "PR #1 conflicts with main"}, 409, "wrong_stage")
	e.do(userTok, "POST", path, map[string]any{"notes": "rebuild with the kasa plug"}, 200, nil)
	d := e.get(id)
	last := d.Events[len(d.Events)-1]
	if d.Item.Stage != "build" || d.Item.Question != "" || d.Item.ReturnStage != "" || last.Kind != "rebuild" ||
		last.From != "needs_you" || last.Data["notes"] != "rebuild with the kasa plug" || last.Data["question"] != "which plug?" {
		t.Fatalf("%+v, last %+v", d.Item, last)
	}
}

// The trust OK is the user's (a runner token is refused), for the commit verify refused, and
// sends the item back to build on the hold list.
func TestTrust(t *testing.T) {
	e := newEnv(t)
	id := e.addItem("Shoot dark mode too")
	e.mustClaim("triager", id, "triage")
	e.upload(runnerTok, id, "repro.log", []byte("x\n"), 201)
	e.advance(id, "reproduced", map[string]string{"area": "harness", "size": "S", "risk": "normal", "resources": ""}, 200)
	e.mustClaim("builder", id, "build")
	e.refused(userTok, "POST", fmt.Sprintf("/items/%d/trust", id), map[string]any{"head_sha": shaA}, 409, "wrong_stage")
	e.advance(id, "failed", map[string]string{"error": "the change touches tools/pipeline", "trust_sha": shaA,
		"trusted_paths": "tools/pipeline", "trusted_diffstat": "tools/pipeline | 2 +-"}, 200)

	e.refused(runnerTok, "POST", fmt.Sprintf("/items/%d/trust", id), map[string]any{"head_sha": shaA}, 403, "forbidden")
	e.refused(userTok, "POST", fmt.Sprintf("/items/%d/trust", id), map[string]any{"head_sha": shaB}, 409, "sha_mismatch")
	e.do(userTok, "POST", fmt.Sprintf("/items/%d/trust", id), map[string]any{"head_sha": shaA}, 200, nil)
	if d := e.get(id); d.Item.Stage != "build" || d.Item.Risk != "hold" || !strings.Contains(d.Item.HoldReason, "trusted files changed") {
		t.Fatalf("after trust: %+v", d.Item)
	}
}
