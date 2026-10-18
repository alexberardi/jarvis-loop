package slack

import (
	"fmt"
	"strings"
	"testing"

	"github.com/alexberardi/jarvis-loop/internal/pipeline"
)

// In a needs_you thread, the approver's reply that opens with rebuild, rebase or fix sends the
// item back to build with the reply as the builder's note; any other reply is still the answer,
// and someone else's reply is refused either way.
func TestNeedsYouReplyRebuilds(t *testing.T) {
	for _, c := range []struct {
		user, reply string
		stage       pipeline.Stage
		kind        string
	}{
		{Approver, "rebuild, rebase on main", pipeline.Build, "rebuild"},
		{Approver, "please rebase and fix the lint", pipeline.Build, "rebuild"},
		{Approver, "re-run it, I think it's flaky", pipeline.Build, "answered"},
		{Approver, "yes, use the staging DB", pipeline.Build, "answered"},
		{Approver, "fix main first", pipeline.Build, "answered"},
		{other, "rebuild, rebase on main", pipeline.NeedsYou, "refused"},
	} {
		t.Run(c.reply+" by "+c.user, func(t *testing.T) {
			e := newBotEnv(t)
			id := e.add("device-kasa")
			e.calls()
			ts := e.thread(id)
			e.toBuildQuestion(id, "which plug do I test with?")
			e.calls()
			e.reply(c.user, ts, c.reply)
			d := e.item(id)
			ev := lastEvent(d)
			if d.Item.Stage != c.stage || ev.Kind != c.kind || ev.Actor != "slack:"+c.user {
				t.Fatalf("stage %s, last %+v", d.Item.Stage, ev)
			}
			if c.kind != "rebuild" {
				return
			}
			if ev.Data["notes"] != c.reply || d.Item.Question != "" {
				t.Fatalf("notes %+v, question %q", ev.Data, d.Item.Question)
			}
			cs := only(t, e.calls(), "chat.postMessage")
			if len(cs) != 1 || cs[0].Body["text"] != fmt.Sprintf("Sent back to build by you: '%s'.", c.reply) || cs[0].Body["thread_ts"] != ts {
				t.Fatalf("posts %+v", cs)
			}
		})
	}
}

// Triage's question has no build to go back to: a rebuild reply there is the answer.
func TestTriageQuestionReplyIsAnAnswer(t *testing.T) {
	e := newBotEnv(t)
	id := e.add("device-kasa")
	e.calls()
	ts := e.thread(id)
	e.toNeedsYou(id, "which repo?")
	e.reply(Approver, ts, "rebuild it in jarvis-node")
	if d := e.item(id); d.Item.Stage != pipeline.Triage || lastEvent(d).Kind != "answered" {
		t.Fatalf("stage %s, last %+v", d.Item.Stage, lastEvent(d))
	}
}

func TestBuildQuestionHasARebuildButton(t *testing.T) {
	e := newBotEnv(t)
	id := e.add("device-kasa")
	e.calls()
	e.toBuildQuestion(id, "which plug?")
	cs := only(t, e.calls(), "chat.postMessage")
	msg := cs[len(cs)-1]
	if ids := actionIDs(msg); strings.Join(ids, ",") != "rebuild,drop" {
		t.Fatalf("buttons %v", ids)
	}
	if v := elementValue(msg, "rebuild"); v != fmt.Sprint(id) {
		t.Fatalf("rebuild value %q", v)
	}
}

func TestHeldMessageHasARebuildButton(t *testing.T) {
	e := newBotEnv(t)
	id := e.add("device-kasa")
	e.calls()
	e.toHeld(id, shaA)
	cs := only(t, e.calls(), "chat.postMessage")
	msg := cs[len(cs)-1]
	if ids := actionIDs(msg); strings.Join(ids, ",") != "approve,keep_holding,rebuild,drop" {
		t.Fatalf("buttons %v", ids)
	}
	if v := elementValue(msg, "rebuild"); v != fmt.Sprintf("%d:%s", id, shaA) {
		t.Fatalf("rebuild value %q", v)
	}
}

