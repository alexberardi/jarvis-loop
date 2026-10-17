package web

import (
	"bytes"
	"context"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/alexberardi/jarvis-loop/internal/pipeline"
	"github.com/alexberardi/jarvis-loop/internal/store"
)

// goldenWorking is a build agent with a to-do list (one entry trying to inject markup), a plan
// turn, and a lease that has sent no activity yet.
func goldenWorking() []store.Working {
	return []store.Working{
		{ItemID: 68, Title: "Board: working now", Stage: "build", Runner: "devbox", Agent: "agent-1", ClaimedAt: now.Add(-12 * time.Minute),
			Activity: &pipeline.Activity{Phase: "build agent", ElapsedS: 720, ToolCalls: 37,
				Todos: []pipeline.Todo{
					{Text: "Write the failing test", Status: pipeline.TodoCompleted},
					{Text: `Fix <script>alert("x")</script> the card`, Status: pipeline.TodoInProgress},
					{Text: "Run the race suite", Status: pipeline.TodoPending},
				},
				Recent:  []string{"Bash: Run the web tests", "Edit: internal/web/board.go"},
				Message: "The golden file needs <b>updating</b>."}},
		{PlanID: 4, Persona: "engineering", Title: "Qwen builder", Stage: "plan", Runner: "devbox", Agent: "agent-2", ClaimedAt: now.Add(-3 * time.Minute),
			Activity: &pipeline.Activity{Phase: "plan agent", Persona: "engineering", PlanID: 4, ElapsedS: 180, ToolCalls: 5}},
		{ItemID: 70, Title: "Verify me", Stage: "build", Runner: "devbox", Agent: "agent-3", ClaimedAt: now.Add(-40 * time.Minute),
			Activity: &pipeline.Activity{Phase: "verify", Step: "loop-sandbox", ElapsedS: 2400, StepElapsedS: 120}},
		{ItemID: 71, Title: "Just claimed", Stage: "triage", Runner: "devbox", Agent: "agent-4", ClaimedAt: now, Note: "waiting for the verify lock"},
	}
}

func TestWorkingGolden(t *testing.T) {
	var b bytes.Buffer
	v := boardView(goldenItems(), goldenPlans(), now, false, "")
	v.Working = workingView(goldenWorking(), now, 0)
	if err := boardTmpl.Execute(&b, v); err != nil {
		t.Fatal(err)
	}
	out := b.String()
	checkGolden(t, "board_working.golden.html", b.Bytes())
	for _, want := range []string{
		`<a class="title" href="/board/items/68">#68 Board: working now</a>`,
		`<a class="title" href="/board/plans/4">P4 Qwen builder</a>`,
		`<li class="completed">✓ Write the failing test</li>`,
		`<li class="in_progress">▶ Fix &lt;script&gt;alert(&#34;x&#34;)&lt;/script&gt; the card</li>`,
		`<li class="pending">○ Run the race suite</li>`,
		`<li>Bash: Run the web tests</li>`,
		`The golden file needs &lt;b&gt;updating&lt;/b&gt;.`,
		`<span>verify: loop-sandbox (2m)</span>`,
		`<h2>Working now · 4</h2>`,
		`data-live="/board/working"`,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("board has no %s", want)
		}
	}
	if strings.Count(out, "<script") != 1 || strings.Contains(out, "<b>updating") {
		t.Error("activity text reached the page unescaped")
	}

	// Nothing running: the section says so.
	b.Reset()
	if err := workingTmpl.ExecuteTemplate(&b, "working", workingView(nil, now, 0)); err != nil {
		t.Fatal(err)
	}
	if got := b.String(); !strings.Contains(got, "Working now · 0") || !strings.Contains(got, "Nothing running.") {
		t.Errorf("empty section %q", got)
	}
}

