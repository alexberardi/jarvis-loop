package slack

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/alexberardi/jarvis-loop/internal/pipeline"

	"github.com/alexberardi/jarvis-loop/internal/store"
)

// Regression runs' report (docs/decisions/2026-10-18-regression-trigger.md): one message in
// #loop when a run ends: the counts, each failure with its name, its assertion and a link to its
// screenshot, a 'File it' button per failure, and a link to the full report on the board. Slack
// gets links, not the screenshots themselves: the bot has no file-upload scope.

// MaxReportFailures is how many failures the message lists; the board's report has them all.
const MaxReportFailures = 15

func (b *Bot) regressionLink(id int64, path, text string) string {
	if b.c.LinkBase == "" {
		return ""
	}
	return fmt.Sprintf("<%s/board/regressions/%d%s|%s>", b.c.LinkBase, id, path, text)
}

// regressionReport posts a finished run's message. An overnight run's waits for the morning
// (its report_at): the tick posts it (morningReports).
func (b *Bot) regressionReport(ctx context.Context, e store.Event) {
	if _, ok := e.Data["report_at"]; ok {
		b.morningReports(ctx)
		return
	}
	r, err := b.c.Store.GetRegression(ctx, idOf(e.Data["regression"]))
	if err != nil {
		b.c.Log.Error("slack: load regression", "regression", e.Data["regression"], "err", err.Error())
		return
	}
	b.postRegression(ctx, r, e)
}

func (b *Bot) postRegression(ctx context.Context, r store.Regression, e store.Event) error {
	m := b.regressionMessage(r)
	if line := regressionQALine(e); line != "" {
		m.Text += "\n" + line
		m.Blocks = append(m.Blocks, section(line))
	}
	_, err := b.post(ctx, "regression report", m)
	return err
}

// morningReports posts the overnight runs' reports that are due (the morning, or at once for one
// that ended in the day), each once; one Slack refuses is tried again on the next tick.
func (b *Bot) morningReports(ctx context.Context) {
	due, err := b.c.Store.DueRegressionReports(ctx)
	if err != nil {
		b.c.Log.Error("slack: due regression reports", "err", err.Error())
		return
	}
	for _, d := range due {
		if b.postRegression(ctx, d.Run, store.Event{Kind: store.KindRegDone, Data: d.Done}) != nil {
			continue
		}
		if err := b.c.Store.MarkRegressionReported(ctx, d.Run.ID); err != nil {
			b.c.Log.Error("slack: record regression report", "regression", d.Run.ID, "err", err.Error())
		}
	}
}

// unstartedNote posts, once, why a scheduled overnight run has still not started at
// pipeline.UnstartedNoteAt (#317); one Slack refuses is tried again on the next tick.
func (b *Bot) unstartedNote(ctx context.Context) {
	u, err := b.c.Store.UnstartedOvernight(ctx)
	if err != nil {
		b.c.Log.Error("slack: unstarted overnight run", "err", err.Error())
		return
	}
	if u == nil {
		return
	}
	text := fmt.Sprintf("*Overnight regression R%d has not started* (set for %s): %s.", u.Run.ID, b.clockText(*u.Run.NotBefore), u.Why)
	if _, err := b.post(ctx, "unstarted regression", Message{Channel: ChannelLoop, Text: text, Blocks: []map[string]any{section(text)}}); err != nil {
		return
	}
	if err := b.c.Store.MarkUnstartedNoted(ctx, u.Run.ID); err != nil {
		b.c.Log.Error("slack: record unstarted note", "regression", u.Run.ID, "err", err.Error())
	}
}

// clockText is t on the bot's clock, as the messages say it ("01:00 on Tue Oct 20").
func (b *Bot) clockText(t time.Time) string {
	return t.In(b.c.Location).Format("15:04 on Mon Jan 2")
}

// laneLine is an overnight run's totals per lane, in pipeline.Lanes' order (the lanes it ran).
func laneLine(tests []store.RegressionTest) string {
	type tally struct{ passed, failed, flaky, skipped int }
	by := map[string]*tally{}
	var order []string
	for _, l := range pipeline.Lanes {
		by[l] = &tally{}
	}
	for _, t := range tests {
		c, ok := by[t.Suite]
		if !ok {
			c = &tally{}
			by[t.Suite] = c
			order = append(order, t.Suite)
		}
		switch {
		case t.Status == "passed":
			c.passed++
		case t.Status == "skipped":
			c.skipped++
		case t.Status == store.TestFlaky:
			c.flaky++
		default:
			c.failed++
		}
	}
	var parts []string
	for _, l := range append(slices.Clone(pipeline.Lanes), order...) {
		c := by[l]
		if *c == (tally{}) {
			continue
		}
		p := fmt.Sprintf("%s %d passed", l, c.passed)
		for _, x := range []struct {
			n    int
			word string
		}{{c.failed, "failed"}, {c.flaky, "flaky"}, {c.skipped, "skipped"}} {
			if x.n > 0 {
				p += fmt.Sprintf(", %d %s", x.n, x.word)
			}
		}
		parts = append(parts, p)
	}
	return strings.Join(parts, " · ")
}

