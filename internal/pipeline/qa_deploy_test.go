package pipeline

import "testing"

// QA after deploy (docs/decisions/2026-10-16-qa-after-deploy.md): a repo whose binaries are the
// loop itself waits in qa for a deploy before the user is asked to check.

func TestMergedAwaitsDeploy(t *testing.T) {
	for _, c := range []struct {
		name         string
		qa, deploy   string
		wantStage    Stage
		wantAwaiting bool
		code         string
	}{
		{"no flags", "", "", Done, false, ""},
		{"qa only", "true", "", QA, false, ""},
		{"after deploy implies qa", "", "true", QA, true, ""},
		{"both", "true", "true", QA, true, ""},
		{"false", "true", "false", QA, false, ""},
		{"bad value", "", "yes", "", false, CodeInvalidField},
	} {
		t.Run(c.name, func(t *testing.T) {
			a := merged(sha1, clean)
			if c.qa != "" {
				a.Fields["needs_qa"] = c.qa
			}
			if c.deploy != "" {
				a.Fields["qa_after_deploy"] = c.deploy
			}
			tr, err := ApplyAdvance(item(Land), a, t0)
			wantCode(t, err, c.code)
			if c.code != "" {
				return
			}
			if tr.Item.Stage != c.wantStage || tr.Item.AwaitingDeploy != c.wantAwaiting {
				t.Fatalf("merged: stage %s awaiting %v, want %s %v", tr.Item.Stage, tr.Item.AwaitingDeploy, c.wantStage, c.wantAwaiting)
			}
			if c.wantAwaiting && (!tr.Item.NeedsQA || tr.Data["awaiting_deploy"] != true) {
				t.Fatalf("awaiting deploy: needs_qa %v, data %v", tr.Item.NeedsQA, tr.Data)
			}
		})
	}
}

func TestDeployed(t *testing.T) {
	waiting := item(QA)
	waiting.MergeSHA, waiting.AwaitingDeploy = shaM, true
	tr, err := ApplyDeployed(waiting, sha1)
	if err != nil {
		t.Fatal(err)
	}
	if tr.Kind != "deployed" || tr.From != QA || tr.To != QA || tr.Item.AwaitingDeploy || tr.Item.Stage != QA {
		t.Fatalf("deployed: %+v", tr)
	}
	if tr.Data["revision"] != sha1 || tr.Data["merge_sha"] != shaM {
		t.Fatalf("deployed data: %v", tr.Data)
	}

	notWaiting := item(QA)
	notWaiting.MergeSHA = shaM
	for name, it := range map[string]Item{"not awaiting a deploy": notWaiting, "done": item(Done), "land": item(Land)} {
		t.Run(name, func(t *testing.T) {
			_, err := ApplyDeployed(it, sha1)
			wantCode(t, err, CodeWrongStage)
		})
	}
	_, err = ApplyDeployed(waiting, "not a sha")
	wantCode(t, err, CodeInvalidField)
}

func TestVerifiedAndReopenEndTheDeployWait(t *testing.T) {
	it := item(QA)
	it.AwaitingDeploy = true
	tr, err := ApplyVerified(it, "")
	if err != nil || tr.Item.AwaitingDeploy {
		t.Fatalf("verified while awaiting deploy: %v, %+v", err, tr.Item)
	}
	tr, err = ApplyReopen(it, "still broken")
	if err != nil || tr.Item.AwaitingDeploy {
		t.Fatalf("reopen while awaiting deploy: %v, %+v", err, tr.Item)
	}
}
