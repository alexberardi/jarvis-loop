package runner

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

// The second opinion's flow (docs/decisions/2026-10-18-gemini-second-opinion.md): a review run
// approved; Gemini (agy, gemini.go) looks for what the review missed; a Claude turn in the same
// worktree judges each finding; one PR comment, one evidence file and one second_opinion event
// record it all. Advisory by default: only with gemini.blocking does an agreed high finding send
// the item back to build, and never twice for one head.

// Second opinion statuses.
const (
	SOAnswered    = "ok"          // Gemini answered (maybe with no findings) and every finding was judged
	SOUnjudged    = "unjudged"    // Gemini answered, but the adjudication turn gave no usable verdicts
	SOUnavailable = "unavailable" // agy gave nothing usable (quota, timeout, error, bad output)
)

// KindSecondOpinion is the runner event that records a second opinion (loopd's api.go).
const KindSecondOpinion = "second_opinion"

// AgyNoTime: the run had too little of its time cap left for agy and the adjudication.
const AgyNoTime = "no_time"

// adjudicationReserve is the run time kept for the adjudication turn after agy; the turn itself
// is capped at adjudicationMax.
const (
	adjudicationReserve = 5 * time.Minute
	adjudicationMax     = 10 * time.Minute
)

// maxPromptDiff caps the diff put in agy's prompt (one argv string, at most 128 KiB on Linux).
const maxPromptDiff = 80 << 10

// Verdict is Claude's judgment of one finding.
type Verdict struct {
	ID      int    `json:"id"`
	Verdict string `json:"verdict"` // agree or disagree
	Reason  string `json:"reason"`
}

// SecondOpinion is what the second opinion on one reviewed head came to.
type SecondOpinion struct {
	Status  string `json:"status"`
	Reason  string `json:"reason,omitempty"` // why unavailable or unjudged
	Model   string `json:"model"`
	HeadSHA string `json:"head_sha"`
	// Findings are Gemini's (numbered), Verdicts Claude's by finding ID.
	Findings []Finding       `json:"findings"`
	Verdicts map[int]Verdict `json:"verdicts,omitempty"`
	Dropped  int             `json:"dropped,omitempty"`
	Usage    string          `json:"usage,omitempty"`
	Blocking bool            `json:"blocking"`
	// Bounced: blocking sent the item back to build with the agreed high findings. Repeat: it
	// would have, but this head was sent back once already.
	Bounced bool    `json:"bounced"`
	Repeat  bool    `json:"repeat,omitempty"`
	Seconds int     `json:"seconds"`
	CostUSD float64 `json:"adjudication_cost_usd,omitempty"`

	// stopped: loopd ended the lease (or the runner is stopping) during it; nothing is reported.
	stopped bool
}

// agreed reports whether Claude agreed with finding f.
func (so SecondOpinion) agreed(f Finding) bool { return so.Verdicts[f.ID].Verdict == "agree" }

// Counts are the numbers the board and digest read: findings and agreed ones, in total and by
// severity, and how many were judged.
func (so SecondOpinion) Counts() map[string]int {
	c := map[string]int{"findings": len(so.Findings), "agreed": 0, "judged": len(so.Verdicts)}
	for _, s := range Severities {
		c[s], c["agreed_"+s] = 0, 0
	}
	for _, f := range so.Findings {
		c[f.Severity]++
		if so.agreed(f) {
			c["agreed"]++
			c["agreed_"+f.Severity]++
		}
	}
	return c
}

// agreedHigh are the high findings Claude agreed with.
func (so SecondOpinion) agreedHigh() []Finding {
	var out []Finding
	for _, f := range so.Findings {
		if f.Severity == "high" && so.agreed(f) {
			out = append(out, f)
		}
	}
	return out
}

