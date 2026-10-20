package pipeline

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// Usage awareness (docs/decisions/2026-10-18-usage-awareness.md): every run's usage goes into a
// ledger, the limits the CLI reports are learned from it, and claims are paced before a limit, never
// in the middle of a run.

// The providers whose usage the loop records. Only claude's is paced: qwen is free, and Gemini has
// no builder yet.
const (
	ProviderClaude = "claude"
	ProviderQwen   = "qwen"
)

// BudgetProviders are the providers a budget can be set for.
var BudgetProviders = []string{ProviderClaude}

// The usage windows: Claude's plan has a rolling 5-hour session window and a weekly cap.
const (
	Window5h   = "5h"
	WindowWeek = "week"
	Session    = 5 * time.Hour
	WeekLen    = 7 * 24 * time.Hour
)

// DefaultPaceAt is the share of a window's budget (in percent) at which pacing starts.
const DefaultPaceAt = 85

// UnknownResetPause is how long claude claims pause after a limit whose reset time the CLI did not
// give (or gave in a form we cannot read): the next run after it either works or hits the limit
// again at once, which costs nothing.
const UnknownResetPause = time.Hour

// The pace levels.
const (
	PaceNormal = 0
	PaceSlow   = 1
	PacePaused = 2
)

// Budget is the user's budget for one provider, per window: in burn units (Burn), or in percent of
// the window's burn when its last limit hit ("what got us limited last time"). Zero: none.
type Budget struct {
	Per5h      int64 `json:"per_5h,omitempty"`
	PerWeek    int64 `json:"per_week,omitempty"`
	Per5hPct   int   `json:"per_5h_pct,omitempty"`
	PerWeekPct int   `json:"per_week_pct,omitempty"`
	// PaceAt is the percent of a budget at which pacing starts (0: DefaultPaceAt).
	PaceAt int `json:"pace_at,omitempty"`
	// Warnings is what the CLI's allowed_warning does: WarningsPace ("" too) slows claims for the
	// window, WarningsIgnore leaves the pace to the budget and the limit hits.
	Warnings string `json:"warnings,omitempty"`
}

// The Budget.Warnings values.
const (
	WarningsPace   = "pace"
	WarningsIgnore = "ignore"
)

// DefaultBudgets: claude's budget is what got it limited last time, in both windows.
func DefaultBudgets() map[string]Budget {
	return map[string]Budget{ProviderClaude: {Per5hPct: 100, PerWeekPct: 100}}
}

// Validate checks the ranges.
func (b Budget) Validate() error {
	if b.Per5h < 0 || b.PerWeek < 0 {
		return errf(CodeInvalidField, "a budget must be 0 or more")
	}
	if b.Per5hPct < 0 || b.Per5hPct > 100 || b.PerWeekPct < 0 || b.PerWeekPct > 100 {
		return errf(CodeInvalidField, "a budget in percent of the last limit must be 0 to 100")
	}
	if b.Per5h > 0 && b.Per5hPct > 0 || b.PerWeek > 0 && b.PerWeekPct > 0 {
		return errf(CodeInvalidField, "a window's budget is in tokens or in percent of the last limit, not both")
	}
	if b.PaceAt < 0 || b.PaceAt > 99 {
		return errf(CodeInvalidField, "pace_at must be 1 to 99 (0: %d)", DefaultPaceAt)
	}
	if b.Warnings != "" && b.Warnings != WarningsPace && b.Warnings != WarningsIgnore {
		return errf(CodeInvalidField, "warnings must be %s or %s", WarningsPace, WarningsIgnore)
	}
	return nil
}

func (b Budget) paceAt() int {
	if b.PaceAt == 0 {
		return DefaultPaceAt
	}
	return b.PaceAt
}

// UsageRun is one agent run's usage, from the run's advance.
type UsageRun struct {
	At       time.Time `json:"at"`
	Started  time.Time `json:"started"`
	ItemID   int64     `json:"item_id"`
	Stage    string    `json:"stage"`
	Provider string    `json:"provider"`
	Model    string    `json:"model,omitempty"`
	Input    int64     `json:"input"`
	Output   int64     `json:"output"`
	// CacheCreation and CacheRead are claude's (qwen reports none).
	CacheCreation int64   `json:"cache_creation"`
	CacheRead     int64   `json:"cache_read"`
	CostUSD       float64 `json:"cost_usd"`
	Requests      int64   `json:"requests,omitempty"`
	// Warning: the CLI said the run is close to a limit (rate_limit_event allowed_warning).
	Warning bool `json:"warning,omitempty"`
}

