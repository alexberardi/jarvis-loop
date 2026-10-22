package runner

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/alexberardi/jarvis-loop/internal/fakeclaude"
	"github.com/alexberardi/jarvis-loop/internal/pipeline"
	"github.com/alexberardi/jarvis-loop/internal/store"
)

// slowPrepare gives the demo repo a Prepare command that waits until the returned func is called
// (a fetch, a lib download), touching .loop/scratch/preparing in the worktree while it waits. The
// release file is in the work root: the fence shows it read-only, while the rest of a test dir
// under the host's /tmp is behind the fence's private /tmp.
func (g *rig) slowPrepare() (release func()) {
	rel := filepath.Join(g.r.Cfg.WorkRoot, "release-prepare")
	rc := g.r.Cfg.Repos["demo"]
	rc.Prepare = []string{"touch .loop/scratch/preparing; while [ ! -e '" + rel + "' ]; do sleep 0.05; done"}
	g.r.Cfg.Repos["demo"] = rc
	return func() { os.WriteFile(rel, nil, 0o644) }
}

// armed waits until at least n of the runner's timers are armed, failing the test after 30 s
// instead of hanging the way BlockUntil does when a heartbeat never arms.
func (g *rig) armed(t *testing.T, n int) {
	t.Helper()
	for i := 0; g.clk.Armed() < n; i++ {
		if i > 600 {
			t.Fatalf("%d of %d timers armed after 30 s; log:\n%s", g.clk.Armed(), n, g.logs.String())
		}
		time.Sleep(50 * time.Millisecond)
	}
}

func (g *rig) buildWorktree(id int64) string {
	return filepath.Join(g.r.Cfg.WorkRoot, fmt.Sprintf("%d-build", id))
}

// A claim made while the item's earlier run is still ending on this runner (its lease expired)
// does not touch the worktree until that run's cleanup is done; the earlier run's cleanup does
// not remove the later run's worktree, and one agent runs (#453).
func TestASecondClaimWaitsForTheEarlierRun(t *testing.T) {
	g := newRig(t, fakeclaude.Good)
	id := g.toBuild("Two claims")
	release := g.slowPrepare()
	wt := g.buildWorktree(id)
	waitA := g.runAsync()
	waitFile(t, filepath.Join(wt, ".loop", "scratch", "preparing"))
	g.armed(t, 1) // the first run's heartbeat
	first := worktreeOwner(wt)
	if first == "" {
		t.Fatal("the worktree names no lease")
	}
	// The first lease expires (the runner's clock has not moved: no heartbeat yet); the next
	// claim gets the item.
	g.loopdClk.Advance(store.LeaseTTL + time.Minute)
	waitB := g.runAsync()
	g.armed(t, 3) // the first run's heartbeat; the second's heartbeat and time cap while it waits
	if !strings.Contains(g.logs.String(), "an earlier run of the item is still ending") {
		t.Fatalf("log:\n%s", g.logs.String())
	}
	if o := worktreeOwner(wt); o != first {
		t.Fatalf("the second run touched the worktree while the first held it: %q", o)
	}
	// The first run's heartbeat finds its lease gone: it ends without an agent and cleans up;
	// then the second prepares its own worktree.
	g.clk.Advance(60 * time.Second)
	for i := 0; ; i++ {
		if o := worktreeOwner(wt); o != "" && o != first {
			break
		}
		if i > 600 {
			t.Fatalf("the second run never prepared; log:\n%s", g.logs.String())
		}
		time.Sleep(50 * time.Millisecond)
	}
	release()
	waitA()
	waitB()
	d := g.item(id)
	if d.Item.Stage != pipeline.Review {
		t.Fatalf("stage %s; log:\n%s", d.Item.Stage, g.logs.String())
	}
	if n := strings.Count(g.logs.String(), "agent starting"); n != 1 {
		t.Fatalf("%d agents started; log:\n%s", n, g.logs.String())
	}
	if n := len(g.events(id, "advanced")); n != 2 { // triage's, and the second run's
		t.Fatalf("%d advances; log:\n%s", n, g.logs.String())
	}
	g.cleanedUp(id)
}

// A run's cleanup leaves a worktree that a later run of the item claimed, or whose item a later
// run holds on this runner: no cleanup command runs and nothing is removed.
func TestCleanupLeavesATakenOverWorktree(t *testing.T) {
	for _, c := range []struct {
		name, marker string
		live         *liveRun
	}{
		{name: "marker", marker: "lease-b\n"},
		{name: "live run", live: &liveRun{lease: "lease-b"}},
	} {
		t.Run(c.name, func(t *testing.T) {
			g := newRig(t, fakeclaude.Good)
			wt := filepath.Join(g.r.Cfg.WorkRoot, "9-build")
			os.MkdirAll(filepath.Join(wt, ".loop"), 0o755)
			if c.marker != "" {
				os.WriteFile(filepath.Join(wt, ".loop", leaseFile), []byte(c.marker), 0o644)
			}
			if c.live != nil {
				g.r.live = map[int64]*liveRun{9: c.live}
			}
			ru := &run{item: Item{ID: 9}, stage: StageBuild, lease: "lease-a", wt: wt, repoDir: wt,
				mirror: g.r.mirrorPath("demo"), repo: RepoConfig{Cleanup: []string{"touch cleaned"}}}
			g.r.cleanup(g.ctx, ru, g.r.Log)
			if _, err := os.Stat(filepath.Join(wt, ".loop")); err != nil {
				t.Fatalf("the worktree was removed: %v", err)
			}
			if _, err := os.Stat(filepath.Join(wt, "cleaned")); err == nil {
				t.Fatal("a cleanup command ran in the later run's worktree")
			}
			if !strings.Contains(g.logs.String(), "worktree taken over by a later run") {
				t.Fatalf("log:\n%s", g.logs.String())
			}
		})
	}
}
