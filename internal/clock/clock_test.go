package clock

import (
	"testing"
	"time"
)

func TestFakeAdvanceFiresDueTimers(t *testing.T) {
	f := NewFake(time.Time{})
	var fired []string

	f.AfterFunc(10*time.Second, func() { fired = append(fired, "b") })
	f.AfterFunc(5*time.Second, func() { fired = append(fired, "a") })

	f.Advance(4 * time.Second)
	if len(fired) != 0 {
		t.Fatalf("nothing should have fired yet, got %v", fired)
	}

	f.Advance(2 * time.Second) // now at 6s: only "a" is due
	if len(fired) != 1 || fired[0] != "a" {
		t.Fatalf("after 6s fired = %v, want [a]", fired)
	}

	f.Advance(10 * time.Second) // now well past 10s
	if len(fired) != 2 || fired[1] != "b" {
		t.Fatalf("fired = %v, want [a b]", fired)
	}
}

func TestFakeChainedTimers(t *testing.T) {
	f := NewFake(time.Time{})
	var steps int
	f.AfterFunc(time.Second, func() {
		steps++
		f.AfterFunc(time.Second, func() { steps++ })
	})
	// A single advance covering both delays should fire the chained timer too.
	f.Advance(5 * time.Second)
	if steps != 2 {
		t.Fatalf("chained steps = %d, want 2", steps)
	}
}

func TestFakeStop(t *testing.T) {
	f := NewFake(time.Time{})
	var fired bool
	timer := f.AfterFunc(time.Second, func() { fired = true })
	if !timer.Stop() {
		t.Fatal("Stop should report it cancelled the timer")
	}
	f.Advance(5 * time.Second)
	if fired {
		t.Fatal("stopped timer should not fire")
	}
	if timer.Stop() {
		t.Fatal("second Stop should report false")
	}
}

func TestRealClockNow(t *testing.T) {
	c := Real()
	before := time.Now()
	got := c.Now()
	if got.Before(before.Add(-time.Second)) {
		t.Fatalf("Real().Now() = %v, unexpectedly far in the past", got)
	}
	done := make(chan struct{})
	c.AfterFunc(time.Millisecond, func() { close(done) })
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("real AfterFunc did not fire")
	}
}