// Burn is the run's usage in one unit: input-token equivalents weighted as Anthropic prices them
// (output 5×, cache writes 1.25×, cache reads 0.1×, the same ratios on every current model), so
// that cache reads, which dominate the raw counts, weigh what they cost. Qwen's are plain tokens.
func (r UsageRun) Burn() int64 {
	if r.Provider != ProviderClaude {
		return r.Input + r.Output
	}
	return r.Input + 5*r.Output + r.CacheCreation*5/4 + r.CacheRead/10
}

// UsageHit is a usage limit a run hit.
type UsageHit struct {
	At       time.Time `json:"at"`
	Provider string    `json:"provider"`
	Window   string    `json:"window"`
	// ResetAt is the reset the CLI reported; zero when it gave none we could read.
	ResetAt time.Time `json:"reset_at"`
	// Burn is the window's burn when the limit hit: the limit, as far as the loop can see it.
	Burn   int64  `json:"burn"`
	ItemID int64  `json:"item_id,omitempty"`
	Error  string `json:"error,omitempty"`
}

// LimitWindow says which window a limit is: the rate_limit_event's rateLimitType when the CLI gave
// one, else the text (weekly and per-model limits are weekly; the session limit is the 5h window).
func LimitWindow(errText, rateLimitType string) string {
	if w, ok := LimitWindowKnown(errText, rateLimitType); ok {
		return w
	}
	return Window5h
}

// LimitWindowKnown is LimitWindow without the default: false when neither the type nor the text
// names the window (a bare "rate limit: rejected").
func LimitWindowKnown(errText, rateLimitType string) (string, bool) {
	switch t := strings.ToLower(rateLimitType); {
	case strings.HasPrefix(t, "five_hour"):
		return Window5h, true
	case strings.HasPrefix(t, "seven_day"):
		return WindowWeek, true
	}
	e := strings.ToLower(errText)
	switch {
	case strings.Contains(e, "weekly") || strings.Contains(e, "opus limit") || strings.Contains(e, "sonnet limit"):
		return WindowWeek, true
	case strings.Contains(e, "session limit") || strings.Contains(e, "usage limit reached"):
		return Window5h, true
	}
	return "", false
}

var (
	resetTZRe   = regexp.MustCompile(`\s*\(([A-Za-z_]+(?:/[A-Za-z_+\-0-9]+)*)\)\s*$`)
	resetTimeRe = regexp.MustCompile(`(?i)^(?:(mon|tue|wed|thu|fri|sat|sun)[a-z]*\.?,?\s+)?(\d{1,2})(?::(\d{2}))?\s*(am|pm)$`)
	weekdays    = map[string]time.Weekday{"sun": time.Sunday, "mon": time.Monday, "tue": time.Tuesday, "wed": time.Wednesday,
		"thu": time.Thursday, "fri": time.Friday, "sat": time.Saturday}
)

// ParseReset reads a limit's reset time: RFC 3339 (from a rate_limit_event's resetsAt), or the
// CLI's text ("3:45pm", "Mon 12:00am", either with an IANA zone in parentheses) as the next such
// time after the hit, in that zone or loc.
func ParseReset(text string, hitAt time.Time, loc *time.Location) (time.Time, bool) {
	text = strings.TrimSpace(text)
	if t, err := time.Parse(time.RFC3339, text); err == nil {
		return t, true
	}
	if m := resetTZRe.FindStringSubmatch(text); m != nil {
		if l, err := time.LoadLocation(m[1]); err == nil {
			loc = l
		}
		text = strings.TrimSpace(text[:len(text)-len(m[0])])
	}
	m := resetTimeRe.FindStringSubmatch(text)
	if m == nil || loc == nil {
		return time.Time{}, false
	}
	h, _ := strconv.Atoi(m[2])
	mins := 0
	if m[3] != "" {
		mins, _ = strconv.Atoi(m[3])
	}
	if h < 1 || h > 12 || mins > 59 {
		return time.Time{}, false
	}
	h %= 12
	if strings.EqualFold(m[4], "pm") {
		h += 12
	}
	local := hitAt.In(loc)
	t := time.Date(local.Year(), local.Month(), local.Day(), h, mins, 0, 0, loc)
	if m[1] != "" {
		wd := weekdays[strings.ToLower(m[1])[:3]]
		t = t.AddDate(0, 0, (int(wd)-int(t.Weekday())+7)%7)
		if !t.After(hitAt) {
			t = t.AddDate(0, 0, 7)
		}
		return t, true
	}
	if !t.After(hitAt) {
		t = t.AddDate(0, 0, 1)
	}
	return t, true
}

