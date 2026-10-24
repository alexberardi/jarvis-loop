package runner

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/alexberardi/jarvis-loop/internal/fakeclaude"
	"github.com/alexberardi/jarvis-loop/internal/pipeline"
)

// QA after deploy (docs/decisions/2026-10-16-qa-after-deploy.md): the runner reports deployed for
// a qa item awaiting a deploy only when BOTH the running loopd's build (GET /version) and its own
// contain the merge commit. loopd is deployed on its own since it moved to fly.io
// (docs/decisions/2026-10-23-loopd-on-fly.md), so one of the two being behind reports nothing.
func TestReportDeployed(t *testing.T) {
	g := newRig(t, fakeclaude.Good)
	id, _ := g.toLand("Loop change")

	// main on the "GitHub" remote: before, the merge, after.
	clone := filepath.Join(t.TempDir(), "clone")
	gitT(t, filepath.Dir(clone), "clone", "-q", g.remote, clone)
	before := gitT(t, clone, "rev-parse", "HEAD")
	commit := func(name string) string {
		os.WriteFile(filepath.Join(clone, name), []byte(name), 0o644)
		gitT(t, clone, "add", name)
		gitT(t, clone, "commit", "-q", "-m", name)
		gitT(t, clone, "push", "-q", "origin", "main")
		return gitT(t, clone, "rev-parse", "HEAD")
	}
	merge := commit("merge.txt")
	after := commit("after.txt")

	it := g.item(id).Item
	if c, err := g.st.Claim(g.ctx, "devbox", pipeline.ClaimRequest{Runner: "devbox", Agent: "lander", Stages: []pipeline.Stage{pipeline.Land}}); err != nil || c == nil {
		t.Fatalf("land claim %v %v", c, err)
	}
	if _, err := g.st.Advance(g.ctx, "devbox", id, pipeline.Merged, map[string]string{"merge_sha": merge, "head_sha": it.HeadSHA, "qa_after_deploy": "true"},
		[]string{"A merge.txt"}); err != nil {
		t.Fatal(err)
	}
	awaiting := func() bool { return g.item(id).Item.AwaitingDeploy }
	if !awaiting() {
		t.Fatal("not awaiting a deploy after merged")
	}
	// report sets loopd's build (GET /version) and the runner's own, then asks the runner to
	// report. Both have to contain the merge.
	report := func(loopdRev, runnerRev string, deployAfter bool) {
		t.Helper()
		g.loopdBuild.Set(loopdRev, "")
		g.r.Revision = runnerRev
		rc := g.r.Cfg.Repos["demo"]
		rc.QAAfterDeploy = deployAfter
		g.r.Cfg.Repos["demo"] = rc
		if err := g.r.ReportDeployed(g.ctx); err != nil {
			t.Fatalf("report deployed: %v", err)
		}
	}
	for _, c := range []struct {
		name        string
		loopdRev    string
		runnerRev   string
		deployAfter bool
	}{
		{"neither revision known", "", "", true},
		{"loopd's revision unknown", "", after, true},
		{"the runner's revision unknown", after, "", true},
		{"repo without qa_after_deploy here", after, after, false},
		{"both builds before the merge", before, before, true},
		{"loopd has it, the runner is behind", after, before, true},
		{"the runner has it, loopd is behind", before, after, true},
		{"a build the remote does not have", strings.Repeat("e", 40), after, true},
	} {
		report(c.loopdRev, c.runnerRev, c.deployAfter)
		if !awaiting() || len(g.events(id, "deployed")) != 0 {
			t.Fatalf("%s: reported deployed; log:\n%s", c.name, g.logs.String())
		}
	}
	report(after, after, true)
	evs := g.events(id, "deployed")
	if awaiting() || len(evs) != 1 || evs[0].Data["revision"] != after || evs[0].Actor != "runner:devbox" {
		t.Fatalf("build after the merge: awaiting %v, events %+v; log:\n%s", awaiting(), evs, g.logs.String())
	}
	// Nothing left to report; the merge commit itself as the build counts too.
	report(merge, merge, true)
	if len(g.events(id, "deployed")) != 1 {
		t.Fatal("reported twice")
	}
}

func TestQAAfterDeployConfig(t *testing.T) {
	body := strings.Replace(cfgBody, `"slug":"alexberardi/jarvis-server"`, `"slug":"alexberardi/jarvis-server","qa_after_deploy":true`, 1)
	c, err := LoadConfig(writeCfg(t, body, 0o600))
	if err != nil {
		t.Fatal(err)
	}
	if rc := c.Repos["jarvis-server"]; !rc.QAAfterDeploy || !rc.QA {
		t.Fatalf("qa_after_deploy implies qa: %+v", rc)
	}
}
