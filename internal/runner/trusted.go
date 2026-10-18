package runner

import (
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// After verify refuses a build's change to trusted files, the user OKs that exact commit; the
// next build claim must then verify that commit, not run the agent again (an agent that commits
// anyway makes a new head, which verify refuses again: #203). So at the refusal the runner keeps
// the build's result (.loop/result.json and its evidence) beside the commits it already keeps in
// its mirror, and the claim after the OK restores them in the fresh worktree and goes straight to
// the runner's report: verify on the OK'd head, push, the PR. Runner-local, like the commits: a
// claim on another runner (or one whose branch moved) has neither, and runs the agent as before.

// trustedDir is where the runner keeps the result of item id's build that verify refused.
func (r *Runner) trustedDir(id int64) string {
	return filepath.Join(r.Config().WorkRoot, "trusted", strconv.FormatInt(id, 10))
}

// saveTrustedResult keeps the refused build's result and evidence for head.
func (r *Runner) saveTrustedResult(ru *run, head string, raw []byte, files []EvidenceFile) {
	dir := r.trustedDir(ru.item.ID)
	err := r.writeTrusted(dir, head, raw, files)
	if err != nil {
		os.RemoveAll(dir)
		r.Log.Warn("keeping the refused build's result (the next build after the OK runs the agent)", "item", ru.item.ID, "err", err)
	}
}

func (r *Runner) writeTrusted(dir, head string, raw []byte, files []EvidenceFile) error {
	os.RemoveAll(dir)
	if err := os.MkdirAll(filepath.Join(dir, "evidence"), 0o700); err != nil {
		return err
	}
	for _, f := range files {
		b, err := os.ReadFile(f.Path)
		if err != nil {
			return err
		}
		if err := os.WriteFile(filepath.Join(dir, "evidence", f.Name), b, 0o600); err != nil {
			return err
		}
	}
	if err := os.WriteFile(filepath.Join(dir, "result.json"), raw, 0o600); err != nil {
		return err
	}
	// Last: a directory with a head names a whole result.
	return os.WriteFile(filepath.Join(dir, "head"), []byte(head+"\n"), 0o600)
}

// restoreTrustedResult puts the kept result back in the build's fresh worktree when the item's
// trust OK is for the branch's head, and returns it; nil means the agent runs. A kept result the
// OK no longer allows (used, or for another commit) is dropped.
func (r *Runner) restoreTrustedResult(ctx context.Context, ru *run, log *slog.Logger) []byte {
	if ru.stage != StageBuild || ru.plan != nil || ru.orch != nil {
		return nil
	}
	dir := r.trustedDir(ru.item.ID)
	b, err := os.ReadFile(filepath.Join(dir, "head"))
	if err != nil {
		return nil
	}
	kept := strings.TrimSpace(string(b))
	if !ru.item.Trust.allows(kept) {
		os.RemoveAll(dir)
		return nil
	}
	head, err := r.Git.Head(ctx, ru.wt)
	if err != nil || head != kept {
		log.Info("the OK'd commit is not the branch's head; the agent runs", "trusted", kept, "head", head, "err", err)
		return nil
	}
	raw, err := os.ReadFile(filepath.Join(dir, "result.json"))
	if err == nil {
		err = copyEvidence(filepath.Join(dir, "evidence"), filepath.Join(ru.wt, ".loop", "evidence"))
	}
	if err == nil {
		err = os.WriteFile(filepath.Join(ru.wt, ".loop", "result.json"), raw, 0o644)
	}
	if err != nil {
		log.Warn("restoring the refused build's result; the agent runs", "err", err)
		return nil
	}
	return raw
}

// dropTrustedResult forgets the kept result (the runner refused it, so the agent runs instead).
func (r *Runner) dropTrustedResult(ru *run) { os.RemoveAll(r.trustedDir(ru.item.ID)) }

func copyEvidence(from, to string) error {
	ents, err := os.ReadDir(from)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(to, 0o755); err != nil {
		return err
	}
	for _, e := range ents {
		if !e.Type().IsRegular() {
			continue
		}
		b, err := os.ReadFile(filepath.Join(from, e.Name()))
		if err != nil {
			return err
		}
		if err := os.WriteFile(filepath.Join(to, e.Name()), b, 0o644); err != nil {
			return err
		}
	}
	return nil
}
