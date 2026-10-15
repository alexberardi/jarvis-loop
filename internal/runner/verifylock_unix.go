//go:build unix

package runner

import (
	"os"
	"path/filepath"
	"syscall"
)

// lockFile opens path and takes an exclusive flock on it, waiting for it. The lock is per open
// file, so two runs in one runner exclude each other as two runners do; it ends when the file is
// closed or the process exits.
func lockFile(path string) (*os.File, error) { return flockFile(path, syscall.LOCK_EX) }

// tryLockFile takes the lock if it is free; it returns nil, nil when another holds it.
func tryLockFile(path string) (*os.File, error) {
	f, err := flockFile(path, syscall.LOCK_EX|syscall.LOCK_NB)
	if err == syscall.EWOULDBLOCK {
		return nil, nil
	}
	return f, err
}

func flockFile(path string, how int) (*os.File, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	for {
		err = syscall.Flock(int(f.Fd()), how)
		if err != syscall.EINTR {
			break
		}
	}
	if err != nil {
		f.Close()
		return nil, err
	}
	return f, nil
}
