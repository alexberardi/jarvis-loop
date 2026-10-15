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

// The repos the default rules are for.
const (
	ServerRepo = "alexberardi/jarvis-server"
	LoopRepo   = "alexberardi/jarvis-loop"
)

// DefaultHoldRules are each repo's CLAUDE.md hold list as path rules. jarvis-server: auth and
// access, signing and the update helper, CI checks, and weakened tests or contract checks.
// jarvis-loop: who may approve or merge (the stage rules and approvals, loopd's API auth, the Slack
// approver check, the store that keeps approvals and hold rules, loopd's start-up wiring), token handling, the landing gates, the agents' guard, CI, and weakened tests or
// scenarios.
func DefaultHoldRules() []HoldRule {
	s, l := ServerRepo, LoopRepo
	return []HoldRule{
		{Name: "auth", Glob: "internal/modules/auth/**", Repo: s},
		{Name: "authn", Glob: "internal/platform/authn/**", Repo: s},
		{Name: "update", Glob: "internal/update/**", Repo: s},
		{Name: "upgrade", Glob: "cmd/jarvisd/upgrade*.go", Repo: s},
		{Name: "release", Glob: ".github/workflows/release.yml", Repo: s},
		{Name: "ci", Glob: ".github/workflows/**", Repo: s},
		{Name: "deleted-test", Glob: "**/*_test.go", Statuses: []string{"D", "R"}, Repo: s},
		{Name: "contract-removed", Glob: "contract/**", Statuses: []string{"D", "R"}, Repo: s},

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

var repoSlugRe = regexp.MustCompile(`^[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+$`)

// ruleApplies says whether a rule for ruleRepo applies to an item of itemRepo. An unscoped rule
// applies everywhere; an item whose repo is not known yet gets every rule (holding more is the
// safe direction); an item whose repo is a bare name (a runner config key, before pr_opened
// reports the slug) matches by the rule repo's name.
func ruleApplies(ruleRepo, itemRepo string) bool {
	if ruleRepo == "" || itemRepo == "" || ruleRepo == itemRepo {
		return true
	}
	if !strings.Contains(itemRepo, "/") {
		_, name, _ := strings.Cut(ruleRepo, "/")
		return name == itemRepo
	}
	return false
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
	if r.Repo != "" && !repoSlugRe.MatchString(r.Repo) {
		return errf(CodeInvalidField, "hold rule %q: repo must be owner/name, not %q", r.Name, r.Repo)
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
