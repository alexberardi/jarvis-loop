package slack

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/alexberardi/jarvis-loop/internal/pipeline"
	"github.com/alexberardi/jarvis-loop/internal/store"
)

var update = flag.Bool("update", false, "rewrite the golden files")

func TestStandupBoundary(t *testing.T) {
	ny, _ := time.LoadLocation("America/New_York")
	cases := []struct{ now, want time.Time }{
		{time.Date(2026, 10, 14, 8, 59, 0, 0, ny), time.Date(2026, 10, 13, 9, 0, 0, 0, ny)},
		{time.Date(2026, 10, 14, 9, 0, 0, 0, ny), time.Date(2026, 10, 14, 9, 0, 0, 0, ny)},
		{time.Date(2026, 10, 14, 23, 59, 0, 0, ny), time.Date(2026, 10, 14, 9, 0, 0, 0, ny)},
		// The day the clocks go back (2026-11-01): still 09:00 on the local clock.
		{time.Date(2026, 11, 1, 10, 0, 0, 0, ny), time.Date(2026, 11, 1, 9, 0, 0, 0, ny)},
	}
	for _, c := range cases {
		if got := standupBoundary(c.now, ny); !got.Equal(c.want) {
			t.Errorf("boundary(%v) = %v, want %v", c.now, got, c.want)
		}
	}
}

// The standup's text, every section (a golden file: go test ./internal/slack -run StandupText -update).
func TestStandupText(t *testing.T) {
	ny, _ := time.LoadLocation("America/New_York")
	d := standupData{
		At:     time.Date(2026, 10, 18, 9, 0, 0, 0, ny),
		Merged: []standupLine{{ID: 12, Title: "Round the kitchen timer"}},
		Night: []nightLine{
			{ID: 21, Action: "sent it back to build to re-run verify on `abc1234` → build", Reason: "scenario 12 regressed on the 9B model; the PR only touches docs/", Undo: `loop rebuild 21 --notes "…" (your notes replace mine)`},
			{ID: 22, Action: "answered with my recommendation (doc-only) and filed #23 (P3) for the rest → build", Reason: "the narrower option", Undo: `loop drop 23, then loop rebuild 22 --notes "…" with the scope you want`},
		},
		InFlight: []standupLine{{ID: 14, Title: "Support Kasa plugs", Note: "build, working · 2h in stage"}},
		Waiting:  []standupLine{{ID: 16, Title: "Kasa scenes", Note: "waiting on #17 → needs you"}},
		NeedsU: []standupLine{
			{ID: 17, Title: "Pick the plug API", Note: "needs you: local or cloud? · I recommend: local, because it works offline"},
			{ID: 18, Title: "Rotate the <tokens>", Note: "held for your approval: touches auth"},
			{ID: 12, Title: "Round the kitchen timer", Note: "merged, waiting for your check"},
		},
		Did:      map[string]int{"retried": 2, "escalated": 1, "refused answer": 1, "note": 3},
		DidLines: []string{"retried #14: git index.lock", "escalated #17: a product question", "refused answer #17: at most 2 answers per head"},
		Turns:    12, Cap: 150, Cost: 1.234, TokIn: 1_250_000, TokOu: 40_400,
		Done: store.DoneReport{Total: 134,
			Counts: map[string]int{pipeline.DoneUnproven: 90, pipeline.DoneProving: 8, pipeline.DoneVerified: 30, pipeline.DoneFailed: 1, pipeline.DoneNeedsYou: 2, pipeline.DoneWaiting: 3},
			Failed: []store.DoneReportItem{{ID: 61, Title: "Board QA column", Reopen: 140,
				Reason: "Proof of the merged change failed 2 times in a row: command: `go test ./internal/web -run TestBoardQA`: exit 1 (filed #140)"}},
			NeedsYou: []store.DoneReportItem{{ID: 19, Title: "Board colours", Looks: []string{"the board on a phone, dark mode"}},
				{ID: 33, Title: "Kitchen timer voice", Looks: []string{"say \"timer 5 minutes\" in the kitchen", "the chime"}}},
		},
	}
	got := standupText(d, ny)
	path := filepath.Join("testdata", "standup.golden.txt")
	if *update {
		os.MkdirAll("testdata", 0o755)
		if err := os.WriteFile(path, []byte(got), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err, "(run with -update)")
	}
	if got != string(want) {
		t.Errorf("standup differs from %s; review and rerun with -update:\n%s", path, got)
	}
	empty := standupText(standupData{At: d.At, Did: map[string]int{}, Cap: 150}, ny)
	if strings.Contains(empty, "Night shift") {
		t.Errorf("a standup with no night actions has a Night shift section:\n%s", empty)
	}
	if i, j := strings.Index(got, "*Night shift (2)*"), strings.Index(got, "*Merged yesterday"); i < 0 || i > j {
		t.Errorf("the night shift's section is not at the top:\n%s", got)
	}
	for _, w := range []string{"Nothing open, nothing merged.", "*What I did*\nNothing.", "0 turns (cap 150 a day)\n"} {
		if !strings.Contains(empty, w) {
			t.Errorf("an empty standup lacks %q:\n%s", w, empty)
		}
	}
}

