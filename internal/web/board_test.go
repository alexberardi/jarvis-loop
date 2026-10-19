package web

import (
	"bytes"
	"context"
	"flag"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/alexberardi/jarvis-loop/internal/api"
	"github.com/alexberardi/jarvis-loop/internal/clock"
	"github.com/alexberardi/jarvis-loop/internal/pipeline"
	"github.com/alexberardi/jarvis-loop/internal/store"
)

var update = flag.Bool("update", false, "rewrite the golden files")

const boardTok = "board-token-0123456789abcdef"

var now = time.Date(2026, 10, 15, 12, 0, 0, 0, time.UTC)

func boardSetup(t *testing.T) (*httptest.Server, *store.Store) {
	t.Helper()
	st, err := store.Open(t.TempDir(), clock.NewFake(now))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	toks, err := api.ParseTokens(strings.NewReader(userTok + "\tuser\talex\n" + runnerTok + "\trunner\tdevbox\n" + boardTok + "\tboard\tphone\n"))
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(NewBoard(st, toks, slog.New(slog.DiscardHandler), BoardConfig{Clock: clock.NewFake(now)}))
	t.Cleanup(srv.Close)
	return srv, st
}

func get(t *testing.T, url string, cookie *http.Cookie) (*http.Response, string) {
	t.Helper()
	req, _ := http.NewRequest("GET", url, nil)
	if cookie != nil {
		req.AddCookie(cookie)
	}
	return send(t, req)
}

func TestBoardCookieFlow(t *testing.T) {
	srv, st := boardSetup(t)
	if _, err := st.AddItem(context.Background(), "user:alex", store.NewItem{Title: "wake word", Priority: "P1"}); err != nil {
		t.Fatal(err)
	}

	// The token in the query once: a cookie, then a redirect without it (other parameters kept).
	resp, _ := get(t, srv.URL+"/board?closed=1&t="+boardTok, nil)
	if resp.StatusCode != http.StatusSeeOther || resp.Header.Get("Location") != "/board?closed=1" {
		t.Fatalf("?t=: %d, Location %q", resp.StatusCode, resp.Header.Get("Location"))
	}
	set := resp.Header.Get("Set-Cookie")
	for _, want := range []string{"loop_board=" + boardTok, "Path=/board", "HttpOnly", "SameSite=Strict"} {
		if !strings.Contains(set, want) {
			t.Errorf("Set-Cookie %q lacks %q", set, want)
		}
	}
	if strings.Contains(set, "Secure") {
		t.Errorf("Secure on a plain-HTTP cookie: %q", set)
	}
	if resp.Header.Get("Referrer-Policy") != "no-referrer" {
		t.Errorf("Referrer-Policy %q", resp.Header.Get("Referrer-Policy"))
	}
	cookie := resp.Cookies()[0]

	// The cookie alone then reads the board and an item.
	resp, body := get(t, srv.URL+"/board", cookie)
	if resp.StatusCode != 200 || !strings.Contains(body, "wake word") {
		t.Fatalf("board with the cookie: %d %s", resp.StatusCode, body)
	}
	for h, want := range map[string]string{"Cache-Control": "no-store", "X-Frame-Options": "DENY", "Referrer-Policy": "no-referrer"} {
		if got := resp.Header.Get(h); got != want {
			t.Errorf("%s %q, want %q", h, got, want)
		}
	}
	// The board runs live.js, from itself only: no inline script, no other origin.
	if csp := resp.Header.Get("Content-Security-Policy"); !strings.Contains(csp, "default-src 'none'") || !strings.Contains(csp, "script-src 'self';") ||
		strings.Contains(csp, "unsafe-inline'; img") == false || strings.Contains(csp, "script-src 'self' ") {
		t.Errorf("CSP %q", csp)
	}
	if resp, body := get(t, srv.URL+"/board/items/1", cookie); resp.StatusCode != 200 || !strings.Contains(body, "wake word") {
		t.Fatalf("item with the cookie: %d %s", resp.StatusCode, body)
	}

	// The user token works too; a runner token, a wrong one and none do not.
	if resp, _ := get(t, srv.URL+"/board?t="+userTok, nil); resp.StatusCode != http.StatusSeeOther {
		t.Errorf("user token: %d", resp.StatusCode)
	}
	resp, _ = get(t, srv.URL+"/board?t="+runnerTok, nil)
	if resp.StatusCode != 403 || resp.Header.Get("Set-Cookie") != "" {
		t.Errorf("runner token: %d, Set-Cookie %q", resp.StatusCode, resp.Header.Get("Set-Cookie"))
	}
	if resp, _ := get(t, srv.URL+"/board", &http.Cookie{Name: "loop_board", Value: runnerTok}); resp.StatusCode != 403 {
		t.Errorf("runner token in the cookie: %d", resp.StatusCode)
	}
	resp, _ = get(t, srv.URL+"/board?t=not-a-token-at-all-0000", nil)
	if resp.StatusCode != 401 || resp.Header.Get("Set-Cookie") != "" {
		t.Errorf("bad token: %d, Set-Cookie %q", resp.StatusCode, resp.Header.Get("Set-Cookie"))
	}
	if resp, _ := get(t, srv.URL+"/board", &http.Cookie{Name: "loop_board", Value: "not-a-token-at-all-0000"}); resp.StatusCode != 401 {
		t.Errorf("bad cookie: %d", resp.StatusCode)
	}
	if resp, _ := get(t, srv.URL+"/board", nil); resp.StatusCode != 401 {
		t.Errorf("no token: %d", resp.StatusCode)
	}
	// Basic auth and bearer headers are not the board's sign-in.
	req, _ := http.NewRequest("GET", srv.URL+"/board", nil)
	req.Header.Set("Authorization", "Bearer "+boardTok)
	if resp, _ := send(t, req); resp.StatusCode != 401 {
		t.Errorf("bearer: %d", resp.StatusCode)
	}
}

