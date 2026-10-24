package pipeline

import (
	"fmt"
	"path"
	"regexp"
	"slices"
	"strings"
)

// HoldRule puts a PR on the hold list when one of its changed paths matches Glob (and, when
// Statuses is set, has one of those statuses). loopd computes hold risk from these rules; it does
// not trust an agent's label (docs/decisions/2026-10-14-hold-rules.md). A rule with a Repo
// (owner/name) applies only to that repo's items; one without, to every repo
// (docs/decisions/2026-10-15-land.md).
type HoldRule struct {
	Name     string   `json:"name"`
	Glob     string   `json:"glob"`
	Statuses []string `json:"statuses,omitempty"` // A, M, D, R; empty = any
	Repo     string   `json:"repo,omitempty"`     // owner/name; empty = every repo
}

// The repos the default rules are for. A rule's repo may end in "*" (every repo whose slug
// starts with the rest): the 29 command and device packages share their rules that way.
const (
	ServerRepo     = "alexberardi/jarvis-server"
	LoopRepo       = "alexberardi/jarvis-loop"
	NodeMobileRepo = "alexberardi/jarvis-node-mobile"
	RecipesRepo    = "alexberardi/jarvis-recipes-mobile"
	DocsRepo       = "alexberardi/jarvis-docs"
	InstallerRepo  = "alexberardi/jarvis-installer"
	NodeSetupRepo  = "alexberardi/jarvis-node-setup"
	CommandSDKRepo = "alexberardi/jarvis-command-sdk"
	CmdPackages    = "alexberardi/jarvis-cmd-*"
	DevicePackages = "alexberardi/jarvis-device-*"
)

// DefaultHoldRules are each repo's CLAUDE.md hold list as path rules. jarvis-server: auth and
// access, signing and the update helper, CI checks, and weakened tests or contract checks.
// jarvis-loop: who may approve or merge (the outcome-to-stage table, review approval, the merge
// gate and the user's approvals, in their own files), loopd's API auth and acting for the user,
// the Slack approver check, the hold rules and their persistence, token handling, the landing
// gates, the agents' guard and sandbox, CI, and weakened tests or scenarios; the rest of
// internal/pipeline, internal/api, internal/store and cmd/loopd lands on review and CI
// (docs/decisions/2026-10-18-narrow-loop-holds.md). The mobile apps, the docs, the installer and the packages: release and store config,
// native code, auth and pairing, deploy and hosting, secret generation, package manifests and
// install scripts, CI, and deleted tests (docs/phases/06b-all-repos.md). jarvis-node-setup (a push
// to main publishes the node images): its release and image config, auth and secrets, provisioning
// and pairing, the install and update scripts that run as root, CI and deleted tests;
// jarvis-command-sdk (every package imports it): its auth helpers, release config, CI and deleted
// tests (docs/decisions/2026-10-23-node-setup-hold-rules.md).
// Every rule names a repo, so a repo no rule names faces no hold rules at all: a new one belongs
// in the const block above and in a list here (TestEveryNamedRepoHasHoldRules).
func DefaultHoldRules() []HoldRule {
	return defaultHoldRules(loopRules(), true, true)
}

// defaultHoldRules is the default list with jarvis-loop's rules loop, the trusted-file rules when
// trusted and jarvis-node-setup's and jarvis-command-sdk's when nodeSetup (earlier loopds shipped
// it without each, and those lists must stay recognisable: EarlierDefaultHoldRules).
func defaultHoldRules(loop []HoldRule, trusted, nodeSetup bool) []HoldRule {
	out := append(serverRules(), loop...)
	if trusted {
		out = append(out, trustedFileRules()...)
	}
	for _, repo := range []string{NodeMobileRepo, RecipesRepo} {
		out = append(out, mobileRules(repo)...)
	}
	out = append(out, docsRules(DocsRepo)...)
	out = append(out, installerRules(InstallerRepo)...)
	for _, repo := range []string{CmdPackages, DevicePackages} {
		out = append(out, packageRules(repo)...)
	}
	if nodeSetup {
		out = append(out, nodeSetupRules(NodeSetupRepo)...)
		out = append(out, commandSDKRules(CommandSDKRepo)...)
	}
	return out
}

