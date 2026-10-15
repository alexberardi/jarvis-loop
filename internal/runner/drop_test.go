package runner

import (
	"context"
	"fmt"
	"os"
	"strings"
	"sync"
	"testing"

	"github.com/alexberardi/jarvis-loop/internal/pipeline"
)

// fakeCloser is GitHub for the drop sweep: PR states, and the closes the runner asked for.
type fakeCloser struct {
	mu     sync.Mutex
	state  map[int]string
	closed []string // "slug#n: comment"
}

func (f *fakeCloser) PRState(_ context.Context, slug string, pr int) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	s, ok := f.state[pr]
	if !ok {
		return "", fmt.Errorf("no PR #%d on %s", pr, slug)
	}
	return s, nil
}

func (f *fakeCloser) ClosePR(_ context.Context, slug string, pr int, comment string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.closed = append(f.closed, fmt.Sprintf("%s#%d: %s", slug, pr, comment))
	f.state[pr] = "CLOSED"
	return nil
}

// withPR takes an item through triage and build to review, with PR pr on alex/demo.
func (g *rig) withPR(title string, pr int) int64 {
	g.t.Helper()
	id := g.toBuild(title)
	c, err := g.st.Claim(g.ctx, "devbox", pipeline.ClaimRequest{Runner: "devbox", Agent: "builder", Stages: []pipeline.Stage{pipeline.Build}})
	if err != nil || c == nil || c.Item.ID != id {
		g.t.Fatalf("build claim %v %v", c, err)
	}
	if _, err := g.st.AddEvidence(g.ctx, "devbox", id, "log", "build.log", strings.NewReader("ok")); err != nil {
		g.t.Fatal(err)
	}
	if _, err := g.st.Advance(g.ctx, "devbox", id, pipeline.PROpened, map[string]string{"repo": "alex/demo", "pr": fmt.Sprint(pr),
		"branch": fmt.Sprintf("loop/%d-x", id), "head_sha": strings.Repeat("a", 40)}, []string{"A feature.txt"}); err != nil {
		g.t.Fatal(err)
	}
	return id
}

func TestCloseDroppedPRs(t *testing.T) {
	g := newRig(t, "")
	gh := &fakeCloser{state: map[int]string{7: "OPEN", 8: "CLOSED", 9: "MERGED"}}
	g.r.Close = gh
	open, byHand, merged, kept := g.withPR("kasa", 7), g.withPR("lifx", 8), g.withPR("merged", 9), g.withPR("kept", 10)
	noPR := g.toBuild("no pr")
	for _, id := range []int64{open, byHand, merged, noPR} {
		if _, err := g.st.Drop(g.ctx, "user:alex", id, "no hardware"); err != nil {
			t.Fatal(err)
		}
	}

	if err := g.r.CloseDropped(g.ctx); err != nil {
		t.Fatal(err)
	}
	if want := []string{"alex/demo#7: " + DropComment}; strings.Join(gh.closed, "|") != strings.Join(want, "|") {
		t.Fatalf("closed %q, want %q", gh.closed, want)
	}
	for id, want := range map[int64]string{open: "closed", byHand: "already_closed", merged: "already_merged"} {
		ev := g.lastEvent(id, "pr_closed")
		if ev == nil || ev.Data["state"] != want || ev.Actor != "runner:devbox" {
			t.Fatalf("item %d pr_closed %+v, want %s", id, ev, want)
		}
	}
	if ev := g.lastEvent(kept, "pr_closed"); ev != nil || gh.state[10] != "" {
		t.Fatalf("an item that was not dropped: %+v", ev)
	}
	// The next pass finds nothing to do: no second close, no second event.
	if err := g.r.CloseDropped(g.ctx); err != nil {
		t.Fatal(err)
	}
	if len(gh.closed) != 1 || len(g.events(open, "pr_closed")) != 1 {
		t.Fatalf("second pass: closed %q, events %d", gh.closed, len(g.events(open, "pr_closed")))
	}
}

// A PR GitHub cannot tell about stays on the list for the next pass; the others go on.
func TestCloseDroppedPRsKeepsGoing(t *testing.T) {
	g := newRig(t, "")
	gh := &fakeCloser{state: map[int]string{8: "OPEN"}}
	g.r.Close = gh
	unknown, open := g.withPR("unknown", 7), g.withPR("open", 8)
	for _, id := range []int64{unknown, open} {
		if _, err := g.st.Drop(g.ctx, "user:alex", id, ""); err != nil {
			t.Fatal(err)
		}
	}
	g.r.CloseDropped(g.ctx)
	if g.lastEvent(unknown, "pr_closed") != nil || g.lastEvent(open, "pr_closed") == nil {
		t.Fatalf("events: unknown %v, open %v", g.lastEvent(unknown, "pr_closed"), g.lastEvent(open, "pr_closed"))
	}
	if !strings.Contains(g.logs.String(), "no PR #7") {
		t.Fatalf("the failure is not logged: %s", g.logs.String())
	}
}

// The close's gh argv: a comment, never --delete-branch.
func TestGHClosePR(t *testing.T) {
	ctx := context.Background()
	gh, log := fakeGH(t, `case "$1 $2" in
"pr view") echo '{"state":"OPEN"}';;
"pr close") echo '✓ Closed pull request o/r#7';;
esac
`)
	if s, err := gh.PRState(ctx, "o/r", 7); err != nil || s != "OPEN" {
		t.Fatalf("state %q %v", s, err)
	}
	if err := gh.ClosePR(ctx, "o/r", 7, DropComment); err != nil {
		t.Fatal(err)
	}
	if err := gh.ClosePR(ctx, "o/r", 0, DropComment); err == nil {
		t.Fatal("closed PR 0")
	}
	b, _ := os.ReadFile(log)
	want := "pr view 7 --repo o/r --json state\npr close 7 --repo o/r --comment Dropped by the owner"
	if got := strings.TrimSpace(string(b)); got != want {
		t.Fatalf("gh argv:\n%s\nwant:\n%s", got, want)
	}
}
