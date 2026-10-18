package runner

import (
	"path/filepath"
	"strings"
)

// The macOS fence (docs/decisions/2026-10-17-mac-runners.md) is a sandbox-exec profile in SBPL,
// built here without a build tag so its rules are tested on Linux too (fence_darwin.go runs it).

// sbplSpec are the profile's paths, already resolved (sandbox-exec matches real paths: /tmp is
// /private/tmp on macOS).
type sbplSpec struct {
	// ReadOnly are the trees no write may reach (home, the work root, /tmp) but for Writable;
	// Protected are made read-only again inside those; Masked can be neither read nor written.
	ReadOnly, Writable, Protected, Masked []string
}

// sbplProfile is the profile: everything allowed (the host's filesystem, loopback and the
// network, as under bubblewrap), then writes refused under ReadOnly, allowed again under
// Writable, refused again under Protected, and the masks last (the last matching rule wins). The
// keychain's service is refused too: gh and claude keep their logins in the keychain on macOS,
// which `security find-generic-password` would read.
func sbplProfile(s sbplSpec) string {
	var b strings.Builder
	b.WriteString("(version 1)\n(allow default)\n")
	rule := func(action string, paths []string) {
		if len(paths) == 0 {
			return
		}
		b.WriteString("(" + action)
		for _, p := range uniq(paths) {
			b.WriteString("\n  (subpath " + sbplString(p) + ")")
		}
		b.WriteString(")\n")
	}
	rule("deny file-write*", s.ReadOnly)
	rule("allow file-write*", s.Writable)
	rule("deny file-write*", s.Protected)
	rule("deny file-read* file-write*", s.Masked)
	b.WriteString("(deny mach-lookup (global-name \"com.apple.SecurityServer\") (global-name \"com.apple.securityd\"))\n")
	return b.String()
}

// sbplString is s as an SBPL string literal: backslashes and quotes escaped, so a path cannot end
// the string and add rules of its own.
func sbplString(s string) string {
	return `"` + strings.NewReplacer(`\`, `\\`, `"`, `\"`).Replace(s) + `"`
}

// realPath is p with its symlinks resolved, or cleaned when it does not exist.
func realPath(p string) string {
	if r, err := filepath.EvalSymlinks(p); err == nil {
		return r
	}
	return filepath.Clean(p)
}
