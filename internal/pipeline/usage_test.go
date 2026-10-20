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
		// #316: pacing slows claims, never starves them: with nothing P1, S or triage, the highest
		// priority, oldest item goes anyway, at most max(1, Agents/3) claude runs at once.
		{"slow: nothing qualifies, a P2 M goes anyway", slow, []Item{sized(waiting(1, Build, "P2", 0), "M", "")}, nil, nil, nil, req, 1, false},
		{"slow: the fallback takes the highest priority, oldest", Pace{Level: PaceSlow, Agents: 3},
			[]Item{sized(waiting(1, Build, "P3", 2*time.Hour), "M", ""), sized(waiting(2, Build, "P2", 0), "L", ""), sized(waiting(3, Build, "P2", time.Hour), "M", "")}, nil, nil, nil, req, 3, false},
		{"slow: the fallback's cap is reached", Pace{Level: PaceSlow, Agents: 3}, []Item{sized(waiting(1, Build, "P2", 0), "M", "")},
			[]LiveLease{{ItemID: 8, Stage: Build, Claude: true}}, nil, nil, req, 0, false},
		{"slow: the fallback's cap grows with the pace", Pace{Level: PaceSlow, Agents: 6}, []Item{sized(waiting(1, Build, "P2", 0), "M", "")},
			[]LiveLease{{ItemID: 8, Stage: Build, Claude: true}}, nil, nil, ClaimRequest{Runner: "r", Agent: "a", Kinds: []string{WorkItem}}, 1, false},
		{"slow: claude runs at the paced number", slow, []Item{sized(waiting(1, Build, "P1", 0), "S", "")}, claudeLive, nil, nil, req, 0, false},
		{"slow: qwen runs do not count", slow, []Item{sized(waiting(1, Build, "P1", 0), "S", "")}, qwenLive, nil, nil, req, 1, false},
		{"slow: a qwen item builds", slow, []Item{sized(waiting(1, Build, "P3", 0), "L", BuilderQwen)}, claudeLive, nil, nil, req, 1, false},
		{"slow: land goes on", slow, []Item{waiting(1, Land, "P4", 0)}, claudeLive, nil, nil, req, 1, false},
		{"slow: a P3 M build routes to qwen", slow, []Item{sized(waiting(1, Build, "P3", 0), "M", "")}, claudeLive, nil, nil, qwenReq, 1, true},
		{"slow: no qwen on the runner, no route", slow, []Item{sized(waiting(1, Build, "P3", 0), "M", "")}, claudeLive, nil, nil, req, 0, false},
		{"slow: an L build does not fit qwen", slow, []Item{sized(waiting(1, Build, "P3", 0), "L", "")}, claudeLive[:1], nil, nil, qwenReq, 0, false},
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
			w, ok := ChooseWorkPaced(ctl, tc.pace, tc.items, nil, tc.live, tc.turns, tc.orch, nil, nil, tc.req, nil)
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

// A frozen repo's fix goes first, but under the pace like any item: paused, claude does not build
// it; slow, it is a P1 and goes, even with every slot taken.
func TestChooseWorkPacedFrozen(t *testing.T) {
	fix := sized(waiting(9, Build, "P1", 0), "M", "")
	fix.Repo = "o/a"
	other := sized(waiting(2, Build, "P1", time.Hour), "S", "")
	other.Repo = "o/a"
	land := waiting(3, Land, "P5", 0)
	frozen := Frozen{"o/a": 9}
	req := ClaimRequest{Runner: "r", Agent: "a", Kinds: []string{WorkItem}}
	full := []LiveLease{{ItemID: 5}, {ItemID: 6}, {ItemID: 7}, {ItemID: 8}}
	for _, tc := range []struct {
		name string
		pace Pace
		live []LiveLease
		want int64
	}{
		{"slow: the fix, over the cap", Pace{Level: PaceSlow, Agents: 2}, full, 9},
		{"paused, slots free: land, not the fix", Pace{Level: PacePaused}, nil, 3},
		{"paused, every slot taken: land, which takes no slot (#210)", Pace{Level: PacePaused}, full, 3},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w, ok := ChooseWorkPaced(DefaultControl(), tc.pace, []Item{fix, other, land}, nil, tc.live, nil, nil, nil, nil, req, frozen)
			var got int64
			if ok && w.Item != nil {
				got = w.Item.ID
			}
			if got != tc.want {
				t.Fatalf("got %d, want %d", got, tc.want)
			}
		})
	}
}

// A regression run starts no agent, so the pace leaves it alone: paused, the queued run is still
// claimed, while the claude build beside it waits.
func TestChooseWorkPacedRegression(t *testing.T) {
	req := ClaimRequest{Runner: "devbox", Agent: "a", Kinds: []string{WorkItem, WorkRegression}}
	reg := &RegCandidate{RunID: 7, Suite: SuiteWeb}
	build := waiting(1, Build, "P1", 0)
	paused := Pace{Level: PacePaused}
	w, ok := ChooseWorkPaced(DefaultControl(), paused, []Item{build}, nil, nil, nil, nil, reg, nil, req, nil)
	if !ok || w.Reg == nil || w.Reg.RunID != 7 {
		t.Fatalf("paused: got %+v, %v; want regression run 7", w, ok)
	}
	if w, ok := ChooseWorkPaced(DefaultControl(), paused, []Item{build}, nil, nil, nil, nil, nil, nil, req, nil); ok {
		t.Fatalf("paused, no regression: got %+v, want nothing", w)
	}
}