// EventData is the second_opinion event's data: counts and status, no finding text (the PR
// comment and the evidence file have it).
func (so SecondOpinion) EventData() map[string]any {
	d := map[string]any{"status": so.Status, "model": so.Model, "head_sha": so.HeadSHA, "blocking": so.Blocking,
		"bounced": so.Bounced, "seconds": so.Seconds}
	for k, v := range so.Counts() {
		d[k] = v
	}
	if so.Reason != "" {
		d["reason"] = so.Reason
	}
	if so.Repeat {
		d["repeat"] = true
	}
	if so.Usage != "" {
		d["gemini_usage"] = so.Usage
	}
	if so.CostUSD > 0 {
		d["adjudication_cost_usd"] = strconv.FormatFloat(so.CostUSD, 'f', 4, 64)
	}
	return d
}

func where(f Finding) string {
	if f.Line > 0 {
		return fmt.Sprintf("%s:%d", f.File, f.Line)
	}
	return f.File
}

// GeminiPrompt is agy's prompt: the ask, Claude's review and the diff inline (agy may skip the
// git-ignored .loop/), and what to look for.
func GeminiPrompt(it Item, review Result, diff string) string {
	if len(diff) > maxPromptDiff {
		cut := maxPromptDiff
		for cut > 0 && !utf8.RuneStart(diff[cut]) {
			cut--
		}
		diff = diff[:cut] + "\n\n[diff cut here: read the changed files in this checkout for the rest]\n"
	}
	var b strings.Builder
	fmt.Fprintf(&b, `You are an adversarial second reviewer of a pull request, from a different model family than the agents that built and reviewed it. Your job is to find what they missed, not to agree with them.

The checkout in your working directory is the PR's head (%s). Read any file you need; change nothing.

Look for:
- bugs: wrong logic, unhandled errors or edge cases, races, broken callers;
- missing or weak tests: behaviour the change adds or alters that no test would catch breaking;
- security issues: secrets, injection, unsafe paths or permissions, trust boundaries;
- gaps between the ask below and the code: what was asked and not done, or done differently.

Report only what you can point at: a file and line in the head, the claim, and the evidence (what the code does there, and why it is wrong). Severity: high for something that should block the merge, medium for something that should be fixed soon, low for the rest. Do not repeat points the first review already raised. If you find nothing, return an empty list.

Answer only with JSON matching the given schema: {"findings": [{"severity", "file", "line", "claim", "evidence"}]}.

## The ask (loop item #%d: %s)

%s

## The first review (Claude; it approved the PR)

%s

## The diff

`+"```diff\n%s\n```\n", it.HeadSHA, it.ID, oneLine(it.Title), strings.TrimSpace(it.Ask), strings.TrimSpace(review.Review), strings.TrimRight(diff, "\n"))
	return b.String()
}

// AdjudicationBrief is the Claude turn's brief: judge each of Gemini's findings, in the review's
// worktree, with the review's boundaries.
func AdjudicationBrief(it Item, slug string) string {
	return fmt.Sprintf(`# Adjudicate a second opinion

You are a review agent in Jarvis Loop, running unattended. A review of this PR approved it; then a
second reviewer from a different model family (Gemini) looked for what the review missed. Judge
each of its findings.

- Item: #%d: %s
- Stage: adjudicate
- Repository: %s, PR #%d, head `+"`%s`"+` (checked out in your working directory)

The findings are in `+"`.loop/context/gemini-findings.json`"+` (numbered by `+"`id`"+`); the diff is in
`+"`.loop/context/diff.patch`"+` and the item's ask in `+"`.loop/context/ask.md`"+`. Check each claim
against the code at this head (read it; run a test when that settles it). Change no file but
`+"`.loop/result.json`"+`.

- **agree**: the finding is real and worth a change: a bug, a missing or weak test, a security
  issue, or a gap between the ask and the code.
- **disagree**: it is wrong, already handled, out of scope for the ask, or not worth a change.

Give one sentence of reason for each, naming what you checked. Do not agree to be agreeable: a
wrong finding judged right sends good work back.

Write `+"`.loop/result.json`"+` as your last step, one verdict for every finding:

`+"```json\n"+`{"verdicts": [{"id": 1, "verdict": "agree", "reason": "…"}]}
`+"```\n", it.ID, oneLine(it.Title), slug, it.PR, it.HeadSHA)
}