func serverRules() []HoldRule {
	s := ServerRepo
	return []HoldRule{
		{Name: "auth", Glob: "internal/modules/auth/**", Repo: s},
		{Name: "authn", Glob: "internal/platform/authn/**", Repo: s},
		{Name: "update", Glob: "internal/update/**", Repo: s},
		{Name: "upgrade", Glob: "cmd/jarvisd/upgrade*.go", Repo: s},
		{Name: "release", Glob: ".github/workflows/release.yml", Repo: s},
		{Name: "ci", Glob: ".github/workflows/**", Repo: s},
		{Name: "deleted-test", Glob: "**/*_test.go", Statuses: []string{"D", "R"}, Repo: s},
		{Name: "contract-removed", Glob: "contract/**", Statuses: []string{"D", "R"}, Repo: s},
	}
}

// loopRules are jarvis-loop's: the files that implement who may approve or merge, API auth and
// tokens, the hold rules, the guard and the landing gates, not the packages they sit in.
func loopRules() []HoldRule {
	l := LoopRepo
	return []HoldRule{
		{Name: "approval", Glob: "internal/pipeline/approve.go", Repo: l},
		{Name: "approval", Glob: "internal/pipeline/trust.go", Repo: l},
		{Name: "approval", Glob: "internal/api/approve.go", Repo: l},
		{Name: "approval", Glob: "internal/store/approve.go", Repo: l},
		{Name: "hold-rules", Glob: "internal/pipeline/holdrules.go", Repo: l},
		{Name: "hold-rules", Glob: "internal/store/control.go", Repo: l},
		{Name: "land", Glob: "internal/runner/land*.go", Repo: l},
		{Name: "land", Glob: "internal/runner/pr.go", Repo: l},
		{Name: "land", Glob: "internal/runner/git.go", Repo: l},
		{Name: "api-auth", Glob: "internal/api/auth.go", Repo: l},
		{Name: "api-auth", Glob: "internal/api/onbehalf.go", Repo: l},
		{Name: "api-auth", Glob: "internal/store/approver.go", Repo: l},
		{Name: "api-auth", Glob: "cmd/loopd/main.go", Repo: l},
		{Name: "slack-approver", Glob: "internal/slack/approve*.go", Repo: l},
		{Name: "tokens", Glob: "internal/slack/env.go", Repo: l},
		{Name: "tokens", Glob: "internal/runner/config.go", Repo: l},
		{Name: "tokens", Glob: "internal/runner/client_auth.go", Repo: l},
		{Name: "guard", Glob: "internal/guard/**", Repo: l},
		{Name: "guard", Glob: "cmd/loop-guard/**", Repo: l},
		{Name: "guard", Glob: "internal/runner/settings.go", Repo: l},
		{Name: "ci", Glob: ".github/workflows/**", Repo: l},
		{Name: "deleted-test", Glob: "**/*_test.go", Statuses: []string{"D", "R"}, Repo: l},
		{Name: "scenario-removed", Glob: "scenarios/**", Statuses: []string{"D", "R"}, Repo: l},
	}
}

// loopRulesV1 are jarvis-loop's rules as phase 6 shipped them, until #204: frozen, never edited,
// so a database carrying a copy of a list built on them is still recognised (IsBuiltInHoldRuleSet).
func loopRulesV1() []HoldRule {
	l := LoopRepo
	return []HoldRule{
		{Name: "approval", Glob: "internal/pipeline/**", Repo: l},
		{Name: "land", Glob: "internal/runner/land*.go", Repo: l},
		{Name: "land", Glob: "internal/runner/pr.go", Repo: l},
		{Name: "land", Glob: "internal/runner/git.go", Repo: l},
		{Name: "api-auth", Glob: "internal/api/**", Repo: l},
		{Name: "store", Glob: "internal/store/**", Repo: l},
		{Name: "loopd-wiring", Glob: "cmd/loopd/**", Repo: l},
		{Name: "slack-approver", Glob: "internal/slack/approve*.go", Repo: l},
		{Name: "tokens", Glob: "internal/slack/env.go", Repo: l},
		{Name: "tokens", Glob: "internal/runner/config.go", Repo: l},
		{Name: "tokens", Glob: "internal/runner/client.go", Repo: l},
		{Name: "guard", Glob: "internal/guard/**", Repo: l},
		{Name: "guard", Glob: "cmd/loop-guard/**", Repo: l},
		{Name: "guard", Glob: "internal/runner/settings.go", Repo: l},
		{Name: "ci", Glob: ".github/workflows/**", Repo: l},
		{Name: "deleted-test", Glob: "**/*_test.go", Statuses: []string{"D", "R"}, Repo: l},
		{Name: "scenario-removed", Glob: "scenarios/**", Statuses: []string{"D", "R"}, Repo: l},
	}
}