// UsageState is one provider's usage now: the open 5h window and the week, their budgets in burn
// units, and a pause from a limit hit.
type UsageState struct {
	Provider string    `json:"provider"`
	Now      time.Time `json:"now"`
	// WindowStart and WindowEnd are the open 5h window (zero when none is open: the next run opens one).
	WindowStart time.Time `json:"window_start"`
	WindowEnd   time.Time `json:"window_end"`
	Burn5h      int64     `json:"burn_5h"`
	// WeekStart is the week's start: from the last weekly reset the CLI reported, else 7 days ago
	// (a rolling week, WeekEnd zero).
	WeekStart time.Time `json:"week_start"`
	WeekEnd   time.Time `json:"week_end"`
	BurnWeek  int64     `json:"burn_week"`
	// Limit5h and LimitWeek are the budgets in burn units (0: none known).
	Limit5h   int64 `json:"limit_5h"`
	LimitWeek int64 `json:"limit_week"`
	// PausedUntil is a limit hit's reset still ahead (zero: none).
	PausedUntil time.Time `json:"paused_until"`
	PauseWindow string    `json:"pause_window,omitempty"`
	// Warning: a run in the open window got the CLI's allowed_warning.
	Warning bool `json:"warning,omitempty"`
	// LastHit is when the latest limit hit came in (zero: none); it ends a lift of the pace
	// (Control.PaceLifted).
	LastHit time.Time `json:"last_hit"`
}

// ComputeUsage works out a provider's UsageState from its runs (oldest first; the store passes the
// last week and a window) and its limit hits (the latest per window is what counts).
//
// The plan's 5h window starts at the first use after the last one ended; the loop sees only its own
// runs, so a window opens when a run starts after the previous window ended, and lasts 5h, except
// that a window with a reported reset ends at that reset. A run counts in the window it ended in.
func ComputeUsage(provider string, b Budget, runs []UsageRun, hits []UsageHit, now time.Time) UsageState {
	st := UsageState{Provider: provider, Now: now}
	var last5h, lastWeek *UsageHit
	for i := range hits {
		h := &hits[i]
		if h.Provider != provider {
			continue
		}
		if h.At.After(st.LastHit) {
			st.LastHit = h.At
		}
		if h.Window == WindowWeek {
			if lastWeek == nil || h.At.After(lastWeek.At) {
				lastWeek = h
			}
		} else if last5h == nil || h.At.After(last5h.At) {
			last5h = h
		}
		until := h.ResetAt
		if until.IsZero() || !until.After(h.At) {
			until = h.At.Add(UnknownResetPause)
		}
		if until.After(now) && until.After(st.PausedUntil) {
			st.PausedUntil, st.PauseWindow = until, h.Window
		}
	}
	anchor := func(t time.Time) (time.Time, bool) {
		for _, h := range hits {
			if h.Provider == provider && h.Window != WindowWeek && !h.ResetAt.IsZero() &&
				!t.Before(h.ResetAt.Add(-Session)) && t.Before(h.ResetAt) {
				return h.ResetAt, true
			}
		}
		return time.Time{}, false
	}
	var start, end time.Time
	var burn int64
	warn := false
	for _, r := range runs {
		if r.Provider != provider || r.At.After(now) {
			continue
		}
		if end.IsZero() || !r.At.Before(end) {
			opened := r.Started
			if opened.IsZero() || opened.After(r.At) {
				opened = r.At
			}
			if !end.IsZero() && opened.Before(end) {
				opened = end
			}
			if reset, ok := anchor(r.At); ok {
				start, end = reset.Add(-Session), reset
			} else {
				start, end = opened, opened.Add(Session)
			}
			burn, warn = 0, false
		}
		burn += r.Burn()
		warn = warn || r.Warning
	}
	if !end.IsZero() && now.Before(end) {
		st.WindowStart, st.WindowEnd, st.Burn5h, st.Warning = start, end, burn, warn
	}

	st.WeekStart = now.Add(-WeekLen)
	if lastWeek != nil && !lastWeek.ResetAt.IsZero() {
		ws := lastWeek.ResetAt
		for ws.After(now) {
			ws = ws.Add(-WeekLen)
		}
		for !ws.Add(WeekLen).After(now) {
			ws = ws.Add(WeekLen)
		}
		st.WeekStart, st.WeekEnd = ws, ws.Add(WeekLen)
	}
	for _, r := range runs {
		if r.Provider == provider && !r.At.Before(st.WeekStart) && !r.At.After(now) {
			st.BurnWeek += r.Burn()
		}
	}

	st.Limit5h = limitFor(b.Per5h, b.Per5hPct, last5h)
	st.LimitWeek = limitFor(b.PerWeek, b.PerWeekPct, lastWeek)
	return st
}