// MaxVerdictReason caps one verdict's reason.
const MaxVerdictReason = 600

// ValidateAdjudication checks the adjudication turn's result: one verdict (agree or disagree,
// with a reason) for every finding.
func ValidateAdjudication(raw []byte, findings []Finding) (map[int]Verdict, error) {
	var res struct {
		Verdicts []Verdict `json:"verdicts"`
	}
	if err := json.Unmarshal(raw, &res); err != nil {
		return nil, fmt.Errorf("result.json is not JSON: %v", err)
	}
	ids := map[int]bool{}
	for _, f := range findings {
		ids[f.ID] = true
	}
	out := map[int]Verdict{}
	for _, v := range res.Verdicts {
		switch {
		case !ids[v.ID]:
			return nil, fmt.Errorf("verdict for finding %d, which does not exist", v.ID)
		case out[v.ID].Verdict != "":
			return nil, fmt.Errorf("two verdicts for finding %d", v.ID)
		case v.Verdict != "agree" && v.Verdict != "disagree":
			return nil, fmt.Errorf("finding %d: verdict must be agree or disagree, not %q", v.ID, v.Verdict)
		case !textOK(v.Reason, MaxVerdictReason):
			return nil, fmt.Errorf("finding %d: a reason is required (at most %d characters)", v.ID, MaxVerdictReason)
		}
		v.Reason = strings.TrimSpace(v.Reason)
		out[v.ID] = v
	}
	if len(out) != len(ids) {
		return nil, fmt.Errorf("%d verdicts for %d findings: judge every one", len(out), len(ids))
	}
	return out, nil
}

// SecondOpinionComment is the PR comment: every finding with Claude's verdict, and what it did.
func SecondOpinionComment(so SecondOpinion, it Item) string {
	var b strings.Builder
	c := so.Counts()
	b.WriteString("## Second opinion (Gemini)\n\n")
	fmt.Fprintf(&b, "Gemini (`%s`) looked for what the loop review of `%s` missed; Claude judged each finding.\n\n", so.Model, short7(so.HeadSHA))
	if len(so.Findings) == 0 {
		b.WriteString("**No findings.**\n")
	} else {
		fmt.Fprintf(&b, "**%d findings** (%d high, %d medium, %d low)", c["findings"], c["high"], c["medium"], c["low"])
		if so.Status == SOAnswered {
			fmt.Fprintf(&b, "; Claude agreed with %d (%d high).\n", c["agreed"], c["agreed_high"])
		} else {
			fmt.Fprintf(&b, "; not judged (%s).\n", oneLine(so.Reason))
		}
	}
	for _, f := range so.Findings {
		fmt.Fprintf(&b, "\n### %d. %s: `%s`\n\n%s\n", f.ID, f.Severity, where(f), strings.TrimSpace(f.Claim))
		if f.Evidence != "" {
			fmt.Fprintf(&b, "\nEvidence: %s\n", strings.TrimSpace(f.Evidence))
		}
		if v, ok := so.Verdicts[f.ID]; ok {
			verdict := "agrees"
			if v.Verdict == "disagree" {
				verdict = "disagrees"
			}
			fmt.Fprintf(&b, "\n**Claude %s:** %s\n", verdict, oneLine(v.Reason))
		}
	}
	if so.Dropped > 0 {
		fmt.Fprintf(&b, "\n(%d more findings were malformed or over the limit of %d and are not shown.)\n", so.Dropped, MaxFindings)
	}
	b.WriteString("\n---\n\n")
	switch {
	case so.Bounced:
		fmt.Fprintf(&b, "**Sent back to build:** Claude agreed with %d high finding(s), and blocking is on.\n", len(so.agreedHigh()))
	case so.Repeat:
		b.WriteString("Blocking is on, but this head was already sent back once for a second opinion: not sent back again.\n")
	case so.Blocking:
		b.WriteString("Blocking is on; no agreed high finding, so the review's approval stands.\n")
	default:
		b.WriteString("Advisory: recorded only; the review's approval stands.\n")
	}
	fmt.Fprintf(&b, "\nLoop item #%d; the findings and verdicts are on the item as `gemini-findings.json`.\n", it.ID)
	return b.String()
}