// trustedFileRules hold the files the runner trusts: jarvis-server's deny_write paths (the
// harness its verify runs; docs/decisions/2026-10-15-all-repos.md), and jarvis-loop's verify
// gate itself. A PR changing them gets verified only on the user's trust OK (trust.go), and lands
// only with the user's approval. They are their own list so the built-in sets an earlier loopd
// wrote out stay recognised (IsBuiltInHoldRuleSet).
func trustedFileRules() []HoldRule {
	s, l := ServerRepo, LoopRepo
	return []HoldRule{
		{Name: "trusted-file", Glob: "scripts/sandbox", Repo: s},
		{Name: "trusted-file", Glob: "scripts/screenshots", Repo: s},
		{Name: "trusted-file", Glob: "scripts/mobile-shot", Repo: s},
		{Name: "trusted-file", Glob: "tools/pipeline", Repo: s},
		{Name: "trusted-file", Glob: "tools/pipeline/**", Repo: s},
		{Name: "trusted-file", Glob: "internal/runner/verify.go", Repo: l},
	}
}

var deletedOrRenamed = []string{"D", "R"}

// tsTestRules hold deleted or renamed TypeScript tests.
func tsTestRules(repo string) []HoldRule {
	return []HoldRule{
		{Name: "deleted-test", Glob: "**/__tests__/**", Statuses: deletedOrRenamed, Repo: repo},
		{Name: "deleted-test", Glob: "**/*.test.ts", Statuses: deletedOrRenamed, Repo: repo},
		{Name: "deleted-test", Glob: "**/*.test.tsx", Statuses: deletedOrRenamed, Repo: repo},
	}
}

// pyTestRules hold deleted or renamed Python tests.
func pyTestRules(repo string) []HoldRule {
	return []HoldRule{
		{Name: "deleted-test", Glob: "tests/**", Statuses: deletedOrRenamed, Repo: repo},
		{Name: "deleted-test", Glob: "**/test_*.py", Statuses: deletedOrRenamed, Repo: repo},
	}
}

// mobileRules: a merge to main ships to the testers (TestFlight, Play internal), so the release
// and store config, native code, crypto, auth, pairing and presence are held.
func mobileRules(r string) []HoldRule {
	return append([]HoldRule{
		{Name: "release-config", Glob: "eas.json", Repo: r},
		{Name: "release-config", Glob: "app.json", Repo: r},
		{Name: "release-config", Glob: "app.config.*", Repo: r},
		{Name: "ci", Glob: ".github/**", Repo: r},
		{Name: "native", Glob: "plugins/**", Repo: r},
		{Name: "native", Glob: "targets/**", Repo: r},
		{Name: "native", Glob: "native/**", Repo: r},
		{Name: "native", Glob: "ios/**", Repo: r},
		{Name: "native", Glob: "android/**", Repo: r},
		{Name: "store-scripts", Glob: "scripts/store/**", Repo: r},
		{Name: "crypto", Glob: "modules/jarvis-crypto/**", Repo: r},
		{Name: "auth", Glob: "src/auth/**", Repo: r},
		{Name: "auth", Glob: "src/services/tokenStorage.ts", Repo: r},
		{Name: "auth", Glob: "src/services/clearUserData.ts", Repo: r},
		{Name: "auth", Glob: "src/services/oauthService.ts", Repo: r},
		{Name: "pairing", Glob: "src/services/qr*Service.ts", Repo: r},
		{Name: "presence", Glob: "src/services/*presence*", Repo: r},
		{Name: "presence", Glob: "src/services/*Presence*", Repo: r},
		{Name: "config", Glob: "src/config/env.ts", Repo: r},
		{Name: "test-config", Glob: "jest.config.js", Repo: r},
	}, tsTestRules(r)...)
}

