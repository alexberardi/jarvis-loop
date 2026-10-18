package slack

import (
	"fmt"
	"strings"
	"testing"

	"github.com/alexberardi/jarvis-loop/internal/pipeline"
	"github.com/alexberardi/jarvis-loop/internal/store"
)

func TestRunwayWarningNamesWhatFreesTheMost(t *testing.T) {
	e, id, _ := heldSetup(t)
	e.calls()
	if err := e.st.Record(e.ctx, store.Event{Actor: "loopd", Kind: store.KindRunwayLow, Data: map[string]any{
		"hours": 0.5, "threshold": 3, "runnable": 1, "unblockers": []map[string]any{
			{"item": id, "action": pipeline.UnblockApprove, "frees": 4, "sha": shaA},
			{"item": 99, "action": pipeline.UnblockAnswer, "frees": 1},
		}}}); err != nil {
		t.Fatal(err)
	}
	cs := only(t, e.calls(), "chat.postMessage")
	if len(cs) != 1 {
		t.Fatalf("%d messages, want one warning", len(cs))
	}
	w := cs[0]
	if w.Body["channel"] != ChannelLoop || w.Body["thread_ts"] != nil {
		t.Fatalf("warning in %v / thread %v, want #loop's top level", w.Body["channel"], w.Body["thread_ts"])
	}
	txt := allText(w)
	for _, want := range []string{"<@" + Approver + ">", "runs dry in about 0.5 agent-hours", fmt.Sprintf("Approve #%d auth fix", id), "frees 4", "Answer #99", "reply in its thread"} {
		if !strings.Contains(txt, want) {
			t.Errorf("warning lacks %q: %s", want, txt)
		}
	}
	if ids := actionIDs(w); strings.Join(ids, ",") != "approve" {
		t.Fatalf("buttons %v, want only the approval's", ids)
	}
	if v := elementValue(w, "approve"); v != fmt.Sprintf("%d:%s", id, shaA) {
		t.Fatalf("approve value %q", v)
	}

	// The button approves through the same path as the held message's, and leaves the warning.
	e.press(Approver, ChannelLoop, "1700000000.999999", "approve", fmt.Sprintf("runway:%d", id), fmt.Sprintf("%d:%s", id, shaA))
	if st := e.item(id).Item.Stage; st != pipeline.Land {
		t.Fatalf("stage %s after Approve on the warning, want land", st)
	}
	for _, c := range e.calls() {
		if c.Method == "chat.update" && c.Body["ts"] == "1700000000.999999" {
			t.Fatalf("the warning was replaced: %s", allText(c))
		}
	}
}

func TestRunwayWarningButtonRefusesOthers(t *testing.T) {
	e, id, _ := heldSetup(t)
	e.calls()
	e.press("U0SOMEONE", ChannelLoop, "1700000000.999999", "approve", fmt.Sprintf("runway:%d", id), fmt.Sprintf("%d:%s", id, shaA))
	if st := e.item(id).Item.Stage; st != pipeline.Held {
		t.Fatalf("stage %s after someone else's Approve, want held", st)
	}
}