// SecondOpinionNotes are a blocking bounce's notes for the next build: the agreed high findings.
func SecondOpinionNotes(so SecondOpinion) string {
	var b strings.Builder
	b.WriteString("Second opinion (Gemini), agreed by Claude: fix these high findings.\n")
	for _, f := range so.agreedHigh() {
		fmt.Fprintf(&b, "\n- `%s`: %s Evidence: %s Claude: %s\n", where(f), oneLine(f.Claim), oneLine(f.Evidence), oneLine(so.Verdicts[f.ID].Reason))
	}
	return clip(b.String(), MaxNotes)
}

// secondOpinion runs the second opinion on an approved review: agy, then (with findings) the
// adjudication turn, then the comment, the evidence and the event. Nothing in it fails the item.
func (r *Runner) secondOpinion(ctx context.Context, ru *run, review Result) SecondOpinion {
	g := ru.cfg.Gemini
	it := ru.item
	log := r.Log.With("item", it.ID)
	so := SecondOpinion{Model: g.Model, HeadSHA: it.HeadSHA, Blocking: g.Blocking, Findings: []Finding{}}
	left := r.maxRun(ctx, ru) - 30*time.Second - r.Clock.Now().Sub(ru.started)
	limit := min(g.TimeoutDur(), left-adjudicationReserve)
	var res agyResult
	if limit < time.Minute {
		res = agyResult{Reason: AgyNoTime}
	} else {
		diff, _ := os.ReadFile(filepath.Join(ru.wt, ".loop", "context", "diff.patch"))
		res = r.runAgy(ctx, ru, GeminiPrompt(it, review, string(diff)), limit)
	}
	so.Seconds, so.Usage, so.Dropped = res.Seconds, res.Usage, res.Dropped
	switch {
	case res.Reason == AgyStopped:
		so.stopped = true
		return so
	case res.Reason != "":
		so.Status, so.Reason = SOUnavailable, res.Reason
		log.Warn("gemini: second opinion unavailable", "reason", res.Reason)
	case len(res.Findings) == 0:
		so.Status = SOAnswered
	default:
		so.Findings = res.Findings
		so.Status = SOAnswered
		if why := r.adjudicate(ctx, ru, &so); why != "" {
			if so.stopped {
				return so
			}
			so.Status, so.Reason = SOUnjudged, why
			log.Warn("gemini: findings not judged", "why", why)
		}
	}
	if so.Status == SOAnswered && g.Blocking && len(so.agreedHigh()) > 0 {
		if r.bouncedAt(ctx, ru) {
			so.Repeat = true
		} else {
			so.Bounced = true
		}
	}
	if so.Status != SOUnavailable {
		r.publishSecondOpinion(ctx, ru, so)
	}
	// The event is the record the once-per-head rule reads: no bounce without it.
	if err := r.Client.Event(ctx, it.ID, KindSecondOpinion, so.EventData()); err != nil {
		log.Warn("gemini: second_opinion event", "err", err)
		so.Bounced = false
	}
	return so
}

// bouncedAt reports whether a second opinion already sent this head back (or loopd could not
// say: then it does not bounce again either).
func (r *Runner) bouncedAt(ctx context.Context, ru *run) bool {
	d, err := r.Client.Detail(ctx, ru.item.ID)
	if err != nil {
		r.Log.Warn("gemini: reading the item's events", "item", ru.item.ID, "err", err)
		return true
	}
	for _, e := range d.Events {
		if e.Kind == KindSecondOpinion && e.Data["bounced"] == true && e.Data["head_sha"] == ru.item.HeadSHA {
			return true
		}
	}
	return false
}