// duration is a run's length as the report says it: "2h 14m", "38m".
func duration(d time.Duration) string {
	d = d.Round(time.Minute)
	if d >= time.Hour {
		return fmt.Sprintf("%dh %02dm", int(d/time.Hour), int(d%time.Hour/time.Minute))
	}
	return fmt.Sprintf("%dm", int(d/time.Minute))
}

// regressionQALine is what the run decided in the QA column (#195): the items in qa whose e2e
// checks it passed (verified) or failed (reopened); each item's thread has its own line.
func regressionQALine(e store.Event) string {
	var parts []string
	for _, k := range []struct{ key, verb string }{{"verified", "verified"}, {"reopened", "reopened"}} {
		var refs []string
		for _, id := range idsOf(e.Data[k.key]) {
			refs = append(refs, fmt.Sprintf("#%d", id))
		}
		if len(refs) > 0 {
			parts = append(parts, k.verb+" "+strings.Join(refs, ", "))
		}
	}
	if len(parts) == 0 {
		return ""
	}
	return "QA from this run's E2E checks: " + strings.Join(parts, "; ") + "."
}

// regressionMessage is a finished run's report, from the store (an update after 'File it' builds
// it again, with the filed item in place of the button).
func (b *Bot) regressionMessage(r store.Regression) Message {
	verdict := map[string]string{store.RegPassed: ":large_green_circle: passed", store.RegFailed: ":red_circle: failed",
		store.RegError: ":warning: could not run"}[r.Status]
	head := fmt.Sprintf("*Regression R%d (%s) %s*", r.ID, r.Suite, verdict)
	if r.Status != store.RegError {
		head += fmt.Sprintf(": %d passed, %d failed, %d skipped", r.Passed, r.Failed, r.Skipped)
		if r.Overnight() {
			head = fmt.Sprintf("*Overnight regression R%d %s*: %d passed, %d failed, %d flaky, %d skipped", r.ID, verdict, r.Passed, r.Failed, r.Flaky, r.Skipped)
		}
	} else if r.Overnight() {
		head = fmt.Sprintf("*Overnight regression R%d %s*", r.ID, verdict)
	}
	if d := r.Duration(); r.Overnight() && d > 0 {
		head += " in " + duration(d)
	}
	if r.ServerHead != "" {
		head += fmt.Sprintf(" · jarvis-server `%s`", short(r.ServerHead))
	}
	if r.MobileHead != "" {
		head += fmt.Sprintf(" · jarvis-node-mobile `%s`", short(r.MobileHead))
	}
	if l := b.regressionLink(r.ID, "", "full report"); l != "" {
		head += " · " + l
	} else {
		head += fmt.Sprintf(" · full report: /board/regressions/%d on loopd", r.ID)
	}
	if r.Overnight() {
		if lanes := laneLine(r.Tests); lanes != "" {
			head += "\n" + lanes
		}
	}
	if r.Error != "" {
		head += "\n" + Escape(Clip(r.Error, 1000, "…"))
	}
	blocks := []map[string]any{section(head)}
	text := head
	shown := 0
	for _, t := range r.Tests {
		if !t.Failed() {
			continue
		}
		if shown == MaxReportFailures {
			more := fmt.Sprintf("…and %d more failures in the full report.", r.Failed-shown)
			blocks = append(blocks, section(more))
			break
		}
		shown++
		line := fmt.Sprintf("*%s: %s*", Escape(t.Suite), Escape(Clip(t.Title, 300, "…")))
		if t.Status != "failed" {
			line += " (" + Escape(t.Status) + ")"
		}
		if t.Error != "" {
			line += "\n```" + Escape(Clip(strings.TrimSpace(t.Error), 600, "…")) + "```"
		}
		for _, f := range []struct {
			n    int
			what string
		}{{t.Screenshot, "screenshot"}, {t.Recording, "recording"}} {
			if f.n == 0 {
				continue
			}
			if l := b.regressionLink(r.ID, fmt.Sprintf("/files/%d", f.n), f.what); l != "" {
				line += "\n" + l
			} else {
				line += "\n(" + f.what + " on the board)"
			}
		}
		blk := section(line)
		if t.ItemID != 0 {
			blk["text"] = mrkdwn(Clip(line+fmt.Sprintf("\nFiled as #%d.", t.ItemID), MaxChars, ""))
		} else {
			blk["accessory"] = button("File it", "regression_file", fmt.Sprintf("R%d:%d", r.ID, t.N), "")
		}
		blocks = append(blocks, blk)
	}
	if fl := flakyLine(r.Tests); fl != "" {
		blocks = append(blocks, section(fl))
	}
	return Message{Channel: ChannelLoop, Text: text, Blocks: blocks}
}

// flakyLine lists the flaky tests (failed, then passed on the rerun) apart from the failures, at
// most MaxReportFailures; "" when there are none.
func flakyLine(tests []store.RegressionTest) string {
	var names []string
	n := 0
	for _, t := range tests {
		if t.Status != store.TestFlaky {
			continue
		}
		n++
		if len(names) < MaxReportFailures {
			names = append(names, Escape(t.Suite+": "+Clip(t.Title, 200, "…")))
		}
	}
	if n == 0 {
		return ""
	}
	line := "*Flaky* (failed, then passed on the rerun): " + strings.Join(names, "; ")
	if n > len(names) {
		line += fmt.Sprintf("; and %d more in the full report", n-len(names))
	}
	return line + "."
}
