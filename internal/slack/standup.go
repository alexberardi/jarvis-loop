package slack

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/alexberardi/jarvis-loop/internal/pipeline"
	"github.com/alexberardi/jarvis-loop/internal/store"
)

// The daily standup (docs/phases/08-orchestrator.md, piece 6; docs/decisions/2026-10-18-standup.md):
// at 09:00 in #loop-digest, from the @orchestrator app when loopd runs it, composed by loopd from
// the store (no model call): merged in the 24 hours to 09:00, in flight, waiting and on what, what
// the orchestrator did, what needs the user, and the orchestrator's own spend. It replaces the
// digest's item list.

// StandupHour is the standup's hour on the local clock.
const StandupHour = 9

// standupSectionMax caps the lines of a section; the rest is counted.
const standupSectionMax = 15

// standupBoundary is the latest 09:00 at or before now, on the local clock of loc.
func standupBoundary(now time.Time, loc *time.Location) time.Time {
	t := now.In(loc)
	b := time.Date(t.Year(), t.Month(), t.Day(), StandupHour, 0, 0, 0, loc)
	if t.Before(b) {
		b = time.Date(t.Year(), t.Month(), t.Day()-1, StandupHour, 0, 0, 0, loc)
	}
	return b
}

// standupLine is an item in a section: its title and what the line says about it.
type standupLine struct {
	ID    int64
	Title string
	Note  string
}

// standupData is what a standup says, gathered from the store.
type standupData struct {
	// At is the 09:00 it is for; the window is the 24 hours before it.
	At                                time.Time
	Merged, InFlight, Waiting, NeedsU []standupLine
	// Did is what the orchestrator did in the window: a count per action, and its lines.
	Did      map[string]int
	DidLines []string
	// The orchestrator's spend in the window: its turns, today's cap, claude's cost and tokens.
	Turns, Cap   int
	Cost         float64
	TokIn, TokOu int64
}

// standupText is the standup's message text.
func standupText(d standupData, loc *time.Location) string {
	var sb strings.Builder
	at := d.At.In(loc)
	fmt.Fprintf(&sb, "*Standup* · %s (New York) · since %s 09:00\n", at.Format("Mon Jan 2"), at.AddDate(0, 0, -1).Format("Mon"))
	section := func(name string, lines []standupLine) {
		if len(lines) == 0 {
			return
		}
		fmt.Fprintf(&sb, "\n*%s (%d)*\n", name, len(lines))
		for i, l := range lines {
			if i == standupSectionMax {
				fmt.Fprintf(&sb, "• … and %d more\n", len(lines)-i)
				break
			}
			sb.WriteString("• ")
			if l.ID != 0 {
				fmt.Fprintf(&sb, "#%d ", l.ID)
			}
			sb.WriteString(Escape(Clip(strings.TrimSpace(l.Title), 100, "")))
			if l.Note != "" {
				sb.WriteString(" — " + Escape(Clip(oneLine(l.Note), 160, "")))
			}
			sb.WriteString("\n")
		}
	}
	section("Merged yesterday", d.Merged)
	section("In flight", d.InFlight)
	section("Waiting", d.Waiting)
	section("Needs you", d.NeedsU)
	if len(d.Merged)+len(d.InFlight)+len(d.Waiting)+len(d.NeedsU) == 0 {
		sb.WriteString("\nNothing open, nothing merged.\n")
	}
	sb.WriteString("\n*What I did*\n")
	if len(d.Did) == 0 {
		sb.WriteString("Nothing.\n")
	} else {
		kinds := make([]string, 0, len(d.Did))
		for k := range d.Did {
			kinds = append(kinds, k)
		}
		sort.Strings(kinds)
		parts := make([]string, len(kinds))
		for i, k := range kinds {
			parts[i] = fmt.Sprintf("%s %d", Escape(k), d.Did[k])
		}
		sb.WriteString(strings.Join(parts, " · ") + "\n")
		for i, l := range d.DidLines {
			if i == standupSectionMax {
				fmt.Fprintf(&sb, "• … and %d more\n", len(d.DidLines)-i)
				break
			}
			sb.WriteString("• " + Escape(Clip(oneLine(l), 200, "")) + "\n")
		}
	}
	sb.WriteString("\n*My spend*\n")
	fmt.Fprintf(&sb, "%d turn%s (cap %d a day)", d.Turns, plural(d.Turns), d.Cap)
	if d.Cost > 0 || d.TokIn > 0 {
		fmt.Fprintf(&sb, " · $%.2f · %s tokens in, %s out", d.Cost, tokens(d.TokIn), tokens(d.TokOu))
	}
	sb.WriteString("\n")
	return Clip(sb.String(), MaxChars, "loop status")
}