// publishSecondOpinion posts the PR comment and uploads gemini-findings.json; a failure is
// logged, not reported (the second opinion is advice).
func (r *Runner) publishSecondOpinion(ctx context.Context, ru *run, so SecondOpinion) {
	it := ru.item
	body := filepath.Join(ru.dir, "gemini-comment.md")
	if err := os.WriteFile(body, []byte(SecondOpinionComment(so, it)), 0o600); err == nil {
		if err := r.PRs.Comment(ctx, ru.repo.Slug, it.PR, body); err != nil {
			r.Log.Warn("gemini: PR comment", "item", it.ID, "err", err)
		}
	}
	b, _ := json.MarshalIndent(so, "", "  ")
	p := filepath.Join(ru.dir, "gemini-findings.json")
	if err := os.WriteFile(p, append(b, '\n'), 0o600); err == nil {
		if err := r.Client.Upload(ctx, it.ID, p, "gemini-findings.json", "second opinion (Gemini): findings and Claude's verdicts"); err != nil {
			r.Log.Warn("gemini: evidence", "item", it.ID, "err", err)
		}
	}
}

// adjudicate runs the Claude turn that judges so's findings, in the review's worktree with the
// review's settings and guard. It sets so.Verdicts, or returns why there are none.
func (r *Runner) adjudicate(ctx context.Context, ru *run, so *SecondOpinion) string {
	ctxDir := filepath.Join(ru.wt, ".loop", "context")
	b, _ := json.MarshalIndent(map[string]any{"findings": so.Findings}, "", "  ")
	if err := os.WriteFile(filepath.Join(ctxDir, "gemini-findings.json"), b, 0o644); err != nil {
		return "the runner could not write the findings: " + err.Error()
	}
	if err := os.WriteFile(filepath.Join(ctxDir, "ask.md"), []byte(fmt.Sprintf("# #%d: %s\n\n%s\n", ru.item.ID, oneLine(ru.item.Title), strings.TrimSpace(ru.item.Ask))), 0o644); err != nil {
		return "the runner could not write the ask: " + err.Error()
	}
	// The review's own files stay: its transcript, stderr and brief.
	for _, f := range [][2]string{{"stream.jsonl", "stream-review.jsonl"}, {"stderr.log", "stderr-review.log"}, {"brief.md", "brief-review.md"}} {
		if err := os.Rename(filepath.Join(ru.dir, f[0]), filepath.Join(ru.dir, f[1])); err != nil && !errors.Is(err, fs.ErrNotExist) {
			r.Log.Warn("gemini: keeping the review's files", "file", f[0], "err", err)
		}
	}
	result := filepath.Join(ru.wt, ".loop", "result.json")
	os.Remove(result)
	if err := r.writeBrief(ru, AdjudicationBrief(ru.item, ru.repo.Slug)); err != nil {
		return "the runner could not write the brief: " + err.Error()
	}
	// The turn has what is left of the run's time cap, and at most adjudicationMax.
	ru.spent = r.Clock.Now().Sub(ru.started)
	ru.maxRun = ru.spent + 30*time.Second + adjudicationMax
	// A shutdown ends the turn as it ends an agent run.
	tctx, cancel := context.WithCancel(ctx)
	defer cancel()
	go func() {
		select {
		case <-ru.done:
			cancel()
		case <-tctx.Done():
		}
	}()
	out := r.runAgent(tctx, ru, "")
	if res := out.stats.result; res != nil {
		so.CostUSD = res.CostUSD
	}
	switch {
	case out.reason == endStopped || out.reason == endShutdown:
		so.stopped = true
		return "stopped"
	case out.stats.limit != nil:
		return "Claude's usage limit"
	case out.reason != endExited:
		return "the turn ended early (" + out.reason + ")"
	}
	raw, err := readResult(result)
	if err != nil {
		return fmt.Sprintf("no usable result (exit %d)", out.exitCode)
	}
	v, err := ValidateAdjudication(raw, so.Findings)
	if err != nil {
		return "the result was refused: " + clip(err.Error(), 200)
	}
	so.Verdicts = v
	return ""
}