// docsRules: what the site serves beyond pages (headers, redirects, .well-known), how it deploys,
// its build config, the security pages and the internal runbooks.
func docsRules(r string) []HoldRule {
	return append([]HoldRule{
		{Name: "well-known", Glob: ".well-known/**", Repo: r},
		{Name: "hosting", Glob: "_headers", Repo: r},
		{Name: "hosting", Glob: "_redirects", Repo: r},
		{Name: "hosting", Glob: "app-fallback/**", Repo: r},
		{Name: "deploy", Glob: "deploy.sh", Repo: r},
		{Name: "site-config", Glob: "mkdocs.yml", Repo: r},
		{Name: "security-docs", Glob: "docs/security/**", Repo: r},
		{Name: "internal-ops", Glob: "internal-ops/**", Repo: r},
		{Name: "ci", Glob: ".github/**", Repo: r},
	}, pyTestRules(r)...)
}

// installerRules: the installer generates secrets and compose files and pins image digests; a
// merge deploys the site.
func installerRules(r string) []HoldRule {
	return append([]HoldRule{
		{Name: "secrets", Glob: "src/lib/secret-generator.ts", Repo: r},
		{Name: "secrets", Glob: "src/lib/*generator*.ts", Repo: r},
		{Name: "image-digests", Glob: "src/data/image-digests.json", Repo: r},
		{Name: "image-digests", Glob: "src/lib/image-digests.ts", Repo: r},
		{Name: "service-registry", Glob: "public/service-registry.json", Repo: r},
		{Name: "domain", Glob: "public/CNAME", Repo: r},
		{Name: "scripts", Glob: "scripts/**", Repo: r},
		{Name: "ci", Glob: ".github/**", Repo: r},
	}, tsTestRules(r)...)
}

// nodeSetupRules: jarvis-node-setup is the Python node that runs on the user's Pis, and a push to
// main publishes the ghcr.io/alexberardi/jarvis-node images (`.github/workflows/release.yml`,
// "fast enough to run on every push"), so a merge ships to real devices like the mobile apps'.
// Held: the release and image config, CI, the node's auth, secrets and K1/K2 keys, provisioning
// and pairing, and the install, update and sudoers paths that run as root on a device.
func nodeSetupRules(r string) []HoldRule {
	return append([]HoldRule{
		{Name: "release-config", Glob: ".github/workflows/release.yml", Repo: r},
		{Name: "release-config", Glob: "Dockerfile", Repo: r},
		{Name: "release-config", Glob: "Dockerfile.*", Repo: r},
		{Name: "release-config", Glob: ".dockerignore", Repo: r},
		// `*` does not cross `/` and the repo writes `.yaml` today, so these are `**`-prefixed
		// and extension-agnostic: a compose file or requirements list added in a subdirectory
		// (`deploy/docker-compose.yml`, `requirements/base.txt`) is held too, not only the
		// root-level ones that exist now.
		{Name: "release-config", Glob: "**/docker-compose*.y*ml", Repo: r},
		{Name: "release-config", Glob: "compose/**", Repo: r},
		{Name: "release-config", Glob: "pyproject.toml", Repo: r},
		{Name: "release-config", Glob: "**/*requirements*.txt", Repo: r},
		{Name: "release-config", Glob: "**/requirements*/**/*", Repo: r},
		{Name: "release-config", Glob: "build/**", Repo: r},
		{Name: "ci", Glob: ".github/**", Repo: r},
		{Name: "auth", Glob: "core/ijarvis_authentication.py", Repo: r},
		{Name: "auth", Glob: "core/ijarvis_secret.py", Repo: r},
		{Name: "auth", Glob: "services/command_auth_service.py", Repo: r},
		{Name: "auth", Glob: "services/secret_service.py", Repo: r},
		{Name: "auth", Glob: "repositories/secrets_repository.py", Repo: r},
		{Name: "auth", Glob: "models/secret.py", Repo: r},
		{Name: "auth", Glob: "models/command_auth.py", Repo: r},
		{Name: "auth", Glob: "agents/token_refresh_agent.py", Repo: r},
		{Name: "auth", Glob: "utils/set_secret.py", Repo: r},
		{Name: "auth", Glob: "utils/encryption_utils.py", Repo: r},
		{Name: "auth", Glob: "utils/authorize_node.py", Repo: r},
		{Name: "auth", Glob: "utils/generate_dev_k2.py", Repo: r},
		{Name: "pairing", Glob: "provisioning/**", Repo: r},
		{Name: "pairing", Glob: "services/bluetooth_pair_agent.py", Repo: r},
		{Name: "pairing", Glob: "services/camera_credentials_handler.py", Repo: r},
		{Name: "pairing", Glob: "utils/mqtt_credentials.py", Repo: r},
		{Name: "pairing", Glob: "scripts/run_provisioning.py", Repo: r},
		{Name: "pairing", Glob: "scripts/setup_mode.py", Repo: r},
		{Name: "install-scripts", Glob: "install.sh", Repo: r},
		{Name: "install-scripts", Glob: "setup.sh", Repo: r},
		{Name: "install-scripts", Glob: "setup_*.sh", Repo: r},
		{Name: "install-scripts", Glob: "setup/**", Repo: r},
		{Name: "install-scripts", Glob: "scripts/jarvis-*", Repo: r},
		{Name: "install-scripts", Glob: "services/post_install_dropins.py", Repo: r},
		{Name: "install-scripts", Glob: "services/update_service.py", Repo: r},
		{Name: "install-scripts", Glob: "services/package_install_handler.py", Repo: r},
		{Name: "install-scripts", Glob: "scripts/install_command.py", Repo: r},
		{Name: "install-scripts", Glob: "scripts/verify_package.py", Repo: r},
		// pyTestRules covers the Python tests; node-setup also keeps a shell one
		// (scripts/test_install_signature_verify.sh, the release signature check).
		{Name: "deleted-test", Glob: "**/test_*.sh", Statuses: deletedOrRenamed, Repo: r},
	}, pyTestRules(r)...)
}

