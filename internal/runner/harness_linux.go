package runner

import (
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
)

// peerIn checks the caller on c is this user's process with its working directory in wt.
func peerIn(c *net.UnixConn, wt string) error {
	if c == nil {
		return fmt.Errorf("not a Unix socket")
	}
	raw, err := c.SyscallConn()
	if err != nil {
		return err
	}
	var cred *syscall.Ucred
	var cerr error
	if err := raw.Control(func(fd uintptr) {
		cred, cerr = syscall.GetsockoptUcred(int(fd), syscall.SOL_SOCKET, syscall.SO_PEERCRED)
	}); err != nil {
		return err
	}
	if cerr != nil {
		return fmt.Errorf("who is calling: %w", cerr)
	}
	if int(cred.Uid) != os.Getuid() {
		return fmt.Errorf("the caller is uid %d, not this runner's user", cred.Uid)
	}
	cwd, err := os.Readlink("/proc/" + strconv.Itoa(int(cred.Pid)) + "/cwd")
	if err != nil {
		return fmt.Errorf("the caller's working directory: %w", err)
	}
	if cwd != wt && !strings.HasPrefix(cwd, wt+string(filepath.Separator)) {
		return fmt.Errorf("this session belongs to %s; run loop-harness from inside it", wt)
	}
	return nil
}
