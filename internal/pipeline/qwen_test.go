package pipeline

import (
	"testing"
)

// Routing with no pace (docs/decisions/2026-10-19-qwen-routing.md): a build with no builder set
// goes to qwen when the claiming runner has qwen and the qwen limit has room: P3–P5 of size S or
// M, or a P2 size S that triage says is docs or tests only.
func TestQwenRouting(t *testing.T) {
	ctl := DefaultControl()
	normal := Pace{Agents: ctl.ActiveAgents}
	slow := Pace{Level: PaceSlow, Agents: 2}
	req := ClaimRequest{Runner: "r", Agent: "a", Kinds: []string{WorkItem}}
	qwenReq := req
	qwenReq.Builders = []string{BuilderQwen}
	kind := func(it Item, k string) Item { it.ChangeKind = k; return it }
	fell := sized(waiting(1, Build, "P3", 0), "S", BuilderClaude)
	fell.QwenFailures = QwenFailLimit
	qwenBusy := []LiveLease{{ItemID: 9, Stage: Build, Qwen: true}}
	claudeLive := []LiveLease{{ItemID: 8, Stage: Build, Claude: true}, {ItemID: 9, Stage: Review, Claude: true}}
	noLimit := DefaultControl()
	delete(noLimit.Limits, LimitQwen)

	for _, tc := range []struct {
		name   string
		ctl    Control
		pace   Pace
		it     Item
		live   []LiveLease
		req    ClaimRequest
		want   bool // claimed
		routed bool
	}{
		{"P3 M", ctl, normal, sized(waiting(1, Build, "P3", 0), "M", ""), nil, qwenReq, true, true},
		{"P5 S", ctl, normal, sized(waiting(1, Build, "P5", 0), "S", ""), nil, qwenReq, true, true},
		{"P3 L builds with claude", ctl, normal, sized(waiting(1, Build, "P3", 0), "L", ""), nil, qwenReq, true, false},
		{"P2 M builds with claude", ctl, normal, sized(waiting(1, Build, "P2", 0), "M", ""), nil, qwenReq, true, false},
		{"P2 S code builds with claude", ctl, normal, kind(sized(waiting(1, Build, "P2", 0), "S", ""), ChangeCode), nil, qwenReq, true, false},
		{"P2 S of no change kind builds with claude", ctl, normal, sized(waiting(1, Build, "P2", 0), "S", ""), nil, qwenReq, true, false},
		{"P2 S docs", ctl, normal, kind(sized(waiting(1, Build, "P2", 0), "S", ""), ChangeDocs), nil, qwenReq, true, true},
		{"P2 S test", ctl, normal, kind(sized(waiting(1, Build, "P2", 0), "S", ""), ChangeTest), nil, qwenReq, true, true},
		{"P2 M docs builds with claude", ctl, normal, kind(sized(waiting(1, Build, "P2", 0), "M", ""), ChangeDocs), nil, qwenReq, true, false},
		{"P1 S docs builds with claude", ctl, normal, kind(sized(waiting(1, Build, "P1", 0), "S", ""), ChangeDocs), nil, qwenReq, true, false},
		{"builder claude stays", ctl, normal, sized(waiting(1, Build, "P4", 0), "S", BuilderClaude), nil, qwenReq, true, false},
		{"a fallen-back item stays claude", ctl, normal, fell, nil, qwenReq, true, false},
		{"no qwen on the runner", ctl, normal, sized(waiting(1, Build, "P3", 0), "M", ""), nil, req, true, false},
		{"triage is not routed", ctl, normal, sized(waiting(1, Triage, "P3", 0), "S", ""), nil, qwenReq, true, false},
		{"qwen busy: claude builds it", ctl, normal, sized(waiting(1, Build, "P3", 0), "M", ""), qwenBusy, qwenReq, true, false},
		{"qwen busy: a qwen item waits", ctl, normal, sized(waiting(1, Build, "P3", 0), "M", BuilderQwen), qwenBusy, qwenReq, false, false},
		{"qwen busy under a slow pace: it waits", ctl, slow, sized(waiting(1, Build, "P3", 0), "M", ""), append(qwenBusy, claudeLive...), qwenReq, false, false},
		{"no qwen limit: a second qwen build", noLimit, normal, sized(waiting(1, Build, "P3", 0), "M", ""), qwenBusy, qwenReq, true, true},
		{"slow, qwen free: routed", ctl, slow, sized(waiting(1, Build, "P4", 0), "M", ""), claudeLive, qwenReq, true, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w, ok := ChooseWorkPaced(tc.ctl, tc.pace, []Item{tc.it}, nil, tc.live, nil, nil, nil, nil, tc.req, nil)
			if ok != tc.want || ok && (w.Item == nil || w.RouteQwen != tc.routed) {
				t.Fatalf("ChooseWorkPaced = %+v, %v; want claimed %v routed %v", w, ok, tc.want, tc.routed)
			}
		})
	}
}

