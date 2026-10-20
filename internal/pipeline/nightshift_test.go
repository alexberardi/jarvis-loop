package pipeline

import (
	"strings"
	"testing"
	"time"
	_ "time/tzdata"
)

func newYork(t *testing.T) *time.Location {
	t.Helper()
	loc, err := time.LoadLocation("America/New_York")
	if err != nil {
		t.Fatal(err)
	}
	return loc
}

// The auto hours are 23:00 to 08:00 on New York's clock, on both sides of a DST change: the spring
// night is an hour shorter, the autumn one an hour longer (01:00-02:00 comes twice), and the edges
// stay at 23:00 and 08:00 local.
func TestInNightHoursAcrossDST(t *testing.T) {
	ny := newYork(t)
	utc := func(s string) time.Time {
		at, err := time.Parse(time.RFC3339, s)
		if err != nil {
			t.Fatal(err)
		}
		return at
	}
	cases := []struct {
		at   string
		want bool
	}{
		{"2026-10-20T02:59:00Z", false}, // 22:59 EDT
		{"2026-10-20T03:00:00Z", true},  // 23:00 EDT
		{"2026-10-20T11:59:00Z", true},  // 07:59 EDT
		{"2026-10-20T12:00:00Z", false}, // 08:00 EDT
		// Spring forward, 2026-03-08 02:00 EST -> 03:00 EDT.
		{"2026-03-08T03:59:00Z", false}, // 22:59 EST on the 7th
		{"2026-03-08T04:00:00Z", true},  // 23:00 EST on the 7th
		{"2026-03-08T07:30:00Z", true},  // 03:30 EDT (02:30 does not exist)
		{"2026-03-08T11:59:00Z", true},  // 07:59 EDT
		{"2026-03-08T12:00:00Z", false}, // 08:00 EDT (would be 07:00 EST)
		{"2026-03-08T13:00:00Z", false}, // 09:00 EDT
		// Fall back, 2026-11-01 02:00 EDT -> 01:00 EST.
		{"2026-11-01T03:00:00Z", true},  // 23:00 EDT on Oct 31
		{"2026-11-01T05:30:00Z", true},  // 01:30 EDT
		{"2026-11-01T06:30:00Z", true},  // 01:30 EST, the second time
		{"2026-11-01T12:59:00Z", true},  // 07:59 EST
		{"2026-11-01T13:00:00Z", false}, // 08:00 EST (would be 09:00 EDT)
		{"2026-11-01T12:00:00Z", true},  // 07:00 EST
		{"2026-11-02T03:59:00Z", false}, // 22:59 EST
		{"2026-11-02T04:00:00Z", true},  // 23:00 EST
	}
	for _, c := range cases {
		if got := InNightHours(utc(c.at), ny); got != c.want {
			t.Errorf("%s (%s): night %v, want %v", c.at, utc(c.at).In(ny).Format("Jan 2 15:04 MST"), got, c.want)
		}
	}
	// The night that began at 23:00 EST on Mar 7 ends at 08:00 EDT: 8 hours, not 9.
	if got := NightStart(utc("2026-03-08T11:00:00Z"), ny); !got.Equal(utc("2026-03-08T04:00:00Z")) {
		t.Errorf("spring night start %s", got)
	}
	// The night that began at 23:00 EDT on Oct 31 is still on at 07:00 EST: 10 hours.
	if got := NightStart(utc("2026-11-01T12:00:00Z"), ny); !got.Equal(utc("2026-11-01T03:00:00Z")) {
		t.Errorf("autumn night start %s", got)
	}
	// In the day, the night is the last one.
	if got := NightStart(utc("2026-10-20T15:00:00Z"), ny); !got.Equal(utc("2026-10-20T03:00:00Z")) {
		t.Errorf("day: night start %s", got)
	}
}

