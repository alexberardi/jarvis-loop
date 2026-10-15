package slack

import (
	"fmt"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/alexberardi/jarvis-loop/internal/pipeline"
	"github.com/alexberardi/jarvis-loop/internal/store"
)

// The land script's lines in the item's thread (docs/phases/06-land.md, Slack): one line each,
// and nothing for a wait (the parent's stage line says it instead).
func TestLandLines(t *testing.T) {
	e := newBotEnv(t)
	id := e.add("x")
	it := e.item(id).Item
	it.PR, it.Repo = 7, "alexberardi/jarvis-loop"
	d := store.Detail{Item: it}
	merge := "ff0f006aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	cases := []struct {
		ev   store.Event
		want string // "" for no reply
	}{
		{store.Event{Kind: "advanced", Outcome: "merged", Data: map[string]any{"merge_sha": merge, "main_ci": "watching"}},
			"^Merged <https://github.com/alexberardi/jarvis-loop/pull/7\\|alexberardi/jarvis-loop#7> as `ff0f006`; watching main's CI\\.$"},
		{store.Event{Kind: "advanced", Outcome: "merged", Data: map[string]any{"merge_sha": merge, "main_ci": "watching", "merged_outside": "true"}},
			"^Merged .*#7> as `ff0f006` on GitHub, outside the loop; watching main's CI\\.$"},
		{store.Event{Kind: "advanced", Outcome: "merged", Data: map[string]any{"merge_sha": merge}},
			"^Merged .*#7> as `ff0f006`\\. Done\\.$"},
		{store.Event{Kind: "advanced", Outcome: "back_to_review", Data: map[string]any{"notes": "The PR's head is `bbbbbbb`."}},
			"^Back to review: The PR's head is `bbbbbbb`\\.$"},
		{store.Event{Kind: "advanced", Outcome: "back_to_build", Data: map[string]any{"reason": "CI failed on `aaaaaaa`: check"}},
			"^Back to build: CI failed on `aaaaaaa`: check$"},
		{store.Event{Kind: "main_ci", Data: map[string]any{"result": "green", "merge_sha": merge}},
			"^main green on `ff0f006`\\.$"},
		{store.Event{Kind: "main_ci", Data: map[string]any{"result": "red", "merge_sha": merge, "failing": []any{"ci", "<!channel>"}, "p1_item": float64(12)}},
			"^main red on `ff0f006` \\(failing: ci, &lt;!channel&gt;\\): filed #12 \\(P1\\)\\.$"},
		{store.Event{Kind: "main_ci", Data: map[string]any{"result": "red", "merge_sha": merge, "failing": []any{"ci"}}},
			"^main red on `ff0f006` \\(failing: ci\\); no P1 item could be filed, see `loop show \\d+`\\.$"},
		{store.Event{Kind: "main_ci", Data: map[string]any{"result": "timeout", "merge_sha": merge}},
			"^main's CI on `ff0f006` did not finish in time; see the runs on GitHub\\.$"},
		{store.Event{Kind: "land_waiting", Data: map[string]any{"waiting_for": "CI (check)"}}, ""},
	}
	for _, c := range cases {
		c.ev.ItemID = id
		m, ok := e.b.describe(c.ev, d)
		if c.want == "" {
			if ok {
				t.Errorf("%s posted %q, want no reply", c.ev.Kind, m.Text)
			}
			continue
		}
		if ok2, _ := matchRe(c.want, m.Text); !ok || !ok2 {
			t.Errorf("%s %v: %q, want /%s/", c.ev.Kind, c.ev.Data, m.Text, c.want)
		}
	}
}

// An item the loop files at P1 (main red) mentions the approver in its thread; the user's own
// P1s, and lower priorities, do not.
func TestP1FindingMentionsTheApprover(t *testing.T) {
	e := newBotEnv(t)
	p1, err := e.st.AddItem(e.ctx, "runner:devbox", store.NewItem{Title: "main red after #7: ci", Repo: "alexberardi/jarvis-loop", Priority: "P1", Source: "finding"})
	if err != nil {
		t.Fatal(err)
	}
	posts := only(t, e.calls(), "chat.postMessage")
	if len(posts) != 2 || posts[1].Body["thread_ts"] != e.thread(p1.ID) {
		t.Fatalf("posts %+v", posts)
	}
	if txt := allText(posts[1]); !strings.Contains(txt, "<@"+Approver+"> P1, filed by the loop: main red after #7: ci") {
		t.Fatalf("mention %s", txt)
	}
	for _, n := range []store.NewItem{
		{Title: "mine", Priority: "P1", Source: "user"},
		{Title: "finding, not urgent", Priority: "P3", Source: "finding"},
	} {
		if _, err := e.st.AddItem(e.ctx, "user:alex", n); err != nil {
			t.Fatal(err)
		}
		for _, p := range only(t, e.calls(), "chat.postMessage") {
			if strings.Contains(allText(p), "<@") {
				t.Fatalf("%q mentioned someone: %s", n.Title, allText(p))
			}
		}
	}
}