func limitFor(tokens int64, pct int, last *UsageHit) int64 {
	if tokens > 0 {
		return tokens
	}
	if pct > 0 && last != nil && last.Burn > 0 {
		return last.Burn * int64(pct) / 100
	}
	return 0
}

// Pace is how claims of a provider's work go now.
type Pace struct {
	Level int `json:"level"`
	// Agents is the most runs of the provider's work at once (the active-agents number when normal).
	Agents int `json:"agents"`
	// Until is when a pause ends (zero: when the window's burn allows, re-checked at every claim).
	Until time.Time `json:"until"`
	// Window is the window that set the pace ("5h", "week"); Percent its burn in percent of its budget.
	Window  string `json:"window,omitempty"`
	Percent int    `json:"percent"`
	// Hit: the pause is a limit the CLI reported, not the budget.
	Hit bool `json:"hit,omitempty"`
	// Lifted: the user lifted pacing (loop pace normal), until Until (zero: the next limit hit).
	Lifted bool `json:"lifted,omitempty"`
}

// PaceFor decides the pace: paused while a limit hit's reset is ahead; normal while the user lifted
// pacing (Control.PaceLifted); paused while a window is at its budget; slow from pace_at (default
// 85%) of a budget, or when the CLI warned (unless the budget ignores its warnings), with one agent
// fewer at once and one fewer again every further step to the budget (never below one); normal
// otherwise.
func PaceFor(c Control, b Budget, st UsageState) Pace {
	p := Pace{Level: PaceNormal, Agents: c.ActiveAgents}
	pct5, pctW := percentOf(st.Burn5h, st.Limit5h), percentOf(st.BurnWeek, st.LimitWeek)
	p.Window, p.Percent = Window5h, pct5
	if pctW > pct5 {
		p.Window, p.Percent = WindowWeek, pctW
	}
	if st.PausedUntil.After(st.Now) {
		p.Level, p.Agents, p.Until, p.Hit, p.Window = PacePaused, 0, st.PausedUntil, true, st.PauseWindow
		return p
	}
	if c.PaceLifted(st) {
		p.Lifted, p.Until = true, c.PaceNormalUntil
		return p
	}
	if p.Percent >= 100 {
		p.Level, p.Agents = PacePaused, 0
		if p.Window == Window5h {
			p.Until = st.WindowEnd
		} else {
			p.Until = st.WeekEnd
		}
		return p
	}
	at := b.paceAt()
	if p.Percent < at && (!st.Warning || b.Warnings == WarningsIgnore) {
		return p
	}
	n := c.ActiveAgents
	p.Level, p.Agents = PaceSlow, n-1
	if over := p.Percent - at; over > 0 {
		p.Agents -= over * (n - 1) / (100 - at)
	}
	p.Agents = max(p.Agents, min(n, 1))
	return p
}