// commandSDKRules: jarvis-command-sdk is the published SDK every command package and the Python
// node import, so its auth, secret and process helpers reach every one of them. Its slug is not
// under the jarvis-cmd-* wildcard and it has no manifest or `*_scripts/`, so it gets its own list
// rather than a wider wildcard.
func commandSDKRules(r string) []HoldRule {
	return append([]HoldRule{
		{Name: "auth", Glob: "jarvis_command_sdk/auth.py", Repo: r},
		{Name: "auth", Glob: "jarvis_command_sdk/authentication.py", Repo: r},
		{Name: "auth", Glob: "jarvis_command_sdk/secret.py", Repo: r},
		{Name: "auth", Glob: "jarvis_command_sdk/cc_http.py", Repo: r},
		{Name: "auth", Glob: "jarvis_command_sdk/process.py", Repo: r},
		{Name: "release-config", Glob: "pyproject.toml", Repo: r},
		{Name: "ci", Glob: ".github/**", Repo: r},
	}, pyTestRules(r)...)
}

// packageRules: any change to the manifest (its secrets, packages, apt_packages, post_install and
// version are the risky fields, and a path rule cannot see fields), install scripts, the shared
// OAuth and token code, CI.
func packageRules(r string) []HoldRule {
	return append([]HoldRule{
		{Name: "package-manifest", Glob: "jarvis_package.yaml", Repo: r},
		{Name: "package-scripts", Glob: "*_scripts/**", Repo: r},
		{Name: "package-shared", Glob: "*_shared/**", Repo: r},
		{Name: "ci", Glob: ".github/**", Repo: r},
	}, pyTestRules(r)...)
}

// EarlierDefaultHoldRules are the built-in lists earlier loopds shipped: jarvis-server's alone
// before phase 6, then with jarvis-loop's, then every repo's (phase 6b), then with the trusted
// files, then #204's narrowed jarvis-loop list, until #549 added jarvis-node-setup's and
// jarvis-command-sdk's.
func EarlierDefaultHoldRules() [][]HoldRule {
	return [][]HoldRule{
		serverRules(),
		append(serverRules(), loopRulesV1()...),
		defaultHoldRules(loopRulesV1(), false, false),
		defaultHoldRules(loopRulesV1(), true, false),
		defaultHoldRules(loopRules(), true, false),
	}
}