// A land wait over 10 minutes edits the parent's stage line; the next outcome replaces it.
func TestParentShowsLandWaiting(t *testing.T) {
	e := newBotEnv(t)
	id := e.add("x")
	e.runner(id, "a1", pipeline.Reproduced, map[string]string{"area": "a", "size": "S", "risk": "normal", "resources": ""})
	e.runner(id, "builder", pipeline.PROpened, map[string]string{"repo": "jarvis-server", "pr": "7", "branch": "loop/1-x", "head_sha": shaA}, "M x.go")
	e.runner(id, "reviewer", pipeline.Approved, map[string]string{"summary": "ok", "head_sha": shaA})
	cl, err := e.st.Claim(e.ctx, "devbox", pipeline.ClaimRequest{Runner: "devbox", Agent: "lander"})
	if err != nil || cl == nil {
		t.Fatalf("claim %+v %v", cl, err)
	}
	e.calls()
	since := t0.Add(-11 * time.Minute)
	if _, err := e.st.RunnerEvent(e.ctx, "devbox", id, "land_waiting", map[string]any{"waiting_for": "CI (check)", "since": since.Format(time.RFC3339)}); err != nil {
		t.Fatal(err)
	}
	cs := e.calls()
	if posts := only(t, cs, "chat.postMessage"); len(posts) != 0 {
		t.Fatalf("a wait posted %+v", posts)
	}
	ups := only(t, cs, "chat.update")
	if len(ups) != 1 || !strings.Contains(allText(ups[0]), "Stage *land, waiting for CI (check) since 11:49 UTC*") {
		t.Fatalf("parent %+v", ups)
	}
	if _, err := e.st.Advance(e.ctx, "devbox", id, pipeline.BackToBuild, map[string]string{"reason": "CI failed"}, nil); err != nil {
		t.Fatal(err)
	}
	ups = only(t, e.calls(), "chat.update")
	if len(ups) != 1 || strings.Contains(allText(ups[0]), "waiting") {
		t.Fatalf("parent after the wait %+v", ups)
	}
}

// The digest lists main's CI results that were not green.
func TestDigestMainCI(t *testing.T) {
	e := newBotEnv(t)
	e.clk.Advance(30 * time.Minute)
	e.b.Tick(e.ctx)
	id := e.add("ship it")
	e.runner(id, "a1", pipeline.Reproduced, map[string]string{"area": "a", "size": "S", "risk": "normal", "resources": ""})
	e.runner(id, "builder", pipeline.PROpened, map[string]string{"repo": "jarvis-server", "pr": "7", "branch": "loop/1-x", "head_sha": shaA}, "M x.go")
	e.runner(id, "reviewer", pipeline.Approved, map[string]string{"summary": "ok", "head_sha": shaA})
	e.runner(id, "lander", pipeline.Merged, map[string]string{"merge_sha": shaB, "head_sha": shaA}, "M x.go")
	if _, err := e.st.MainCI(e.ctx, "devbox", id, map[string]any{"result": "timeout", "merge_sha": shaB}); err != nil {
		t.Fatal(err)
	}
	e.calls()
	e.clk.Advance(4 * time.Hour)
	e.b.Tick(e.ctx)
	posts := only(t, e.calls(), "chat.postMessage")
	if len(posts) != 1 {
		t.Fatalf("posts %+v", posts)
	}
	txt := allText(posts[0])
	if !strings.Contains(txt, "Merged (1)") || !strings.Contains(txt, "Main CI not green (1)") || !strings.Contains(txt, fmt.Sprintf("#%d ship it: timeout", id)) {
		t.Fatalf("digest %s", txt)
	}
}

func matchRe(pattern, s string) (bool, error) {
	re, err := regexp.Compile(pattern)
	if err != nil {
		return false, err
	}
	return re.MatchString(s), nil
}
