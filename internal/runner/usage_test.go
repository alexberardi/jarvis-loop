package runner

import (
	"bufio"
	"os"
	"testing"
)

func playStream(t *testing.T, path string) (*Runner, *run, *streamStats) {
	t.Helper()
	r := &Runner{Clock: NewFakeClock(t0), Cfg: &Config{}}
	ru := &run{stage: StageBuild, wt: t.TempDir(), dir: t.TempDir(), started: t0}
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	var st streamStats
	sc := bufio.NewScanner(f)
	sc.Buffer(nil, 1<<20)
	for sc.Scan() {
		if why, detail := r.handle(ru, &st, sc.Bytes()); why != "" {
			t.Fatalf("handle: %s %s", why, detail)
		}
	}
	return r, ru, &st
}

// TestRunUsageFields plays a recorded claude stream: the run's advance carries the model, the
// usage and cost, and the CLI's last rate-limit reading (the early warning).
func TestRunUsageFields(t *testing.T) {
	r, ru, st := playStream(t, "testdata/usage_stream.jsonl")
	f := r.metrics(ru, outcome{stats: *st}, map[string]string{})
	want := map[string]string{
		"model":                  "claude-opus-5-5",
		"total_cost_usd":         "1.8342",
		"usage_input":            "4210",
		"usage_output":           "18800",
		"usage_cache_creation":   "61200",
		"usage_cache_read":       "1450000",
		"rate_limit_status":      "allowed_warning",
		"rate_limit_type":        "five_hour",
		"rate_limit_utilization": "0.91",
		"rate_limit_resets_at":   "2026-10-15T15:45:00Z",
	}
	for k, v := range want {
		if f[k] != v {
			t.Errorf("%s = %q, want %q", k, f[k], v)
		}
	}
}

// TestLimitHitWindow: the fake's (and the CLI's) shape, a rate_limit_event with a reset and no
// type, then the session-limit text: the hit keeps the exact reset and learns the window from the
// text.
func TestLimitHitWindow(t *testing.T) {
	for _, tc := range []struct {
		name  string
		lines []string
		want  LimitHit
	}{
		{"event then text", []string{
			`{"type":"rate_limit_event","rate_limit_info":{"status":"rejected","resetsAt":1792079100}}`,
			`{"type":"assistant","error":"rate_limit","message":{"content":[{"type":"text","text":"You've hit your session limit · resets 3:45pm"}]}}`,
		}, LimitHit{Error: "rate limit: rejected", Reset: "2026-10-15T15:45:00Z", Window: "5h"}},
		{"typed event", []string{
			`{"type":"rate_limit_event","rate_limit_info":{"status":"rejected","resetsAt":1792079100,"rateLimitType":"seven_day"}}`,
		}, LimitHit{Error: "rate limit: rejected", Reset: "2026-10-15T15:45:00Z", Window: "week"}},
		{"weekly text only", []string{
			`{"type":"assistant","message":{"content":[{"type":"text","text":"You've hit your weekly limit · resets Mon 12:00am"}]}}`,
		}, LimitHit{Error: "You've hit your weekly limit · resets Mon 12:00am", Reset: "Mon 12:00am", Window: "week"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := &Runner{Clock: NewFakeClock(t0), Cfg: &Config{}}
			ru := &run{stage: StageBuild, wt: t.TempDir(), dir: t.TempDir(), started: t0}
			var st streamStats
			for _, l := range tc.lines {
				r.handle(ru, &st, []byte(l))
			}
			if st.limit == nil || *st.limit != tc.want {
				t.Fatalf("limit = %+v, want %+v", st.limit, tc.want)
			}
		})
	}
}
