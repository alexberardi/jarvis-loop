package sandbox

import (
	"fmt"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"strings"
	"time"

	"github.com/alexberardi/jarvis-loop/internal/slack"
)

// Regression runs in the sandbox (docs/decisions/2026-10-18-regression-trigger.md): the demo repo
// stands in for jarvis-server, and a scenario's "e2e" step gives it a scripts/e2e that writes a
// canned report as the real one does (.e2e/<run>/report.json, report.md and the failure's
// screenshot), so the runner's whole path runs: checkout, the fence, the report, the uploads.

// E2EOutcomes are the stand-in's plays: fail (one pass, one failure with its screenshot), pass
// (two passes), suite_error (two passes, but a second suite did not run), no_report (it exits 2
// before writing one).
var E2EOutcomes = []string{"fail", "pass", "suite_error", "no_report"}

// e2eRun is the stand-in's scripts/e2e run id.
const e2eRun = "20261015-130000-sbx001"

func e2eScript(outcome string) (string, error) {
	var tests, verdict, exit string
	switch outcome {
	case "fail":
		verdict, exit = "failed", "1"
		tests = `[
    {"suite": "web", "id": "inbox-1", "title": "inbox.spec.ts › deleting from the detail page removes the item", "file": "tests/inbox.spec.ts",
     "line": 32, "status": "passed", "duration_ms": 600, "error": null, "screenshots": [], "trace": null, "console_errors": []},
    {"suite": "web", "id": "login-2", "title": "login.spec.ts › a wrong password is refused", "file": "tests/login.spec.ts", "line": 9,
     "status": "failed", "duration_ms": 1900, "error": "Error: expect(locator).toHaveText(expected)\nExpected: \"Wrong password\"\nReceived: \"Welcome back\"",
     "screenshots": ["web/artifacts/login-2/test-failed-1.png"], "trace": "web/artifacts/login-2/trace.zip", "console_errors": []}
  ]`
	case "pass", "suite_error":
		verdict, exit = "passed", "0"
		tests = `[
    {"suite": "web", "id": "inbox-1", "title": "inbox.spec.ts › deleting from the detail page removes the item", "file": "tests/inbox.spec.ts",
     "line": 32, "status": "passed", "duration_ms": 600, "error": null, "screenshots": [], "trace": null, "console_errors": []},
    {"suite": "web", "id": "login-2", "title": "login.spec.ts › a wrong password is refused", "file": "tests/login.spec.ts", "line": 9,
     "status": "passed", "duration_ms": 700, "error": null, "screenshots": [], "trace": null, "console_errors": []}
  ]`
	case "no_report":
		return "#!/usr/bin/env bash\n# The sandbox's stand-in for jarvis-server's scripts/e2e: the sandbox does not come up.\n" +
			"echo \"e2e: args: $*\"\necho 'e2e: the sandbox did not come up (scripts/sandbox up exited 1)' >&2\nexit 2\n", nil
	default:
		return "", fmt.Errorf("e2e outcome %q: use %s", outcome, strings.Join(E2EOutcomes, ", "))
	}
	suites := `"web": {"status": "` + verdict + `", "exit_code": ` + exit + `, "error": null, "duration_ms": 2500}`
	if outcome == "suite_error" {
		verdict, exit = "error", "1"
		suites += `, "mobile": {"status": "error", "exit_code": null, "error": "the emulator did not boot (adb wait-for-device timed out)", "duration_ms": 0}`
	}
	return `#!/usr/bin/env bash
# The sandbox's stand-in for jarvis-server's scripts/e2e: a canned report.json (schema 1) and
# report.md under .e2e/<run>/, as scripts/lib/e2e_report.py writes them.
set -euo pipefail
echo "e2e: args: $*"
out=.e2e/` + e2eRun + `
mkdir -p "$out/web/artifacts/login-2"
printf '\x89PNG\r\n\x1a\nthe sandbox login page' > "$out/web/artifacts/login-2/test-failed-1.png"
cat > "$out/report.json" <<'JSON'
{
  "schema": 1, "run": "` + e2eRun + `", "status": "` + verdict + `",
  "started_at": "2026-10-15T09:00:00-04:00", "finished_at": "2026-10-15T09:02:00-04:00",
  "sandbox": "loop-reg", "filter": null, "git": {"sha": "", "branch": "HEAD", "dirty": false},
  "suites": {` + suites + `},
  "tests": ` + tests + `
}
JSON
printf '# E2E run %s: %s\n\nargs: %s\n' "` + e2eRun + `" "` + strings.ToUpper(verdict) + `" "$*" > "$out/report.md"
echo "e2e: ` + verdict + `" >&2
exit ` + exit + `
`, nil
}

// InstallE2E commits the stand-in scripts/e2e (outcome: one of E2EOutcomes) to the demo repo's main.
func (s *Stack) InstallE2E(outcome string) error {
	script, err := e2eScript(outcome)
	if err != nil {
		return err
	}
	_, err = s.GitHub.PushFile(RepoSlug, "main", "scripts/e2e", script, "scripts/e2e: the sandbox's stand-in ("+outcome+")", 0o755)
	return err
}

// RegressionSlack is user's top-level mention of the loop bot in #loop.
func (s *Stack) RegressionSlack(user, text string) error {
	_, err := s.Slack.Mention(user, slack.ChannelLoop, "", text)
	return err
}

// RegressionBoard presses the board's "Run regression" button with suite, signed in with the
// user's token (tok "user") or the board's ("board"), and returns the HTTP status.
func (s *Stack) RegressionBoard(tok, suite string) (int, error) {
	t := s.boardTok
	if tok == "user" {
		t = s.user.token
	}
	jar, _ := cookiejar.New(nil)
	c := &http.Client{Jar: jar, Timeout: 10 * time.Second}
	if _, _, err := boardGet(c, s.BoardURL+"/board?t="+t); err != nil {
		return 0, err
	}
	c.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	resp, err := c.PostForm(s.BoardURL+"/board/regressions", url.Values{"suite": {suite}})
	if err != nil {
		return 0, err
	}
	resp.Body.Close()
	return resp.StatusCode, nil
}

// RegressionPage is a run's report page on the board (board token).
func (s *Stack) RegressionPage(id int64) (string, error) {
	_, c, err := s.boardSession()
	if err != nil {
		return "", err
	}
	page, _, err := boardGet(c, fmt.Sprintf("%s/board/regressions/%d", s.BoardURL, id))
	return page, err
}
