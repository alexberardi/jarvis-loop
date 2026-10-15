package pipeline

import (
	"strings"
	"testing"
)

func TestGlobMatch(t *testing.T) {
	cases := []struct {
		glob, path string
		want       bool
	}{
		{"internal/modules/auth/**", "internal/modules/auth/x.go", true},
		{"internal/modules/auth/**", "internal/modules/auth/sub/dir/x.go", true},
		{"internal/modules/auth/**", "internal/modules/auth", true}, // ** matches zero segments
		{"internal/modules/auth/**", "internal/modules/authz/x.go", false},
		{"internal/modules/auth/**", "internal/modules/x.go", false},
		{"cmd/jarvisd/upgrade*.go", "cmd/jarvisd/upgrade.go", true},
		{"cmd/jarvisd/upgrade*.go", "cmd/jarvisd/upgrade_linux.go", true},
		{"cmd/jarvisd/upgrade*.go", "cmd/jarvisd/main.go", false},
		{"cmd/jarvisd/upgrade*.go", "cmd/jarvisd/upgrade/x.go", false}, // * does not cross /
		{"**/*_test.go", "x_test.go", true},
		{"**/*_test.go", "a/b/c/x_test.go", true},
		{"**/*_test.go", "a/b/x.go", false},
		{"**/*_test.go", "a/b/x_test.go.orig", false},
		{".github/workflows/release.yml", ".github/workflows/release.yml", true},
		{".github/workflows/release.yml", ".github/workflows/release.yaml", false},
		{"contract/**", "contract/a/b_test.go", true},
		{"a/**/z", "a/z", true},
		{"a/**/z", "a/b/c/z", true},
		{"a/**/z", "a/b/c/y", false},
		{"*", "x", true},
		{"*", "a/x", false},
		{"**", "a/b/c", true},
		{"a*b*c", "abc", true},
		{"a*b*c", "axxbyyc", true},
		{"a*b*c", "axxbyy", false},
	}
	for _, c := range cases {
		if got := GlobMatch(c.glob, c.path); got != c.want {
			t.Errorf("GlobMatch(%q, %q) = %v, want %v", c.glob, c.path, got, c.want)
		}
	}
}

func TestParseChangedPaths(t *testing.T) {
	good := []struct {
		in   string
		want ChangedPath
	}{
		{"M internal/x.go", ChangedPath{Status: "M", Path: "internal/x.go"}},
		{"A\tnew.go", ChangedPath{Status: "A", Path: "new.go"}},
		{"D  foo_test.go", ChangedPath{Status: "D", Path: "foo_test.go"}},
		{"R100\tcontract/a.go\tcontract/b.go", ChangedPath{Status: "R", OldPath: "contract/a.go", Path: "contract/b.go"}},
		{"R a.go b.go", ChangedPath{Status: "R", OldPath: "a.go", Path: "b.go"}},
		{"modified x.go", ChangedPath{Status: "M", Path: "x.go"}},
		{"deleted x.go", ChangedPath{Status: "D", Path: "x.go"}},
		{"renamed a b", ChangedPath{Status: "R", OldPath: "a", Path: "b"}},
		{"added ./x.go", ChangedPath{Status: "A", Path: "x.go"}},
	}
	for _, g := range good {
		got, err := ParseChangedPaths([]string{g.in})
		if err != nil || len(got) != 1 || got[0] != g.want {
			t.Errorf("ParseChangedPaths(%q) = %+v, %v; want %+v", g.in, got, err, g.want)
		}
	}
	for _, bad := range [][]string{
		nil,
		{},
		{"  "},
		{"X foo.go"},
		{"M"},
		{"M a b"},
		{"R onlyone"},
		{"M /etc/passwd"},
		{"M ../outside.go"},
		{"M a/../../b"},
	} {
		if _, err := ParseChangedPaths(bad); err == nil {
			t.Errorf("ParseChangedPaths(%q) accepted", bad)
		} else {
			wantCode(t, err, CodeInvalidField)
		}
	}
}