// The qwen limit is a control limit like gpu's, default 1, and the flow panel names it.
func TestQwenLimit(t *testing.T) {
	ctl := DefaultControl()
	if ctl.Limits[LimitQwen] != 1 {
		t.Fatalf("default qwen limit = %d, want 1", ctl.Limits[LimitQwen])
	}
	ctl.Limits[LimitQwen] = 2
	if err := ctl.Validate(); err != nil {
		t.Fatalf("Validate: %v", err)
	}
	busy := []LiveLease{{ItemID: 9, Stage: Build, Qwen: true}}
	qwenItem := sized(waiting(1, Build, "P3", 0), "M", BuilderQwen)
	normal := Pace{Agents: 4}
	if got := WaitReason(DefaultControl(), normal, qwenItem, nil, busy, nil, nil, nil, []string{BuilderQwen}); got != LimitReason(LimitQwen, 1) {
		t.Fatalf("a qwen item while qwen is busy: %q", got)
	}
	routed := sized(waiting(1, Build, "P3", 0), "M", "")
	if got := WaitReason(DefaultControl(), normal, routed, nil, busy, nil, nil, nil, []string{BuilderQwen}); got != WaitReady {
		t.Fatalf("a routable item while qwen is busy and claude is not paced: %q", got)
	}
	if got := WaitReason(DefaultControl(), Pace{Level: PacePaused}, routed, nil, busy, nil, nil, nil, []string{BuilderQwen}); got != WaitPaused {
		t.Fatalf("a routable item while qwen is busy and claude paused: %q", got)
	}
}

// A qwen build's failures and review bounces (docs/decisions/2026-10-19-qwen-routing.md): one
// review bounce of a qwen-built PR, or a second failed qwen build, makes claude the item's builder
// with a builder_fallback note naming the reason. Routed items (no builder set) count too.
func TestQwenFallback(t *testing.T) {
	qwenRun := func(extra map[string]string) map[string]string {
		f := map[string]string{"builder": BuilderQwen, "session_id": "ses_1"}
		for k, v := range extra {
			f[k] = v
		}
		return f
	}
	wantFallback := func(t *testing.T, tr Transition, reason string) {
		t.Helper()
		if tr.Item.Builder != BuilderClaude || tr.Item.BuildSession != "" {
			t.Fatalf("builder %q session %q, want claude and none", tr.Item.Builder, tr.Item.BuildSession)
		}
		if len(tr.Also) != 1 || tr.Also[0].Kind != KindBuilderFallback || tr.Also[0].Data["reason"] != reason ||
			tr.Also[0].Data["from"] != BuilderQwen || tr.Also[0].Data["to"] != BuilderClaude {
			t.Fatalf("also = %+v, want one builder_fallback for %s", tr.Also, reason)
		}
	}

	// pr_opened records who built the head: a routed item (no builder) built by qwen.
	it := item(Build)
	tr, err := ApplyAdvance(it, prOpened(sha2, clean, qwenRun(nil)), t0)
	wantCode(t, err, "")
	if tr.Item.BuiltBy != BuilderQwen || tr.Item.Builder != "" {
		t.Fatalf("pr_opened: built by %q builder %q", tr.Item.BuiltBy, tr.Item.Builder)
	}
	// A review bounce of it: claude builds next, still a bounce.
	rv := tr.Item
	rv.Stage = Review
	tr, err = ApplyAdvance(rv, Advance{Outcome: Bounce, Fields: map[string]string{"notes": "fix x"}, Agent: "r"}, t0)
	wantCode(t, err, "")
	wantFallback(t, tr, FallbackReviewBounce)
	if tr.Item.Stage != Build || tr.Item.BounceCount != 1 || tr.Data["built_by"] != BuilderQwen {
		t.Fatalf("bounce: %+v data %v", tr.Item, tr.Data)
	}
	// A review bounce of a claude build changes nothing.
	cl := rv
	cl.BuiltBy = BuilderClaude
	tr, _ = ApplyAdvance(cl, Advance{Outcome: Bounce, Fields: map[string]string{"notes": "x"}, Agent: "r"}, t0)
	if tr.Item.Builder != "" || len(tr.Also) != 0 {
		t.Fatalf("claude-built bounce: %+v %v", tr.Item, tr.Also)
	}

	// A qwen item's verify failure: the first keeps qwen and its session (resumed next).
	q := item(Build)
	q.Builder, q.BuildSession = BuilderQwen, "ses_1"
	tr, err = ApplyAdvance(q, Advance{Outcome: VerifyFailed, Fields: qwenRun(map[string]string{"reason": "verify failed"}), Evidence: 1}, t0)
	wantCode(t, err, "")
	if tr.Item.Builder != BuilderQwen || tr.Item.BuildSession != "ses_1" || tr.Item.QwenFailures != 1 || len(tr.Also) != 0 {
		t.Fatalf("first failure: %+v %v", tr.Item, tr.Also)
	}
	// The second (a failed run): claude from now on.
	tr, err = ApplyAdvance(tr.Item, Advance{Outcome: Failed, Fields: qwenRun(map[string]string{"error": "no result"})}, t0)
	wantCode(t, err, "")
	wantFallback(t, tr, FallbackFailedTwice)
	if tr.Also[0].Data["qwen_failures"] != 2 || tr.Also[0].Data["outcome"] != string(Failed) {
		t.Fatalf("data: %v", tr.Also[0].Data)
	}

	// A claude run's failure (a runner with no qwen config fell back) counts nothing.
	tr, _ = ApplyAdvance(q, Advance{Outcome: Failed, Fields: map[string]string{"builder": BuilderClaude, "error": "x"}}, t0)
	if tr.Item.QwenFailures != 0 || tr.Item.Builder != BuilderQwen {
		t.Fatalf("claude failure: %+v", tr.Item)
	}
	// Nor does the runner's own failure before the agent ran (no builder reported).
	tr, _ = ApplyAdvance(q, Advance{Outcome: Failed, Fields: map[string]string{"error": "the runner could not prepare the run"}}, t0)
	if tr.Item.QwenFailures != 0 {
		t.Fatalf("setup failure counted: %+v", tr.Item)
	}
	// Nor a trusted-file refusal: the change is the agent's, the refusal is verify's rule.
	tr, err = ApplyAdvance(q, Advance{Outcome: Failed, Fields: qwenRun(map[string]string{"error": "trusted",
		FieldTrustSHA: sha1, FieldTrustPaths: "tools/pipeline"})}, t0)
	wantCode(t, err, "")
	if tr.Item.QwenFailures != 0 {
		t.Fatalf("trust refusal counted: %+v", tr.Item)
	}

	// Red CI on a qwen-built head is a failed qwen build.
	ci := item(Land)
	ci.BuiltBy, ci.QwenFailures = BuilderQwen, 1
	tr, _ = ApplyAdvance(ci, Advance{Outcome: BackToBuild, Agent: "lander", Fields: map[string]string{"reason": "CI failed", "ci_head_sha": sha1}}, t0)
	wantFallback(t, tr, FallbackFailedTwice)

	// A run past the time cap (Overtime's caller, the sweep) counts as one too.
	ot, n := QwenFailed(item(Build), "overtime")
	if ot.QwenFailures != 1 || n != nil {
		t.Fatalf("overtime: %+v %v", ot, n)
	}
}

