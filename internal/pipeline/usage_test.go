package pipeline

import (
	"testing"
	"time"
)

var usageNow = time.Date(2026, 10, 18, 20, 0, 0, 0, time.UTC)

func TestBurnWeighsTokensLikeTheirPrice(t *testing.T) {
	r := UsageRun{Provider: ProviderClaude, Input: 1000, Output: 100, CacheCreation: 400, CacheRead: 10000}
	// 1000 + 5*100 + 1.25*400 + 0.1*10000
	if got := r.Burn(); got != 3000 {
		t.Fatalf("Burn = %d, want 3000", got)
	}
	q := UsageRun{Provider: ProviderQwen, Input: 1000, Output: 100}
	if got := q.Burn(); got != 1100 {
		t.Fatalf("qwen Burn = %d, want 1100 (plain tokens)", got)
	}
}

func TestLimitWindow(t *testing.T) {
	for _, tc := range []struct{ err, kind, want string }{
		{"You've hit your session limit · resets 3:45pm", "", Window5h},
		{"You've hit your weekly limit · resets Mon 12:00am", "", WindowWeek},
		{"You've hit your Opus limit · resets Mon 12:00am", "", WindowWeek},
		{"rate limit: rejected", "five_hour", Window5h},
		{"rate limit: rejected", "seven_day_opus", WindowWeek},
		{"rate limit: rejected", "", Window5h},
		{"Claude AI usage limit reached", "", Window5h},
	} {
		if got := LimitWindow(tc.err, tc.kind); got != tc.want {
			t.Errorf("LimitWindow(%q, %q) = %q, want %q", tc.err, tc.kind, got, tc.want)
		}
	}
}

func TestParseReset(t *testing.T) {
	ny, err := time.LoadLocation("America/New_York")
	if err != nil {
		t.Skip("no tzdata")
	}
	hit := time.Date(2026, 10, 18, 14, 0, 0, 0, ny) // a Sunday, 2pm in New York
	for _, tc := range []struct {
		text string
		want time.Time
		ok   bool
	}{
		{"2026-10-18T19:45:00Z", time.Date(2026, 10, 18, 19, 45, 0, 0, time.UTC), true},
		{"3:45pm", time.Date(2026, 10, 18, 15, 45, 0, 0, ny), true},
		{"3pm", time.Date(2026, 10, 18, 15, 0, 0, 0, ny), true},
		{"1:30pm", time.Date(2026, 10, 19, 13, 30, 0, 0, ny), true}, // already past today: tomorrow
		{"Mon 12:00am", time.Date(2026, 10, 19, 0, 0, 0, 0, ny), true},
		{"Sun 9am", time.Date(2026, 10, 25, 9, 0, 0, 0, ny), true},
		{"5pm (Europe/London)", time.Date(2026, 10, 18, 17, 0, 0, 0, mustLoc(t, "Europe/London")).Add(24 * time.Hour), true},
		{"soon", time.Time{}, false},
		{"", time.Time{}, false},
	} {
		got, ok := ParseReset(tc.text, hit, ny)
		if ok != tc.ok || !got.Equal(tc.want) {
			t.Errorf("ParseReset(%q) = %v, %v; want %v, %v", tc.text, got, ok, tc.want, tc.ok)
		}
	}
}

func mustLoc(t *testing.T, name string) *time.Location {
	t.Helper()
	l, err := time.LoadLocation(name)
	if err != nil {
		t.Skip("no tzdata")
	}
	return l
}

func run(at time.Time, mins int, input int64) UsageRun {
	return UsageRun{At: at, Started: at.Add(-time.Duration(mins) * time.Minute), Provider: ProviderClaude, Input: input}
}