// The Rebuild button opens a modal for the builder's note (the approver's only); submitting it
// sends the item back to build with the note, or the default when it is left empty.
func TestRebuildButtonAndModal(t *testing.T) {
	for _, c := range []struct {
		name  string
		held  bool
		note  string
		notes string
	}{
		{"needs_you, a note", false, "rebase and fix the lint", "rebase and fix the lint"},
		{"needs_you, the default", false, "", RebuildDefaultNote},
		{"held", true, "rebase on main", "rebase on main"},
	} {
		t.Run(c.name, func(t *testing.T) {
			e := newBotEnv(t)
			id := e.add("device-kasa")
			block, value := "needs_you", fmt.Sprint(id)
			if c.held {
				e.toHeld(id, shaA)
				block, value = "held", fmt.Sprintf("%d:%s", id, shaA)
			} else {
				e.toBuildQuestion(id, "which plug?")
			}
			e.calls()

			e.press(other, ChannelLoop, "msg-ts", "rebuild", block, value)
			cs := e.calls()
			if len(only(t, cs, "views.open")) != 0 || len(only(t, cs, "chat.postEphemeral")) != 1 || lastEvent(e.item(id)).Kind != "refused" {
				t.Fatalf("someone else's press: %+v", cs)
			}

			e.press(Approver, ChannelLoop, "msg-ts", "rebuild", block, value)
			opens := only(t, e.calls(), "views.open")
			if len(opens) != 1 || opens[0].Body["trigger_id"] != "trig-"+Approver {
				t.Fatalf("views.open %+v", opens)
			}
			view, _ := opens[0].Body["view"].(map[string]any)
			if view["callback_id"] != rebuildCallback || view["private_metadata"] != fmt.Sprint(id) ||
				!strings.Contains(fmt.Sprint(view["blocks"]), "initial_value:"+RebuildDefaultNote) {
				t.Fatalf("view %+v", view)
			}
			if d := e.item(id); d.Item.Stage == pipeline.Build {
				t.Fatal("the press alone sent it back")
			}

			e.submit(other, fmt.Sprint(id), c.note)
			if d := e.item(id); d.Item.Stage == pipeline.Build || lastEvent(d).Kind != "refused" {
				t.Fatalf("someone else's submission: %s %+v", d.Item.Stage, lastEvent(d))
			}
			e.calls()
			e.submit(Approver, fmt.Sprint(id), c.note)
			d := e.item(id)
			ev := lastEvent(d)
			if d.Item.Stage != pipeline.Build || ev.Kind != "rebuild" || ev.Actor != "slack:"+Approver || ev.Data["notes"] != c.notes {
				t.Fatalf("stage %s, last %+v", d.Item.Stage, ev)
			}
			cs = only(t, e.calls(), "chat.postMessage")
			if len(cs) != 1 || cs[0].Body["text"] != fmt.Sprintf("Sent back to build by you: '%s'.", c.notes) {
				t.Fatalf("posts %+v", cs)
			}

			// A second submission (the item moved on) says why in the thread.
			e.submit(Approver, fmt.Sprint(id), c.note)
			cs = only(t, e.calls(), "chat.postMessage")
			if len(cs) != 1 || !strings.HasPrefix(fmt.Sprint(cs[0].Body["text"]), "Refused rebuild from you: item is build") {
				t.Fatalf("posts %+v", cs)
			}
		})
	}
}

// submit is a view_submission of the Rebuild modal by user, for the item meta, with note typed.
func (e *botEnv) submit(user, meta, note string) {
	e.b.HandleEnvelope(e.ctx, envelope("interactive", map[string]any{
		"type": "view_submission", "user": map[string]any{"id": user},
		"view": map[string]any{"callback_id": rebuildCallback, "private_metadata": meta, "state": map[string]any{"values": map[string]any{
			rebuildBlock: map[string]any{rebuildBlock: map[string]any{"type": "plain_text_input", "value": note}}}}}}))
}

// toBuildQuestion takes the item through triage and has its build ask a question.
func (e *botEnv) toBuildQuestion(id int64, question string) {
	e.t.Helper()
	e.runner(id, "a1", pipeline.Reproduced, map[string]string{"area": "auth", "size": "S", "risk": "normal", "resources": ""})
	e.runner(id, "builder", pipeline.Question, map[string]string{"question": question})
}
