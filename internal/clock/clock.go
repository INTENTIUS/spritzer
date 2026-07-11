// Package clock provides a small time abstraction so that time-stamped state
// (a sprite's creation time, and any future timed behavior) can be driven
// deterministically in tests. Production code uses Real(); tests use a Fake
// whose Advance method fires scheduled callbacks without any real sleeping.
package clock

import (
	"sync"
	"time"
)

// Clock is the subset of the time package that spritzer depends on.
type Clock interface {
	// Now returns the current time as seen by this clock.
	Now() time.Time
	// AfterFunc schedules fn to run after d has elapsed and returns a Timer
	// that can cancel it.
	AfterFunc(d time.Duration, fn func()) Timer
}

// Timer can cancel a scheduled callback.
type Timer interface {
	// Stop cancels the timer, reporting whether it did so before it fired.
	Stop() bool
}

// Real returns a Clock backed by the standard library time package.
func Real() Clock { return realClock{} }

type realClock struct{}

func (realClock) Now() time.Time { return time.Now() }

func (realClock) AfterFunc(d time.Duration, fn func()) Timer {
	return realTimer{time.AfterFunc(d, fn)}
}

type realTimer struct{ t *time.Timer }

func (r realTimer) Stop() bool { return r.t.Stop() }

// Fake is a deterministic Clock. Time only moves when Advance is called, at
// which point any callbacks whose deadline has passed run synchronously on the
// calling goroutine. Callbacks may schedule further callbacks, which are also
// fired if they fall within the advanced window.
type Fake struct {
	mu     sync.Mutex
	now    time.Time
	timers []*fakeTimer
}

type fakeTimer struct {
	deadline time.Time
	fn       func()
	fired    bool
	stopped  bool
}

func (t *fakeTimer) Stop() bool {
	if t.fired || t.stopped {
		return false
	}
	t.stopped = true
	return true
}

// NewFake returns a Fake clock started at the given time. If start is the zero
// value a fixed, readable epoch is used.
func NewFake(start time.Time) *Fake {
	if start.IsZero() {
		start = time.Date(2025, time.January, 1, 0, 0, 0, 0, time.UTC)
	}
	return &Fake{now: start}
}

// Now returns the fake clock's current time.
func (f *Fake) Now() time.Time {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.now
}

// AfterFunc registers fn to fire once the fake clock has advanced past d.
func (f *Fake) AfterFunc(d time.Duration, fn func()) Timer {
	f.mu.Lock()
	defer f.mu.Unlock()
	t := &fakeTimer{deadline: f.now.Add(d), fn: fn}
	f.timers = append(f.timers, t)
	return t
}

// Advance moves the clock forward by d, firing every due callback in deadline
// order. Callbacks run without the internal lock held so they may schedule more
// work on the same clock.
func (f *Fake) Advance(d time.Duration) {
	f.mu.Lock()
	target := f.now.Add(d)
	for {
		var next *fakeTimer
		for _, t := range f.timers {
			if t.fired || t.stopped {
				continue
			}
			if !t.deadline.After(target) && (next == nil || t.deadline.Before(next.deadline)) {
				next = t
			}
		}
		if next == nil {
			break
		}
		next.fired = true
		f.now = next.deadline
		fn := next.fn
		f.mu.Unlock()
		fn()
		f.mu.Lock()
	}
	f.now = target
	f.mu.Unlock()
}