func percentOf(burn, limit int64) int {
	if limit <= 0 {
		return 0
	}
	return int(burn * 100 / limit)
}

// Forecast is when the open 5h window's budget runs out at the window's rate so far; false when it
// lasts to the window's end (or there is no window or budget).
func Forecast(st UsageState) (time.Time, bool) {
	if st.WindowStart.IsZero() || st.Limit5h <= 0 || st.Burn5h <= 0 {
		return time.Time{}, false
	}
	elapsed := st.Now.Sub(st.WindowStart)
	if elapsed <= 0 {
		return time.Time{}, false
	}
	left := st.Limit5h - st.Burn5h
	if left <= 0 {
		return st.Now, true
	}
	t := st.Now.Add(time.Duration(float64(elapsed) * float64(left) / float64(st.Burn5h)))
	if !t.Before(st.WindowEnd) {
		return time.Time{}, false
	}
	return t, true
}

// Describe says the pace in a few words, for Slack and the board.
func (p Pace) Describe(loc *time.Location) string {
	window := "the 5h window"
	if p.Window == WindowWeek {
		window = "the week"
	}
	switch p.Level {
	case PacePaused:
		until := "the window frees up"
		if !p.Until.IsZero() {
			until = p.Until.In(loc).Format("Mon 15:04")
		}
		if p.Hit {
			return "paused until " + until + " (the usage limit was hit)"
		}
		return fmt.Sprintf("paused until %s (%s is at %d%% of its budget)", until, window, p.Percent)
	case PaceSlow:
		if p.Percent == 0 {
			return fmt.Sprintf("pacing: the CLI warned of a limit; %d at once", p.Agents)
		}
		return fmt.Sprintf("pacing: %s is at %d%% of its budget; %d at once", window, p.Percent, p.Agents)
	}
	if p.Lifted {
		if p.Until.IsZero() {
			return "normal (pacing lifted until the next limit hit)"
		}
		return "normal (pacing lifted until " + p.Until.In(loc).Format("Mon 15:04") + ")"
	}
	return "normal"
}

// FormatTokens writes a token count short: 950, 12.3k, 4.1M.
func FormatTokens(n int64) string {
	switch {
	case n >= 1_000_000:
		return strconv.FormatFloat(float64(n)/1e6, 'f', 1, 64) + "M"
	case n >= 10_000:
		return strconv.FormatFloat(float64(n)/1e3, 'f', 0, 64) + "k"
	case n >= 1_000:
		return strconv.FormatFloat(float64(n)/1e3, 'f', 1, 64) + "k"
	}
	return strconv.FormatInt(n, 10)
}

// ParseTokens reads a budget in tokens: 1500000, 1.5M, 500k.
func ParseTokens(s string) (int64, error) {
	s = strings.TrimSpace(strings.ToLower(s))
	mult := 1.0
	switch {
	case strings.HasSuffix(s, "m"):
		mult, s = 1e6, strings.TrimSuffix(s, "m")
	case strings.HasSuffix(s, "k"):
		mult, s = 1e3, strings.TrimSuffix(s, "k")
	}
	f, err := strconv.ParseFloat(strings.ReplaceAll(s, "_", ""), 64)
	if err != nil || f < 0 {
		return 0, errf(CodeInvalidField, "a budget is tokens (1500000, 1.5M, 500k) or a percent of the last limit (80%%)")
	}
	return int64(f * mult), nil
}

// DescribeBudget says a budget in a few words.
func DescribeBudget(b Budget) string {
	one := func(tokens int64, pct int) string {
		switch {
		case tokens > 0:
			return FormatTokens(tokens)
		case pct > 0:
			return fmt.Sprintf("%d%% of the last limit", pct)
		}
		return "none"
	}
	s := fmt.Sprintf("per 5h %s · per week %s · pacing from %d%%", one(b.Per5h, b.Per5hPct), one(b.PerWeek, b.PerWeekPct), b.paceAt())
	if b.Warnings == WarningsIgnore {
		s += " · CLI warnings ignored"
	}
	return s
}
