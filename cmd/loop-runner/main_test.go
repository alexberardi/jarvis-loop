package main

import (
	"errors"
	"fmt"
	"testing"
)

// A drained runner exits non-zero, so a service that restarts only on failure starts it again
// (#284); other errors stay 1.
func TestExitCode(t *testing.T) {
	for _, c := range []struct {
		err  error
		want int
	}{
		{nil, 0},
		{errDrained, exitDrained},
		{fmt.Errorf("run: %w", errDrained), exitDrained},
		{errors.New("bad config"), 1},
	} {
		if got := exitCode(c.err); got != c.want {
			t.Errorf("exitCode(%v) = %d, want %d", c.err, got, c.want)
		}
	}
	if exitDrained == 0 {
		t.Fatal("a drain must not exit 0")
	}
}
