package pipeline

import (
	"go/ast"
	"go/parser"
	"go/token"
	"slices"
	"strconv"
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
		// The harness verify runs (jarvis-server's deny_write).
		{"M scripts/sandbox", "trusted-file"},
		{"M scripts/screenshots", "trusted-file"},
		{"M scripts/mobile-shot", "trusted-file"},
		{"M tools/pipeline/run.go", "trusted-file"},
		{"A tools/pipeline", "trusted-file"},
		{"M scripts/check", ""},
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
		{"M internal/pipeline/approve.go", "approval"},
		{"M internal/pipeline/trust.go", "approval"},
		{"M internal/api/approve.go", "approval"},
		{"M internal/store/approve.go", "approval"},
		{"M internal/pipeline/holdrules.go", "hold-rules"},
		{"M internal/store/control.go", "hold-rules"},
		{"M internal/runner/land.go", "land"},
		{"M internal/runner/land_gh.go", "land"},
		{"M internal/runner/pr.go", "land"},
		{"M internal/runner/git.go", "land"},
		{"M internal/api/auth.go", "api-auth"},
		{"M internal/api/onbehalf.go", "api-auth"},
		{"M internal/store/approver.go", "api-auth"},
		{"M cmd/loopd/main.go", "api-auth"},
		{"M internal/slack/approve.go", "slack-approver"},
		{"M internal/slack/approve_trust.go", "slack-approver"},
		{"M internal/slack/env.go", "tokens"},
		{"M internal/runner/config.go", "tokens"},
		{"M internal/runner/client_auth.go", "tokens"},
		{"M internal/guard/guard.go", "guard"},
		{"M cmd/loop-guard/main.go", "guard"},
		{"M internal/runner/settings.go", "guard"},
		{"M internal/runner/verify.go", "trusted-file"},
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
		"A scenarios/16-auto-land.json", "M internal/slack/text_test.go", "M docs/DESIGN.md",
		"M internal/pipeline/pipeline.go", "M internal/api/api.go", "M internal/store/store.go",
		"A internal/store/migrations/0021_x.sql", "M internal/runner/client.go", "A cmd/loopd/x.go"} {
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

// jarvis-node-setup and jarvis-command-sdk (item #549, docs/decisions/2026-10-23-node-setup-hold-rules.md):
// until this both repos matched no default rule at all, so a change to their auth, pairing, CI or
// release config would have landed on review and CI alone. node-setup's push to main publishes the
// ghcr.io/alexberardi/jarvis-node images, and the SDK is what every package and the node import.
func TestNodeSetupAndCommandSDKHoldRules(t *testing.T) {
	rules := DefaultHoldRules()
	cases := []struct{ repo, line, rule string }{
		// The paths item #549 names, each of which matched nothing before.
		{NodeSetupRepo, "M core/ijarvis_authentication.py", "auth"},
		{NodeSetupRepo, "M provisioning/pairing.py", "pairing"},
		{NodeSetupRepo, "M .github/workflows/ci.yml", "ci"},
		{NodeSetupRepo, "M .github/workflows/release.yml", "release-config"},
		{NodeSetupRepo, "M pyproject.toml", "release-config"},
		{NodeSetupRepo, "D tests/test_auth.py", "deleted-test"},
		{NodeSetupRepo, "R tests/test_pairing.py jarvis_node/pairing_check.py", "deleted-test"},
		// release and packaging: a merge to main pushes the images.
		{NodeSetupRepo, "M Dockerfile", "release-config"},
		{NodeSetupRepo, "M Dockerfile.audio", "release-config"},
		{NodeSetupRepo, "M docker-compose.audio.yaml", "release-config"},
		{NodeSetupRepo, "M compose/base.yaml", "release-config"},
		{NodeSetupRepo, "M requirements.txt", "release-config"},
		{NodeSetupRepo, "M requirements-pi.txt", "release-config"},
		{NodeSetupRepo, "M custom-requirements.txt", "release-config"},
		{NodeSetupRepo, "M .dockerignore", "release-config"},
		{NodeSetupRepo, "M build/build-tarball.sh", "release-config"},
		// A compose file or requirements list the repo does not have yet: a subdirectory or the
		// other YAML extension must not slip the release hold ("*" does not cross "/").
		{NodeSetupRepo, "M docker-compose.override.yml", "release-config"},
		{NodeSetupRepo, "M deploy/docker-compose.pi.yml", "release-config"},
		{NodeSetupRepo, "M requirements/base.txt", "release-config"},
		{NodeSetupRepo, "M deploy/requirements-extra.txt", "release-config"},
		// auth, secrets and the K1/K2 keys.
		{NodeSetupRepo, "M core/ijarvis_secret.py", "auth"},
		{NodeSetupRepo, "M services/command_auth_service.py", "auth"},
		{NodeSetupRepo, "M services/secret_service.py", "auth"},
		{NodeSetupRepo, "M repositories/secrets_repository.py", "auth"},
		{NodeSetupRepo, "M models/secret.py", "auth"},
		{NodeSetupRepo, "M models/command_auth.py", "auth"},
		{NodeSetupRepo, "M agents/token_refresh_agent.py", "auth"},
		{NodeSetupRepo, "M utils/set_secret.py", "auth"},
		{NodeSetupRepo, "M utils/encryption_utils.py", "auth"},
		{NodeSetupRepo, "M utils/authorize_node.py", "auth"},
		{NodeSetupRepo, "M utils/generate_dev_k2.py", "auth"},
		// provisioning and pairing.
		{NodeSetupRepo, "M provisioning/wifi_credentials.py", "pairing"},
		{NodeSetupRepo, "A provisioning/state_machine.py", "pairing"},
		{NodeSetupRepo, "M services/bluetooth_pair_agent.py", "pairing"},
		{NodeSetupRepo, "M services/camera_credentials_handler.py", "pairing"},
		{NodeSetupRepo, "M utils/mqtt_credentials.py", "pairing"},
		{NodeSetupRepo, "M scripts/run_provisioning.py", "pairing"},
		{NodeSetupRepo, "M scripts/setup_mode.py", "pairing"},
		// install, update and root: the sudoers drop-in, the systemd units, the self-updater.
		{NodeSetupRepo, "M install.sh", "install-scripts"},
		{NodeSetupRepo, "M setup.sh", "install-scripts"},
		{NodeSetupRepo, "M setup_macos.sh", "install-scripts"},
		{NodeSetupRepo, "M setup/pi.sh", "install-scripts"},
		{NodeSetupRepo, "M setup/systemd/jarvis-node.service", "install-scripts"},
		{NodeSetupRepo, "M scripts/jarvis-deploy.sudoers", "install-scripts"},
		{NodeSetupRepo, "M scripts/jarvis-self-update", "install-scripts"},
		{NodeSetupRepo, "M scripts/jarvis-post-install.sh", "install-scripts"},
		{NodeSetupRepo, "M scripts/jarvis-apt-hold", "install-scripts"},
		{NodeSetupRepo, "M services/post_install_dropins.py", "install-scripts"},
		{NodeSetupRepo, "M services/update_service.py", "install-scripts"},
		{NodeSetupRepo, "M services/package_install_handler.py", "install-scripts"},
		{NodeSetupRepo, "M scripts/install_command.py", "install-scripts"},
		{NodeSetupRepo, "M scripts/verify_package.py", "install-scripts"},
		// deleted tests, wherever node-setup keeps them (top level, services/, utils/).
		{NodeSetupRepo, "D test_node.py", "deleted-test"},
		{NodeSetupRepo, "D services/test_secret_service.py", "deleted-test"},
		{NodeSetupRepo, "D utils/test_command.py", "deleted-test"},
		{NodeSetupRepo, "D scripts/test_install_signature_verify.sh", "deleted-test"},
		{NodeSetupRepo, "M scripts/test_install_signature_verify.sh", ""},
		// Ordinary node code, a modified or added test, docs: nothing.
		{NodeSetupRepo, "M commands/timer_command.py", ""},
		{NodeSetupRepo, "M README.md", ""},
		{NodeSetupRepo, "M tests/test_auth.py", ""},
		{NodeSetupRepo, "A tests/test_new.py", ""},
		{NodeSetupRepo, "M agents/conversation_agent.py", ""},
		{NodeSetupRepo, "M docs/architecture.md", ""},
		// command-sdk: the auth and process helpers every package imports, its release config, CI.
		{CommandSDKRepo, "M jarvis_command_sdk/auth.py", "auth"},
		{CommandSDKRepo, "M jarvis_command_sdk/authentication.py", "auth"},
		{CommandSDKRepo, "M jarvis_command_sdk/secret.py", "auth"},
		{CommandSDKRepo, "M jarvis_command_sdk/cc_http.py", "auth"},
		{CommandSDKRepo, "M jarvis_command_sdk/process.py", "auth"},
		{CommandSDKRepo, "M pyproject.toml", "release-config"},
		{CommandSDKRepo, "M .github/workflows/ci.yml", "ci"},
		{CommandSDKRepo, "D tests/test_auth.py", "deleted-test"},
		{CommandSDKRepo, "R jarvis_command_sdk/test_x.py jarvis_command_sdk/x.py", "deleted-test"},
		{CommandSDKRepo, "M jarvis_command_sdk/logging.py", ""},
		{CommandSDKRepo, "M README.md", ""},
		{CommandSDKRepo, "M tests/test_auth.py", ""},
	}
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
	// Neither repo's rules leak into the other, into the packages or into jarvis-node-mobile:
	// NodeSetupRepo is an exact slug, never a jarvis-node-* wildcard.
	for _, c := range []struct{ repo, line string }{
		{"alexberardi/jarvis-node-mobile", "M install.sh"},
		{"alexberardi/jarvis-node-mobile", "M provisioning/api.py"},
		{"alexberardi/jarvis-cmd-weather", "M install.sh"},
		{"alexberardi/jarvis-cmd-weather", "M core/ijarvis_secret.py"},
		{NodeSetupRepo, "M jarvis_package.yaml"},
		{NodeSetupRepo, "M jarvis_command_sdk/auth.py"},
		{CommandSDKRepo, "M install.sh"},
		{CommandSDKRepo, "M provisioning/api.py"},
		{CommandSDKRepo, "M jarvis_package.yaml"},
		{ServerRepo, "M core/ijarvis_authentication.py"},
		// The SDK's slug is not under the jarvis-cmd-* wildcard, and must not be made so.
		{CommandSDKRepo, "M weather_scripts/install.sh"},
	} {
		paths, _ := ParseChangedPaths([]string{c.line})
		if m := MatchHoldRules(rules, c.repo, paths); len(m) != 0 {
			t.Errorf("%s %q matched %v, want nothing", c.repo, c.line, m)
		}
	}
}

// The guard that would have caught #549: every repo the default rules name in holdrules.go's
// const block (read from the source, so a const added with no rules fails here) has at least one
// rule that applies to it. A repo nobody named faces no hold rules at all.
func TestEveryNamedRepoHasHoldRules(t *testing.T) {
	repos := repoConstsInSource(t)
	if len(repos) < 8 {
		t.Fatalf("found only %d repo consts in holdrules.go: %v", len(repos), repos)
	}
	rules := DefaultHoldRules()
	for name, slug := range repos {
		// A wildcard repo stands for every slug with that prefix: test a representative one.
		item := slug
		if p, ok := strings.CutSuffix(slug, "*"); ok {
			item = p + "example"
		}
		n := 0
		for _, r := range rules {
			if ruleApplies(r.Repo, item) {
				n++
			}
		}
		t.Logf("%-16s %-40s %3d rules", name, item, n)
		if n == 0 {
			t.Errorf("%s (%s): no default hold rule applies to it", name, item)
		}
	}
}

// repoConstsInSource reads holdrules.go and returns its owner/name string consts by const name.
func repoConstsInSource(t *testing.T) map[string]string {
	t.Helper()
	f, err := parser.ParseFile(token.NewFileSet(), "holdrules.go", nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]string{}
	for _, d := range f.Decls {
		gd, ok := d.(*ast.GenDecl)
		if !ok || gd.Tok != token.CONST {
			continue
		}
		for _, s := range gd.Specs {
			vs, ok := s.(*ast.ValueSpec)
			if !ok {
				continue
			}
			for i, name := range vs.Names {
				if i >= len(vs.Values) {
					continue
				}
				lit, ok := vs.Values[i].(*ast.BasicLit)
				if !ok || lit.Kind != token.STRING {
					continue
				}
				v, err := strconv.Unquote(lit.Value)
				if err != nil || !strings.Contains(v, "/") {
					continue
				}
				out[name.Name] = v
			}
		}
	}
	return out
}

// Rules a store saved as a copy of an earlier built-in set (any control change used to write
// them out) are recognised, so loopd can go back to the live defaults.
func TestIsBuiltInHoldRuleSet(t *testing.T) {
	server := serverRules()
	phase6 := append(serverRules(), loopRulesV1()...)
	reversed := append([]HoldRule(nil), phase6...)
	for i, j := 0, len(reversed)-1; i < j; i, j = i+1, j-1 {
		reversed[i], reversed[j] = reversed[j], reversed[i]
	}
	// Before #204 jarvis-loop's list held all of internal/pipeline, internal/api, internal/store
	// and cmd/loopd: a database carrying that default list moves to the new one.
	before204 := defaultHoldRules(loopRulesV1(), true, false)
	phase6b := defaultHoldRules(loopRulesV1(), false, false)
	// Before #549 no rule named jarvis-node-setup or jarvis-command-sdk: the list loopd shipped
	// until then is still recognised, so a database carrying a copy of it moves to the new one.
	before549 := defaultHoldRules(loopRules(), true, false)
	for name, rules := range map[string][]HoldRule{"server only": server, "phase 6": phase6, "reordered": reversed,
		"phase 6b": phase6b, "before #204": before204, "before #549": before549, "today's": DefaultHoldRules()} {
		if !IsBuiltInHoldRuleSet(rules) {
			t.Errorf("%s: not recognised as a built-in set", name)
		}
	}
	for name, rules := range map[string][]HoldRule{"before #204": before204, "before #549": before549} {
		if sameRuleSet(rules, DefaultHoldRules()) {
			t.Fatalf("today's default list is still the one %s", name)
		}
	}
	changed := append([]HoldRule(nil), phase6...)
	changed[0].Glob = "elsewhere/**"
	for name, rules := range map[string][]HoldRule{"empty": nil, "one removed": phase6[1:], "one changed": changed,
		"one added":                append(append([]HoldRule(nil), phase6...), HoldRule{Name: "x", Glob: "x/**", Repo: LoopRepo}),
		"before #204, one removed": before204[1:], "before #549, one removed": before549[1:]} {
		if IsBuiltInHoldRuleSet(rules) {
			t.Errorf("%s: taken for a built-in set", name)
		}
	}
	// The frozen list is the one loopd shipped: these globs are what it held.
	var globs []string
	for _, r := range loopRulesV1() {
		globs = append(globs, r.Glob)
	}
	for _, g := range []string{"internal/pipeline/**", "internal/api/**", "internal/store/**", "cmd/loopd/**", "internal/runner/client.go"} {
		if !slices.Contains(globs, g) {
			t.Errorf("loopRulesV1 lacks %s", g)
		}
	}
}

// Narrowed in #204 (docs/decisions/2026-10-18-narrow-loop-holds.md): the loop's recent PRs, which
// all held on the broad internal/pipeline, internal/api, internal/store and cmd/loopd globs, land
// without a hold once review and CI pass. Each is the PR's full changed-path list.
func TestRecentLoopPRsAreNotHeld(t *testing.T) {
	for name, lines := range map[string][]string{
		"#193 deps at merge (PR #67)": {
			"A docs/decisions/2026-10-18-deps-at-merge.md",
			"M docs/phases/07-planning.md",
			"M docs/phases/08-orchestrator.md",
			"M internal/pipeline/claim.go",
			"M internal/pipeline/deps.go",
			"M internal/pipeline/deps_test.go",
			"M internal/pipeline/pipeline.go",
			"M internal/runner/testdata/brief_engineering.golden.md",
			"M internal/slack/bot.go",
			"M internal/slack/bot_test.go",
			"M internal/store/board.go",
			"M internal/store/deps.go",
			"M internal/store/deps_test.go",
			"M internal/store/orchsnap.go",
			"M internal/store/orchturn.go",
			"M internal/store/qa.go",
			"A internal/store/reopened.go",
			"M internal/web/board_test.go",
			"M internal/web/templates/board.tmpl",
			"M internal/web/testdata/board_closed.golden.html",
			"M internal/web/testdata/board_open.golden.html",
			"M internal/web/testdata/board_orchestrator.golden.html",
			"M internal/web/testdata/board_working.golden.html",
			"A scenarios/66-deps-at-merge.json",
			"M skills/engineering/SKILL.md",
		},
		"#201 flow panel (PR #71)": {
			"A docs/decisions/2026-10-18-flow-panel.md",
			"M docs/phases/02-loopd-core.md",
			"M internal/api/api.go",
			"A internal/api/flow_test.go",
			"M internal/pipeline/claim.go",
			"A internal/pipeline/flow.go",
			"A internal/pipeline/flow_test.go",
			"M internal/runner/client.go",
			"M internal/runner/runner.go",
			"M internal/runner/runner_test.go",
			"M internal/sandbox/check.go",
			"A internal/sandbox/flow.go",
			"M internal/sandbox/scenario.go",
			"A internal/store/flow.go",
			"A internal/store/flow_test.go",
			"A internal/store/migrations/0020_flow.sql",
			"M internal/store/store.go",
			"M internal/web/board.go",
			"A internal/web/flow.go",
			"A internal/web/flow_test.go",
			"M internal/web/templates/board.tmpl",
			"A internal/web/templates/flow.tmpl",
			"M internal/web/testdata/board_closed.golden.html",
			"M internal/web/testdata/board_open.golden.html",
			"M internal/web/testdata/board_orchestrator.golden.html",
			"M internal/web/testdata/board_working.golden.html",
			"A internal/web/testdata/flow.golden.html",
			"A scenarios/66-flow-panel.json",
		},
		"#202 loop CLI (PR #72)": {
			"M cmd/loop/main.go",
			"M cmd/loop/main_test.go",
			"M docs/DESIGN.md",
			"A docs/decisions/2026-10-18-item-corrections.md",
			"M docs/phases/04b-loop-harness.md",
			"M internal/api/api.go",
			"A internal/pipeline/edit.go",
			"A internal/pipeline/edit_test.go",
			"M internal/sandbox/scenario.go",
			"M internal/slack/bot.go",
			"A internal/slack/corrections_test.go",
			"A internal/store/edit.go",
			"A internal/store/edit_test.go",
			"M internal/store/orchestrator.go",
			"M internal/store/store.go",
			"A scenarios/66-resources-fix.json",
		},
		"#192 slot hold for Mac work (PR #63)": {
			"M cmd/loop-runner/main.go",
			"M docs/DESIGN.md",
			"M docs/decisions/2026-10-17-mac-runners.md",
			"A docs/decisions/2026-10-18-slot-hold.md",
			"M docs/runbooks/mac-runner.md",
			"M internal/api/api.go",
			"M internal/api/runners_test.go",
			"M internal/pipeline/claim.go",
			"M internal/pipeline/claim_test.go",
			"M internal/pipeline/freeze_test.go",
			"M internal/pipeline/orchturn_test.go",
			"M internal/pipeline/plans_test.go",
			"A internal/pipeline/slothold.go",
			"M internal/pipeline/unservable.go",
			"M internal/runner/client.go",
			"M internal/sandbox/scenario.go",
			"M internal/sandbox/stack.go",
			"A internal/store/migrations/0020_runner_only.sql",
			"A internal/store/slothold_test.go",
			"M internal/store/store.go",
			"M internal/store/unservable.go",
			"M internal/web/board.go",
			"M internal/web/board_test.go",
			"M internal/web/templates/board.tmpl",
			"A internal/web/testdata/board_runners.golden.html",
			"A scenarios/66-slot-held-for-mac.json",
			"A scenarios/67-slot-not-held-outside-mac-stages.json",
		},
	} {
		paths, err := ParseChangedPaths(lines)
		if err != nil {
			t.Fatal(err)
		}
		if m := MatchHoldRules(DefaultHoldRules(), LoopRepo, paths); len(m) != 0 {
			t.Errorf("%s is held: %v", name, m)
		}
	}
}

// A change to who may approve or merge, API auth, tokens, the guard, the gates or the hold rules
// still holds, among ordinary files.
func TestLoopApprovalAuthTokenGuardChangesStillHold(t *testing.T) {
	ordinary := []string{"M internal/pipeline/pipeline.go", "M internal/api/api.go", "M internal/store/store.go", "M docs/DESIGN.md"}
	for _, c := range []struct{ line, rule string }{
		{"M internal/pipeline/approve.go", "approval"},
		{"M internal/pipeline/holdrules.go", "hold-rules"},
		{"M internal/api/auth.go", "api-auth"},
		{"M internal/store/control.go", "hold-rules"},
		{"M internal/slack/approve.go", "slack-approver"},
		{"M internal/runner/client_auth.go", "tokens"},
		{"M internal/guard/guard.go", "guard"},
		{"M internal/runner/land.go", "land"},
		{"M internal/runner/verify.go", "trusted-file"},
		{"M .github/workflows/ci.yml", "ci"},
		{"D internal/store/store_test.go", "deleted-test"},
		{"R scenarios/x.json scenarios/y.json", "scenario-removed"},
	} {
		paths, err := ParseChangedPaths(append([]string{c.line}, ordinary...))
		if err != nil {
			t.Fatal(err)
		}
		m := MatchHoldRules(DefaultHoldRules(), LoopRepo, paths)
		if len(m) != 1 || m[0].Rule != c.rule {
			t.Errorf("%q among ordinary files matched %v, want only rule %q", c.line, m, c.rule)
		}
	}
}