func TestDefaultHoldRules(t *testing.T) {
	rules := DefaultHoldRules()
	cases := []struct {
		line string
		want string // the rule name that matches, "" for none
	}{
		{"M internal/modules/auth/handlers.go", "auth"},
		{"A internal/platform/authn/jwt.go", "authn"},
		{"M internal/update/helper.go", "update"},
		{"M cmd/jarvisd/upgrade_darwin.go", "upgrade"},
		{"M .github/workflows/release.yml", "release"},
		{"M .github/workflows/ci.yml", "ci"},
		{"D internal/modules/cc/turn_test.go", "deleted-test"},
		{"R internal/x_test.go internal/x.go", "deleted-test"},
		{"M internal/modules/cc/turn_test.go", ""},
		{"A internal/modules/cc/new_test.go", ""},
		{"D contract/auth/login.go", "contract-removed"},
		{"R contract/a.go elsewhere/a.go", "contract-removed"},
		{"M contract/auth/login.go", ""},
		{"M internal/modules/cc/turn.go", ""},
		{"M internal/modules/authz/x.go", ""},
		{"M cmd/jarvisd/main.go", ""},
		{"M docs/STATUS.md", ""},
	}
	for _, c := range cases {
		paths, err := ParseChangedPaths([]string{c.line})
		if err != nil {
			t.Fatal(err)
		}
		hits := MatchHoldRules(rules, ServerRepo, paths)
		if c.want == "" {
			if len(hits) != 0 {
				t.Errorf("%q matched %v, want nothing", c.line, hits)
			}
			continue
		}
		found := false
		for _, h := range hits {
			found = found || h.Rule == c.want
		}
		if !found {
			t.Errorf("%q matched %v, want rule %q", c.line, hits, c.want)
		}
	}
	for _, r := range rules {
		if err := r.Validate(); err != nil {
			t.Errorf("default rule %q invalid: %v", r.Name, err)
		}
	}
}

func TestHoldRuleValidate(t *testing.T) {
	for _, r := range []HoldRule{
		{Name: "", Glob: "a/**"},
		{Name: "x", Glob: ""},
		{Name: "x", Glob: "/abs/**"},
		{Name: "x", Glob: "a/../b"},
		{Name: "x", Glob: "a/**", Statuses: []string{"Z"}},
	} {
		wantCode(t, r.Validate(), CodeInvalidField)
	}
	for _, repo := range []string{"jarvis-loop", "a/b/c", "/x", "x/", "a b/c"} {
		wantCode(t, HoldRule{Name: "x", Glob: "a/**", Repo: repo}.Validate(), CodeInvalidField)
	}
	wantCode(t, HoldRule{Name: "x", Glob: "a/**", Repo: "alexberardi/jarvis-loop"}.Validate(), "")
	c := DefaultControl()
	c.HoldRules = append(c.HoldRules, c.HoldRules[0])
	if err := c.Validate(); err == nil || !strings.Contains(err.Error(), "duplicate") {
		t.Fatalf("duplicate rule names: %v", err)
	}
	// The same name in two repos is two rules (both repos have a "ci" rule).
	c = DefaultControl()
	c.HoldRules = []HoldRule{{Name: "ci", Glob: "a/**", Repo: "o/one"}, {Name: "ci", Glob: "a/**", Repo: "o/two"}, {Name: "ci", Glob: "a/**"}}
	wantCode(t, c.Validate(), "")
}

// A rule with a repo applies only to that repo's items; a rule without one to every repo. An item
// whose repo is not known yet gets every rule (holding more is the safe direction), and an item
// whose repo is a bare name (a runner config key) matches the rule's repo by name.
func TestPerRepoHoldRules(t *testing.T) {
	rules := []HoldRule{
		{Name: "server-auth", Glob: "auth/**", Repo: "o/server"},
		{Name: "loop-auth", Glob: "auth/**", Repo: "o/loop"},
		{Name: "everywhere", Glob: "ci/**"},
	}
	paths, _ := ParseChangedPaths([]string{"M auth/x.go", "M ci/y.yml"})
	names := func(repo string) []string {
		var out []string
		for _, m := range MatchHoldRules(rules, repo, paths) {
			out = append(out, m.Rule)
		}
		return out
	}
	cases := map[string]string{
		"o/server": "server-auth everywhere",
		"o/loop":   "loop-auth everywhere",
		"o/other":  "everywhere",
		"server":   "server-auth everywhere",
		"x/server": "everywhere",
		"":         "server-auth loop-auth everywhere",
	}
	for repo, want := range cases {
		if got := strings.Join(names(repo), " "); got != want {
			t.Errorf("repo %q: rules %q, want %q", repo, got, want)
		}
	}
}