// Triage may say what kind of change it is and who should build it.
func TestTriageChangeKindAndBuilder(t *testing.T) {
	adv := func(extra map[string]string) Advance {
		f := map[string]string{"area": "docs", "size": "S", "risk": "normal", "resources": ""}
		for k, v := range extra {
			f[k] = v
		}
		return Advance{Outcome: Reproduced, Fields: f, Evidence: 1}
	}
	tr, err := ApplyAdvance(item(Triage), adv(map[string]string{"change_kind": "docs", "builder": "qwen"}), t0)
	wantCode(t, err, "")
	if tr.Item.ChangeKind != ChangeDocs || tr.Item.Builder != BuilderQwen {
		t.Fatalf("triage: kind %q builder %q", tr.Item.ChangeKind, tr.Item.Builder)
	}
	tr, err = ApplyAdvance(item(Triage), adv(nil), t0)
	wantCode(t, err, "")
	if tr.Item.ChangeKind != "" || tr.Item.Builder != "" {
		t.Fatalf("no fields: kind %q builder %q", tr.Item.ChangeKind, tr.Item.Builder)
	}
	_, err = ApplyAdvance(item(Triage), adv(map[string]string{"change_kind": "config"}), t0)
	wantCode(t, err, CodeInvalidField)
	_, err = ApplyAdvance(item(Triage), adv(map[string]string{"builder": "gpt"}), t0)
	wantCode(t, err, CodeInvalidField)
	// A plan's piece already named one: triage's stays out.
	planned := item(Triage)
	planned.Builder = BuilderClaude
	tr, _ = ApplyAdvance(planned, adv(map[string]string{"builder": "qwen"}), t0)
	if tr.Item.Builder != BuilderClaude {
		t.Fatalf("triage overrode the plan's builder: %q", tr.Item.Builder)
	}
}

