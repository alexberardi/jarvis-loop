package runner

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// A report.json as jarvis-server's scripts/lib/e2e_report.py writes it (docs/dev/e2e.md, schema 1).
const e2eReport = `{
  "schema": 1, "run": "20261018-101500-abc123", "status": "failed",
  "started_at": "2026-10-18T10:15:00-04:00", "finished_at": "2026-10-18T10:19:00-04:00",
  "sandbox": "loop-reg-3", "filter": null,
  "git": {"sha": "0123456789abcdef0123456789abcdef01234567", "branch": "HEAD", "dirty": false},
  "counts": {"total": 4, "passed": 1, "failed": 2, "skipped": 1},
  "suites": {
    "web": {"status": "failed", "exit_code": 1, "error": null, "duration_ms": 87400},
    "mobile": {"status": "error", "exit_code": 2, "error": "the emulator did not boot", "duration_ms": 0}
  },
  "tests": [
    {"suite": "web", "id": "a", "title": "inbox.spec.ts › deleting removes the item", "file": "tests/inbox.spec.ts", "line": 32,
     "status": "passed", "duration_ms": 600, "error": null, "screenshots": [], "trace": null, "console_errors": []},
    {"suite": "web", "id": "b", "title": "login.spec.ts › a wrong password is refused", "file": "tests/login.spec.ts", "line": 9,
     "status": "failed", "duration_ms": 1900, "error": "expect(locator).toHaveText(expected)\nExpected: \"Wrong password\"",
     "screenshots": ["web/artifacts/login-b/test-failed-1.png", "web/artifacts/login-b/test-failed-2.png"], "trace": "web/artifacts/login-b/trace.zip", "console_errors": []},
    {"suite": "mobile", "id": "c", "title": "Pairing", "file": ".maestro/pairing.yaml", "line": 1,
     "status": "timedOut", "duration_ms": 30000, "error": "timed out", "screenshots": ["../../etc/passwd"], "trace": null, "console_errors": []},
    {"suite": "mobile", "id": "d", "title": "Biometric login", "file": ".maestro/biometric-login.yaml", "line": 1, "status": "skipped",
     "duration_ms": 0, "error": null, "screenshots": [], "trace": null, "console_errors": [], "skip_reason": "no enrollment"}
  ]
}`

func TestParseE2EReport(t *testing.T) {
	rep, err := ParseE2EReport([]byte(e2eReport))
	if err != nil {
		t.Fatal(err)
	}
	if rep.Run != "20261018-101500-abc123" || rep.Git.SHA[:7] != "0123456" || len(rep.Tests) != 4 {
		t.Fatalf("report %+v", rep)
	}
	if got := rep.SuiteErrors(); got != "suite mobile: the emulator did not boot" {
		t.Fatalf("suite errors %q", got)
	}
	tests, shots := rep.RegressionTests(map[string]string{"web": "o/server", "mobile": "o/mobile"})
	if len(tests) != 4 {
		t.Fatalf("tests %+v", tests)
	}
	login := tests[1]
	if login.Status != "failed" || login.Screenshot != "2-test-failed-1.png" || login.Repo != "o/server" ||
		!strings.Contains(login.Error, "Wrong password") || login.MS != 1900 || login.Line != 9 {
		t.Fatalf("the failure %+v", login)
	}
	if shots["2-test-failed-1.png"] != "web/artifacts/login-b/test-failed-1.png" || len(shots) != 1 {
		t.Fatalf("shots %v (a path out of the run dir is never uploaded)", shots)
	}
	if tests[2].Screenshot != "" || tests[2].Repo != "o/mobile" || tests[0].Screenshot != "" || tests[3].Error != "" {
		t.Fatalf("others %+v", tests)
	}

	if _, err := ParseE2EReport([]byte(`{"schema": 2}`)); err == nil {
		t.Fatal("schema 2 accepted")
	}
	if _, err := ParseE2EReport([]byte(`not json`)); err == nil {
		t.Fatal("garbage accepted")
	}
}

func TestRegressionCommand(t *testing.T) {
	for _, tc := range []struct{ suite, mobile, want string }{
		{"web", "/w/R3-mobile", "scripts/e2e web --name 'loop-reg-3'"},
		{"mobile", "/w/R3-mobile", "scripts/e2e mobile --name 'loop-reg-3' --mobile-repo '/w/R3-mobile'"},
		{"all", "", "scripts/e2e all --name 'loop-reg-3'"},
	} {
		if got := RegressionCommand(tc.suite, "loop-reg-3", tc.mobile); got != tc.want {
			t.Errorf("%s: %q, want %q", tc.suite, got, tc.want)
		}
	}
}

func TestLatestE2EReport(t *testing.T) {
	dir := t.TempDir()
	if _, _, err := latestE2EReport(dir); err == nil {
		t.Fatal("no report found one")
	}
	for _, run := range []string{"20261018-090000-aaaaaa", "20261018-100000-bbbbbb"} {
		os.MkdirAll(filepath.Join(dir, ".e2e", run), 0o755)
		os.WriteFile(filepath.Join(dir, ".e2e", run, "report.json"), []byte(run), 0o644)
	}
	runDir, raw, err := latestE2EReport(dir)
	if err != nil || filepath.Base(runDir) != "20261018-100000-bbbbbb" || string(raw) != "20261018-100000-bbbbbb" {
		t.Fatalf("%s %s %v", runDir, raw, err)
	}
}

func TestRegressionConfig(t *testing.T) {
	two := strings.Replace(cfgBody, `}}}`, `},"jarvis-node-mobile":{"remote":"https://github.com/alexberardi/jarvis-node-mobile.git","slug":"alexberardi/jarvis-node-mobile"}}`, 1)
	ok := strings.Replace(two, `"name"`, `"stages":["build","regression"],"regression":{"repo":"jarvis-server","mobile_repo":"jarvis-node-mobile"},"name"`, 1)
	c, err := LoadConfig(writeCfg(t, ok+"}", 0o600))
	if err != nil {
		t.Fatal(err)
	}
	if c.Regression.Timeout() != DefaultRegressionTimeout {
		t.Fatalf("timeout %s", c.Regression.Timeout())
	}
	for name, body := range map[string]string{
		"stage without the section": strings.Replace(two, `"name"`, `"stages":["regression"],"name"`, 1),
		"unknown repo":              strings.Replace(two, `"name"`, `"stages":["regression"],"regression":{"repo":"server"},"name"`, 1),
		"unknown mobile repo":       strings.Replace(two, `"name"`, `"regression":{"repo":"jarvis-server","mobile_repo":"mobile"},"name"`, 1),
		"timeout past loopd's cap":  strings.Replace(two, `"name"`, `"regression":{"repo":"jarvis-server","timeout":"4h"},"name"`, 1),
	} {
		if _, err := LoadConfig(writeCfg(t, body+"}", 0o600)); err == nil || !strings.Contains(err.Error(), "regression") {
			t.Errorf("%s: %v", name, err)
		}
	}
}