// IsBuiltInHoldRuleSet reports whether rules are, as a set, a built-in default list: today's, or
// one an earlier loopd shipped (EarlierDefaultHoldRules).
// Any control change used to write the built-in list out as if the user had set it, which froze
// it; loopd recognises such a copy and goes back to the live defaults.
func IsBuiltInHoldRuleSet(rules []HoldRule) bool {
	if len(rules) == 0 {
		return false
	}
	for _, set := range append(EarlierDefaultHoldRules(), DefaultHoldRules()) {
		if sameRuleSet(rules, set) {
			return true
		}
	}
	return false
}

func ruleKey(r HoldRule) string {
	st := append([]string(nil), r.Statuses...)
	slices.Sort(st)
	return r.Repo + "\x00" + r.Name + "\x00" + r.Glob + "\x00" + strings.Join(st, ",")
}

func sameRuleSet(a, b []HoldRule) bool {
	if len(a) != len(b) {
		return false
	}
	count := map[string]int{}
	for _, r := range a {
		count[ruleKey(r)]++
	}
	for _, r := range b {
		k := ruleKey(r)
		if count[k] == 0 {
			return false
		}
		count[k]--
	}
	return true
}

var repoSlugRe = regexp.MustCompile(`^[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]*\*?$`)

// ruleApplies says whether a rule for ruleRepo applies to an item of itemRepo. An unscoped rule
// applies everywhere; an item whose repo is not known yet gets every rule (holding more is the
// safe direction); an item whose repo is a bare name (a runner config key, before pr_opened
// reports the slug) matches by the rule repo's name. A rule repo ending in "*" matches every repo
// (or name) that starts with the rest.
func ruleApplies(ruleRepo, itemRepo string) bool {
	if ruleRepo == "" || itemRepo == "" || ruleRepo == itemRepo {
		return true
	}
	match := func(pattern, s string) bool {
		if p, ok := strings.CutSuffix(pattern, "*"); ok {
			return strings.HasPrefix(s, p)
		}
		return pattern == s
	}
	if !strings.Contains(itemRepo, "/") {
		_, name, _ := strings.Cut(ruleRepo, "/")
		return match(name, itemRepo)
	}
	return match(ruleRepo, itemRepo)
}

var pathStatuses = []string{"A", "M", "D", "R"}

// Validate checks a rule.
func (r HoldRule) Validate() error {
	if strings.TrimSpace(r.Name) == "" {
		return errf(CodeInvalidField, "a hold rule needs a name")
	}
	if r.Glob == "" || strings.HasPrefix(r.Glob, "/") || slices.Contains(strings.Split(r.Glob, "/"), "..") {
		return errf(CodeInvalidField, "hold rule %q: glob must be a relative path pattern", r.Name)
	}
	for _, s := range r.Statuses {
		if !slices.Contains(pathStatuses, s) {
			return errf(CodeInvalidField, "hold rule %q: status %q is not one of A, M, D, R", r.Name, s)
		}
	}
	if r.Repo != "" && (!repoSlugRe.MatchString(r.Repo) || strings.Count(r.Repo, "*") > 1 || strings.HasSuffix(r.Repo, "/") && !strings.HasSuffix(r.Repo, "/*")) {
		return errf(CodeInvalidField, "hold rule %q: repo must be owner/name, or owner/prefix* for every repo starting so, not %q", r.Name, r.Repo)
	}
	return nil
}

// GlobMatch matches a slash-separated path against a glob where "**" (a whole segment) matches
// zero or more segments and "*" matches any run of characters within one segment. Every other
// character is literal.
func GlobMatch(glob, p string) bool {
	return matchSegs(strings.Split(glob, "/"), strings.Split(p, "/"))
}

func matchSegs(g, p []string) bool {
	if len(g) == 0 {
		return len(p) == 0
	}
	if g[0] == "**" {
		for i := 0; i <= len(p); i++ {
			if matchSegs(g[1:], p[i:]) {
				return true
			}
		}
		return false
	}
	return len(p) > 0 && matchSeg(g[0], p[0]) && matchSegs(g[1:], p[1:])
}

func matchSeg(g, s string) bool {
	star := strings.IndexByte(g, '*')
	if star < 0 {
		return g == s
	}
	if !strings.HasPrefix(s, g[:star]) {
		return false
	}
	rest := g[star+1:]
	for i := star; i <= len(s); i++ {
		if matchSeg(rest, s[i:]) {
			return true
		}
	}
	return false
}

