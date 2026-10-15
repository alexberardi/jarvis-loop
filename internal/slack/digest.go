package slack

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/alexberardi/jarvis-loop/internal/pipeline"
	"github.com/alexberardi/jarvis-loop/internal/store"
)

// digestBoundary is the latest digest time at or before now: midnight and every `hours` after
// it, on the local clock of loc (00/04/08/12/16/20 for 4 hours).
func digestBoundary(now time.Time, loc *time.Location, hours int) time.Time {
	t := now.In(loc)
	return time.Date(t.Year(), t.Month(), t.Day(), t.Hour()-t.Hour()%hours, 0, 0, 0, loc)
}

// digest posts the digest for the window since the last one, once per boundary. The first run
// only starts the clock; an empty window moves it on without a message.
func (b *Bot) digest(ctx context.Context) {
	now := b.c.Clock.Now()
	end := digestBoundary(now, b.c.Location, b.c.DigestHours)
	st, err := b.c.Store.SlackState(ctx)
	if err != nil {
		return
	}
	if st.LastDigest.IsZero() {
		b.setLastDigest(ctx, end)
		return
	}
	if !end.After(st.LastDigest) {
		return
	}
	evs, err := b.c.Store.EventsBetween(ctx, st.LastDigest, end)
	if err != nil {
		b.c.Log.Error("slack: digest events", "err", err.Error())
		return
	}
	m, ok := b.digestMessage(ctx, st.LastDigest, end, evs)
	if !ok {
		b.setLastDigest(ctx, end)
		return
	}
	if _, err := b.post(ctx, "digest", m); err != nil {
		b.mu.Lock()
		if !b.digestTS.Equal(end) {
			b.digestTS, b.digestNG = end, 0
		}
		b.digestNG++
		give := b.digestNG >= digestTries
		b.mu.Unlock()
		if give {
			b.c.Log.Error("slack: digest not posted; skipping it", "window_end", end.Format(time.RFC3339))
			b.setLastDigest(ctx, end)
		}
		return
	}
	b.setLastDigest(ctx, end)
}

func (b *Bot) setLastDigest(ctx context.Context, t time.Time) {
	if err := b.c.Store.SetLastDigest(ctx, t); err != nil {
		b.c.Log.Error("slack: record digest time", "err", err.Error())
	}
}

func (b *Bot) digestMessage(ctx context.Context, from, to time.Time, evs []store.Event) (Message, bool) {
	type group struct {
		name string
		ids  []int64
	}
	secs := []*group{{name: "Merged"}, {name: "Main CI not green"}, {name: "Held"}, {name: "Needs you"}, {name: "Failing"}, {name: "New"}}
	notes := map[string]string{} // "group/id" -> a suffix for that line
	seen := map[string]bool{}
	addTo := func(s *group, id int64) {
		k := fmt.Sprintf("%s/%d", s.name, id)
		if id == 0 || seen[k] {
			return
		}
		seen[k] = true
		s.ids = append(s.ids, id)
	}
	for _, e := range evs {
		out := pipeline.Outcome(e.Outcome)
		if e.Kind == "advanced" && out == pipeline.Merged {
			addTo(secs[0], e.ItemID)
		}
		if e.Kind == "main_ci" && str(e.Data, "result") != "green" {
			addTo(secs[1], e.ItemID)
			notes[fmt.Sprintf("%s/%d", secs[1].name, e.ItemID)] = ": " + Escape(str(e.Data, "result"))
		}
		if e.To == string(pipeline.Held) && e.From == string(pipeline.Land) {
			addTo(secs[2], e.ItemID)
		}
		if e.To == string(pipeline.NeedsYou) && e.From != string(pipeline.NeedsYou) {
			addTo(secs[3], e.ItemID)
		}
		if e.Kind == "usage_limit" || e.Kind == "advanced" && out == pipeline.Failed {
			addTo(secs[4], e.ItemID)
		}
		if e.Kind == "created" {
			addTo(secs[5], e.ItemID)
		}
	}
	found := false
	for _, s := range secs {
		found = found || len(s.ids) > 0
	}
	if !found {
		return Message{}, false
	}
	// The items waiting in qa now, whatever happened in the window (they alone post no digest).
	if rows, err := b.c.Store.ListItems(ctx, string(pipeline.QA), ""); err == nil {
		qa := &group{name: "Waiting for your check"}
		for _, r := range rows {
			addTo(qa, r.ID)
		}
		secs = append(secs, qa)
	}
	f, t := from.In(b.c.Location), to.In(b.c.Location)
	window := fmt.Sprintf("%s %s–%s", t.Format("Mon Jan 2"), f.Format("15:04"), t.Format("15:04"))
	if f.YearDay() != t.YearDay() || f.Year() != t.Year() {
		window = fmt.Sprintf("%s – %s", f.Format("Mon Jan 2 15:04"), t.Format("Mon Jan 2 15:04"))
	}
	var sb strings.Builder
	fmt.Fprintf(&sb, "*Loop digest* · %s (New York)\n", window)
	titles := map[int64]string{}
	for _, s := range secs {
		if len(s.ids) == 0 {
			continue
		}
		fmt.Fprintf(&sb, "\n*%s (%d)*\n", s.name, len(s.ids))
		for _, id := range s.ids {
			title, ok := titles[id]
			if !ok {
				if d, err := b.c.Store.GetItem(ctx, id); err == nil {
					title = d.Item.Title
				}
				titles[id] = title
			}
			fmt.Fprintf(&sb, "• #%d %s%s\n", id, b.field(title, 120, id), notes[fmt.Sprintf("%s/%d", s.name, id)])
		}
	}
	if ctl, err := b.c.Store.Control(ctx); err == nil {
		fmt.Fprintf(&sb, "\nActive agents: %d of 6", ctl.ActiveAgents)
		if ctl.Stopped {
			sb.WriteString(" (stopped)")
		}
	}
	if e, ok, _ := b.c.Store.LastEvent(ctx, "usage"); ok {
		fmt.Fprintf(&sb, "\nLast /loop usage: %v%% (%s)", e.Data["percent"], e.At.In(b.c.Location).Format("Mon Jan 2 15:04"))
	}
	text := Clip(sb.String(), MaxChars, "loop status")
	return Message{Channel: ChannelDigest, Text: text, Blocks: []map[string]any{section(text)}}, true
}
