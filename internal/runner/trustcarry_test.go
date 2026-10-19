package runner

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/alexberardi/jarvis-loop/internal/fakeclaude"
)

// An OK carries to a new head whose trusted files' diff against the merge base is byte-identical
// to the one the approver OK'd (#277): verify runs and the report names the carry. A new head
// that changes a trusted file again, an OK not given, or an OK with no digest is refused as before.
func TestVerifyCarriesTheOKToAnUnchangedTrustedDiff(t *testing.T) {
	g := newRig(t, fakeclaude.Good)
	g.verifyRepo(VerifyStep{Name: "harness", Run: "touch ran"})
	rc := g.r.Cfg.Repos["demo"]
	rc.DenyWrite = []string{"scripts"}
	g.r.Cfg.Repos["demo"] = rc
	id := g.toBuild("Change the harness")
	cl, err := g.r.Client.Claim(g.ctx, "a", nil, []string{StageBuild})
	if err != nil || cl == nil || cl.Item.ID != id {
		t.Fatalf("claim %+v %v", cl, err)
	}
	ru, err := g.r.newRun(cl, "a")
	if err != nil {
		t.Fatal(err)
	}
	wt := ru.wt
	if err := os.MkdirAll(filepath.Join(wt, "scripts"), 0o755); err != nil {
		t.Fatal(err)
	}
	write := func(name, body string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(wt, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	commit := func(msg string) string {
		gitT(t, wt, "add", "-A", "scripts", "feature.txt")
		gitT(t, wt, "commit", "-q", "-m", msg)
		return gitT(t, wt, "rev-parse", "HEAD")
	}
	gitT(t, wt, "init", "-q", "-b", "main", ".")
	write("scripts/harness.sh", "echo base\n")
	write("feature.txt", "a\n")
	base := commit("base")
	gitT(t, wt, "update-ref", "refs/remotes/origin/main", base)

	write("scripts/harness.sh", "echo agent\n")
	write("feature.txt", "b\n")
	trusted := commit("change the harness")
	changed := []string{"M feature.txt", "M scripts/harness.sh"}
	diff, err := g.r.trustedDiff(g.ctx, ru, trusted, []string{"scripts/harness.sh"})
	if err != nil || diff == "" {
		t.Fatalf("trusted diff %q %v", diff, err)
	}
	ok := &ItemTrust{HeadSHA: trusted, By: "user:alex", Used: true, Paths: []string{"scripts/harness.sh"}, DiffSHA256: diffSHA256(diff)}

	// A review fix that touches only feature.txt: same trusted diff, so the OK carries.
	write("feature.txt", "c\n")
	fix := commit("review fix")
	marker := filepath.Join(wt, "ran")
	refused := func(name string, tr *ItemTrust, head string) {
		t.Helper()
		ru.item.Trust, ru.trustUsed, ru.trustCarried, ru.trustDiffSHA = tr, "", "", ""
		_, err := g.r.runVerify(g.ctx, ru, head, changed)
		var refusal *trustRefusal
		if !errors.As(err, &refusal) || refusal.head != head {
			t.Fatalf("%s: runVerify: %v", name, err)
		}
		if _, err := os.Stat(marker); err == nil || ru.trustUsed != "" || ru.trustCarried != "" {
			t.Fatalf("%s: verify ran", name)
		}
	}
	refused("not given", &ItemTrust{HeadSHA: trusted, Paths: ok.Paths, DiffSHA256: ok.DiffSHA256}, fix)
	noDigest := *ok
	noDigest.DiffSHA256 = ""
	refused("no digest", &noDigest, fix)
	otherPaths := *ok
	otherPaths.Paths = []string{"scripts/harness.sh", "scripts/other.sh"}
	refused("other paths", &otherPaths, fix)

	ru.item.Trust = ok
	res, err := g.r.runVerify(g.ctx, ru, fix, changed)
	if err != nil || len(res) != 1 || !res[0].Passed() {
		t.Fatalf("carried runVerify: %+v %v", res, err)
	}
	if _, err := os.Stat(marker); err != nil || ru.trustUsed != fix || ru.trustCarried != fix || ru.trustDiffSHA != ok.DiffSHA256 {
		t.Fatalf("carry: used %q carried %q digest %q", ru.trustUsed, ru.trustCarried, ru.trustDiffSHA)
	}
	os.Remove(marker)

	// One byte more in the trusted file: a different diff, so the user is asked again.
	write("scripts/harness.sh", "echo agent!\n")
	again := commit("touch the harness again")
	refused("changed trusted diff", ok, again)
}
