//go:build unix

package runner

import (
	"os/exec"
	"syscall"
)

// ownGroup starts the command in its own process group, so the runner can stop the agent and
// everything it started, and nothing else.
func ownGroup(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
}

// signalGroup signals the process group led by pid.
func signalGroup(pid int, sig syscall.Signal) error {
	return syscall.Kill(-pid, sig)
}
