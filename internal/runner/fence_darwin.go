//go:build darwin

package runner

import (
	"os"
	"os/exec"
	"path/filepath"
)

// fenceName is the fence here, as messages name it.
const fenceName = "sandbox-exec"

func (r *Runner) fenceBinary() string {
	if r.Config().SandboxExec == "" {
		return DefaultSandboxExec
	}
	return r.Config().SandboxExec
}

// brewPrefixes are Homebrew's prefixes (Apple Silicon's, Intel's). The user who installed Homebrew
// owns them and the runner's PATH puts their bin before /usr/bin, so a fenced command that could
// write there could replace gh or git for the runner's next unfenced call: they are read-only.
var brewPrefixes = []string{"/opt/homebrew", "/usr/local"}

// fenceCommand is the command under sandbox-exec: HOME, the work root, /tmp and Homebrew's
// prefixes read-only, the writable paths and a private TMPDIR (under <work_root>/fence-tmp,
// removed when the command ends, and the rest cleared at the runner's start) writable, the plumbing and read-only paths inside them read-only again, the masks unreadable.
// There is no mount namespace: /tmp cannot be made private, so it is read-only instead, and
// Visible needs nothing (nothing is hidden behind a private /tmp).
func (r *Runner) fenceCommand(f fenceSpec) (*exec.Cmd, error) {
	dir, o := f.dir, f.o
	tmpRoot := filepath.Join(r.Config().WorkRoot, "fence-tmp")
	if err := os.MkdirAll(tmpRoot, 0o700); err != nil {
		return nil, err
	}
	tmp, err := os.MkdirTemp(tmpRoot, "t")
	if err != nil {
		return nil, err
	}
	var s sbplSpec
	for _, p := range []string{f.home, r.Config().WorkRoot, "/tmp", "/private/tmp"} {
		s.ReadOnly = append(s.ReadOnly, realPath(p))
	}
	for _, p := range brewPrefixes {
		if _, err := os.Stat(p); err == nil {
			s.ReadOnly = append(s.ReadOnly, realPath(p))
		}
	}
	admin := gitAdminDir(dir)
	writable := append([]string{dir, f.cacheRoot, tmp}, o.Writable...)
	writable = append(append(writable, r.Config().VerifyWritable...), admin)
	for _, p := range writable {
		if fi, err := os.Stat(p); err == nil && fi.IsDir() {
			s.Writable = append(s.Writable, realPath(p))
		}
	}
	for _, p := range append(worktreePlumbing(dir, admin), o.ReadOnly...) {
		if _, err := os.Stat(p); err == nil {
			s.Protected = append(s.Protected, realPath(p))
		}
	}
	for _, p := range r.Config().VerifyMask {
		if _, err := os.Stat(p); err == nil {
			s.Masked = append(s.Masked, realPath(p))
		}
	}
	// A shell outside the fence runs it and removes the private TMPDIR after; a kill (the process
	// group) leaves it to the next start's clearing.
	cmd := exec.Command("/bin/sh", "-c", `"$0" -p "$1" bash -c "$2"; rc=$?; rm -rf "$3"; exit $rc`,
		r.fenceBinary(), sbplProfile(s), f.script, tmp)
	cmd.Dir = dir
	cmd.Env = append(f.env, "TMPDIR="+tmp)
	return cmd, nil
}