func plural(n int) string {
	if n == 1 {
		return ""
	}
	return "s"
}

func oneLine(s string) string { return strings.Join(strings.Fields(s), " ") }

// tokens is a token count, short: 950, 12k, 1.2M.
func tokens(n int64) string {
	switch {
	case n >= 1_000_000:
		return fmt.Sprintf("%.1fM", float64(n)/1e6)
	case n >= 1000:
		return fmt.Sprintf("%dk", (n+500)/1000)
	}
	return fmt.Sprint(n)
}

// ageText is how long something has waited, short: 40m, 5h, 3d.
func ageText(d time.Duration) string {
	switch {
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d/time.Minute))
	case d < 48*time.Hour:
		return fmt.Sprintf("%dh", int(d/time.Hour))
	}
	return fmt.Sprintf("%dd", int(d/(24*time.Hour)))
}

// standupData gathers the standup for the 09:00 at from the store: the window's events and the
// board as it is now.
func (b *Bot) standupData(ctx context.Context, at time.Time) (standupData, error) {
	d := standupData{At: at, Did: map[string]int{}}
	from := at.Add(-24 * time.Hour)
	evs, err := b.c.Store.EventsBetween(ctx, from, at)
	if err != nil {
		return d, err
	}
	items, err := b.c.Store.BoardItems(ctx)
	if err != nil {
		return d, err
	}
	byID := map[int64]store.BoardItem{}
	for _, it := range items {
		byID[it.ID] = it
	}
	merged := map[int64]bool{}
	for _, e := range evs {
		switch {
		case e.Kind == "advanced" && pipeline.Outcome(e.Outcome) == pipeline.Merged && !merged[e.ItemID]:
			merged[e.ItemID] = true
			d.Merged = append(d.Merged, standupLine{ID: e.ItemID, Title: byID[e.ItemID].Title})
		case e.Kind == store.KindOrchDone || e.Kind == store.KindOrchFailed:
			d.Turns++
			d.Cost += num(e.Data["total_cost_usd"])
			d.TokIn += int64(num(e.Data["usage_input"]))
			d.TokOu += int64(num(e.Data["usage_output"]))
		case strings.HasPrefix(e.Actor, orchestratorPrefix) && e.Kind != "created":
			action := strings.TrimPrefix(e.Kind, "orchestrator.")
			if e.Kind == "refused" {
				action = "refused " + str(e.Data, "action")
			}
			d.Did[action]++
			if e.Kind == store.KindOrchNote || e.Kind == store.KindOrchReply {
				continue // its notebook and its replies are counted, not listed
			}
			line := action
			if e.ItemID != 0 {
				line += fmt.Sprintf(" #%d", e.ItemID)
			}
			why := str(e.Data, "reason")
			if e.Kind == "refused" {
				why = str(e.Data, "message")
			}
			if why != "" {
				line += ": " + why
			}
			d.DidLines = append(d.DidLines, line)
		}
	}
	board, err := b.c.Store.OrchestratorBoard(ctx, 50)
	if err != nil {
		return d, err
	}
	recommends := map[int64]string{}
	for _, e := range board.Escalations {
		if r := str(e.Data, "recommendation"); r != "" && e.ItemID != 0 && recommends[e.ItemID] == "" {
			recommends[e.ItemID] = r
		}
	}
	now := b.c.Clock.Now()
	for _, it := range items {
		st := pipeline.Stage(it.Stage)
		l := standupLine{ID: it.ID, Title: it.Title}
		switch {
		case st == pipeline.NeedsYou:
			l.Note = "needs you: " + it.Question
			if r := recommends[it.ID]; r != "" {
				l.Note += " · I recommend: " + r
			}
			d.NeedsU = append(d.NeedsU, l)
		case st == pipeline.Held:
			l.Note = "held for your approval"
			if it.HoldReason != "" {
				l.Note += ": " + it.HoldReason
			}
			d.NeedsU = append(d.NeedsU, l)
		case st == pipeline.QA:
			l.Note = "merged, waiting for your check"
			d.NeedsU = append(d.NeedsU, l)
		case st == pipeline.Done || st == pipeline.Closed:
		case len(it.Waits) > 0:
			waits := make([]string, len(it.Waits))
			for i, c := range it.Waits {
				waits[i] = c.String()
			}
			l.Note = "waiting on " + strings.Join(waits, ", ")
			d.Waiting = append(d.Waiting, l)
		default:
			l.Note = it.Stage
			if it.ClaimedBy != "" {
				l.Note += ", working"
			}
			l.Note += " · " + ageText(now.Sub(it.UpdatedAt)) + " in stage"
			d.InFlight = append(d.InFlight, l)
		}
	}
	// Escalations about no item: the user's too.
	for _, e := range board.Escalations {
		if e.ItemID == 0 {
			l := standupLine{Title: str(e.Data, "text")}
			if r := str(e.Data, "recommendation"); r != "" {
				l.Note = "I recommend: " + r
			}
			d.NeedsU = append(d.NeedsU, l)
		}
	}
	if ctl, err := b.c.Store.Control(ctx); err == nil {
		d.Cap = ctl.OrchTurnsPerDay
	}
	return d, nil
}