func TestBoardSecureCookieOverTLS(t *testing.T) {
	st, err := store.Open(t.TempDir(), clock.NewFake(now))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	toks, _ := api.ParseTokens(strings.NewReader(boardTok + "\tboard\tphone\n"))
	srv := httptest.NewTLSServer(NewBoard(st, toks, slog.New(slog.DiscardHandler), BoardConfig{}))
	t.Cleanup(srv.Close)
	c := srv.Client()
	c.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	resp, err := c.Get(srv.URL + "/board?t=" + boardTok)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if !strings.Contains(resp.Header.Get("Set-Cookie"), "Secure") {
		t.Errorf("Set-Cookie over TLS lacks Secure: %q", resp.Header.Get("Set-Cookie"))
	}
}

// The board's handler serves its GET routes and nothing else: no API, no status page, no
// state change.
func TestBoardRoutesOnly(t *testing.T) {
	srv, st := boardSetup(t)
	if _, err := st.AddItem(context.Background(), "user:alex", store.NewItem{Title: "x"}); err != nil {
		t.Fatal(err)
	}
	cookie := &http.Cookie{Name: "loop_board", Value: boardTok}
	for _, path := range []string{"/", "/items", "/items/1", "/control", "/ui/control", "/items/1/evidence/1", "/board/x"} {
		if resp, _ := get(t, srv.URL+path, cookie); resp.StatusCode != 404 {
			t.Errorf("GET %s: %d, want 404", path, resp.StatusCode)
		}
	}
	for _, path := range []string{"/board", "/board/working", "/board/live.js", "/board/items/1", "/board/items/1/evidence/1", "/board/plans/1", "/board/plans/1/brief/1", "/items", "/claim", "/ui/control"} {
		for _, method := range []string{"POST", "PUT", "PATCH", "DELETE"} {
			req, _ := http.NewRequest(method, srv.URL+path, strings.NewReader("{}"))
			req.AddCookie(cookie)
			req.Header.Set("Authorization", "Bearer "+userTok)
			if resp, _ := send(t, req); resp.StatusCode != 404 && resp.StatusCode != 405 {
				t.Errorf("%s %s: %d, want 404 or 405", method, path, resp.StatusCode)
			}
		}
	}
	if resp, _ := get(t, srv.URL+"/board/items/99", cookie); resp.StatusCode != 404 {
		t.Errorf("missing item: %d", resp.StatusCode)
	}
}