// jarvis-loop's own rules (docs/phases/06-land.md): who may approve or merge, token handling and
// the landing gates are held, so the land script never merges a change to itself unattended.
func TestJarvisLoopHoldRules(t *testing.T) {
	rules := DefaultHoldRules()
	held := func(repo string, lines ...string) []HoldMatch {
		t.Helper()
		paths, err := ParseChangedPaths(lines)
		if err != nil {
			t.Fatal(err)
		}
		return MatchHoldRules(rules, repo, paths)
	}
	// Item #2's actual paths (the first real run, 2026-10-15) are not held.
	if m := held(LoopRepo, "M cmd/loop/main.go", "M internal/slack/bot.go", "M internal/slack/commands.go",
		"A scenarios/16-show-repo.json", "M scenarios/01-happy-path.json"); len(m) != 0 {
		t.Fatalf("item #2's paths are held: %v", m)
	}
	cases := []struct{ line, rule string }{
		{"M internal/pipeline/pipeline.go", "approval"},
		{"A internal/pipeline/approve.go", "approval"},
		{"M internal/runner/land.go", "land"},
		{"M internal/runner/land_gh.go", "land"},
		{"M internal/runner/pr.go", "land"},
		{"M internal/runner/git.go", "land"},
		{"M internal/api/auth.go", "api-auth"},
		{"M internal/slack/approve.go", "slack-approver"},
		{"M internal/slack/env.go", "tokens"},
		{"M internal/runner/config.go", "tokens"},
		{"M internal/runner/client.go", "tokens"},
		{"M internal/guard/guard.go", "guard"},
		{"M cmd/loop-guard/main.go", "guard"},
		{"M internal/runner/settings.go", "guard"},
		{"M .github/workflows/ci.yml", "ci"},
		{"D internal/slack/text_test.go", "deleted-test"},
		{"R internal/x_test.go internal/x.go", "deleted-test"},
		{"D scenarios/04-someone-else.json", "scenario-removed"},
		{"R scenarios/04-someone-else.json scenarios/x.json", "scenario-removed"},
	}
	for _, c := range cases {
		found := false
		m := held(LoopRepo, c.line)
		for _, h := range m {
			found = found || h.Rule == c.rule
		}
		if !found {
			t.Errorf("%q in jarvis-loop matched %v, want rule %q", c.line, m, c.rule)
		}
	}
	for _, line := range []string{"M internal/slack/bot.go", "M internal/runner/runner.go", "M internal/runner/stages.go",
		"A scenarios/16-auto-land.json", "M internal/slack/text_test.go", "M docs/DESIGN.md"} {
		if m := held(LoopRepo, line); len(m) != 0 {
			t.Errorf("%q in jarvis-loop is held: %v", line, m)
		}
	}
	// The repos do not share rules: jarvis-server's auth path is nothing to jarvis-loop, and
	// jarvis-loop's pipeline is nothing to jarvis-server.
	if m := held(LoopRepo, "M internal/modules/auth/x.go"); len(m) != 0 {
		t.Errorf("a jarvis-server rule applied to jarvis-loop: %v", m)
	}
	if m := held(ServerRepo, "M internal/pipeline/pipeline.go"); len(m) != 0 {
		t.Errorf("a jarvis-loop rule applied to jarvis-server: %v", m)
	}
	for _, r := range rules {
		if r.Repo != ServerRepo && r.Repo != LoopRepo {
			t.Errorf("default rule %q has repo %q; every default names its repo", r.Name, r.Repo)
		}
	}
}
