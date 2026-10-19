package slack

import (
	"context"
	"fmt"
	"strings"

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

// regressionReport posts a finished run's message.
func (b *Bot) regressionReport(ctx context.Context, e store.Event) {
	r, err := b.c.Store.GetRegression(ctx, idOf(e.Data["regression"]))
	if err != nil {
		b.c.Log.Error("slack: load regression", "regression", e.Data["regression"], "err", err.Error())
		return
	}
	m := b.regressionMessage(r)
	if line := regressionQALine(e); line != "" {
		m.Text += "\n" + line
		m.Blocks = append(m.Blocks, section(line))
	}
	b.post(ctx, "regression report", m)
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
		if t.Screenshot != 0 {
			if l := b.regressionLink(r.ID, fmt.Sprintf("/files/%d", t.Screenshot), "screenshot"); l != "" {
				line += "\n" + l
			} else {
				line += "\n(screenshot on the board)"
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
	return Message{Channel: ChannelLoop, Text: text, Blocks: blocks}
}
