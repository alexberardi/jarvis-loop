// Package clock is the injected source of time. Everything in loopd that depends on the time
// takes a Clock, so tests move time forward instead of sleeping.
package clock

import (
	"sync"
	"time"
)

// Clock tells the time.
type Clock interface {
	Now() time.Time
}

// Real is the wall clock, in UTC.
type Real struct{}

// Now returns the current time in UTC.
func (Real) Now() time.Time { return time.Now().UTC() }

// Fake is a clock that only moves when told to. It is safe for concurrent use.
type Fake struct {
	mu  sync.Mutex
	now time.Time
}

// NewFake returns a fake clock set to t.
func NewFake(t time.Time) *Fake { return &Fake{now: t.UTC()} }

// Now returns the fake's current time.
func (f *Fake) Now() time.Time {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.now
}

// Advance moves the fake clock forward by d.
func (f *Fake) Advance(d time.Duration) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.now = f.now.Add(d)
}
