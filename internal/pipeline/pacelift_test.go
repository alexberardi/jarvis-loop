package pipeline

import (
	"strings"
	"testing"
	"time"
)

// #316: the CLI's allowed_warning can be ignored (budgets and real hits still pace), and the user
// can lift pacing until a time or the next limit hit.

func TestPaceForWarningsIgnore(t *testing.T) {
	ctl := Control{ActiveAgents: 4}
	base := UsageState{Provider: ProviderClaude, Now: usageNow, WindowStart: usageNow.Add(-2 * time.Hour), WindowEnd: usageNow.Add(3 * time.Hour), Limit5h: 1000}
	ignore := Budget{Warnings: WarningsIgnore}

	st := base
	st.Burn5h, st.Warning = 100, true
	if p := PaceFor(ctl, ignore, st); p.Level != PaceNormal || p.Agents != 4 {
		t.Fatalf("warning, ignore: %+v, want normal", p)
	}
	if p := PaceFor(ctl, Budget{Warnings: WarningsPace}, st); p.Level != PaceSlow {
		t.Fatalf("warning, pace: %+v, want slow", p)
	}
	// No budget at all and a warning: ignored too.
	if p := PaceFor(ctl, ignore, UsageState{Now: usageNow, Warning: true}); p.Level != PaceNormal {
		t.Fatalf("no budget, warning, ignore: %+v", p)
	}
	// The budget still paces.
	st.Burn5h = 900
	if p := PaceFor(ctl, ignore, st); p.Level != PaceSlow || p.Agents != 2 {
		t.Fatalf("90%% with ignore: %+v, want slow at 2", p)
	}
	st.Burn5h = 1000
	if p := PaceFor(ctl, ignore, st); p.Level != PacePaused {
		t.Fatalf("at the budget with ignore: %+v, want paused", p)
	}
	// A real limit hit still pauses.
	st = base
	st.Warning, st.PausedUntil, st.PauseWindow = true, usageNow.Add(time.Hour), Window5h
	if p := PaceFor(ctl, ignore, st); p.Level != PacePaused || !p.Hit {
		t.Fatalf("hit with ignore: %+v, want paused", p)
	}
}

func TestBudgetWarningsValidate(t *testing.T) {
	for _, w := range []string{"", WarningsPace, WarningsIgnore} {
		if err := (Budget{Warnings: w}).Validate(); err != nil {
			t.Fatalf("%q: %v", w, err)
		}
	}
	if err := (Budget{Warnings: "sometimes"}).Validate(); err == nil {
		t.Fatal("warnings sometimes: no error")
	}
	if got := DescribeBudget(Budget{Warnings: WarningsIgnore}); !strings.Contains(got, "CLI warnings ignored") {
		t.Fatalf("DescribeBudget = %q", got)
	}
	if got := DescribeBudget(Budget{}); strings.Contains(got, "warnings") {
		t.Fatalf("DescribeBudget default = %q", got)
	}
}