// The user's "night" and "morning" override the auto hours until the next edge (23:00 or 08:00);
// they do nothing when the night shift is off or on.
func TestNightSettingOn(t *testing.T) {
	ny := newYork(t)
	at := func(day, h, m int) time.Time { return time.Date(2026, 10, day, h, m, 0, 0, ny) }
	cases := []struct {
		name string
		set  NightSetting
		now  time.Time
		want bool
	}{
		{"unset is off", NightSetting{}, at(20, 2, 0), false},
		{"off at night", NightSetting{Mode: NightOff}, at(20, 2, 0), false},
		{"on in the day", NightSetting{Mode: NightAlways}, at(20, 14, 0), true},
		{"auto at night", NightSetting{Mode: NightAuto}, at(20, 2, 0), true},
		{"auto in the day", NightSetting{Mode: NightAuto}, at(20, 14, 0), false},
		{"night at 21:00", NightSetting{Mode: NightAuto, Override: OverrideNight, OverrideAt: at(19, 21, 0)}, at(19, 21, 5), true},
		{"night then the window", NightSetting{Mode: NightAuto, Override: OverrideNight, OverrideAt: at(19, 21, 0)}, at(20, 2, 0), true},
		{"night ends at 08:00", NightSetting{Mode: NightAuto, Override: OverrideNight, OverrideAt: at(19, 21, 0)}, at(20, 8, 0), false},
		{"morning at 06:30", NightSetting{Mode: NightAuto, Override: OverrideMorning, OverrideAt: at(20, 6, 30)}, at(20, 6, 31), false},
		{"morning lasts the day", NightSetting{Mode: NightAuto, Override: OverrideMorning, OverrideAt: at(20, 6, 30)}, at(20, 22, 59), false},
		{"morning ends at 23:00", NightSetting{Mode: NightAuto, Override: OverrideMorning, OverrideAt: at(20, 6, 30)}, at(20, 23, 0), true},
		{"night when off", NightSetting{Mode: NightOff, Override: OverrideNight, OverrideAt: at(19, 21, 0)}, at(19, 21, 5), false},
		{"morning when on", NightSetting{Mode: NightAlways, Override: OverrideMorning, OverrideAt: at(19, 21, 0)}, at(19, 21, 5), true},
	}
	for _, c := range cases {
		if got := c.set.On(c.now, ny); got != c.want {
			t.Errorf("%s: on %v, want %v", c.name, got, c.want)
		}
	}
}

func TestCheckNightSetting(t *testing.T) {
	cases := []struct {
		mode string
		off  []string
		want string
	}{
		{"auto", nil, ""},
		{"", nil, ""},
		{"on", []string{ClassScopeChoice, ClassTicketLimit}, ""},
		{"sometimes", nil, CodeInvalidField},
		{"auto", []string{"merges"}, CodeInvalidField},
	}
	for _, c := range cases {
		if got := code(CheckNightSetting(c.mode, c.off)); got != c.want {
			t.Errorf("%q %v: code %q, want %q", c.mode, c.off, got, c.want)
		}
	}
}

func TestCheckEscalationClass(t *testing.T) {
	cases := []struct {
		name, class, rec, deferred string
		want                       string
	}{
		{"none", "", "", "", ""},
		{"verify flake", ClassVerifyFlake, "re-run verify", "", ""},
		{"scope with the rest", ClassScopeChoice, "doc-only", "the code change", ""},
		{"scope without the rest", ClassScopeChoice, "doc-only", " ", CodeMissingField},
		{"no recommendation", ClassBounceLimit, "", "", CodeMissingField},
		{"ticket limit is a cap", ClassTicketLimit, "file it", "", CodeInvalidField},
		{"unknown", "merge", "merge it", "", CodeInvalidField},
		{"deferred without scope", ClassVerifyFlake, "re-run", "more", CodeInvalidField},
	}
	for _, c := range cases {
		if got := code(CheckEscalationClass(c.class, c.rec, c.deferred)); got != c.want {
			t.Errorf("%s: code %q, want %q", c.name, got, c.want)
		}
	}
}