// The work of a held slot (SlotHolds) is paced like any item: paused, the last slot's Mac build
// does not go to claude; a P4 S build routes to qwen there when the runner has it.
func TestChooseWorkPacedHeldSlot(t *testing.T) {
	two := DefaultControl()
	two.ActiveAgents = 2
	holds := []SlotHold{{Resource: "macos", Runner: "mbp", ItemID: 2}}
	live := []LiveLease{{ItemID: 9, Stage: Build}}
	mac := sized(waiting(2, Build, "P4", 0, "macos"), "S", "")
	req := ClaimRequest{Runner: "mbp", Agent: "mbp-1", Capabilities: []string{"macos"}, OnlyResources: []string{"macos"}, Kinds: []string{WorkItem}}
	qwenReq := req
	qwenReq.Builders = []string{BuilderQwen}
	paused := Pace{Level: PacePaused}
	if w, ok := ChooseWorkPaced(two, paused, []Item{mac}, nil, live, nil, nil, nil, holds, req, nil); ok {
		t.Fatalf("paused: got %+v, want nothing", w)
	}
	w, ok := ChooseWorkPaced(two, paused, []Item{mac}, nil, live, nil, nil, nil, holds, qwenReq, nil)
	if !ok || w.Item == nil || w.Item.ID != 2 || !w.RouteQwen {
		t.Fatalf("paused, qwen on the runner: got %+v, %v; want #2 routed to qwen", w, ok)
	}
	if w, ok := ChooseWorkPaced(two, Pace{Agents: 2}, []Item{mac}, nil, live, nil, nil, nil, holds, req, nil); !ok || w.Item == nil || w.Item.ID != 2 || w.RouteQwen {
		t.Fatalf("normal: got %+v, %v; want #2 with claude", w, ok)
	}
}

// A slot is held only for work the pace lets run: paused, the last slot is not kept for a Mac
// claude build, so the dev box's P4 build goes on with qwen; not paused, the hold stands as before.
// (Land passes a held slot anyway: it takes no agent slot, #210.)
func TestSlotHoldsPaced(t *testing.T) {
	two := DefaultControl()
	two.ActiveAgents = 2
	live := []LiveLease{{ItemID: 9, Stage: Build, Claude: true}}
	mbp := RunnerSeen{Name: "mbp", Capabilities: []string{"macos"}, OnlyResources: []string{"macos"}, PollSeconds: 30, LastSeen: t0.Add(-10 * time.Second)}
	mac := sized(waiting(2, Build, "P4", 0, "macos"), "S", "")
	bigMac := sized(waiting(4, Build, "P2", 0, "macos"), "L", "")
	qwenBuild := sized(waiting(3, Build, "P4", time.Hour), "S", "")
	devbox := ClaimRequest{Runner: "devbox", Agent: "devbox-1", Kinds: []string{WorkItem}, Builders: []string{BuilderQwen}}
	paused := Pace{Level: PacePaused}
	for _, items := range [][]Item{{mac, qwenBuild}, {bigMac, qwenBuild}} {
		holds := SlotHolds(two, paused, items, nil, live, []RunnerSeen{mbp}, nil, t0)
		if len(holds) != 0 {
			t.Fatalf("paused, #%d waiting: holds %+v, want none", items[0].ID, holds)
		}
		w, ok := ChooseWorkPaced(two, paused, items, nil, live, nil, nil, nil, holds, devbox, nil)
		if !ok || w.Item == nil || w.Item.ID != 3 || !w.RouteQwen {
			t.Fatalf("paused, #%d waiting: devbox got %+v, %v; want #3 with qwen", items[0].ID, w, ok)
		}
	}
	// Slow with room for one more claude run: a P1 or size S Mac build holds the slot, a P2 L does not.
	slow := Pace{Level: PaceSlow, Agents: 2}
	if h := SlotHolds(two, slow, []Item{bigMac, qwenBuild}, nil, live, []RunnerSeen{mbp}, nil, t0); len(h) != 0 {
		t.Fatalf("slow, P2 L Mac build: holds %+v, want none", h)
	}
	p1Mac := sized(waiting(5, Build, "P1", 0, "macos"), "L", "")
	if h := SlotHolds(two, slow, []Item{p1Mac, qwenBuild}, nil, live, []RunnerSeen{mbp}, nil, t0); len(h) != 1 || h[0].ItemID != 5 {
		t.Fatalf("slow, P1 Mac build: holds %+v, want #5 held", h)
	}
	// Not paced: the hold stands, and the devbox gets nothing.
	normal := Pace{Agents: 2}
	holds := SlotHolds(two, normal, []Item{mac, qwenBuild}, nil, live, []RunnerSeen{mbp}, nil, t0)
	if len(holds) != 1 || holds[0].ItemID != 2 {
		t.Fatalf("normal: holds %+v, want #2 held", holds)
	}
	if w, ok := ChooseWorkPaced(two, normal, []Item{mac, qwenBuild}, nil, live, nil, nil, nil, holds, devbox, nil); ok || w.Held != "slot held for macos work" {
		t.Fatalf("normal: devbox got %+v, %v; want the slot held", w, ok)
	}
}
