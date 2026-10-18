//go:build !darwin

package runner

import (
	"os"
	"os/exec"
)

// fenceName is the fence here, as messages name it.
const fenceName = "bubblewrap"

func (r *Runner) fenceBinary() string {
	if r.Config().Bwrap == "" {
		return DefaultBwrap
	}
	return r.Config().Bwrap
}

// fenceCommand is the command under bubblewrap: a private /tmp, HOME and the work root bound
// read-only, the masks over them, then the writable paths and the read-only ones inside those.
func (r *Runner) fenceCommand(f fenceSpec) (*exec.Cmd, error) {
	dir, o := f.dir, f.o
	env := append(f.env, "TMPDIR=/tmp")
	// A private /tmp first: what is bound below (home, the work root, the working dirs) may itself
	// live under the host's /tmp, and later mounts go over earlier ones.
	args := []string{"--dev-bind", "/", "/", "--tmpfs", "/tmp"}
	if fi, err := os.Stat(f.home); err == nil && fi.IsDir() {
		args = append(args, "--ro-bind", f.home, f.home)
	}
	// The runner's work root too, wherever it is: the mirrors' config and hooks are read by the
	// runner's own (unfenced) git calls.
	if fi, err := os.Stat(r.Config().WorkRoot); err == nil && fi.IsDir() {
		args = append(args, "--ro-bind", r.Config().WorkRoot, r.Config().WorkRoot)
	}
	for _, p := range uniq(o.Visible) {
		if _, err := os.Stat(p); err == nil {
			args = append(args, "--ro-bind", p, p)
		}
	}
	for _, p := range r.Config().VerifyMask {
		fi, err := os.Stat(p)
		switch {
		case err != nil:
			continue // nothing there to hide
		case fi.IsDir():
			args = append(args, "--tmpfs", p)
		default:
			args = append(args, "--ro-bind", "/dev/null", p)
		}
	}
	// The worktree's admin dir last, so it is a mount point of its own (it cannot be renamed
	// away) whatever holds it.
	admin := gitAdminDir(dir)
	writable := append([]string{dir, f.cacheRoot}, o.Writable...)
	writable = append(append(writable, r.Config().VerifyWritable...), admin)
	for _, p := range uniq(writable) {
		if fi, err := os.Stat(p); err == nil && fi.IsDir() {
			args = append(args, "--bind", p, p)
		}
	}
	for _, p := range uniq(append(worktreePlumbing(dir, admin), o.ReadOnly...)) {
		if _, err := os.Stat(p); err == nil {
			args = append(args, "--ro-bind", p, p)
		}
	}
	args = append(args, "--unshare-pid", "--proc", "/proc", "--die-with-parent", "--new-session",
		"--chdir", dir, "--", "bash", "-c", f.script)
	cmd := exec.Command(r.fenceBinary(), args...)
	cmd.Dir = dir
	cmd.Env = env
	return cmd, nil
}
