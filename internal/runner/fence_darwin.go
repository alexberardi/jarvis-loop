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
	if r.Cfg.SandboxExec == "" {
		return DefaultSandboxExec
	}
	return r.Cfg.SandboxExec
}

// fenceCommand is the command under sandbox-exec: HOME, the work root and /tmp read-only, the
// writable paths and a private TMPDIR (under <work_root>/fence-tmp, cleared at the runner's start)
// writable, the plumbing and read-only paths inside them read-only again, the masks unreadable.
// There is no mount namespace: /tmp cannot be made private, so it is read-only instead, and
// Visible needs nothing (nothing is hidden behind a private /tmp).
func (r *Runner) fenceCommand(f fenceSpec) (*exec.Cmd, error) {
	dir, o := f.dir, f.o
	tmpRoot := filepath.Join(r.Cfg.WorkRoot, "fence-tmp")
	if err := os.MkdirAll(tmpRoot, 0o700); err != nil {
		return nil, err
	}
	tmp, err := os.MkdirTemp(tmpRoot, "t")
	if err != nil {
		return nil, err
	}
	var s sbplSpec
	for _, p := range []string{f.home, r.Cfg.WorkRoot, "/tmp", "/private/tmp"} {
		s.ReadOnly = append(s.ReadOnly, realPath(p))
	}
	admin := gitAdminDir(dir)
	writable := append([]string{dir, f.cacheRoot, tmp}, o.Writable...)
	writable = append(append(writable, r.Cfg.VerifyWritable...), admin)
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
	for _, p := range r.Cfg.VerifyMask {
		if _, err := os.Stat(p); err == nil {
			s.Masked = append(s.Masked, realPath(p))
		}
	}
	cmd := exec.Command(r.fenceBinary(), "-p", sbplProfile(s), "bash", "-c", f.script)
	cmd.Dir = dir
	cmd.Env = append(f.env, "TMPDIR="+tmp)
	return cmd, nil
}