func num(v any) float64 {
	f, _ := v.(float64)
	return f
}

// standup posts the standup once per 09:00, from the @orchestrator app when loopd runs it (the
// loop bot otherwise). The first run only starts the clock; a standup Slack refuses is tried on
// the next ticks, then skipped (digestTries).
func (b *Bot) standup(ctx context.Context) {
	at := standupBoundary(b.c.Clock.Now(), b.c.Location)
	last, err := b.c.Store.LastStandup(ctx)
	if err != nil {
		return
	}
	if last.IsZero() {
		b.recordStandup(ctx, at, false)
		return
	}
	if !at.After(last) {
		return
	}
	d, err := b.standupData(ctx, at)
	if err != nil {
		b.c.Log.Error("slack: standup", "err", err.Error())
		return
	}
	text := standupText(d, b.c.Location)
	poster := b
	if b.c.Orchestrator != nil {
		poster = b.c.Orchestrator.bot
	}
	if _, err := poster.post(ctx, "standup", Message{Channel: ChannelDigest, Text: text, Blocks: []map[string]any{section(text)}}); err != nil {
		b.mu.Lock()
		if !b.standupTS.Equal(at) {
			b.standupTS, b.standupNG = at, 0
		}
		b.standupNG++
		give := b.standupNG >= digestTries
		b.mu.Unlock()
		if give {
			b.c.Log.Error("slack: standup not posted; skipping it", "for", at.Format(time.RFC3339))
			b.recordStandup(ctx, at, false)
		}
		return
	}
	b.recordStandup(ctx, at, true)
}

func (b *Bot) recordStandup(ctx context.Context, at time.Time, posted bool) {
	if err := b.c.Store.RecordStandup(ctx, at, posted); err != nil {
		b.c.Log.Error("slack: record standup", "err", err.Error())
	}
}
