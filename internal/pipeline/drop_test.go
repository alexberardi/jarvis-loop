package pipeline

import "testing"

func TestApplyDrop(t *testing.T) {
	for _, s := range []Stage{Triage, Build, Review, Land, NeedsYou, Held} {
		t.Run(string(s), func(t *testing.T) {
			it := item(s)
			it.ReturnStage, it.Question = Build, "which repo?"
			tr, err := ApplyDrop(it, "  no hardware  ")
			wantCode(t, err, "")
			if tr.From != s || tr.To != Closed || tr.Item.Stage != Closed || tr.Kind != "dropped" {
				t.Fatalf("drop from %s: %+v", s, tr)
			}
			if tr.Item.ReturnStage != "" || tr.Item.Question != "" {
				t.Fatalf("drop left return stage or question: %+v", tr.Item)
			}
			if tr.Data["reason"] != DropReason || tr.Data["words"] != "no hardware" {
				t.Fatalf("drop data: %+v", tr.Data)
			}
		})
	}
	for _, s := range []Stage{Done, Closed} {
		_, err := ApplyDrop(item(s), "")
		wantCode(t, err, CodeWrongStage)
	}
	tr, err := ApplyDrop(item(Held), "")
	wantCode(t, err, "")
	if tr.Data["words"] != "" {
		t.Fatalf("drop without words: %+v", tr.Data)
	}
}