// ChangedPath is one file of a PR, as `git diff --name-status` reports it.
type ChangedPath struct {
	Status  string // A, M, D or R
	Path    string
	OldPath string // R only
}

func (c ChangedPath) String() string {
	if c.Status == "R" {
		return fmt.Sprintf("R %s -> %s", c.OldPath, c.Path)
	}
	return c.Status + " " + c.Path
}

var statusWords = map[string]string{"added": "A", "modified": "M", "deleted": "D", "renamed": "R"}

// ParseChangedPaths reads lines like "M internal/x.go", "D foo_test.go" or "R100<TAB>old<TAB>new"
// (also "added"/"modified"/"deleted"/"renamed"). Tab-separated lines may hold paths with spaces.
func ParseChangedPaths(lines []string) ([]ChangedPath, error) {
	if len(lines) == 0 {
		return nil, errf(CodeInvalidField, "changed_paths is empty")
	}
	out := make([]ChangedPath, 0, len(lines))
	for _, line := range lines {
		var parts []string
		if strings.Contains(line, "\t") {
			for _, p := range strings.Split(strings.TrimSpace(line), "\t") {
				if p = strings.TrimSpace(p); p != "" {
					parts = append(parts, p)
				}
			}
		} else {
			parts = strings.Fields(line)
		}
		if len(parts) == 0 {
			return nil, errf(CodeInvalidField, "changed_paths has an empty line")
		}
		st := statusWords[strings.ToLower(parts[0])]
		if st == "" {
			st = strings.ToUpper(parts[0][:1])
			if !slices.Contains(pathStatuses, st) || (len(parts[0]) > 1 && strings.Trim(parts[0][1:], "0123456789") != "") {
				return nil, errf(CodeInvalidField, "changed path %q: status must be A, M, D or R", line)
			}
		}
		want := 2
		if st == "R" {
			want = 3
		}
		if len(parts) != want {
			return nil, errf(CodeInvalidField, "changed path %q: want %q followed by %d path(s)", line, st, want-1)
		}
		var paths []string
		for _, p := range parts[1:] {
			c := path.Clean(p)
			if path.IsAbs(c) || c == ".." || strings.HasPrefix(c, "../") || c == "." {
				return nil, errf(CodeInvalidField, "changed path %q: paths must be inside the repo", line)
			}
			paths = append(paths, c)
		}
		cp := ChangedPath{Status: st, Path: paths[len(paths)-1]}
		if st == "R" {
			cp.OldPath = paths[0]
		}
		out = append(out, cp)
	}
	return out, nil
}

// HoldMatch is a rule that a changed path hit.
type HoldMatch struct {
	Rule string `json:"rule"`
	Path string `json:"path"`
}

func (m HoldMatch) String() string { return m.Rule + ": " + m.Path }

// MatchHoldRules returns every (rule, path) hit of the rules that apply to repo. A rename matches
// on its old or its new path.
func MatchHoldRules(rules []HoldRule, repo string, paths []ChangedPath) []HoldMatch {
	var out []HoldMatch
	for _, p := range paths {
		for _, r := range rules {
			if !ruleApplies(r.Repo, repo) {
				continue
			}
			if len(r.Statuses) > 0 && !slices.Contains(r.Statuses, p.Status) {
				continue
			}
			if GlobMatch(r.Glob, p.Path) || (p.OldPath != "" && GlobMatch(r.Glob, p.OldPath)) {
				out = append(out, HoldMatch{Rule: r.Name, Path: p.String()})
			}
		}
	}
	return out
}

// raiseRisk makes an item hold when there is any reason to, adding the reasons to HoldReason.
// Risk only goes up: nothing here ever sets it back to normal.
func raiseRisk(it *Item, reasons []string) {
	if len(reasons) == 0 {
		return
	}
	have := []string{}
	if it.HoldReason != "" {
		have = strings.Split(it.HoldReason, "; ")
	}
	for _, r := range reasons {
		if !slices.Contains(have, r) {
			have = append(have, r)
		}
	}
	it.Risk = RiskHold
	it.HoldReason = strings.Join(have, "; ")
}
