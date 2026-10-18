package runner

import (
	"strings"
	"testing"
)

// The macOS profile: writes refused under the read-only trees, allowed under the writable ones,
// refused again under the protected ones, the masks last (the last matching rule wins), the
// keychain's service refused; and a path cannot end its string and add rules of its own.
func TestSBPLProfile(t *testing.T) {
	p := sbplProfile(sbplSpec{
		ReadOnly:  []string{"/Users/a", "/private/tmp"},
		Writable:  []string{"/Users/a/work/1-build", `/Users/a/odd") (allow file-read* (subpath "/`},
		Protected: []string{"/Users/a/work/1-build/.git"},
		Masked:    []string{"/Users/a/.ssh", `/Users/a/back\slash`},
	})
	order := []string{"(version 1)\n(allow default)\n",
		"(deny file-write*\n  (subpath \"/Users/a\")\n  (subpath \"/private/tmp\"))\n",
		"(allow file-write*\n  (subpath \"/Users/a/work/1-build\")\n  (subpath \"/Users/a/odd\\\") (allow file-read* (subpath \\\"/\"))\n",
		"(deny file-write*\n  (subpath \"/Users/a/work/1-build/.git\"))\n",
		"(deny file-read* file-write*\n  (subpath \"/Users/a/.ssh\")\n  (subpath \"/Users/a/back\\\\slash\"))\n",
		`(deny mach-lookup (global-name "com.apple.SecurityServer")`}
	at := 0
	for _, want := range order {
		i := strings.Index(p[at:], want)
		if i < 0 {
			t.Fatalf("profile lacks (or misorders) %q:\n%s", want, p)
		}
		at += i + len(want)
	}
	if strings.Count(p, "\n(allow ") != 2 { // the rules start lines; the escaped path's text does not
		t.Fatalf("a path added a rule:\n%s", p)
	}
	if got := sbplProfile(sbplSpec{}); strings.Contains(got, "subpath") {
		t.Fatalf("empty spec:\n%s", got)
	}
}
