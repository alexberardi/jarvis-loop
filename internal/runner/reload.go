package runner

import (
	"context"
	"fmt"
	"maps"
	"slices"
	"strings"
	"sync"
	"time"
)

// Reloading the config and draining (docs/decisions/2026-10-18-runner-reload-drain.md): SIGHUP
// reloads runner.json in place, for the next claim, and SIGUSR1 (`loop-runner drain`) stops
// claiming and ends Run once the live runs have, so a restart for a new binary loses no agent's
// work.

// RunnerDraining is the state a draining runner reports to loopd (the board shows it).
const RunnerDraining = "draining"

// Config is the runner's current config: Cfg, or the last one Reload took. A run reads the one
// it started with (run.cfg) instead.
func (r *Runner) Config() *Config {
	if c := r.cur.Load(); c != nil {
		return c
	}
	return r.Cfg
}

// cfgOf is the config ru started with (the current one for a run made without it).
func (r *Runner) cfgOf(ru *run) *Config {
	if ru != nil && ru.cfg != nil {
		return ru.cfg
	}
	return r.Config()
}

func (r *Runner) ctl() {
	r.ctlOnce.Do(func() {
		r.reloaded = make(chan struct{}, 1)
		r.drainC = make(chan struct{})
	})
}

// Reload reads the config file again (Config().Path()) and, when it is valid, makes it the
// config of every later claim: repos, slots, stages, verify steps and the rest. Live runs keep
// theirs. An invalid file, or one that changes what only a restart can (fixedFields), is refused
// and the config stays as it was.
func (r *Runner) Reload() error {
	old := r.Config()
	c, err := LoadConfig(old.Path())
	if err != nil {
		return err
	}
	return r.ReloadConfig(c)
}

// ReloadConfig is Reload with the new config already loaded (LoadConfig).
func (r *Runner) ReloadConfig(c *Config) error {
	old := r.Config()
	if f := fixedChanged(old, c); len(f) > 0 {
		return fmt.Errorf("%s changed, which only a restart applies: the config stays as it was", strings.Join(f, ", "))
	}
	r.cur.Store(c)
	r.ctl()
	select {
	case r.reloaded <- struct{}{}:
	default:
	}
	added, removed := repoDiff(old, c)
	r.Log.Info("config reloaded: it applies to the next claim; live runs keep theirs", "path", c.Path(),
		"slots", c.Slots, "land_slots", c.LandSlots, "stages", c.Stages, "repos_added", added, "repos_removed", removed)
	return nil
}

// fixedChanged names the fields that differ between old and c and that a reload cannot change:
// who the runner is to loopd, where its work lives, and its fence (probed once at start, and
// shared by the live runs).
func fixedChanged(old, c *Config) []string {
	var out []string
	for _, f := range []struct {
		name     string
		old, new string
	}{
		{"name", old.Name, c.Name},
		{"loopd_url", old.LoopdURL, c.LoopdURL},
		{"token", old.Token, c.Token},
		{"work_root", old.WorkRoot, c.WorkRoot},
		{"bwrap", old.Bwrap, c.Bwrap},
		{"sandbox_exec", old.SandboxExec, c.SandboxExec},
	} {
		if f.old != f.new {
			out = append(out, f.name)
		}
	}
	// The fence's masks and writable paths: every fenced command reads them, a live run's too.
	if !slices.Equal(old.VerifyMask, c.VerifyMask) {
		out = append(out, "verify_mask")
	}
	if !slices.Equal(old.VerifyWritable, c.VerifyWritable) {
		out = append(out, "verify_writable")
	}
	return out
}

func repoDiff(old, c *Config) (added, removed []string) {
	for _, k := range slices.Sorted(maps.Keys(c.Repos)) {
		if _, ok := old.Repos[k]; !ok {
			added = append(added, k)
		}
	}
	for _, k := range slices.Sorted(maps.Keys(old.Repos)) {
		if _, ok := c.Repos[k]; !ok {
			removed = append(removed, k)
		}
	}
	return added, removed
}

// Drain stops the runner claiming: Run returns once the runs it has (and its main-CI watches)
// end. It tells loopd, which shows the runner as draining until its next claim. Draining twice
// is draining once.
func (r *Runner) Drain(ctx context.Context) error {
	r.ctl()
	if !r.draining.CompareAndSwap(false, true) {
		return nil
	}
	close(r.drainC)
	r.Log.Info("draining: claiming nothing new; the runner exits once its live runs end")
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	return r.Client.SetState(ctx, RunnerDraining)
}

// Draining says whether Drain was called.
func (r *Runner) Draining() bool { return r.draining.Load() }

// DrainLogEvery is how often a draining Run logs what it still waits on.
const DrainLogEvery = 30 * time.Second

// waitSet names what Run waits on before it can return (the live runs, the background loops,
// the main-CI watches) and since when, so a drain that does not end says why (#284).
type waitSet struct {
	mu sync.Mutex
	m  map[string]time.Time
	n  int
}

// add records a wait that began at; done ends it.
func (w *waitSet) add(name string, at time.Time) (done func()) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.m == nil {
		w.m = map[string]time.Time{}
	}
	w.n++
	key := fmt.Sprintf("%s\x00%d", name, w.n) // two waits may share a name
	w.m[key] = at
	return func() {
		w.mu.Lock()
		defer w.mu.Unlock()
		delete(w.m, key)
	}
}

// list says each wait and how long it has lasted, the oldest first.
func (w *waitSet) list(now time.Time) []string {
	w.mu.Lock()
	defer w.mu.Unlock()
	keys := slices.Collect(maps.Keys(w.m))
	slices.SortFunc(keys, func(a, b string) int { return w.m[a].Compare(w.m[b]) })
	out := make([]string, 0, len(keys))
	for _, k := range keys {
		name, _, _ := strings.Cut(k, "\x00")
		out = append(out, fmt.Sprintf("%s (%s)", name, now.Sub(w.m[k]).Round(time.Second)))
	}
	return out
}

// logDrainWaits logs what Run still waits on every DrainLogEvery until stop closes.
func (r *Runner) logDrainWaits(stop <-chan struct{}) {
	for {
		t := r.Clock.NewTimer(DrainLogEvery)
		select {
		case <-stop:
			t.Stop()
			return
		case <-t.C():
		}
		r.Log.Info("draining: still waiting", "on", strings.Join(r.waits.list(r.Clock.Now()), "; "))
	}
}
