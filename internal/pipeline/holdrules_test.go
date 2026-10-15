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
		{"M internal/store/store.go", "store"},
		{"A internal/store/migrations/0005_x.sql", "store"},
		{"M cmd/loopd/main.go", "loopd-wiring"},
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
		if r.Repo == "" {
			t.Errorf("default rule %q has no repo; every default names its repo", r.Name)
		}
	}
}

// A rule's repo may end in "*" (docs/phases/06b-all-repos.md): it applies to every repo whose
// slug starts with the rest, and to a bare name whose name part does.
func TestHoldRuleRepoWildcard(t *testing.T) {
	cases := []struct {
		rule, item string
		want       bool
	}{
		{"alexberardi/jarvis-cmd-*", "alexberardi/jarvis-cmd-weather", true},
		{"alexberardi/jarvis-cmd-*", "alexberardi/jarvis-cmd-", true},
		{"alexberardi/jarvis-cmd-*", "alexberardi/jarvis-device-nest", false},
		{"alexberardi/jarvis-cmd-*", "someone/jarvis-cmd-weather", false},
		{"alexberardi/jarvis-cmd-*", "alexberardi/jarvis-server", false},
		{"alexberardi/jarvis-cmd-*", "jarvis-cmd-weather", true}, // a runner config key
		{"alexberardi/jarvis-cmd-*", "jarvis-server", false},
		{"alexberardi/jarvis-cmd-*", "", true}, // unknown repo: every rule
		{"alexberardi/*", "alexberardi/anything", true},
		{"alexberardi/*", "other/anything", false},
		{"alexberardi/jarvis-server", "alexberardi/jarvis-server-2", false}, // no star, exact
	}
	for _, c := range cases {
		if got := ruleApplies(c.rule, c.item); got != c.want {
			t.Errorf("rule repo %q, item %q: %v, want %v", c.rule, c.item, got, c.want)
		}
	}
	for _, repo := range []string{"alexberardi/jarvis-cmd-*", "alexberardi/*", "a/b*"} {
		wantCode(t, HoldRule{Name: "x", Glob: "a/**", Repo: repo}.Validate(), "")
	}
	for _, repo := range []string{"*/jarvis", "alexberardi/*cmd", "alexberardi/jar*vis", "alexberardi/x**", "*", "alexberardi*/x"} {
		wantCode(t, HoldRule{Name: "x", Glob: "a/**", Repo: repo}.Validate(), CodeInvalidField)
	}
}