func TestItemWorkingGolden(t *testing.T) {
	d := store.Detail{Item: pipeline.Item{ID: 68, Title: "Board: working now", Repo: "alexberardi/jarvis-loop", Priority: "P1", Stage: pipeline.Build,
		CreatedAt: now.Add(-time.Hour), UpdatedAt: now}, Display: "build",
		Lease: &store.Lease{ID: "l1", ItemID: 68, Stage: pipeline.Build, Runner: "devbox", Agent: "agent-1"}}
	v := itemView(d, "", "", now, "")
	ws := workingView(goldenWorking()[:1], now, 68)
	v.Working = &ws
	var b bytes.Buffer
	if err := itemTmpl.Execute(&b, v); err != nil {
		t.Fatal(err)
	}
	checkGolden(t, "item_working.golden.html", b.Bytes())
	if !strings.Contains(b.String(), `data-live="/board/working?item=68"`) {
		t.Error("the item's card does not refresh from its own fragment")
	}
}

// TestWorkingLive: the fragment live.js fetches shows a live lease's to-dos while it runs and
// drops them once it ends; the pages that load live.js allow it, the others do not.
func TestWorkingLive(t *testing.T) {
	srv, st := boardSetup(t)
	ctx := context.Background()
	it, err := st.AddItem(ctx, "user:alex", store.NewItem{Title: "live one", Priority: "P1"})
	if err != nil {
		t.Fatal(err)
	}
	c, err := st.Claim(ctx, "devbox", pipeline.ClaimRequest{Runner: "devbox", Agent: "agent-1"})
	if err != nil || c == nil {
		t.Fatalf("claim %v %v", c, err)
	}
	act := &pipeline.Activity{Phase: "triage agent", Todos: []pipeline.Todo{{Text: "read the <code>", Status: pipeline.TodoInProgress}}}
	if _, err := st.HeartbeatActivity(ctx, "devbox", c.Lease.ID, "", act); err != nil {
		t.Fatal(err)
	}
	cookie := &http.Cookie{Name: "loop_board", Value: boardTok}
	for _, path := range []string{"/board/working", "/board/working?item=1"} {
		resp, body := get(t, srv.URL+path, cookie)
		if resp.StatusCode != 200 || !strings.HasPrefix(resp.Header.Get("Content-Type"), "text/html") ||
			!strings.Contains(body, "▶ read the &lt;code&gt;") || strings.Contains(body, "<html") {
			t.Fatalf("GET %s: %d %q", path, resp.StatusCode, body)
		}
	}
	if _, body := get(t, srv.URL+"/board/working?item=2", cookie); strings.Contains(body, "read the") {
		t.Fatalf("another item's fragment shows this one: %q", body)
	}
	for _, path := range []string{"/board/working?item=x", "/board/working?item=-1"} {
		if resp, _ := get(t, srv.URL+path, cookie); resp.StatusCode != 404 {
			t.Errorf("GET %s: %d", path, resp.StatusCode)
		}
	}
	if resp, _ := get(t, srv.URL+"/board/working", nil); resp.StatusCode != 401 {
		t.Errorf("fragment without the cookie: %d", resp.StatusCode)
	}
	resp, js := get(t, srv.URL+"/board/live.js", cookie)
	if resp.StatusCode != 200 || !strings.HasPrefix(resp.Header.Get("Content-Type"), "text/javascript") || !strings.Contains(js, "dataset.live") {
		t.Fatalf("live.js: %d %s", resp.StatusCode, resp.Header.Get("Content-Type"))
	}
	resp, body := get(t, srv.URL+"/board/items/1", cookie)
	if !strings.Contains(body, "▶ read the &lt;code&gt;") || !strings.Contains(resp.Header.Get("Content-Security-Policy"), "script-src 'self'") {
		t.Fatalf("item page while it runs: %s %q", resp.Header.Get("Content-Security-Policy"), body)
	}

	// The lease ends: the card goes, and the item page has no live section (nor its script).
	if _, err := st.Advance(ctx, "devbox", it.ID, pipeline.Question, map[string]string{"question": "which?"}, nil); err != nil {
		t.Fatal(err)
	}
	if _, body := get(t, srv.URL+"/board/working", cookie); strings.Contains(body, "read the") || !strings.Contains(body, "Nothing running.") {
		t.Fatalf("fragment after the lease ended: %q", body)
	}
	resp, body = get(t, srv.URL+"/board/items/1", cookie)
	if strings.Contains(body, "<script") || strings.Contains(resp.Header.Get("Content-Security-Policy"), "script-src") {
		t.Fatalf("item page after: %s", resp.Header.Get("Content-Security-Policy"))
	}
}
