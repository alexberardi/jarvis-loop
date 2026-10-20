package pipeline

import (
	"fmt"
	"strings"
)

// The Qwen builder's routing and fallback (docs/decisions/2026-10-19-qwen-routing.md): which
// builds go to qwen with no builder set, the one-at-a-time limit on prod's background slot, the
// user's (or triage's) choice of builder, and when an item's builds go back to claude.

// The kinds of change triage says an item is (Item.ChangeKind; "" is code).
const (
	ChangeCode = "code"
	ChangeDocs = "docs"
	ChangeTest = "test"
)

// ChangeKinds are the values of triage's change_kind.
var ChangeKinds = []string{ChangeCode, ChangeDocs, ChangeTest}

// LimitQwen is the limit on qwen builds at once, across runners (Control.Limits, default 1): the
// model is prod's background label, which the household's background jobs share.
const LimitQwen = "qwen"

// QwenFailLimit is how many failed qwen builds of an item (a failed run, a failed verify, red CI,
// a run past the time cap) send its next build to claude. One review bounce of a qwen build does.
const QwenFailLimit = 2

// Event kinds: an item's builder fell back to claude (data reason), and the user or triage set it.
const (
	KindBuilderFallback = "builder_fallback"
	KindBuilderSet      = "builder_set"
)

// The reasons of loopd's builder_fallback.
const (
	FallbackFailedTwice  = "failed_twice"
	FallbackReviewBounce = "review_bounce"
)

// autoRoutable: a build with no builder set that goes to qwen when the claiming runner has qwen
// and the qwen limit has room: P3–P5 of size S or M, or P2 of size S that triage says changes only
// docs or tests. A builder set (a plan's piece, loop builder, triage, a fallback) stays.
func autoRoutable(it Item) bool {
	if it.Stage != Build || it.Builder != "" {
		return false
	}
	switch p := PriorityRank(it.Priority); {
	case p >= 3 && p <= 5:
		return it.Size == "S" || it.Size == "M"
	case p == 2:
		return it.Size == "S" && (it.ChangeKind == ChangeDocs || it.ChangeKind == ChangeTest)
	}
	return false
}

// qwenFree reports whether another qwen build may start (no qwen limit set: no limit).
func qwenFree(c Control, inUse map[string]int) bool {
	limit, ok := c.Limits[LimitQwen]
	return !ok || inUse[LimitQwen] < limit
}

// QwenFailed records a failed qwen build of it (outcome: what failed); at QwenFailLimit its next
// build goes to claude, with the builder_fallback note to record (none when claude builds it
// already).
func QwenFailed(it Item, outcome string) (Item, *Note) {
	it.QwenFailures++
	if it.QwenFailures < QwenFailLimit || it.Builder == BuilderClaude {
		return it, nil
	}
	it, n := qwenFallback(it, FallbackFailedTwice, outcome)
	return it, &n
}

// qwenFallback makes claude the item's builder for the rest (a new session; loop builder can
// set qwen again) and says why.
func qwenFallback(it Item, reason, outcome string) (Item, Note) {
	it.Builder, it.BuildSession = BuilderClaude, ""
	return it, Note{Kind: KindBuilderFallback, Data: map[string]any{
		"from": BuilderQwen, "to": BuilderClaude, "reason": reason, "outcome": outcome,
		"qwen_failures": it.QwenFailures, "bounce_count": it.BounceCount}}
}

// ParseBuilder reads a builder the user or triage gave: qwen, claude, or auto ("" or none too:
// no builder set, so routing decides).
func ParseBuilder(raw string) (string, error) {
	switch b := strings.ToLower(strings.TrimSpace(raw)); b {
	case BuilderQwen, BuilderClaude:
		return b, nil
	case "", "auto", "none":
		return "", nil
	}
	return "", errf(CodeInvalidField, "builder must be qwen, claude or auto, not %q", raw)
}