// DecideNight: each class acts after the grace period and not before; an answer, a new question or
// the user speaking wins; the never-list stays escalated; the per-head and per-night limits.
func TestDecideNight(t *testing.T) {
	ny := newYork(t)
	escAt := time.Date(2026, 10, 20, 2, 0, 0, 0, ny)
	on := NightSetting{Mode: NightAuto}
	item := func(ret Stage) Item {
		return Item{ID: 7, Stage: NeedsYou, ReturnStage: ret, HeadSHA: "abc1234", Risk: RiskNormal, Question: "fix the nil check in handler.go"}
	}
	verify := NightCase{Esc: NightEscalation{Class: ClassVerifyFlake, Recommendation: "re-run verify", At: escAt, Night: true},
		Item: item(Build), NeedsYouOutcome: VerifyFailed}
	bounceC := NightCase{Esc: NightEscalation{Class: ClassBounceLimit, Recommendation: "one more build with the notes", At: escAt, Night: true},
		Item: item(Build), NeedsYouOutcome: Bounce}
	scope := NightCase{Esc: NightEscalation{Class: ClassScopeChoice, Recommendation: "doc-only for now", Deferred: "the code change", At: escAt, Night: true},
		Item: item(Build), NeedsYouOutcome: Question}
	with := func(c NightCase, f func(*NightCase)) NightCase { f(&c); return c }
	grace := escAt.Add(NightGrace)
	cases := []struct {
		name string
		c    NightCase
		set  NightSetting
		now  time.Time
		want NightVerdict
		why  string
	}{
		{"verify before grace", verify, on, grace.Add(-time.Second), NightWait, ""},
		{"verify at grace", verify, on, grace, NightAct, ""},
		{"bounce at grace", bounceC, on, grace, NightAct, ""},
		{"bounce before grace", bounceC, on, grace.Add(-time.Minute), NightWait, ""},
		{"scope at grace", scope, on, grace, NightAct, ""},
		{"scope before grace", scope, on, grace.Add(-time.Minute), NightWait, ""},

		{"answered in grace", with(verify, func(c *NightCase) { c.Item.Stage = Build }), on, grace, NightGone, "moved on"},
		{"new question", with(verify, func(c *NightCase) { c.NewQuestion = true }), on, grace, NightGone, "moved on"},
		{"user spoke", with(verify, func(c *NightCase) { c.UserSince = true }), on, grace, NightGone, "you"},

		{"night off", verify, NightSetting{Mode: NightOff}, grace, NightWait, ""},
		{"day", verify, on, time.Date(2026, 10, 20, 9, 0, 0, 0, ny), NightWait, ""},
		{"class off", verify, NightSetting{Mode: NightAuto, ClassesOff: []string{ClassVerifyFlake}}, grace, NightWait, ""},
		{"escalated in the day", with(verify, func(c *NightCase) { c.Esc.Night = false }), on, grace, NightStay, "night shift was off"},

		{"approval", with(verify, func(c *NightCase) { c.Esc.Approval = true }), on, grace, NightStay, "approval"},
		{"hold list", with(verify, func(c *NightCase) { c.Item.Risk, c.Item.HoldReason = RiskHold, "auth: internal/api/auth.go" }), on, grace, NightStay, "hold list"},
		{"trust pending", with(verify, func(c *NightCase) { c.Item.Trust = &Trust{HeadSHA: "abc1234"} }), on, grace, NightStay, "trusted"},
		{"merge", with(scope, func(c *NightCase) { c.Esc.Recommendation = "merge the doc part now" }), on, grace, NightStay, "merges"},
		{"credentials", with(scope, func(c *NightCase) { c.Esc.Deferred = "rotate the Slack token" }), on, grace, NightStay, "credentials"},
		{"release", with(scope, func(c *NightCase) { c.Esc.Recommendation = "cut a release with the docs" }), on, grace, NightStay, "releases"},
		{"prod", with(scope, func(c *NightCase) { c.Esc.Recommendation = "deploy to prod with the fix" }), on, grace, NightStay, "prod"},
		{"deleting work", with(scope, func(c *NightCase) { c.Esc.Recommendation = "delete the branch and start over" }), on, grace, NightStay, "deleting"},
		{"limits", with(scope, func(c *NightCase) { c.Esc.Recommendation = "raise the budget for this" }), on, grace, NightStay, "limits"},
		{"ci files", with(scope, func(c *NightCase) { c.Esc.Deferred = "change .github/workflows/ci.yml" }), on, grace, NightStay, "CI"},
		{"weaken a test", with(scope, func(c *NightCase) { c.Esc.Recommendation = "skip the flaky test" }), on, grace, NightStay, "test"},
		{"hand fix", with(verify, func(c *NightCase) { c.Esc.Recommendation = "fix the mirror by hand" }), on, grace, NightStay, "hand"},
		{"auth", with(bounceC, func(c *NightCase) { c.Item.Question = "the auth check is missing" }), on, grace, NightStay, "auth"},

		{"verify not a verify failure", with(verify, func(c *NightCase) { c.NeedsYouOutcome = Question }), on, grace, NightStay, "verify failure"},
		{"bounce not a bounce", with(bounceC, func(c *NightCase) { c.NeedsYouOutcome = VerifyFailed }), on, grace, NightStay, "bounce"},
		{"verify before a build", with(verify, func(c *NightCase) { c.Item.ReturnStage = Triage }), on, grace, NightStay, "build"},

		{"verify once per head", with(verify, func(c *NightCase) {
			c.Prior = []NightPrior{{Class: ClassVerifyFlake, HeadSHA: "abc1234", At: escAt.Add(-3 * time.Hour)}}
		}), on, grace, NightStay, "once per head"},
		{"verify new head", with(verify, func(c *NightCase) {
			c.Prior = []NightPrior{{Class: ClassVerifyFlake, HeadSHA: "0000fff", At: escAt.Add(-time.Hour)}}
		}), on, grace, NightAct, ""},
		{"bounce once per night", with(bounceC, func(c *NightCase) {
			c.Prior = []NightPrior{{Class: ClassBounceLimit, HeadSHA: "0000fff", At: escAt.Add(-2 * time.Hour)}}
		}), on, grace, NightWait, ""},
		{"bounce last night", with(bounceC, func(c *NightCase) {
			c.Prior = []NightPrior{{Class: ClassBounceLimit, HeadSHA: "0000fff", At: escAt.Add(-24 * time.Hour)}}
		}), on, grace, NightAct, ""},
		{"other class tonight", with(bounceC, func(c *NightCase) {
			c.Prior = []NightPrior{{Class: ClassVerifyFlake, HeadSHA: "abc1234", At: escAt.Add(-time.Hour)}}
		}), on, grace, NightAct, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, why := DecideNight(c.c, c.set, c.now, ny)
			if got != c.want {
				t.Fatalf("verdict %v (%s), want %v", got, why, c.want)
			}
			if c.why != "" && !strings.Contains(why, c.why) {
				t.Fatalf("why %q, want it to say %q", why, c.why)
			}
		})
	}
}

// The ticket cap rises to OrchAddsPerNight at night, for process tickets only.
func TestOrchAddCap(t *testing.T) {
	ny := newYork(t)
	night, day := time.Date(2026, 10, 20, 2, 0, 0, 0, ny), time.Date(2026, 10, 20, 10, 0, 0, 0, ny)
	on := NightSetting{Mode: NightAuto}
	cases := []struct {
		name    string
		set     NightSetting
		now     time.Time
		process bool
		want    int
	}{
		{"process at night", on, night, true, OrchAddsPerNight},
		{"other at night", on, night, false, OrchAddsPerDay},
		{"process in the day", on, day, true, OrchAddsPerDay},
		{"night shift off", NightSetting{Mode: NightOff}, night, true, OrchAddsPerDay},
		{"class off", NightSetting{Mode: NightAuto, ClassesOff: []string{ClassTicketLimit}}, night, true, OrchAddsPerDay},
	}
	for _, c := range cases {
		if got := OrchAddCap(c.set, c.now, ny, c.process); got != c.want {
			t.Errorf("%s: cap %d, want %d", c.name, got, c.want)
		}
	}
}