// The standup posts once at 09:00 in #loop-digest, from the @orchestrator app, from the board and
// the window's events: not again that day, nor after a restart; the next day again.
func TestStandup(t *testing.T) {
	e := newOrchEnv(t) // t0: 08:00 in New York
	e.b.Tick(e.ctx)    // the first tick only starts the clock
	if _, orch := e.orchCalls(); len(only(t, orch, "chat.postMessage")) != 0 {
		t.Fatalf("a standup before 09:00: %+v", orch)
	}
	idM := e.add("merged one")
	e.toQA(idM)
	idQ := e.add("asks")
	e.toNeedsYou(idQ, "which repo?")
	idH := e.add("held one")
	e.toHeld(idH, shaA)
	idB := e.add("building one")
	e.runner(idB, "a1", pipeline.Reproduced, map[string]string{"area": "a", "size": "S", "risk": "normal", "resources": ""})
	idW := e.add("waits") // left in triage: it waits on #idQ
	if _, err := e.st.OrchLink(e.ctx, orchActor, idW, idQ, "needs its answer"); err != nil {
		t.Fatal(err)
	}
	if _, err := e.st.OrchEscalate(e.ctx, orchActor, idQ, "which repo is it?", "jarvis-server, because it names the API", "a product question"); err != nil {
		t.Fatal(err)
	}
	e.clk.Advance(10 * time.Minute) // one escalation per 10 minutes
	if _, err := e.st.OrchEscalate(e.ctx, orchActor, 0, "the git host is down", "wait an hour", "every run fails"); err != nil {
		t.Fatal(err)
	}
	if err := e.st.Record(e.ctx, store.Event{Actor: "runner:devbox", Kind: store.KindOrchDone,
		Data: map[string]any{"total_cost_usd": 0.42, "usage_input": 100000, "usage_output": 4000}}); err != nil {
		t.Fatal(err)
	}
	e.orchCalls()

	e.clk.Advance(50 * time.Minute) // 09:00
	e.b.Tick(e.ctx)
	loop, orch := e.orchCalls()
	posts := only(t, orch, "chat.postMessage")
	if len(posts) != 1 || posts[0].Body["channel"] != ChannelDigest || len(only(t, loop, "chat.postMessage")) != 0 {
		t.Fatalf("standup: orchestrator %+v, loop bot %+v", posts, loop)
	}
	txt := allText(posts[0])
	for _, w := range []string{
		"*Standup* · Wed Oct 14 (New York) · since Tue 09:00",
		fmt.Sprintf("*Merged yesterday (1)*\n• #%d merged one\n", idM),
		fmt.Sprintf("*In flight (1)*\n• #%d building one — build · 1h in stage\n", idB),
		fmt.Sprintf("*Waiting (1)*\n• #%d waits — waiting on #%d → needs you\n", idW, idQ),
		fmt.Sprintf("• #%d asks — needs you: which repo? · I recommend: jarvis-server, because it names the API\n", idQ),
		fmt.Sprintf("• #%d held one — held for your approval", idH),
		fmt.Sprintf("• #%d merged one — merged, waiting for your check\n", idM),
		"• the git host is down — I recommend: wait an hour\n",
		"*What I did*\nescalated 2 · ",
		fmt.Sprintf("• escalated #%d: a product question\n", idQ),
		"• escalated: every run fails\n",
		"*My spend*\n1 turn (cap 150 a day) · $0.42 · 100k tokens in, 4k out\n",
	} {
		if !strings.Contains(txt, w) {
			t.Errorf("standup lacks %q:\n%s", w, txt)
		}
	}

	e.clk.Advance(time.Minute)
	e.b.Tick(e.ctx)
	e.newBot().Tick(e.ctx)
	if loop, orch := e.orchCalls(); len(only(t, orch, "chat.postMessage")) != 0 || len(digestStandups(t, loop)) != 0 {
		t.Fatalf("a second standup the same day: %+v %+v", orch, loop)
	}
	// Parked now, it has waited more than a day by the next standup.
	if _, err := e.st.Wait(e.ctx, "user:alex", idB, []int64{idQ}, nil, "needs its answer"); err != nil {
		t.Fatal(err)
	}
	e.clk.Advance(24 * time.Hour)
	e.b.Tick(e.ctx)
	_, orch = e.orchCalls()
	if posts := only(t, orch, "chat.postMessage"); len(posts) != 1 || !strings.Contains(allText(posts[0]), "*Standup* · Thu Oct 15") ||
		strings.Contains(allText(posts[0]), "Merged yesterday") ||
		!strings.Contains(allText(posts[0]), fmt.Sprintf("*Waiting over a day (1)*\n• #%d building one — parked, waiting on #%d · 24h\n", idB, idQ)) {
		t.Fatalf("the next day's standup: %+v", posts)
	}
}

// Without the @orchestrator app the loop bot posts it; one Slack refuses is tried on the next
// ticks, then skipped.
func TestStandupFromTheLoopBotAndSkippedAfterRetries(t *testing.T) {
	e := newBotEnv(t)
	e.b.Tick(e.ctx)
	e.clk.Advance(time.Hour)
	e.b.Tick(e.ctx)
	if posts := digestStandups(t, e.calls()); len(posts) != 1 {
		t.Fatalf("standup %+v", posts)
	}
	e.clk.Advance(24 * time.Hour)
	e.f.failNext("chat.postMessage", "not_in_channel", "not_in_channel", "not_in_channel")
	for i := 0; i < 3; i++ {
		if last, _ := e.st.LastStandup(e.ctx); !last.Equal(t0.Add(time.Hour)) {
			t.Fatalf("attempt %d: moved on before giving up: %v", i, last)
		}
		e.b.Tick(e.ctx)
	}
	if last, _ := e.st.LastStandup(e.ctx); !last.Equal(t0.Add(25 * time.Hour)) {
		t.Fatalf("last standup %v", last)
	}
	if !strings.Contains(e.logs.String(), "standup not posted") {
		t.Fatal("not logged")
	}
}

func digestStandups(t *testing.T, calls []call) []call {
	t.Helper()
	var out []call
	for _, c := range only(t, calls, "chat.postMessage") {
		if strings.HasPrefix(allText(c), "*Standup*") {
			out = append(out, c)
		}
	}
	return out
}