func TestPaceLift(t *testing.T) {
	set := usageNow.Add(-time.Hour)
	until := usageNow.Add(2 * time.Hour)
	warned := UsageState{Provider: ProviderClaude, Now: usageNow, WindowStart: usageNow.Add(-2 * time.Hour), WindowEnd: usageNow.Add(3 * time.Hour),
		Limit5h: 1000, Burn5h: 900, Warning: true}
	lifted := Control{ActiveAgents: 4, PaceNormalSet: set, PaceNormalUntil: until}

	p := PaceFor(lifted, Budget{}, warned)
	if p.Level != PaceNormal || p.Agents != 4 || !p.Lifted || !p.Until.Equal(until) {
		t.Fatalf("lifted: %+v, want normal at 4, lifted until %v", p, until)
	}
	if got := p.Describe(time.UTC); !strings.Contains(got, "pacing lifted until") {
		t.Fatalf("Describe = %q", got)
	}
	// The budget's own pause is lifted too.
	atBudget := warned
	atBudget.Burn5h = 1000
	if p := PaceFor(lifted, Budget{}, atBudget); p.Level != PaceNormal {
		t.Fatalf("lifted at the budget: %+v, want normal", p)
	}
	// It expires at its time.
	later := warned
	later.Now = until
	if p := PaceFor(lifted, Budget{}, later); p.Level != PaceSlow || p.Lifted {
		t.Fatalf("after --until: %+v, want slow", p)
	}
	// A limit hit still pauses, and ends the lift: the next free reset is the user's to apply.
	hit := warned
	hit.LastHit, hit.PausedUntil, hit.PauseWindow = usageNow.Add(-time.Minute), usageNow.Add(time.Hour), Window5h
	if p := PaceFor(lifted, Budget{}, hit); p.Level != PacePaused || !p.Hit {
		t.Fatalf("hit while lifted: %+v, want paused", p)
	}
	hit.Now, hit.PausedUntil = usageNow.Add(90*time.Minute), time.Time{}
	if p := PaceFor(lifted, Budget{}, hit); p.Level != PaceSlow {
		t.Fatalf("after the hit's reset: %+v, want slow (the lift ended at the hit)", p)
	}
	// A hit before the lift does not end it.
	old := warned
	old.LastHit = set.Add(-time.Minute)
	if p := PaceFor(lifted, Budget{}, old); p.Level != PaceNormal || !p.Lifted {
		t.Fatalf("hit before the lift: %+v, want lifted", p)
	}
	// A lift set while a hit's pause still runs does not cut that pause short.
	old.PausedUntil, old.PauseWindow = usageNow.Add(time.Hour), Window5h
	if p := PaceFor(lifted, Budget{}, old); p.Level != PacePaused || !p.Hit {
		t.Fatalf("lifted during a hit's pause: %+v, want paused", p)
	}

	// "reset": until the next limit hit, however long.
	reset := Control{ActiveAgents: 4, PaceNormalSet: set}
	far := warned
	far.Now = usageNow.Add(5 * 24 * time.Hour)
	p = PaceFor(reset, Budget{}, far)
	if p.Level != PaceNormal || !p.Lifted || !p.Until.IsZero() {
		t.Fatalf("lifted to the next hit: %+v", p)
	}
	if got := p.Describe(time.UTC); !strings.Contains(got, "until the next limit hit") {
		t.Fatalf("Describe = %q", got)
	}
	far.LastHit = usageNow.Add(24 * time.Hour)
	if p := PaceFor(reset, Budget{}, far); p.Level != PaceSlow || p.Lifted {
		t.Fatalf("after the next hit: %+v, want slow", p)
	}
}

func TestComputeUsageLastHit(t *testing.T) {
	hits := []UsageHit{
		{At: usageNow.Add(-3 * time.Hour), Provider: ProviderClaude, Window: Window5h},
		{At: usageNow.Add(-2 * time.Hour), Provider: ProviderClaude, Window: WindowWeek},
		{At: usageNow.Add(-time.Hour), Provider: ProviderQwen, Window: Window5h},
	}
	if st := ComputeUsage(ProviderClaude, Budget{}, nil, hits, usageNow); !st.LastHit.Equal(usageNow.Add(-2 * time.Hour)) {
		t.Fatalf("LastHit = %v", st.LastHit)
	}
}

func TestValidatePaceLift(t *testing.T) {
	c := DefaultControl()
	c.PaceNormalUntil = usageNow
	if err := c.Validate(); err == nil {
		t.Fatal("an until without a set time: no error")
	}
	c.PaceNormalSet = usageNow
	if err := c.Validate(); err == nil {
		t.Fatal("an until not after the set time: no error")
	}
	c.PaceNormalUntil = usageNow.Add(time.Hour)
	if err := c.Validate(); err != nil {
		t.Fatal(err)
	}
}
