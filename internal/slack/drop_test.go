package slack

import (
	"fmt"
	"strings"
	"testing"

	"github.com/alexberardi/jarvis-loop/internal/pipeline"
)

func TestNeedsYouMessageHasADropButton(t *testing.T) {
	e := newBotEnv(t)
	id := e.add("device-kasa")
	e.calls()
	e.toNeedsYou(id, "no plug to test with?")
	cs := only(t, e.calls(), "chat.postMessage")
	msg := cs[len(cs)-1]
	if !strings.Contains(allText(msg), "needs you") {
		t.Fatalf("last post is not the needs_you message: %s", allText(msg))
	}
	if ids := actionIDs(msg); len(ids) != 1 || ids[0] != "drop" {
		t.Fatalf("buttons %v", ids)
	}
	if v := elementValue(msg, "drop"); v != fmt.Sprint(id) {
		t.Fatalf("drop value %q", v)
	}
}

func TestDropButton(t *testing.T) {
	for _, tc := range []struct {
		name    string
		user    string
		held    bool
		blockID string
		value   func(id int64) string
		want    pipeline.Stage
		event   string
	}{
		{"held, approver", Approver, true, "held", func(id int64) string { return fmt.Sprintf("%d:%s", id, shaA) }, pipeline.Closed, "dropped"},
		{"held, someone else", other, true, "held", func(id int64) string { return fmt.Sprintf("%d:%s", id, shaA) }, pipeline.Held, "refused"},
		{"needs_you, approver", Approver, false, "needs_you", func(id int64) string { return fmt.Sprint(id) }, pipeline.Closed, "dropped"},
		{"needs_you, someone else", other, false, "needs_you", func(id int64) string { return fmt.Sprint(id) }, pipeline.NeedsYou, "refused"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := newBotEnv(t)
			id := e.add("device-kasa")
			if tc.held {
				e.toHeld(id, shaA)
			} else {
				e.toNeedsYou(id, "which plug?")
			}
			e.calls()
			e.press(tc.user, ChannelLoop, "msg-ts", "drop", tc.blockID, tc.value(id))
			d := e.item(id)
			last := lastEvent(d)
			if d.Item.Stage != tc.want || last.Kind != tc.event || last.Actor != "slack:"+tc.user {
				t.Fatalf("stage %s, last event %+v", d.Item.Stage, last)
			}
			if tc.event == "refused" {
				return
			}
			if last.Data["words"] != "Drop button" {
				t.Fatalf("words %+v", last.Data)
			}
			e.drain()
			var lines []string
			for _, c := range only(t, e.calls(), "chat.postMessage") {
				lines = append(lines, allText(c))
			}
			if len(lines) != 1 || !strings.HasPrefix(lines[0], "Dropped by you") {
				t.Fatalf("thread lines %q", lines)
			}
		})
	}
}

// A held item dropped while a reply approval waits: the approval does not apply later.
func TestDropCancelsAPendingReplyApproval(t *testing.T) {
	e, id, ts := heldSetup(t)
	e.reply(Approver, ts, "ok")
	e.press(Approver, ChannelLoop, "held-ts", "drop", "held", fmt.Sprintf("%d:%s", id, shaA))
	e.b.mu.Lock()
	n := len(e.b.pending)
	e.b.mu.Unlock()
	if n != 0 || e.item(id).Item.Stage != pipeline.Closed {
		t.Fatalf("pending %d, stage %s", n, e.item(id).Item.Stage)
	}
}

func TestLoopDropCommand(t *testing.T) {
	for _, tc := range []struct {
		name, user, text string
		want             string
		stage            pipeline.Stage
	}{
		{"approver", Approver, "drop 1 no hardware to verify", "#1 dropped", pipeline.Closed},
		{"approver, hash", Approver, "drop #1", "#1 dropped", pipeline.Closed},
		{"someone else", other, "drop 1", "only the approver", pipeline.Triage},
		{"bad id", Approver, "drop one", "/loop drop", pipeline.Triage},
		{"no id", Approver, "drop", "/loop drop", pipeline.Triage},
		{"unknown item", Approver, "drop 99", "no item 99", pipeline.Triage},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := newBotEnv(t)
			e.add("device-lifx")
			out := e.command(tc.user, tc.text)
			if !strings.Contains(out, tc.want) {
				t.Fatalf("/loop %s: %q, want %q", tc.text, out, tc.want)
			}
			d := e.item(1)
			if d.Item.Stage != tc.stage {
				t.Fatalf("stage %s, want %s", d.Item.Stage, tc.stage)
			}
			if tc.user == other {
				if ev := lastEvent(d); ev.Kind != "refused" || ev.Actor != "slack:"+other {
					t.Fatalf("refusal not recorded on the item: %+v", ev)
				}
			}
			if tc.name == "approver" {
				if ev := lastEvent(d); ev.Data["words"] != "no hardware to verify" {
					t.Fatalf("words %+v", ev.Data)
				}
			}
		})
	}
	e := newBotEnv(t)
	id := e.add("x")
	e.command(Approver, fmt.Sprintf("drop %d", id))
	if out := e.command(Approver, fmt.Sprintf("drop %d", id)); !strings.Contains(out, "closed") {
		t.Fatalf("second drop %q", out)
	}
	if help := e.command(other, "help"); !strings.Contains(help, "/loop drop") {
		t.Fatalf("help lacks drop: %q", help)
	}
}

func TestDroppedLine(t *testing.T) {
	e := newBotEnv(t)
	id := e.add("x")
	e.toHeld(id, shaA)
	e.calls()
	if _, err := e.st.Drop(e.ctx, "slack:"+Approver, id, "no hardware"); err != nil {
		t.Fatal(err)
	}
	e.drain()
	cs := only(t, e.calls(), "chat.postMessage")
	want := "Dropped by you: 'no hardware'. Closed; the runner closes"
	if len(cs) != 1 || !strings.HasPrefix(allText(cs[0]), want) || !strings.Contains(allText(cs[0]), "pull/142") {
		t.Fatalf("dropped line: %q", allText(cs[0]))
	}
	if _, err := e.st.PRClosed(e.ctx, "devbox", id, map[string]any{"pr": float64(142), "state": "closed"}); err != nil {
		t.Fatal(err)
	}
	e.drain()
	if cs := only(t, e.calls(), "chat.postMessage"); len(cs) != 0 {
		t.Fatalf("pr_closed posted: %q", allText(cs[0]))
	}
}
