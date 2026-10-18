package runner

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/alexberardi/jarvis-loop/internal/fakeclaude"
)

// The result a refused build left comes back, in the next build's fresh worktree, only while the
// approver's OK is for the branch's head and unused (#203); otherwise the agent runs, and a kept
// result the OK no longer allows is dropped.
func TestTrustedResultComesBackForTheOKdHead(t *testing.T) {
	g := newRig(t, fakeclaude.Good)
	id := g.toBuild("Change the harness")
	cl, err := g.r.Client.Claim(g.ctx, "a", nil, []string{StageBuild})
	if err != nil || cl == nil || cl.Item.ID != id {
		t.Fatalf("claim %+v %v", cl, err)
	}
	ru, err := g.r.newRun(cl, "a")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(ru.wt, ".loop", "evidence"), 0o755); err != nil {
		t.Fatal(err)
	}
	gitT(t, ru.wt, "init", "-q", "-b", "main", ".")
	os.WriteFile(filepath.Join(ru.wt, "x"), []byte("x\n"), 0o644)
	gitT(t, ru.wt, "add", "x")
	gitT(t, ru.wt, "-c", "user.name=t", "-c", "user.email=t@example.com", "commit", "-q", "-m", "x")
	head, err := g.r.Git.Head(g.ctx, ru.wt)
	if err != nil {
		t.Fatal(err)
	}
	ev := filepath.Join(t.TempDir(), "after.txt")
	os.WriteFile(ev, []byte("ok\n"), 0o644)
	raw := []byte(`{"outcome":"ready_for_pr","pr_title":"t","pr_body":"b","summary":"s"}`)
	keep := func() { g.r.saveTrustedResult(ru, head, raw, []EvidenceFile{{Name: "after.txt", Path: ev}}) }
	kept := func() bool { _, err := os.Stat(g.r.trustedDir(id)); return err == nil }

	for name, tr := range map[string]*ItemTrust{
		"no OK":        nil,
		"not given":    {HeadSHA: head},
		"used":         {HeadSHA: head, By: "user:alex", Used: true},
		"other commit": {HeadSHA: strings.Repeat("b", 40), By: "user:alex"},
	} {
		keep()
		ru.item.Trust = tr
		if got := g.r.restoreTrustedResult(g.ctx, ru, g.r.Log); got != nil || kept() {
			t.Fatalf("%s: restored %s, kept %v", name, got, kept())
		}
	}

	// The OK is for a commit that is not the branch's head (another runner's mirror, a moved
	// branch): the agent runs, and the result stays for a claim that has the commit.
	other := strings.Repeat("c", 40)
	g.r.saveTrustedResult(ru, other, raw, nil)
	ru.item.Trust = &ItemTrust{HeadSHA: other, By: "user:alex"}
	if got := g.r.restoreTrustedResult(g.ctx, ru, g.r.Log); got != nil || !kept() {
		t.Fatalf("head mismatch: restored %s, kept %v", got, kept())
	}

	keep()
	ru.item.Trust = &ItemTrust{HeadSHA: head, By: "user:alex"}
	got := g.r.restoreTrustedResult(g.ctx, ru, g.r.Log)
	if string(got) != string(raw) {
		t.Fatalf("restored %q", got)
	}
	if b, err := os.ReadFile(filepath.Join(ru.wt, ".loop", "result.json")); err != nil || string(b) != string(raw) {
		t.Fatalf("worktree result.json %q %v", b, err)
	}
	if b, err := os.ReadFile(filepath.Join(ru.wt, ".loop", "evidence", "after.txt")); err != nil || string(b) != "ok\n" {
		t.Fatalf("worktree evidence %q %v", b, err)
	}
}