func TestComputeUsageOpensFiveHourWindows(t *testing.T) {
	runs := []UsageRun{
		run(usageNow.Add(-9*time.Hour), 10, 500),  // an old window, long over
		run(usageNow.Add(-3*time.Hour), 20, 1000), // opens the current window at -3h20m
		run(usageNow.Add(-1*time.Hour), 30, 2000),
	}
	st := ComputeUsage(ProviderClaude, Budget{Per5h: 10000, PerWeek: 100000}, runs, nil, usageNow)
	if want := usageNow.Add(-3*time.Hour - 20*time.Minute); !st.WindowStart.Equal(want) {
		t.Fatalf("WindowStart = %v, want %v", st.WindowStart, want)
	}
	if !st.WindowEnd.Equal(st.WindowStart.Add(5 * time.Hour)) {
		t.Fatalf("WindowEnd = %v", st.WindowEnd)
	}
	if st.Burn5h != 3000 || st.BurnWeek != 3500 || st.Limit5h != 10000 || st.LimitWeek != 100000 {
		t.Fatalf("state = %+v", st)
	}
	if !st.WeekStart.Equal(usageNow.Add(-WeekLen)) {
		t.Fatalf("rolling WeekStart = %v", st.WeekStart)
	}
}

func TestComputeUsageNoOpenWindow(t *testing.T) {
	st := ComputeUsage(ProviderClaude, Budget{}, []UsageRun{run(usageNow.Add(-6*time.Hour), 10, 500)}, nil, usageNow)
	if !st.WindowStart.IsZero() || st.Burn5h != 0 || st.BurnWeek != 500 {
		t.Fatalf("state = %+v", st)
	}
}

func TestComputeUsageAnchorsOnAKnownReset(t *testing.T) {
	reset := usageNow.Add(time.Hour)
	hits := []UsageHit{{At: usageNow.Add(-10 * time.Minute), Provider: ProviderClaude, Window: Window5h, ResetAt: reset, Burn: 8000}}
	runs := []UsageRun{run(usageNow.Add(-3*time.Hour), 20, 1000), run(usageNow.Add(-10*time.Minute), 5, 7000)}
	st := ComputeUsage(ProviderClaude, DefaultBudgets()[ProviderClaude], runs, hits, usageNow)
	if !st.WindowEnd.Equal(reset) || !st.WindowStart.Equal(reset.Add(-Session)) {
		t.Fatalf("window = %v – %v, want ending at the reset %v", st.WindowStart, st.WindowEnd, reset)
	}
	if st.Limit5h != 8000 {
		t.Fatalf("Limit5h = %d, want the burn at the last limit (8000)", st.Limit5h)
	}
	if st.LimitWeek != 0 {
		t.Fatalf("LimitWeek = %d, want 0 (never limited weekly)", st.LimitWeek)
	}
	if !st.PausedUntil.Equal(reset) || st.PauseWindow != Window5h {
		t.Fatalf("PausedUntil = %v (%s), want %v", st.PausedUntil, st.PauseWindow, reset)
	}
	// After the reset the pause is over and the next run opens a fresh window.
	later := ComputeUsage(ProviderClaude, DefaultBudgets()[ProviderClaude], runs, hits, reset.Add(time.Minute))
	if !later.PausedUntil.IsZero() || later.Burn5h != 0 {
		t.Fatalf("after the reset: %+v", later)
	}
}

func TestComputeUsageUnknownResetPausesAnHour(t *testing.T) {
	hits := []UsageHit{{At: usageNow.Add(-10 * time.Minute), Provider: ProviderClaude, Window: Window5h}}
	st := ComputeUsage(ProviderClaude, Budget{}, nil, hits, usageNow)
	if want := usageNow.Add(-10 * time.Minute).Add(UnknownResetPause); !st.PausedUntil.Equal(want) {
		t.Fatalf("PausedUntil = %v, want %v", st.PausedUntil, want)
	}
}

