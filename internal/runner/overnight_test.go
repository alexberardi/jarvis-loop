package runner

import (
	"strings"
	"testing"
)

func TestPipelineTests(t *testing.T) {
	raw := `[
	  {"name": "timer correction", "passed": 1, "runs": 1},
	  {"name": "remember that", "passed": 0, "runs": 1, "failures": ["run 1 turn 2: no remember call", "run 1 turn 2: reply claims it was saved"]},
	  {"name": "voice heard", "passed": 0, "runs": 0, "skipped": "a voice scenario; the sandbox is not up with --voice"},
	  {"name": "silent fail", "passed": 0, "runs": 1}
	]`
	tests, err := PipelineTests([]byte(raw), "alexberardi/jarvis-server")
	if err != nil {
		t.Fatal(err)
	}
	want := []struct{ title, status, err string }{
		{"timer correction", "passed", ""},
		{"remember that", "failed", "run 1 turn 2: no remember call\nrun 1 turn 2: reply claims it was saved"},
		{"voice heard", "skipped", ""},
		{"silent fail", "failed", "0 of 1 runs passed"},
	}
	if len(tests) != len(want) {
		t.Fatalf("tests %+v", tests)
	}
	for i, w := range want {
		g := tests[i]
		if g.Suite != LanePipeline || g.Title != w.title || g.Status != w.status || g.Error != w.err || g.Repo != "alexberardi/jarvis-server" {
			t.Errorf("test %d: %+v, want %+v", i, g, w)
		}
	}
	if _, err := PipelineTests([]byte("not json"), ""); err == nil {
		t.Error("took a summary that is not JSON")
	}
}

func TestRerunFilter(t *testing.T) {
	got := RerunFilter([]string{"login.spec.ts › a wrong password (is) refused", "Biometric login", "timer|silent"})
	if want := `a wrong password \(is\) refused|Biometric login|timer\|silent`; got != want {
		t.Fatalf("filter %q, want %q", got, want)
	}
}

func TestMarkFlaky(t *testing.T) {
	first := []RegressionTest{
		{Suite: "web", Title: "a", Status: "passed"},
		{Suite: "web", Title: "b", Status: "failed", Error: "timeout"},
		{Suite: "web", Title: "c", Status: "timedOut", Error: "slow"},
		{Suite: "mobile", Title: "b", Status: "failed", Error: "gone"},
	}
	MarkFlaky(first, []RegressionTest{{Suite: "web", Title: "b", Status: "passed"}, {Suite: "web", Title: "c", Status: "failed"}})
	if first[1].Status != "flaky" || first[1].Error != "first run: timeout" {
		t.Errorf("b: %+v", first[1])
	}
	if first[2].Status != "timedOut" || first[3].Status != "failed" || first[0].Status != "passed" {
		t.Errorf("the others: %+v", first)
	}
}

func TestRegressionTestsRecordings(t *testing.T) {
	raw := `{"schema": 1, "run": "r1", "tests": [
	  {"suite": "voice", "title": "lights", "status": "failed", "error": "no reply", "screenshots": [],
	   "attachments": ["voice/cases/lights/run-1/agent.log", "voice/cases/lights/run-1/reply.wav", "voice/cases/lights/run-1/utterance.wav"]},
	  {"suite": "voice", "title": "time", "status": "passed", "attachments": ["voice/cases/time/run-1/reply.wav"]},
	  {"suite": "voice", "title": "escape", "status": "failed", "attachments": ["../../etc/x.wav", "/abs.wav"]}
	]}`
	rep, err := ParseE2EReport([]byte(raw))
	if err != nil {
		t.Fatal(err)
	}
	tests, shots := rep.RegressionTests(map[string]string{"voice": "s/r"}, "voice-")
	if tests[0].Recording != "voice-1-reply.wav" || shots["voice-1-reply.wav"] != "voice/cases/lights/run-1/reply.wav" {
		t.Fatalf("lights: %+v %v", tests[0], shots)
	}
	if tests[1].Recording != "" || tests[2].Recording != "" || len(shots) != 1 {
		t.Fatalf("a pass or a path out of the run has a recording: %+v %v", tests, shots)
	}
}

func TestPipelineCommand(t *testing.T) {
	if c := PipelineCommand("loop-1-verify", "", 3); !strings.Contains(c, "--json --repeat 3 > ") {
		t.Errorf("%q lacks the repeat", c)
	}
	c := PipelineCommand("loop-reg-3-2", "a|b", 1)
	for _, want := range []string{"scripts/sandbox up --name 'loop-reg-3-2' || exit 3;", "go run ./tools/pipeline run --sandbox 'loop-reg-3-2' --json --filter 'a|b' > \"$LOOP_PIPELINE_OUT\"",
		"scripts/sandbox down --name 'loop-reg-3-2' --purge; exit $rc"} {
		if !strings.Contains(c, want) {
			t.Errorf("%q lacks %q", c, want)
		}
	}
}
