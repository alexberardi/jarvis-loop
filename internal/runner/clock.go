package runner

import (
	"sync"
	"time"
)

// Clock is the runner's source of time and timers, injected so tests move time instead of
// sleeping.
type Clock interface {
	Now() time.Time
	NewTimer(d time.Duration) Timer
}

// Timer is a one-shot timer.
type Timer interface {
	C() <-chan time.Time
	Stop()
}

// RealClock is the wall clock.
type RealClock struct{}

// Now returns the current time in UTC.
func (RealClock) Now() time.Time { return time.Now().UTC() }

// NewTimer starts a real timer.
func (RealClock) NewTimer(d time.Duration) Timer { return realTimer{time.NewTimer(d)} }

type realTimer struct{ t *time.Timer }

func (r realTimer) C() <-chan time.Time { return r.t.C }
func (r realTimer) Stop()               { r.t.Stop() }

// FakeClock only moves when told to. BlockUntil lets a test wait until the code under test has
// armed its timers before moving time.
type FakeClock struct {
	mu      sync.Mutex
	cond    *sync.Cond
	now     time.Time
	waiters map[*fakeTimer]bool
}

// NewFakeClock returns a fake clock set to t.
func NewFakeClock(t time.Time) *FakeClock {
	f := &FakeClock{now: t, waiters: map[*fakeTimer]bool{}}
	f.cond = sync.NewCond(&f.mu)
	return f
}

type fakeTimer struct {
	f  *FakeClock
	at time.Time
	ch chan time.Time
}

func (t *fakeTimer) C() <-chan time.Time { return t.ch }

func (t *fakeTimer) Stop() {
	t.f.mu.Lock()
	defer t.f.mu.Unlock()
	delete(t.f.waiters, t)
	t.f.cond.Broadcast()
}

// Now returns the fake time.
func (f *FakeClock) Now() time.Time {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.now
}

// NewTimer arms a timer that fires when the clock reaches now+d.
func (f *FakeClock) NewTimer(d time.Duration) Timer {
	f.mu.Lock()
	defer f.mu.Unlock()
	t := &fakeTimer{f: f, at: f.now.Add(d), ch: make(chan time.Time, 1)}
	if d <= 0 {
		t.ch <- f.now
		return t
	}
	f.waiters[t] = true
	f.cond.Broadcast()
	return t
}

// Advance moves the clock and fires the timers that are due.
func (f *FakeClock) Advance(d time.Duration) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.now = f.now.Add(d)
	for t := range f.waiters {
		if !t.at.After(f.now) {
			t.ch <- f.now
			delete(f.waiters, t)
		}
	}
	f.cond.Broadcast()
}

// BlockUntil waits until at least n timers are armed.
func (f *FakeClock) BlockUntil(n int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for len(f.waiters) < n {
		f.cond.Wait()
	}
}

// Armed is the number of timers waiting.
func (f *FakeClock) Armed() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.waiters)
}