func TestComputeUsageWeekAnchor(t *testing.T) {
	reset := usageNow.Add(-2 * 24 * time.Hour) // last week's reset: the week runs reset+7d
	hits := []UsageHit{{At: reset.Add(-time.Hour), Provider: ProviderClaude, Window: WindowWeek, ResetAt: reset, Burn: 90000}}
	st := ComputeUsage(ProviderClaude, Budget{PerWeekPct: 50}, nil, hits, usageNow)
	if !st.WeekStart.Equal(reset) || !st.WeekEnd.Equal(reset.Add(WeekLen)) {
		t.Fatalf("week = %v – %v", st.WeekStart, st.WeekEnd)
	}
	if st.LimitWeek != 45000 {
		t.Fatalf("LimitWeek = %d, want 50%% of 90000", st.LimitWeek)
	}
}

func TestPaceFor(t *testing.T) {
	ctl := Control{ActiveAgents: 4}
	base := UsageState{Provider: ProviderClaude, Now: usageNow, WindowStart: usageNow.Add(-2 * time.Hour), WindowEnd: usageNow.Add(3 * time.Hour), Limit5h: 1000}
	for _, tc := range []struct {
		name   string
		burn5h int64
		warn   bool
		level  int
		agents int
	}{
		{"well under", 500, false, PaceNormal, 4},
		{"just under", 849, false, PaceNormal, 4},
		{"at 85%", 850, false, PaceSlow, 3},
		{"at 90%", 900, false, PaceSlow, 2},
		{"at 95%", 950, false, PaceSlow, 1},
		{"at 99%", 990, false, PaceSlow, 1},
		{"at the budget", 1000, false, PacePaused, 0},
		{"the CLI warned", 100, true, PaceSlow, 3},
	} {
		t.Run(tc.name, func(t *testing.T) {
			st := base
			st.Burn5h, st.Warning = tc.burn5h, tc.warn
			p := PaceFor(ctl, Budget{}, st)
			if p.Level != tc.level || p.Agents != tc.agents {
				t.Fatalf("PaceFor = %+v, want level %d agents %d", p, tc.level, tc.agents)
			}
			if tc.level == PacePaused && !p.Until.Equal(st.WindowEnd) {
				t.Fatalf("Until = %v, want the window's end", p.Until)
			}
		})
	}
	// No budget known: no pacing at all, whatever the burn.
	if p := PaceFor(ctl, Budget{}, UsageState{Now: usageNow, Burn5h: 1e9}); p.Level != PaceNormal {
		t.Fatalf("no budget: %+v", p)
	}
	// A limit hit: paused until its reset.
	st := base
	st.PausedUntil, st.PauseWindow = usageNow.Add(time.Hour), Window5h
	if p := PaceFor(ctl, Budget{}, st); p.Level != PacePaused || !p.Until.Equal(st.PausedUntil) {
		t.Fatalf("hit: %+v", p)
	}
	// A pace_at of its own.
	st = base
	st.Burn5h = 700
	if p := PaceFor(ctl, Budget{PaceAt: 70}, st); p.Level != PaceSlow || p.Agents != 3 {
		t.Fatalf("pace_at 70: %+v", p)
	}
	// One agent: pacing keeps it at one.
	st.Burn5h = 900
	if p := PaceFor(Control{ActiveAgents: 1}, Budget{}, st); p.Agents != 1 {
		t.Fatalf("one agent: %+v", p)
	}
}

func TestForecast(t *testing.T) {
	st := UsageState{Now: usageNow, WindowStart: usageNow.Add(-2 * time.Hour), WindowEnd: usageNow.Add(3 * time.Hour), Burn5h: 400, Limit5h: 1000}
	// 200 an hour: the remaining 600 last 3 hours, exactly to the reset: not before it.
	if _, ok := Forecast(st); ok {
		t.Fatal("a window that lasts to its reset has no run-out time")
	}
	st.Burn5h = 800 // 400 an hour: 200 left lasts 30 min
	if got, ok := Forecast(st); !ok || !got.Equal(usageNow.Add(30*time.Minute)) {
		t.Fatalf("Forecast = %v, %v", got, ok)
	}
	if _, ok := Forecast(UsageState{Now: usageNow, Burn5h: 800}); ok {
		t.Fatal("no window, no forecast")
	}
}

