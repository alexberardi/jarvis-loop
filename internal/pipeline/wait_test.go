package pipeline

import (
	"slices"
	"strings"
	"testing"
	"time"
)

var waitNow = time.Date(2026, 10, 22, 9, 0, 0, 0, time.UTC)

func TestParseWaitPR(t *testing.T) {
	cases := []struct {
		in, repo, want, err string
	}{
		{"alexberardi/jarvis-server#444", "", "alexberardi/jarvis-server#444", ""},
		{"#444", "alexberardi/jarvis-loop", "alexberardi/jarvis-loop#444", ""},
		{"444", "alexberardi/jarvis-loop", "alexberardi/jarvis-loop#444", ""},
		{"#444-merge", "o/n", "o/n#444", ""},
		{"#444", "", "", "has no repo"},
		{"jarvis-server#444", "o/n", "", "want owner/name#444"},
		{"o/n#0", "", "", "positive"},
		{"o/n#x", "", "", "want owner/name#444"},
	}
	for _, c := range cases {
		got, err := ParseWaitPR(c.in, c.repo)
		if c.err != "" {
			if err == nil || !strings.Contains(err.Error(), c.err) {
				t.Errorf("%q: err %v, want %q", c.in, err, c.err)
			}
			continue
		}
		if err != nil || got != c.want {
			t.Errorf("%q: %q %v, want %q", c.in, got, err, c.want)
		}
	}
	if slug, n, ok := SplitWaitPR("o/n#444"); !ok || slug != "o/n" || n != 444 {
		t.Fatalf("split: %q %d %v", slug, n, ok)
	}
}

func TestApplyWait(t *testing.T) {
	for _, s := range []Stage{Build, Review, Land} {
		t.Run(string(s), func(t *testing.T) {
			it := after(item(s), 7, 8)
			it.NotBefore = waitNow.Add(time.Hour)
			tr, err := ApplyWait(it, []int64{7}, []string{"o/n#444"}, map[int64]Stage{7: Build, 8: Done}, "the test fix", waitNow)
			if err != nil {
				t.Fatal(err)
			}
			if tr.From != s || tr.To != Waiting || tr.Kind != "waiting" || tr.Item.Stage != Waiting || tr.Item.ReturnStage != s {
				t.Fatalf("%+v", tr)
			}
			if !tr.Item.WaitingSince.Equal(waitNow) || !tr.Item.NotBefore.IsZero() {
				t.Fatalf("since %v, not before %v", tr.Item.WaitingSince, tr.Item.NotBefore)
			}
			if tr.Data["waits_on"] != "#7, o/n#444" || tr.Data["reason"] != "the test fix" || tr.Data["return_stage"] != string(s) {
				t.Fatalf("data %+v", tr.Data)
			}
		})
	}
	t.Run("a question from build waits and goes back to build", func(t *testing.T) {
		it := after(item(NeedsYou), 7)
		it.ReturnStage, it.Question = Build, "verify fails on main's test"
		tr, err := ApplyWait(it, []int64{7}, nil, map[int64]Stage{7: Land}, "", waitNow)
		if err != nil || tr.Item.ReturnStage != Build || tr.Item.Question != "" || tr.Data["question"] != "verify fails on main's test" {
			t.Fatalf("%v %+v", err, tr)
		}
	})
	t.Run("waiting waits on more and keeps its start", func(t *testing.T) {
		it := after(item(Waiting), 7)
		it.ReturnStage, it.WaitingSince = Review, waitNow.Add(-time.Hour)
		tr, err := ApplyWait(it, nil, []string{"o/n#1"}, map[int64]Stage{7: Build}, "", waitNow)
		if err != nil || tr.Item.ReturnStage != Review || !tr.Item.WaitingSince.Equal(waitNow.Add(-time.Hour)) || tr.Data["waits_on"] != "#7, o/n#1" {
			t.Fatalf("%v %+v", err, tr)
		}
	})
	refused := []struct {
		name  string
		it    Item
		added []int64
		prs   []string
		st    map[int64]Stage
		want  string
	}{
		{"triage", after(item(Triage), 7), []int64{7}, nil, nil, "link it"},
		{"held", after(item(Held), 7), []int64{7}, nil, nil, "only an item in build"},
		{"qa", after(item(QA), 7), []int64{7}, nil, nil, "only an item in build"},
		{"triage's question", func() Item { it := after(item(NeedsYou), 7); it.ReturnStage = Triage; return it }(), []int64{7}, nil, nil, "only an item"},
		{"nothing named", item(Build), nil, nil, nil, "say what it waits on"},
		{"dependency merged", after(item(Build), 7), []int64{7}, nil, map[int64]Stage{7: QA}, "already merged"},
	}
	for _, c := range refused {
		if _, err := ApplyWait(c.it, c.added, c.prs, c.st, "", waitNow); err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: %v, want %q", c.name, err, c.want)
		}
	}
}

