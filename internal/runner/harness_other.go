//go:build !linux

package runner

import (
	"errors"
	"net"
)

// peerIn: harness sessions need Linux (bubblewrap, SO_PEERCRED).
func peerIn(*net.UnixConn, string) error { return errors.New("harness sessions need Linux") }