func TestBudgetValidate(t *testing.T) {
	for _, tc := range []struct {
		b  Budget
		ok bool
	}{
		{Budget{}, true},
		{Budget{Per5h: 1000, PerWeekPct: 80, PaceAt: 90}, true},
		{Budget{Per5h: -1}, false},
		{Budget{Per5hPct: 101}, false},
		{Budget{Per5h: 10, Per5hPct: 50}, false},
		{Budget{PerWeek: 10, PerWeekPct: 50}, false},
		{Budget{PaceAt: 100}, false},
	} {
		if err := tc.b.Validate(); (err == nil) != tc.ok {
			t.Errorf("Validate(%+v) = %v", tc.b, err)
		}
	}
	c := DefaultControl()
	c.Budgets = map[string]Budget{"openai": {}}
	if c.Validate() == nil {
		t.Fatal("an unknown provider's budget was accepted")
	}
}

func sized(it Item, size, builder string) Item {
	it.Size, it.Builder = size, builder
	return it
}

func TestChooseWorkPaced(t *testing.T) {
	ctl := DefaultControl() // 4 agents
	req := ClaimRequest{Runner: "r", Agent: "a", Kinds: []string{WorkItem, WorkPlan, WorkOrchestrate}}
	qwenReq := req
	qwenReq.Builders = []string{BuilderQwen}
	slow := Pace{Level: PaceSlow, Agents: 2}
	paused := Pace{Level: PacePaused}
	claudeLive := []LiveLease{{ItemID: 8, Stage: Build, Claude: true}, {ItemID: 9, Stage: Review, Claude: true}}
	qwenLive := []LiveLease{{ItemID: 8, Stage: Build}, {ItemID: 9, Stage: Build}}
	turn := []TurnCandidate{{TurnID: 1, PlanID: 1, Priority: "P1", CreatedAt: t0}}
	orch := &OrchCandidate{}

	for _, tc := range []struct {
		name   string
		pace   Pace
		items  []Item
		live   []LiveLease
		turns  []TurnCandidate
		orch   *OrchCandidate
		req    ClaimRequest
		want   int64 // item id; -1 a plan turn; -2 the orchestrate turn; 0 nothing
		routed bool
	}{
		{"normal: the usual order", Pace{Agents: 4}, []Item{waiting(1, Build, "P3", time.Hour), waiting(2, Build, "P2", 0)}, nil, nil, nil, req, 2, false},
		{"slow: P1 first", slow, []Item{sized(waiting(1, Build, "P2", time.Hour), "M", ""), sized(waiting(2, Build, "P1", 0), "L", "")}, nil, nil, nil, req, 2, false},
		{"slow: size S next", slow, []Item{sized(waiting(1, Build, "P2", time.Hour), "M", ""), sized(waiting(2, Build, "P3", 0), "S", "")}, nil, nil, nil, req, 2, false},
		{"slow: triage goes on", slow, []Item{waiting(1, Triage, "P3", 0)}, nil, nil, nil, req, 1, false},
		{"slow: a P2 M waits", slow, []Item{sized(waiting(1, Build, "P2", 0), "M", "")}, nil, nil, nil, req, 0, false},
		{"slow: claude runs at the paced number", slow, []Item{sized(waiting(1, Build, "P1", 0), "S", "")}, claudeLive, nil, nil, req, 0, false},
		{"slow: qwen runs do not count", slow, []Item{sized(waiting(1, Build, "P1", 0), "S", "")}, qwenLive, nil, nil, req, 1, false},
		{"slow: a qwen item builds", slow, []Item{sized(waiting(1, Build, "P3", 0), "L", BuilderQwen)}, claudeLive, nil, nil, req, 1, false},
		{"slow: land goes on", slow, []Item{waiting(1, Land, "P4", 0)}, claudeLive, nil, nil, req, 1, false},
		{"slow: a P3 M build routes to qwen", slow, []Item{sized(waiting(1, Build, "P3", 0), "M", "")}, claudeLive, nil, nil, qwenReq, 1, true},
		{"slow: no qwen on the runner, no route", slow, []Item{sized(waiting(1, Build, "P3", 0), "M", "")}, claudeLive, nil, nil, req, 0, false},
		{"slow: an L build does not fit qwen", slow, []Item{sized(waiting(1, Build, "P3", 0), "L", "")}, nil, nil, nil, qwenReq, 0, false},
		{"slow: builder claude from a plan stays", slow, []Item{sized(waiting(1, Build, "P4", 0), "S", BuilderClaude)}, claudeLive, nil, nil, qwenReq, 0, false},
		{"slow: a P2 build is not routed", slow, []Item{sized(waiting(1, Build, "P2", 0), "S", "")}, claudeLive, nil, nil, qwenReq, 0, false},
		{"slow: a P1 plan turn", slow, nil, nil, turn, nil, req, -1, false},
		{"slow: a P3 plan turn waits", slow, nil, nil, []TurnCandidate{{TurnID: 1, Priority: "P3"}}, nil, req, 0, false},
		{"slow: the orchestrator still gets its turn", slow, nil, nil, nil, orch, req, -2, false},
		{"paused: no claude work", paused, []Item{sized(waiting(1, Build, "P1", 0), "S", ""), waiting(2, Triage, "P1", 0), waiting(3, Review, "P1", 0)}, nil, turn, orch, req, 0, false},
		{"paused: qwen builds and land go on", paused, []Item{sized(waiting(1, Build, "P1", 0), "S", ""), sized(waiting(2, Build, "P3", 0), "S", BuilderQwen), waiting(3, Land, "P5", 0)}, nil, nil, nil, req, 2, false},
		{"paused: a routed build goes on", paused, []Item{sized(waiting(1, Build, "P4", 0), "S", "")}, nil, nil, nil, qwenReq, 1, true},
		{"paused: the total cap still holds", paused, []Item{sized(waiting(2, Build, "P3", 0), "S", BuilderQwen)},
			[]LiveLease{{ItemID: 5}, {ItemID: 6}, {ItemID: 7}, {ItemID: 8}}, nil, nil, req, 0, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w, ok := ChooseWorkPaced(ctl, tc.pace, tc.items, nil, tc.live, tc.turns, tc.orch, tc.req)
			var got int64
			switch {
			case !ok:
			case w.Item != nil:
				got = w.Item.ID
			case w.Turn != nil:
				got = -1
			case w.Orch != nil:
				got = -2
			}
			if got != tc.want || w.RouteQwen != tc.routed {
				t.Fatalf("got %d (routed %v), want %d (routed %v)", got, w.RouteQwen, tc.want, tc.routed)
			}
		})
	}
}

func TestTokensFormatAndParse(t *testing.T) {
	for n, want := range map[int64]string{950: "950", 1234: "1.2k", 45_600: "46k", 12_345_678: "12.3M"} {
		if got := FormatTokens(n); got != want {
			t.Errorf("FormatTokens(%d) = %q, want %q", n, got, want)
		}
	}
	for s, want := range map[string]int64{"1500000": 1_500_000, "1.5M": 1_500_000, "500k": 500_000, "2m": 2_000_000} {
		if got, err := ParseTokens(s); err != nil || got != want {
			t.Errorf("ParseTokens(%q) = %d, %v", s, got, err)
		}
	}
	if _, err := ParseTokens("lots"); err == nil {
		t.Error("ParseTokens(lots) accepted")
	}
	if got := DescribeBudget(Budget{Per5h: 2_000_000, PerWeekPct: 80}); got != "per 5h 2.0M · per week 80% of the last limit · pacing from 85%" {
		t.Errorf("DescribeBudget = %q", got)
	}
}