func TestApplyRelease(t *testing.T) {
	it := after(item(Waiting), 7, 8)
	it.ReturnStage, it.WaitingSince = Review, waitNow
	it.WaitPRs = []WaitPR{{Ref: "o/n#444"}}
	if _, ok := ApplyRelease(it, map[int64]Stage{7: Done, 8: Land}); ok {
		t.Fatal("released with #8 in land")
	}
	if _, ok := ApplyRelease(it, map[int64]Stage{7: Done, 8: QA}); ok {
		t.Fatal("released with the PR open")
	}
	it.WaitPRs[0].Merged = true
	tr, ok := ApplyRelease(it, map[int64]Stage{7: Done, 8: QA})
	if !ok || tr.From != Waiting || tr.To != Review || tr.Kind != "released" || tr.Item.Stage != Review || tr.Item.ReturnStage != "" {
		t.Fatalf("%v %+v", ok, tr)
	}
	if !tr.Item.WaitingSince.IsZero() || tr.Item.WaitPRs != nil {
		t.Fatalf("still parked: %+v", tr.Item)
	}
	if want := "#7 done, #8 merged, o/n#444 merged: back to review"; tr.Data["note"] != want {
		t.Fatalf("note %q, want %q", tr.Data["note"], want)
	}
	if _, ok := ApplyRelease(after(item(Build), 7), map[int64]Stage{7: Done}); ok {
		t.Fatal("released an item not waiting")
	}
}

func TestApplyWaitPR(t *testing.T) {
	it := after(item(Waiting), 7)
	it.ReturnStage, it.WaitingSince = Land, waitNow
	it.WaitPRs = []WaitPR{{Ref: "o/n#444"}}
	tr, err := ApplyWaitPR(it, "o/n#444", "MERGED")
	if err != nil || tr.To != Waiting || tr.Kind != "wait_pr_merged" || !tr.Item.WaitPRs[0].Merged || it.WaitPRs[0].Merged {
		t.Fatalf("%v %+v", err, tr)
	}
	if _, err := ApplyWaitPR(tr.Item, "o/n#444", "MERGED"); err == nil {
		t.Fatal("merged twice")
	}
	tr, err = ApplyWaitPR(it, "o/n#444", "CLOSED")
	want := "o/n#444 it waits on was closed without merging: keep, change or drop this one?"
	if err != nil || tr.To != NeedsYou || tr.Item.ReturnStage != Land || tr.Item.Question != want || tr.Item.WaitPRs != nil {
		t.Fatalf("%v %+v", err, tr)
	}
	if _, err := ApplyWaitPR(it, "o/n#444", "OPEN"); err == nil {
		t.Fatal("open accepted")
	}
	if _, err := ApplyWaitPR(item(Build), "o/n#444", "MERGED"); err == nil {
		t.Fatal("not waiting accepted")
	}
}

func TestApplyUnwait(t *testing.T) {
	it := item(Waiting)
	it.ReturnStage, it.WaitPRs = Build, []WaitPR{{Ref: "o/n#1"}}
	tr, err := ApplyUnwait(it)
	if err != nil || tr.To != Build || tr.Item.WaitPRs != nil || tr.Kind != "released" {
		t.Fatalf("%v %+v", err, tr)
	}
	if _, err := ApplyUnwait(item(Build)); err == nil {
		t.Fatal("unwait of a build item")
	}
}

func TestWaitingDependencyClosed(t *testing.T) {
	it := after(item(Waiting), 7, 8)
	it.ReturnStage, it.WaitingSince, it.WaitPRs = Review, waitNow, []WaitPR{{Ref: "o/n#1"}}
	tr, ok := ApplyDependencyClosed(it, 7)
	if !ok || tr.From != Waiting || tr.To != NeedsYou || tr.Item.ReturnStage != Review || !slices.Equal(tr.Item.DependsOn, []int64{8}) {
		t.Fatalf("%v %+v", ok, tr)
	}
	if !tr.Item.WaitingSince.IsZero() || tr.Item.WaitPRs != nil || !strings.HasPrefix(tr.Item.Question, "#7 it depends on was dropped") {
		t.Fatalf("%+v", tr.Item)
	}
	// Answered, it goes back to the stage it waited to return to.
	ans, err := ApplyAnswer(tr.Item, "keep it")
	if err != nil || ans.To != Review {
		t.Fatalf("%v %+v", err, ans)
	}
}

func TestWaitingIsNotClaimed(t *testing.T) {
	if Waiting.IsWork() || !Waiting.Valid() {
		t.Fatal("waiting must be a valid stage nothing claims")
	}
	if got := WaitReason(DefaultControl(), Pace{}, item(Waiting), nil, nil, nil, Frozen{}, nil, nil); got != WaitDeps {
		t.Fatalf("wait reason %q", got)
	}
	if !WaitedLong(Item{Stage: Waiting, WaitingSince: waitNow.Add(-25 * time.Hour)}, waitNow) || WaitedLong(Item{Stage: Waiting, WaitingSince: waitNow.Add(-time.Hour)}, waitNow) {
		t.Fatal("waited long")
	}
}