func TestApplyBuilder(t *testing.T) {
	b := item(Build)
	b.QwenFailures, b.BuildSession, b.Builder = 2, "ses_1", BuilderClaude
	tr, err := ApplyBuilder(b, "qwen", false)
	wantCode(t, err, "")
	if tr.Item.Builder != BuilderQwen || tr.Item.QwenFailures != 0 || tr.Kind != KindBuilderSet ||
		tr.Data["from"] != BuilderClaude || tr.Data["to"] != BuilderQwen {
		t.Fatalf("set qwen: %+v %v", tr.Item, tr.Data)
	}
	q := item(Triage)
	q.Builder, q.BuildSession = BuilderQwen, "ses_1"
	tr, err = ApplyBuilder(q, "claude", false)
	wantCode(t, err, "")
	if tr.Item.Builder != BuilderClaude || tr.Item.BuildSession != "" {
		t.Fatalf("set claude: %+v", tr.Item)
	}
	tr, err = ApplyBuilder(q, "auto", false)
	wantCode(t, err, "")
	if tr.Item.Builder != "" {
		t.Fatalf("auto: %q", tr.Item.Builder)
	}
	asked := item(NeedsYou)
	asked.ReturnStage = Build
	_, err = ApplyBuilder(asked, "qwen", false)
	wantCode(t, err, "")

	_, err = ApplyBuilder(item(Build), "qwen", true)
	wantCode(t, err, CodeWrongStage)
	_, err = ApplyBuilder(item(Review), "qwen", false)
	wantCode(t, err, CodeWrongStage)
	onReview := item(NeedsYou)
	onReview.ReturnStage = Land
	_, err = ApplyBuilder(onReview, "qwen", false)
	wantCode(t, err, CodeWrongStage)
	_, err = ApplyBuilder(item(Build), "gpt", false)
	wantCode(t, err, CodeInvalidField)
}

// A review verdict records who built the head; a builder already claude gets no second fallback.
func TestQwenReviewBuiltBy(t *testing.T) {
	rv := item(Review)
	rv.BuiltBy = BuilderQwen
	tr, err := ApplyAdvance(rv, Advance{Outcome: Approved, Fields: map[string]string{"summary": "ok", "head_sha": sha1}, Agent: "r"}, t0)
	wantCode(t, err, "")
	if tr.Data["built_by"] != BuilderQwen {
		t.Fatalf("approved data %v", tr.Data)
	}
	rv.Builder = BuilderClaude
	tr, _ = ApplyAdvance(rv, Advance{Outcome: Bounce, Fields: map[string]string{"notes": "x"}, Agent: "r"}, t0)
	if len(tr.Also) != 0 || tr.Data["built_by"] != BuilderQwen {
		t.Fatalf("bounce of a claude item's qwen head: also %v data %v", tr.Also, tr.Data)
	}
}

// The Claude spend qwen's builds replaced: per size, the mean claude build's cost times qwen's
// builds of it; review and triage runs are not builds.
func TestQwenReplaced(t *testing.T) {
	sizes := map[int64]string{1: "S", 2: "S", 3: "M", 4: "S", 5: "M", 6: "L"}
	runs := []UsageRun{
		{ItemID: 1, Stage: "build", Provider: ProviderClaude, CostUSD: 1},
		{ItemID: 2, Stage: "build", Provider: ProviderClaude, CostUSD: 3},
		{ItemID: 2, Stage: "review", Provider: ProviderClaude, CostUSD: 50},
		{ItemID: 3, Stage: "build", Provider: ProviderClaude, CostUSD: 5},
		{ItemID: 4, Stage: "build", Provider: ProviderQwen},
		{ItemID: 4, Stage: "build", Provider: ProviderQwen},
		{ItemID: 5, Stage: "build", Provider: ProviderQwen},
		{ItemID: 6, Stage: "build", Provider: ProviderQwen},
	}
	builds, usd, unpriced := QwenReplaced(runs, sizes)
	if builds != 4 || usd != 2*2+5 || unpriced != 1 {
		t.Fatalf("QwenReplaced = %d, %v, %d; want 4, 9, 1", builds, usd, unpriced)
	}
	q := QwenWeek{Builds: 4, ReplacedUSD: 9, Unpriced: 1, Reviewed: 4, FirstApproved: 3, Fallbacks: 1}
	want := "qwen builds (7 days): 4, about $9.00 of claude builds replaced (estimate: same-size claude builds; 1 of a size claude did not build) · first review approved 3 of 4 (75%) · 1 fell back to claude"
	if got := q.Line(); got != want {
		t.Fatalf("Line:\n got %q\nwant %q", got, want)
	}
	if got := (QwenWeek{}).Line(); got != "qwen builds (7 days): none" {
		t.Fatalf("empty Line %q", got)
	}
}