// BuilderSettable reports whether the item's builder may change now (ApplyBuilder): in triage or
// build, or on a question that returns to one, and no run holding it (building).
func BuilderSettable(it Item, building bool) bool {
	stage := it.Stage
	if stage == NeedsYou {
		stage = it.ReturnStage
	}
	return (stage == Triage || stage == Build) && !building
}

// ApplyBuilder sets who builds the item (loop builder), before its build starts: in triage, on a
// question, or waiting in build (building: a build run holds it now). Setting one starts its
// count of failed qwen builds over; a builder other than qwen drops the qwen session.
func ApplyBuilder(it Item, raw string, building bool) (Transition, error) {
	b, err := ParseBuilder(raw)
	if err != nil {
		return Transition{}, err
	}
	if !BuilderSettable(it, building) {
		what := string(it.Stage)
		if building {
			what = "building now"
		}
		return Transition{}, errf(CodeWrongStage, "item is %s: the builder changes only before a build starts", what)
	}
	data := map[string]any{"from": it.Builder, "to": b}
	it.Builder, it.QwenFailures = b, 0
	if b != BuilderQwen {
		it.BuildSession = ""
	}
	return Transition{Item: it, From: it.Stage, To: it.Stage, Kind: KindBuilderSet, Data: data}, nil
}

// QwenWeek is the Qwen builder's last 7 days, for the usage report (loop usage, the board).
type QwenWeek struct {
	// Builds are qwen's build runs.
	Builds int `json:"builds"`
	// ReplacedUSD estimates the Claude spend qwen's builds replaced (QwenReplaced); Unpriced are
	// qwen's runs of a size no claude build run of the week priced.
	ReplacedUSD float64 `json:"replaced_usd"`
	Unpriced    int     `json:"unpriced,omitempty"`
	// Reviewed are the items whose first review verdict, this week, judged a qwen build;
	// FirstApproved are those it approved.
	Reviewed      int `json:"reviewed"`
	FirstApproved int `json:"first_approved"`
	// Fallbacks are the items loopd turned to claude (builder_fallback, not a runner's one-run one).
	Fallbacks int `json:"fallbacks"`
}

// QwenReplaced estimates what qwen's build runs among runs would have cost with claude: per size
// (sizes, by item id), the mean cost of the claude build runs of that size, times qwen's build runs
// of it. A size no claude build run priced counts in unpriced.
func QwenReplaced(runs []UsageRun, sizes map[int64]string) (builds int, usd float64, unpriced int) {
	type mean struct {
		sum float64
		n   int
	}
	claude, qwen := map[string]*mean{}, map[string]int{}
	for _, r := range runs {
		if r.Stage != string(Build) {
			continue
		}
		size := sizes[r.ItemID]
		switch r.Provider {
		case ProviderClaude:
			m := claude[size]
			if m == nil {
				m = &mean{}
				claude[size] = m
			}
			m.sum += r.CostUSD
			m.n++
		case ProviderQwen:
			qwen[size]++
			builds++
		}
	}
	for size, n := range qwen {
		if m := claude[size]; m != nil {
			usd += m.sum / float64(m.n) * float64(n)
		} else {
			unpriced += n
		}
	}
	return builds, usd, unpriced
}

// Line is the week in one line.
func (q QwenWeek) Line() string {
	if q.Builds == 0 && q.Reviewed == 0 && q.Fallbacks == 0 {
		return "qwen builds (7 days): none"
	}
	l := fmt.Sprintf("qwen builds (7 days): %d, about $%.2f of claude builds replaced (estimate: same-size claude builds", q.Builds, q.ReplacedUSD)
	if q.Unpriced > 0 {
		l += fmt.Sprintf("; %d of a size claude did not build", q.Unpriced)
	}
	l += ")"
	if q.Reviewed > 0 {
		l += fmt.Sprintf(" · first review approved %d of %d (%d%%)", q.FirstApproved, q.Reviewed, q.FirstApproved*100/q.Reviewed)
	}
	if q.Fallbacks > 0 {
		l += fmt.Sprintf(" · %d fell back to claude", q.Fallbacks)
	}
	return l
}
