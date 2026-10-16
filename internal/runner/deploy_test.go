package runner

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/alexberardi/jarvis-loop/internal/fakeclaude"
	"github.com/alexberardi/jarvis-loop/internal/pipeline"
)

// QA after deploy (docs/decisions/2026-10-16-qa-after-deploy.md): the runner reports deployed
// for a qa item awaiting a deploy only when its own build contains the merge commit.
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
	report := func(rev string, deployAfter bool) {
		t.Helper()
		g.r.Revision = rev
		rc := g.r.Cfg.Repos["demo"]
		rc.QAAfterDeploy = deployAfter
		g.r.Cfg.Repos["demo"] = rc
		if err := g.r.ReportDeployed(g.ctx); err != nil {
			t.Fatalf("report deployed: %v", err)
		}
	}
	for _, c := range []struct {
		name        string
		rev         string
		deployAfter bool
	}{
		{"unknown revision", "", true},
		{"repo without qa_after_deploy here", after, false},
		{"build before the merge", before, true},
		{"build the remote does not have", strings.Repeat("e", 40), true},
	} {
		report(c.rev, c.deployAfter)
		if !awaiting() || len(g.events(id, "deployed")) != 0 {
			t.Fatalf("%s: reported deployed; log:\n%s", c.name, g.logs.String())
		}
	}
	report(after, true)
	evs := g.events(id, "deployed")
	if awaiting() || len(evs) != 1 || evs[0].Data["revision"] != after || evs[0].Actor != "runner:devbox" {
		t.Fatalf("build after the merge: awaiting %v, events %+v; log:\n%s", awaiting(), evs, g.logs.String())
	}
	// Nothing left to report; the merge commit itself as the build counts too.
	report(merge, true)
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