func TestBoardEvidence(t *testing.T) {
	srv, st := boardSetup(t)
	ctx := context.Background()
	it, err := st.AddItem(ctx, "user:alex", store.NewItem{Title: "x", Repo: "o/r"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.Claim(ctx, "devbox", pipeline.ClaimRequest{Agent: "t", Stages: []pipeline.Stage{pipeline.Triage}}); err != nil {
		t.Fatal(err)
	}
	png := []byte("\x89PNG\r\n\x1a\n\x00\x00\x00\rIHDR\x00\x00\x00\x01\x00\x00\x00\x01\x08\x06\x00\x00\x00\x1f\x15\xc4\x89")
	if _, err := st.AddEvidence(ctx, "devbox", it.ID, "after, phone", "after.png", bytes.NewReader(png)); err != nil {
		t.Fatal(err)
	}
	if _, err := st.AddEvidence(ctx, "devbox", it.ID, "", "test.log", strings.NewReader("ok\n")); err != nil {
		t.Fatal(err)
	}
	cookie := &http.Cookie{Name: "loop_board", Value: boardTok}

	_, body := get(t, srv.URL+"/board/items/1", cookie)
	for _, want := range []string{`<img src="/board/items/1/evidence/1"`, `<a href="/board/items/1/evidence/2" download>test.log</a>`} {
		if !strings.Contains(body, want) {
			t.Errorf("item page lacks %q", want)
		}
	}
	resp, img := get(t, srv.URL+"/board/items/1/evidence/1", cookie)
	if resp.StatusCode != 200 || resp.Header.Get("Content-Type") != "image/png" ||
		!strings.HasPrefix(resp.Header.Get("Content-Disposition"), "inline") || img != string(png) {
		t.Errorf("image: %d %v", resp.StatusCode, resp.Header)
	}
	resp, _ = get(t, srv.URL+"/board/items/1/evidence/2", cookie)
	if resp.StatusCode != 200 || resp.Header.Get("Content-Type") != "application/octet-stream" ||
		resp.Header.Get("Content-Disposition") != `attachment; filename="test.log"` || resp.Header.Get("X-Content-Type-Options") != "nosniff" {
		t.Errorf("log: %d %v", resp.StatusCode, resp.Header)
	}
	for _, path := range []string{"/board/items/1/evidence/3", "/board/items/1/evidence/0", "/board/items/1/evidence/x", "/board/items/x"} {
		if resp, _ := get(t, srv.URL+path, cookie); resp.StatusCode != 404 {
			t.Errorf("%s: %d, want 404", path, resp.StatusCode)
		}
	}
}

// goldenItems has a card for every column the board fills, plus the edge cases: done 15 days ago
// (gone), closed (shown with the toggle only).
func goldenItems() []store.BoardItem {
	day := 24 * time.Hour
	row := func(id int64, title, pri, stage, display, repo string, pr int, claimed string, age time.Duration) store.BoardRow {
		return store.BoardRow{ID: id, Title: title, Priority: pri, Stage: stage, Display: display, Repo: repo, PR: pr,
			ClaimedBy: claimed, CreatedAt: now.Add(-age), AgeSeconds: int64(age / time.Second)}
	}
	return []store.BoardItem{
		{BoardRow: row(1, "Wake word <misses> at night", "P0", "triage", "intake", "", 0, "", 5*time.Minute), UpdatedAt: now},
		{BoardRow: row(2, "Route to the right repo", "P1", "triage", "triage", "alexberardi/jarvis-loop", 0, "devbox/triage-1", 2*time.Hour), UpdatedAt: now},
		{BoardRow: row(3, "Mobile settings crash", "P1", "build", "build", "alexberardi/jarvis-node-mobile", 0, "devbox/builder-2", 3*day),
			SlackChannel: "C0LOOP", SlackTS: "1760000000.000100", UpdatedAt: now},
		{BoardRow: row(4, "Retry the TTS call", "P2", "build", "ready", "alexberardi/jarvis-server", 0, "", 26*time.Hour),
			WaitingOn: []int64{12}, Waits: []pipeline.WaitChain{{Path: []int64{12, 5}, Root: "in review"}}, UpdatedAt: now},
		{BoardRow: row(12, "Show the plan link", "P2", "triage", "intake", "alexberardi/jarvis-loop", 0, "", time.Hour),
			WaitingOn: []int64{5, 8}, Waits: []pipeline.WaitChain{{Path: []int64{5}, Root: "in review"}, {Path: []int64{8}, Root: "held for approval"}}, PlanID: 2, Builder: "qwen", Feature: "pantry", UpdatedAt: now},
		{BoardRow: row(13, "Mac app progress window", "P4", "build", "ready", "alexberardi/jarvis-server", 0, "", 6*time.Hour),
			Unservable: []string{"macos"}, UpdatedAt: now},
		{BoardRow: row(14, "Pair screen copy", "P2", "build", "build", "alexberardi/jarvis-server", 0, "devbox/builder-1", 3*time.Hour),
			ReopenedDeps: []store.ReopenedDependency{{Dependency: 9, ReopenItem: 15, Words: "the code is still <small> on the phone"}}, UpdatedAt: now},
		{BoardRow: row(5, "Board page", "P2", "review", "review", "alexberardi/jarvis-loop", 14, "devbox/reviewer-1", 2*day), UpdatedAt: now},
		{BoardRow: row(6, "Bump sqlite", "P3", "land", "land (queued)", "alexberardi/jarvis-server", 31, "", 4*day), UpdatedAt: now},
		{BoardRow: row(7, "Which emulator?", "P1", "needs_you", "needs-you", "alexberardi/jarvis-server", 0, "", 1*day),
			Question: "Should the test use the sandbox emulator or a fake?", SlackChannel: "C0LOOP", SlackTS: "1760000001.000200", UpdatedAt: now},
		{BoardRow: row(8, "Token rotation", "P2", "held", "held", "alexberardi/jarvis-loop", 12, "", 6*day),
			HoldReason: "triage: risk hold; touches internal/api/auth.go", UpdatedAt: now},
		{BoardRow: row(18, "Mac runners", "P1", "qa", "qa", "alexberardi/jarvis-loop", 60, "", 2*day), UpdatedAt: now,
			Proof: &pipeline.Proof{By: "build", Checks: []pipeline.ProofCheck{{Kind: "usage", Event: "claimed", Actor: []string{"runner:mbp", "runner:mini"}, State: "pending"}}}},
		{BoardRow: row(19, "Board colours", "P2", "qa", "qa", "alexberardi/jarvis-loop", 61, "", 3*day), UpdatedAt: now,
			Proof: &pipeline.Proof{By: "sweep", Checks: []pipeline.ProofCheck{{Kind: "command", Run: "go test ./internal/web", State: "passed"}, {Kind: "manual", Look: "the QA column on a phone", State: "manual"}}}},
		{BoardRow: row(20, "Rebuild button", "P2", "qa", "qa", "alexberardi/jarvis-loop", 62, "", 3*day), UpdatedAt: now,
			Proof: &pipeline.Proof{By: "build", Checks: []pipeline.ProofCheck{{Kind: "command", Run: "scripts/loop-sandbox run scenarios/64-rebuild-button.json", State: "failed", Fails: 1, Evidence: "exit 1 after 9s: 1 of 1 failed"}}}},
		{BoardRow: row(21, "Old QA item", "P3", "qa", "qa", "alexberardi/jarvis-server", 40, "", 5*day), UpdatedAt: now, AwaitingDeploy: false},
		{BoardRow: row(9, "Fix the digest time", "P2", "done", "done", "alexberardi/jarvis-loop", 9, "", 10*day), UpdatedAt: now.Add(-13 * day)},
		{BoardRow: row(10, "Old done item", "P2", "done", "done", "alexberardi/jarvis-loop", 3, "", 30*day), UpdatedAt: now.Add(-15 * day)},
		{BoardRow: row(11, "Not worth it", "P3", "closed", "closed", "", 0, "", 8*day), UpdatedAt: now.Add(-2 * day)},
	}
}

// goldenPlans are the open plans: one waiting on the user, one on the agent.
func goldenPlans() []store.PlanRow {
	return []store.PlanRow{
		{ID: 3, Title: "Pantry <list>", Stage: pipeline.PlanBrief, Priority: "P2", Repos: []string{"alexberardi/jarvis-server"},
			Turn: "you", CreatedAt: now.Add(-3 * time.Hour), UpdatedAt: now.Add(-20 * time.Minute)},
		{ID: 2, Title: "Plan pages", Stage: pipeline.PlanSplit, Priority: "P3", Repos: []string{"alexberardi/jarvis-loop", "alexberardi/jarvis-server"},
			CreatedAt: now.Add(-50 * time.Hour), UpdatedAt: now.Add(-26 * time.Hour)},
		{ID: 1, Title: "Blueprint in progress", Stage: pipeline.PlanBlueprint, Priority: "P3", Turn: "agent",
			CreatedAt: now.Add(-5 * 24 * time.Hour), UpdatedAt: now},
	}
}

func checkGolden(t *testing.T, name string, got []byte) {
	t.Helper()
	path := filepath.Join("testdata", name)
	if *update {
		if err := os.WriteFile(path, got, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, want) {
		t.Errorf("%s differs; rerun with -update and review the diff\n%s", path, got)
	}
}

func TestBoardGolden(t *testing.T) {
	for _, closed := range []bool{false, true} {
		var b bytes.Buffer
		v := boardView(goldenItems(), goldenPlans(), now, closed, "https://jarvis.slack.com/")
		if !closed {
			// main red in one repo: the banner names it, its fix and what waits.
			v.Freezes = []store.Freeze{{Repo: "alexberardi/jarvis-server", ItemID: 40, ItemTitle: "main red in alexberardi/jarvis-server: ci <race>",
				Since: now.Add(-time.Hour), Jobs: []string{"ci", "race"}, Queued: 2}}
		}
		if err := boardTmpl.Execute(&b, v); err != nil {
			t.Fatal(err)
		}
		if !closed && !strings.Contains(b.String(), `<div class="frozen" role="alert"><strong>Main is red in alexberardi/jarvis-server</strong> (failing: ci, race)`) {
			t.Error("no red banner for the frozen repo")
		}
		name := "board_open.golden.html"
		if closed {
			name = "board_closed.golden.html"
		}
		checkGolden(t, name, b.Bytes())
		out := b.String()
		if strings.Count(out, "<script") != 1 || !strings.Contains(out, `<script src="/board/live.js" defer></script>`) ||
			strings.Contains(out, "http-equiv=\"refresh\" content=\"30\"") == false {
			t.Errorf("%s: a script other than live.js, or no 30 s refresh", name)
		}
		if strings.Contains(out, "Old done item") {
			t.Errorf("%s shows an item done 15 days ago", name)
		}
		if strings.Contains(out, "Not worth it") != closed {
			t.Errorf("%s: closed item shown %v, want %v", name, !closed, closed)
		}
	}
}

func TestBoardColumns(t *testing.T) {
	v := boardView(goldenItems(), goldenPlans(), now, false, "")
	got := map[string][]int64{}
	var names []string
	var plans []int64
	for _, c := range v.Columns {
		names = append(names, c.Name)
		for _, cd := range c.Cards {
			got[c.Name] = append(got[c.Name], cd.ID)
		}
		for _, pc := range c.Plans {
			if c.Name != "Planning" {
				t.Errorf("plan P%d in %s", pc.ID, c.Name)
			}
			plans = append(plans, pc.ID)
		}
		if c.Name == "Planning" && (c.Note != "" || c.Count() != 3) {
			t.Errorf("Planning: note %q, count %d", c.Note, c.Count())
		}
	}
	if !slicesEqual(plans, []int64{3, 2, 1}) || v.PlanCount != 3 {
		t.Errorf("plans %v (count %d), want [3 2 1]", plans, v.PlanCount)
	}
	if strings.Join(names, ",") != "Planning,Triage,Build,Review,Needs you,Approval,QA,Done" {
		t.Errorf("columns %v", names)
	}
	want := map[string][]int64{"Triage": {1, 2, 12}, "Build": {3, 4, 13, 14}, "Review": {5, 6}, "Needs you": {7}, "Approval": {8}, "QA": {18, 19, 21, 20}, "Done": {9}}
	for k, ids := range want {
		if !slicesEqual(got[k], ids) {
			t.Errorf("%s: %v, want %v", k, got[k], ids)
		}
	}
	if len(got["Planning"]) != 0 {
		t.Errorf("planning holds items: %v", got)
	}
	// The QA column's cards say their group and why.
	for _, c := range v.Columns {
		if c.Name != "QA" {
			continue
		}
		var lines []string
		for _, cd := range c.Cards {
			lines = append(lines, cd.Label+": "+cd.Reason)
		}
		want := "waiting for a proof: waits for: usage: `claimed` by runner:mbp or runner:mini|" +
			"needs you: look at the QA column on a phone|needs you: no proof: your check|" +
			"failed: failed once, checked again before it reopens: command: `scripts/loop-sandbox run scenarios/64-rebuild-button.json`: exit 1 after 9s: 1 of 1 failed"
		if got := strings.Join(lines, "|"); got != want {
			t.Errorf("QA cards:\n%s\nwant\n%s", got, want)
		}
	}
	// No open plans: the Planning column says so.
	if c := boardView(goldenItems(), nil, now, false, "").Columns[0]; c.Name != "Planning" || c.Note != "No open plans." || c.Count() != 0 {
		t.Errorf("empty Planning: %+v", c)
	}
	// No workspace configured: no Slack links.
	for _, c := range v.Columns {
		for _, cd := range c.Cards {
			if cd.SlackURL != "" {
				t.Errorf("#%d has a Slack link without a workspace", cd.ID)
			}
		}
	}
}

func slicesEqual(a, b []int64) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func TestItemGolden(t *testing.T) {
	at := now.Add(-2 * time.Hour)
	d := store.Detail{
		Item: pipeline.Item{ID: 8, Title: "Token rotation", Ask: "Rotate the tokens <weekly>.\nKeep the old one a day.",
			Repo: "alexberardi/jarvis-loop", Priority: "P2", Stage: pipeline.Held, PR: 12, Risk: "hold",
			HoldReason: "triage: risk hold", Summary: "Adds a rotate command.", PlanID: 2, PlanPiece: 3, CreatedAt: at, UpdatedAt: now},
		Display: "held",
		Events: []store.Event{
			{ID: 1, ItemID: 8, At: at, Actor: "user:alex", Kind: "created", To: "triage", Data: map[string]any{"title": "Token rotation", "priority": "P2"}},
			{ID: 2, ItemID: 8, At: now, Actor: "runner:devbox", Kind: "advanced", From: "land", To: "held", Outcome: "pr_opened", Data: map[string]any{"pr": 12.0}},
		},
		Evidence: []store.Evidence{
			{ItemID: 8, N: 1, Label: "after, phone", Name: "after.png", Size: 20480, ContentType: "image/png", At: now},
			{ItemID: 8, N: 2, Name: "results.md", Size: 912, ContentType: "text/plain; charset=utf-8", At: now},
		},
	}
	var b bytes.Buffer
	if err := itemTmpl.Execute(&b, itemView(d, "C0LOOP", "1760000001.000200", now, "https://jarvis.slack.com")); err != nil {
		t.Fatal(err)
	}
	checkGolden(t, "item_held.golden.html", b.Bytes())
}

// A qa item's page shows its QA group and its proof: each check with its state and evidence.
func TestItemProofGolden(t *testing.T) {
	at := now.Add(-time.Hour)
	d := store.Detail{
		Item: pipeline.Item{ID: 16, Title: "Rebuild button", Repo: "alexberardi/jarvis-loop", Priority: "P2", Stage: pipeline.QA, PR: 62,
			Risk: "normal", CreatedAt: at, UpdatedAt: now, Proof: &pipeline.Proof{By: "build", CheckedAt: &at, Checks: []pipeline.ProofCheck{
				{Kind: "usage", Event: "rebuild", Match: map[string][]string{"source": {"needs_you"}}, State: "passed", Evidence: "seen 2 time(s) since the merge (newest: event 913)", CheckedAt: &at},
				{Kind: "command", Run: "scripts/loop-sandbox run scenarios/64-rebuild-button.json", State: "failed", Fails: 1, Evidence: "exit 1 after 9s: 1 of 1 failed\n<details>", CheckedAt: &at},
				{Kind: "manual", Look: "the button's colour", State: "manual"},
			}}},
		Display: "qa",
	}
	var b bytes.Buffer
	if err := itemTmpl.Execute(&b, itemView(d, "", "", now, "")); err != nil {
		t.Fatal(err)
	}
	checkGolden(t, "item_qa.golden.html", b.Bytes())
}

// goldenOrch is the orchestrator's panel: an action of each shape, an open escalation, a ticket it
// filed (open) and one done, and notes whose markdown must not let HTML through.
func goldenOrch() store.OrchBoard {
	ev := func(id, item int64, ago time.Duration, actor, kind, from, to string, data map[string]any) store.Event {
		return store.Event{ID: id, ItemID: item, At: now.Add(-ago), Actor: actor, Kind: kind, From: from, To: to, Data: data}
	}
	const o = "orchestrator:loop"
	esc := ev(7, 7, 10*time.Minute, o, store.KindOrchEscalated, "", "", map[string]any{
		"text": "Sandbox emulator or a fake?", "recommendation": "the fake: <the emulator> is flaky", "reason": "a product call"})
	return store.OrchBoard{
		Actions: []store.Event{
			ev(9, 7, 5*time.Minute, o, "refused", "", "", map[string]any{"action": "approve", "code": "forbidden", "message": "approvals stay on the button"}),
			ev(8, 4, 6*time.Minute, "slack:U0APPROVER", "patched", "", "", map[string]any{"via": o, "reason": "the user asked"}),
			esc,
			ev(6, 13, time.Hour, o, store.KindOrchAdded, "", "", map[string]any{"signature": "index.lock", "reason": "three runs hit it"}),
			ev(5, 3, 2*time.Hour, o, "retried", "needs_you", "build", map[string]any{"reason": "transient git lock"}),
			ev(4, 0, 3*time.Hour, "loopd", store.KindOrchEscalated, "", "", map[string]any{"text": "failure 3 on the same head", "reason": "the third failure on one head is the user's"}),
		},
		Escalations: []store.Event{esc},
		Tickets:     []int64{9, 13},
		Notes: []store.Event{
			ev(2, 0, 4*time.Hour, o, store.KindOrchNote, "", "", map[string]any{"text": "**#35** waits on the emulator <script>alert(1)</script>"}),
			ev(3, 0, 3*time.Hour, o, store.KindOrchNote, "", "", map[string]any{"text": "- git lock: 3 runs\n- [docs](javascript:alert(1))"}),
		},
	}
}

func TestBoardOrchestratorGolden(t *testing.T) {
	items := append(goldenItems(), store.BoardItem{BoardRow: store.BoardRow{ID: 13, Title: "Clear stale git locks", Priority: "P2",
		Stage: "build", Display: "ready", CreatedAt: now.Add(-time.Hour), AgeSeconds: 3600}, UpdatedAt: now})
	v := boardView(items, goldenPlans(), now, false, "")
	v.Orch = orchView(goldenOrch(), items)
	var b bytes.Buffer
	if err := boardTmpl.Execute(&b, v); err != nil {
		t.Fatal(err)
	}
	checkGolden(t, "board_orchestrator.golden.html", b.Bytes())
	out := b.String()
	for _, bad := range []string{"<script>alert", "javascript:", "<the emulator>"} {
		if strings.Contains(out, bad) {
			t.Errorf("the panel lets %q through", bad)
		}
	}
	for _, want := range []string{"<strong>#35</strong>", "approvals stay on the button", "transient git lock", "Clear stale git locks",
		"waiting on #12 → #5 → in review", "the fake: &lt;the emulator&gt; is flaky", "via orchestrator"} {
		if !strings.Contains(out, want) {
			t.Errorf("the panel lacks %q", want)
		}
	}
	// It never acted: no panel.
	v.Orch = orchView(store.OrchBoard{}, items)
	b.Reset()
	if err := boardTmpl.Execute(&b, v); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(b.String(), "Orchestrator") {
		t.Error("a panel for an orchestrator that never acted")
	}
}

// The board shows the Gemini second opinions in total (its header), per item (the card) and on
// the item's page, so the user can judge whether to turn blocking on.
func TestBoardSecondOpinion(t *testing.T) {
	srv, st := boardSetup(t)
	ctx := context.Background()
	it, err := st.AddItem(ctx, "user:alex", store.NewItem{Title: "x", Repo: "o/r"})
	if err != nil {
		t.Fatal(err)
	}
	cookie := &http.Cookie{Name: "loop_board", Value: boardTok}
	if _, body := get(t, srv.URL+"/board", cookie); strings.Contains(body, "Gemini") {
		t.Fatal("second opinion shown with none recorded")
	}
	if _, err := st.Claim(ctx, "devbox", pipeline.ClaimRequest{Agent: "t", Stages: []pipeline.Stage{pipeline.Triage}}); err != nil {
		t.Fatal(err)
	}
	if _, err := st.RunnerEvent(ctx, "devbox", it.ID, store.KindSecondOpinion, map[string]any{"status": "ok", "findings": 4, "judged": 4,
		"agreed": 3, "high": 1, "medium": 2, "low": 1, "agreed_high": 1, "agreed_medium": 1, "agreed_low": 1, "bounced": false}); err != nil {
		t.Fatal(err)
	}
	line := "1 review · 4 findings: 1 high, 2 medium, 1 low · Claude agreed with 3 of 4 judged (75%): 1 high, 1 medium, 1 low"
	_, body := get(t, srv.URL+"/board", cookie)
	for _, want := range []string{"Second opinion (Gemini): " + line, ">Gemini 4 · 3 agreed</span>"} {
		if !strings.Contains(body, want) {
			t.Errorf("board lacks %q", want)
		}
	}
	if _, body := get(t, srv.URL+"/board/items/1", cookie); !strings.Contains(body, "<h2>Second opinion (Gemini)</h2>\n<p>"+line+"</p>") {
		t.Errorf("item page lacks the second opinion:\n%s", body)
	}
}

func goldenUsage() store.UsageReport {
	fc := now.Add(50 * time.Minute)
	return store.UsageReport{
		Now: now,
		Claude: pipeline.UsageState{Now: now, WindowStart: now.Add(-3 * time.Hour), WindowEnd: now.Add(2 * time.Hour), Burn5h: 8_800_000, Limit5h: 10_000_000,
			WeekStart: now.Add(-WeekAgo), BurnWeek: 41_000_000},
		Pace:     pipeline.Pace{Level: pipeline.PaceSlow, Agents: 2, Window: pipeline.Window5h, Percent: 88},
		Forecast: &fc,
		Days: []store.UsageTotal{
			{Key: "2026-10-15", Provider: "claude", Burn: 9_100_000, Tokens: 60_000_000, CostUSD: 12.5, Runs: 14},
			{Key: "2026-10-15", Provider: "qwen", Burn: 1_200_000, Tokens: 1_200_000, Runs: 3},
			{Key: "2026-10-14", Provider: "claude", Burn: 31_900_000, Tokens: 200_000_000, CostUSD: 40.25, Runs: 1},
		},
		Items: []store.UsageItem{{ItemID: 12, Title: "Rework the <hold> list", Burn: 6_400_000, CostUSD: 9.1, Runs: 4}},
		Hits:  []pipeline.UsageHit{{At: now.Add(-26 * time.Hour), Provider: "claude", Window: pipeline.Window5h, Burn: 10_000_000, ResetAt: now.Add(-24 * time.Hour)}},
	}
}

// WeekAgo is a rolling week.
const WeekAgo = 7 * 24 * time.Hour

func TestBoardUsageGolden(t *testing.T) {
	v := boardView(goldenItems(), goldenPlans(), now, false, "")
	v.Usage = usageView(goldenUsage())
	var b bytes.Buffer
	if err := boardTmpl.Execute(&b, v); err != nil {
		t.Fatal(err)
	}
	checkGolden(t, "board_usage.golden.html", b.Bytes())
	out := b.String()
	for _, want := range []string{
		"Usage · claude pacing: the 5h window is at 88% of its budget; 2 at once",
		"5h window: 8.8M of 10.0M (88%)", "at this rate the 5h window runs out at",
		"Week: 41.0M (no limit known yet) · rolling 7 days",
		"claude 9.1M burn, $12.50, 14 runs · qwen 1.2M tokens, 3 runs", "claude 31.9M burn, $40.25, 1 run</li>",
		"#12 Rework the &lt;hold&gt; list", "5h limit at 10.0M burn",
		`<details class="orch usage" open>`,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("the usage panel lacks %q", want)
		}
	}
	// No usage yet: no panel.
	v.Usage = usageView(store.UsageReport{})
	b.Reset()
	if err := boardTmpl.Execute(&b, v); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(b.String(), "Usage ·") {
		t.Error("a usage panel with no usage")
	}
}

// The runner panel: the runners seen today and the slot held for the Mac's work (#192).
func TestBoardRunnersGolden(t *testing.T) {
	items := goldenItems()
	v := boardView(items, goldenPlans(), now, false, "")
	v.Runners = runnerView(store.RunnerPanel{
		Runners: []pipeline.RunnerSeen{
			{Name: "devbox", Capabilities: []string{"emulator", "gpu", "voice"}, PollSeconds: 30, LastSeen: now.Add(-5 * time.Second)},
			{Name: "mbp", Capabilities: []string{"macos"}, OnlyResources: []string{"macos"}, PollSeconds: 30, LastSeen: now.Add(-20 * time.Second)},
			{Name: "old-mini", Capabilities: []string{"ios"}, OnlyResources: []string{"ios"}, LastSeen: now.Add(-3 * time.Hour)},
		},
		Holds: []pipeline.SlotHold{{Resource: "macos", Runner: "mbp", ItemID: 61}},
	}, now)
	var b bytes.Buffer
	if err := boardTmpl.Execute(&b, v); err != nil {
		t.Fatal(err)
	}
	checkGolden(t, "board_runners.golden.html", b.Bytes())
	out := b.String()
	for _, want := range []string{"Runners · 3 seen today · 1 slot held", "A slot is held for macos work: <a href=\"/board/items/61\">#61</a> is waiting for mbp",
		"old-mini", "(quiet)"} {
		if !strings.Contains(out, want) {
			t.Errorf("the panel lacks %q", want)
		}
	}
	if strings.Count(out, "(quiet)") != 1 {
		t.Errorf("only old-mini is quiet")
	}
}

func TestBoardRunwayLine(t *testing.T) {
	v := boardView(goldenItems(), goldenPlans(), now, false, "")
	v.Runway = runwayView(store.RunwayReport{Hours: 1.5, Runnable: 2, Threshold: 3, Unblockers: []store.RunwayUnblocker{
		{Unblocker: pipeline.Unblocker{ItemID: 184, Action: pipeline.UnblockApprove, Frees: 4}},
		{Unblocker: pipeline.Unblocker{ItemID: 144, Action: pipeline.UnblockTrust, Frees: 2}},
		{Unblocker: pipeline.Unblocker{ItemID: 7, Action: pipeline.UnblockAnswer, Frees: 1}},
		{Unblocker: pipeline.Unblocker{ItemID: 8, Action: pipeline.UnblockAnswer, Frees: 1}},
	}})
	var b strings.Builder
	if err := boardTmpl.Execute(&b, v); err != nil {
		t.Fatal(err)
	}
	got := b.String()
	for _, want := range []string{"<strong>Runway low:</strong> about 1.5 agent-hours of work without you (2 runnable)",
		`approve <a href="/items/184">#184</a> → 4, trust <a href="/items/144">#144</a> → 2, answer <a href="/items/7">#7</a> → 1</p>`} {
		if !strings.Contains(got, want) {
			t.Errorf("board lacks %q", want)
		}
	}
	if strings.Contains(got, "#8</a>") {
		t.Error("board shows more than the top 3 unblockers")
	}
}

// The Done column's Verified filter (docs/decisions/2026-10-18-done-proofs.md): each done card says
// its verification status and why; the filter links count every done item, and a filter shows all
// the done items in that status, however old (go test ./internal/web -run BoardVerify -update).
func TestBoardVerifyFilter(t *testing.T) {
	const day = 24 * time.Hour
	done := func(id int64, title, verify, reason string, age time.Duration) store.BoardItem {
		r := store.BoardRow{ID: id, Title: title, Priority: "P2", Stage: "done", Display: "done", Repo: "alexberardi/jarvis-loop", PR: int(id),
			CreatedAt: now.Add(-age), AgeSeconds: int64(age / time.Second)}
		return store.BoardItem{BoardRow: r,
			UpdatedAt: now.Add(-age), Verify: verify, VerifyReason: reason}
	}
	items := []store.BoardItem{
		done(30, "Fresh and unproven", pipeline.DoneUnproven, "", day),
		done(31, "Proved by its usage", pipeline.DoneVerified, "Proof passed: usage: `claimed` (passed)", 2*day),
		done(32, "Old and failed", pipeline.DoneFailed, "Proof of the merged change failed 2 times in a row: command: `go test ./x`: exit 1 (filed #40)", 40*day),
		done(33, "Old and unproven", pipeline.DoneUnproven, "", 60*day),
		done(34, "Colours", pipeline.DoneNeedsYou, "look at the board on a phone", 3*day),
	}
	all := boardViewVerify(items, nil, now, false, "", "")
	col := all.Columns[len(all.Columns)-1]
	if col.Name != "Done" || col.Count() != 3 {
		t.Fatalf("unfiltered Done column: %+v", col)
	}
	if c := col.Cards[0]; c.Label != "done · unproven" {
		t.Fatalf("card label %q", c.Label)
	}
	want := []verifyFilter{{"unproven", 2, false}, {"verified", 1, false}, {"failed", 1, false}, {"needs-you", 1, false}}
	if !slices.Equal(col.Filters, want) {
		t.Fatalf("filters %+v, want %+v", col.Filters, want)
	}
	v := boardViewVerify(items, nil, now, false, "", pipeline.DoneUnproven)
	col = v.Columns[len(v.Columns)-1]
	if col.Count() != 2 || col.Cards[0].ID != 30 || col.Cards[1].ID != 33 || !col.Filters[0].On {
		t.Fatalf("unproven filter: %+v", col)
	}
	if bogus := boardViewVerify(items, nil, now, false, "", "nope"); bogus.Columns[len(bogus.Columns)-1].Count() != 3 {
		t.Fatal("an unknown filter is no filter")
	}
	v = boardViewVerify(items, nil, now, false, "", pipeline.DoneFailed)
	var b bytes.Buffer
	if err := boardTmpl.Execute(&b, v); err != nil {
		t.Fatal(err)
	}
	checkGolden(t, "board_done_failed.golden.html", b.Bytes())
}
