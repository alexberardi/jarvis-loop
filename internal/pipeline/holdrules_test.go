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
		hits := MatchHoldRules(rules, paths)
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
	c := DefaultControl()
	c.HoldRules = append(c.HoldRules, c.HoldRules[0])
	if err := c.Validate(); err == nil || !strings.Contains(err.Error(), "duplicate") {
		t.Fatalf("duplicate rule names: %v", err)
	}
}
