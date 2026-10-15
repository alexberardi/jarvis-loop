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
	"github.com/alexberardi/jarvis-loop/internal/store"
)

const (
	userTok   = "user-token-0123456789abcdef"
	runnerTok = "runner-token-0123456789abcdef"
	otherTok  = "other-runner-token-0123456789"
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
	toks, err := ParseTokens(strings.NewReader(fmt.Sprintf("# comment\n%s\tuser\talex\n%s\trunner\tdevbox\n\n%s\trunner\tmac\n", userTok, runnerTok, otherTok)))
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
	e.advanceWith(id, "pr_opened", map[string]string{"repo": "r", "pr": "1", "branch": "b", "head_sha": shaA}, paths, 200)
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
		} `json:"hold_rules"`
	}
	e.do(runnerTok, "GET", "/control", nil, 200, &ctl)
	if len(ctl.HoldRules) != 8 || ctl.HoldRules[0].Glob != "internal/modules/auth/**" {
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
