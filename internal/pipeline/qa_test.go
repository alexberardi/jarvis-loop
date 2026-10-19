package pipeline

import (
	"strings"
	"testing"
)

// The QA lane (docs/decisions/2026-10-15-qa.md): land → qa instead of done for an item marked
// needs_qa; nothing claims qa; the user's Verified or Reopen moves it to done.

func TestQAStage(t *testing.T) {
	if QA.IsWork() {
		t.Fatal("qa must not be claimable")
	}
	if !QA.Valid() {
		t.Fatal("qa is a stage")
	}
	i, j, k := -1, -1, -1
	for n, s := range AllStages {
		switch s {
		case Land:
			i = n
		case QA:
			j = n
		case Done:
			k = n
		}
	}
	if !(i < j && j < k) {
		t.Fatalf("qa must come between land and done: %v", AllStages)
	}
	if got := Display(QA, false); got != "qa" {
		t.Fatalf("Display(qa) = %q", got)
	}
}

func TestMergedGoesToQAWhenMarked(t *testing.T) {
	for _, c := range []struct {
		name   string
		marked bool
		field  string
		want   Stage
		code   string
	}{
		{"not marked", false, "", Done, ""},
		{"marked on the item", true, "", QA, ""},
		{"repo default from land", false, "true", QA, ""},
		{"false field keeps the item's mark", true, "false", QA, ""},
		{"bad field", false, "yes", "", CodeInvalidField},
	} {
		t.Run(c.name, func(t *testing.T) {
			it := item(Land)
			it.NeedsQA = c.marked
			a := merged(sha1, clean)
			if c.field != "" {
				a.Fields["needs_qa"] = c.field
			}
			tr, err := ApplyAdvance(it, a, t0)
			wantCode(t, err, c.code)
			if c.code != "" {
				return
			}
			if tr.To != c.want || tr.Item.Stage != c.want || tr.Item.MergeSHA != shaM {
				t.Fatalf("merged: to %s, item %+v", tr.To, tr.Item)
			}
			if c.want == QA && !tr.Item.NeedsQA {
				t.Fatal("an item in qa is marked needs_qa")
			}
		})
	}
}

func TestTriageAndReviewMarkNeedsQA(t *testing.T) {
	repro := func(v string) Advance {
		f := map[string]string{"area": "app", "size": "S", "risk": "normal", "resources": ""}
		if v != "" {
			f["needs_qa"] = v
		}
		return Advance{Outcome: Reproduced, Fields: f, Evidence: 1}
	}
	approved := func(v string) Advance {
		f := map[string]string{"summary": "ok", "head_sha": sha1}
		if v != "" {
			f["needs_qa"] = v
		}
		return Advance{Outcome: Approved, Fields: f, Agent: "reviewer"}
	}
	for _, c := range []struct {
		v    string
		want bool
		code string
	}{{"", false, ""}, {"false", false, ""}, {"true", true, ""}, {"1", false, CodeInvalidField}} {
		tr, err := ApplyAdvance(item(Triage), repro(c.v), t0)
		wantCode(t, err, c.code)
		if c.code == "" && tr.Item.NeedsQA != c.want {
			t.Errorf("reproduced needs_qa %q: %v", c.v, tr.Item.NeedsQA)
		}
		tr, err = ApplyAdvance(item(Review), approved(c.v), t0)
		wantCode(t, err, c.code)
		if c.code == "" && tr.Item.NeedsQA != c.want {
			t.Errorf("approved needs_qa %q: %v", c.v, tr.Item.NeedsQA)
		}
	}
	// A later "false" never clears the user's mark.
	it := item(Review)
	it.NeedsQA = true
	tr, err := ApplyAdvance(it, approved("false"), t0)
	wantCode(t, err, "")
	if !tr.Item.NeedsQA {
		t.Fatal("review's false cleared the mark")
	}
}

func TestApplySetQA(t *testing.T) {
	for _, s := range []Stage{Triage, Build, Review, Land, NeedsYou, Held} {
		tr, err := ApplySetQA(item(s))
		wantCode(t, err, "")
		if !tr.Item.NeedsQA || tr.From != s || tr.To != s || tr.Kind != "qa_marked" {
			t.Fatalf("set qa in %s: %+v", s, tr)
		}
	}
	for _, s := range []Stage{QA, Done, Closed} {
		_, err := ApplySetQA(item(s))
		wantCode(t, err, CodeWrongStage)
	}
}

func TestApplyVerified(t *testing.T) {
	it := item(QA)
	it.NeedsQA = true
	tr, err := ApplyVerified(it, "  works on my phone ")
	wantCode(t, err, "")
	if tr.From != QA || tr.To != Done || tr.Item.Stage != Done || tr.Kind != "verified" || tr.Data["words"] != "works on my phone" {
		t.Fatalf("verified: %+v", tr)
	}
	for _, s := range []Stage{Triage, Build, Review, Land, NeedsYou, Held, Done, Closed} {
		_, err := ApplyVerified(item(s), "")
		wantCode(t, err, CodeWrongStage)
	}
}

func TestApplyReopen(t *testing.T) {
	it := item(QA)
	it.Repo, it.PR = "alexberardi/jarvis-node-mobile", 12
	_, err := ApplyReopen(it, "   ")
	wantCode(t, err, CodeMissingField)
	for _, s := range []Stage{Triage, Build, Review, Land, NeedsYou, Held, Done, Closed} {
		_, err := ApplyReopen(item(s), "broken")
		wantCode(t, err, CodeWrongStage)
	}
	tr, err := ApplyReopen(it, " the button is still tiny ")
	wantCode(t, err, "")
	if tr.From != QA || tr.To != Done || tr.Item.Stage != Done || tr.Kind != "reopened" || tr.Data["words"] != "the button is still tiny" {
		t.Fatalf("reopen: %+v", tr)
	}

	it.Feature = "pantry"
	n, err := NewReopen(it, " the button is still tiny ", t0)
	wantCode(t, err, "")
	if n.ReopenOf != it.ID || n.Feature != "pantry" || n.Priority != "P2" || n.Stage != Triage || n.Repo != it.Repo || n.Source != "user" {
		t.Fatalf("reopened item: %+v", n)
	}
	if n.Ask != "the button is still tiny" || !strings.Contains(n.Title, "#7") || !strings.Contains(n.Title, "the button is still tiny") {
		t.Fatalf("reopened item text: %q / %q", n.Title, n.Ask)
	}
	long, _ := NewReopen(it, strings.Repeat("x", 500), t0)
	if len(long.Title) > 120 {
		t.Fatalf("title not clipped: %d", len(long.Title))
	}
}

// A qa item may still be dropped (closed) or reprioritised; it is open until the user checks it.
func TestQAItemDropAndPatch(t *testing.T) {
	tr, err := ApplyDrop(item(QA), "")
	wantCode(t, err, "")
	if tr.To != Closed {
		t.Fatalf("drop from qa: %+v", tr)
	}
	p := "P1"
	_, err = ApplyPatch(item(QA), &p, nil)
	wantCode(t, err, "")
}
