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

func TestFlowPage(t *testing.T) {
	srv, st := boardSetup(t)
	if _, err := st.AddItem(context.Background(), "user:alex", store.NewItem{Title: "waits for triage"}); err != nil {
		t.Fatal(err)
	}
	cookie := &http.Cookie{Name: "loop_board", Value: boardTok}
	resp, body := get(t, srv.URL+"/board/flow", cookie)
	if resp.StatusCode != 200 || !strings.Contains(body, "<strong>24h</strong>") ||
		!strings.Contains(body, `<tr><td>ready</td><td class="num">1</td><td><a href="/board/items/1">#1</a> </td></tr>`) {
		t.Fatalf("flow page %d:\n%s", resp.StatusCode, body)
	}
	if csp := resp.Header.Get("Content-Security-Policy"); csp != boardCSP {
		t.Errorf("CSP %q", csp)
	}
	if resp, _ := get(t, srv.URL+"/board/flow?window=7d", cookie); resp.StatusCode != 200 {
		t.Errorf("7d: %d", resp.StatusCode)
	}
	if resp, _ := get(t, srv.URL+"/board/flow?window=1y", cookie); resp.StatusCode != 400 {
		t.Errorf("1y: %d", resp.StatusCode)
	}
	if resp, _ := get(t, srv.URL+"/board/flow", nil); resp.StatusCode != 401 {
		t.Errorf("no token: %d", resp.StatusCode)
	}
}

// goldenFlow fills every section of the panel.
func goldenFlow() store.Flow {
	since := now.Add(-6 * time.Hour)
	f := store.Flow{Window: "6h", Since: since, Now: now, Hours: 6, Done: 3, Merged: 4, ActiveAgents: 4, BusyMinutes: 612.4,
		ToMerge: pipeline.Dist{Count: 4, MedianSeconds: 5 * 3600, P90Seconds: 26 * 3600}, ToDone: pipeline.Dist{Count: 3, MedianSeconds: 6 * 3600, P90Seconds: 30 * 3600},
		WaitingTotal: 4, Waiting: []store.FlowWait{{Reason: "ready", Count: 2, Items: []int64{8, 9}}, {Reason: "limit triage:1", Count: 1, Items: []int64{4}}, {Reason: "no runner for a resource", Count: 1, Items: []int64{5}}},
		Bounces:    []store.FlowBounce{{Stage: "review", Count: 2, ByOutcome: map[string]int{"bounce": 2}, Top: []store.FlowBounceText{{Text: "add a <test>", Count: 2, Items: []int64{3, 1}}}}},
		KilledRuns: 1, LostMinutes: 45, Killed: []store.FlowKilled{{Reason: "overtime", Runs: 1, LostMinutes: 45}},
		Features: []store.FlowFeature{{PlanID: 2, Title: "Plan pages", Stage: "split", Items: 3, Done: 1, ByStage: map[string]int{"done": 1, "build": 2}}},
	}
	for i := range 6 {
		f.PerHour = append(f.PerHour, store.FlowHour{Start: since.Add(time.Duration(i) * time.Hour), Done: i % 2, Merged: []int{0, 2, 0, 1, 1, 0}[i]})
	}
	for _, s := range store.FlowStages {
		f.Stages = append(f.Stages, store.FlowStage{Stage: string(s)})
	}
	f.Stages[1] = store.FlowStage{Stage: "build", Time: pipeline.Dist{Count: 5, MedianSeconds: 40 * 60, P90Seconds: 3*3600 + 5*60}, Run: pipeline.Dist{Count: 5, MedianSeconds: 25 * 60, P90Seconds: 50}}
	idle := 180.0
	f.Runners = []store.FlowRunner{
		{Runner: "devbox", Slots: 2, LastSeen: now, Runs: 9, BusyMinutes: 540, CapacityMinutes: 720, IdleMinutes: &idle},
		{Runner: "mbp", Runs: 1, BusyMinutes: 72.4, LastSeen: now.Add(-time.Hour)},
	}
	return f
}

func TestFlowGolden(t *testing.T) {
	var b bytes.Buffer
	if err := flowTmpl.Execute(&b, flowView(goldenFlow())); err != nil {
		t.Fatal(err)
	}
	checkGolden(t, "flow.golden.html", b.Bytes())
}

func TestDur(t *testing.T) {
	for secs, want := range map[int64]string{0: "0s", 59: "59s", 60: "1m", 3599: "59m", 3600: "1h 00m", 3*3600 + 5*60: "3h 05m", 26 * 3600: "1d 02h"} {
		if got := dur(secs); got != want {
			t.Errorf("dur(%d) = %q, want %q", secs, got, want)
		}
	}
}