// The new repos' hold lists (docs/phases/06b-all-repos.md, "Hold rules for the new repos"), as a
// table: repo, changed path, the rule that must hold it ("" for none).
func TestAllReposHoldRules(t *testing.T) {
	const (
		node    = "alexberardi/jarvis-node-mobile"
		recipes = "alexberardi/jarvis-recipes-mobile"
		docs    = "alexberardi/jarvis-docs"
		inst    = "alexberardi/jarvis-installer"
		cmd     = "alexberardi/jarvis-cmd-weather"
		device  = "alexberardi/jarvis-device-nest"
	)
	cases := []struct{ repo, line, rule string }{
		// mobile, both apps
		{node, "M eas.json", "release-config"},
		{recipes, "M app.json", "release-config"},
		{node, "M app.config.ts", "release-config"},
		{node, "M .github/workflows/build.yml", "ci"},
		{node, "A plugins/withFoo.js", "native"},
		{node, "M targets/widget/Info.plist", "native"},
		{node, "M native/x.kt", "native"},
		{recipes, "M ios/Podfile", "native"},
		{node, "M android/app/build.gradle", "native"},
		{node, "M scripts/store/submit.sh", "store-scripts"},
		{node, "M modules/jarvis-crypto/src/index.ts", "crypto"},
		{node, "M src/auth/login.ts", "auth"},
		{node, "M src/services/tokenStorage.ts", "auth"},
		{node, "M src/services/clearUserData.ts", "auth"},
		{recipes, "M src/services/oauthService.ts", "auth"},
		{node, "M src/services/qrPairingService.ts", "pairing"},
		{node, "M src/services/nodePresenceService.ts", "presence"},
		{node, "M src/config/env.ts", "config"},
		{node, "M jest.config.js", "test-config"},
		{node, "D src/screens/__tests__/Home.test.tsx", "deleted-test"},
		{node, "R src/a.test.ts src/a.ts", "deleted-test"},
		{recipes, "D src/x.test.tsx", "deleted-test"},
		{node, "M src/screens/__tests__/Home.test.tsx", ""},
		{node, "A src/x.test.ts", ""},
		{node, "M src/screens/HomeScreen.tsx", ""},
		{node, "M src/services/chatService.ts", ""},
		{node, "M README.md", ""},
		// docs
		{docs, "M .well-known/security.txt", "well-known"},
		{docs, "M _headers", "hosting"},
		{docs, "M _redirects", "hosting"},
		{docs, "M app-fallback/index.html", "hosting"},
		{docs, "M deploy.sh", "deploy"},
		{docs, "M mkdocs.yml", "site-config"},
		{docs, "M docs/security/index.md", "security-docs"},
		{docs, "A internal-ops/runbook.md", "internal-ops"},
		{docs, "M .github/workflows/ci.yml", "ci"},
		{docs, "D tests/test_links.py", "deleted-test"},
		{docs, "D tools/test_build.py", "deleted-test"},
		{docs, "M tests/test_links.py", ""},
		{docs, "M docs/getting-started.md", ""},
		// installer
		{inst, "M src/lib/secret-generator.ts", "secrets"},
		{inst, "M src/lib/compose-generator.ts", "secrets"},
		{inst, "M src/data/image-digests.json", "image-digests"},
		{inst, "M src/lib/image-digests.ts", "image-digests"},
		{inst, "M public/service-registry.json", "service-registry"},
		{inst, "M public/CNAME", "domain"},
		{inst, "M scripts/release.sh", "scripts"},
		{inst, "M .github/workflows/deploy.yml", "ci"},
		{inst, "D src/lib/__tests__/x.test.ts", "deleted-test"},
		{inst, "M src/components/Wizard.tsx", ""},
		{inst, "M src/lib/format.ts", ""},
		// packages, both prefixes
		{cmd, "M jarvis_package.yaml", "package-manifest"},
		{device, "M jarvis_package.yaml", "package-manifest"},
		{cmd, "M weather_scripts/install.sh", "package-scripts"},
		{device, "M nest_shared/oauth.py", "package-shared"},
		{device, "M .github/workflows/ci.yml", "ci"},
		{cmd, "D tests/test_weather.py", "deleted-test"},
		{device, "R nest/test_auth.py nest/auth_check.py", "deleted-test"},
		{cmd, "M commands/weather.py", ""},
		{cmd, "M tests/test_weather.py", ""},
		{device, "A tests/test_new.py", ""},
	}
	rules := DefaultHoldRules()
	for _, c := range cases {
		paths, err := ParseChangedPaths([]string{c.line})
		if err != nil {
			t.Fatal(err)
		}
		hits := MatchHoldRules(rules, c.repo, paths)
		if c.rule == "" {
			if len(hits) != 0 {
				t.Errorf("%s %q matched %v, want nothing", c.repo, c.line, hits)
			}
			continue
		}
		found := false
		for _, h := range hits {
			found = found || h.Rule == c.rule
		}
		if !found {
			t.Errorf("%s %q matched %v, want rule %q", c.repo, c.line, hits, c.rule)
		}
	}
	// No group's rules leak into another repo: a mobile path is nothing to jarvis-server, a
	// package manifest nothing to the installer, jarvis-server's auth nothing to a package.
	for _, c := range []struct{ repo, line string }{
		{ServerRepo, "M src/auth/login.ts"},
		{ServerRepo, "M jarvis_package.yaml"},
		{inst, "M jarvis_package.yaml"},
		{cmd, "M internal/modules/auth/x.go"},
		{cmd, "M src/auth/login.ts"},
		{"alexberardi/jarvis-cmdline", "M jarvis_package.yaml"},
	} {
		paths, _ := ParseChangedPaths([]string{c.line})
		if m := MatchHoldRules(rules, c.repo, paths); len(m) != 0 {
			t.Errorf("%s %q matched %v, want nothing", c.repo, c.line, m)
		}
	}
	for _, r := range rules {
		if err := r.Validate(); err != nil {
			t.Errorf("default rule %q invalid: %v", r.Name, err)
		}
		if r.Repo == "" {
			t.Errorf("default rule %q names no repo", r.Name)
		}
	}
	if err := (Control{ActiveAgents: 1, MaxRunMinutes: 45, Limits: DefaultControl().Limits, HoldRules: rules}).Validate(); err != nil {
		t.Errorf("the defaults do not validate as a control: %v", err)
	}
}

// Rules a store saved as a copy of an earlier built-in set (any control change used to write
// them out) are recognised, so loopd can go back to the live defaults.
func TestIsBuiltInHoldRuleSet(t *testing.T) {
	var server, phase6 []HoldRule
	for _, r := range DefaultHoldRules() {
		if r.Repo == ServerRepo {
			server = append(server, r)
		}
		if r.Repo == ServerRepo || r.Repo == LoopRepo {
			phase6 = append(phase6, r)
		}
	}
	reversed := append([]HoldRule(nil), phase6...)
	for i, j := 0, len(reversed)-1; i < j; i, j = i+1, j-1 {
		reversed[i], reversed[j] = reversed[j], reversed[i]
	}
	for name, rules := range map[string][]HoldRule{"server only": server, "phase 6": phase6, "reordered": reversed, "today's": DefaultHoldRules()} {
		if !IsBuiltInHoldRuleSet(rules) {
			t.Errorf("%s: not recognised as a built-in set", name)
		}
	}
	changed := append([]HoldRule(nil), phase6...)
	changed[0].Glob = "elsewhere/**"
	for name, rules := range map[string][]HoldRule{"empty": nil, "one removed": phase6[1:], "one changed": changed,
		"one added": append(append([]HoldRule(nil), phase6...), HoldRule{Name: "x", Glob: "x/**", Repo: LoopRepo})} {
		if IsBuiltInHoldRuleSet(rules) {
			t.Errorf("%s: taken for a built-in set", name)
		}
	}
}
