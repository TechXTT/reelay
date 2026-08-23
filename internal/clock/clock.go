// Package clock abstracts time so the engine's schedulers and backoff logic
// can be driven by a test instead of by real elapsed time.
//
// This exists from phase 1 deliberately: retrofitting a Clock into code that
// already calls time.Now() directly means touching every state transition.
package clock

import (
	"sync"
	"time"
)

type Clock interface {
	Now() time.Time
	Since(t time.Time) time.Duration
	// NewTicker mirrors time.NewTicker but returns only the channel plus a
	// stop func, which is all the engine needs and all a fake can provide.
	NewTicker(d time.Duration) (<-chan time.Time, func())
	Sleep(d time.Duration)
}

// Real is the production clock.
type Real struct{}

func (Real) Now() time.Time                  { return time.Now() }
func (Real) Since(t time.Time) time.Duration { return time.Since(t) }
func (Real) Sleep(d time.Duration)           { time.Sleep(d) }

func (Real) NewTicker(d time.Duration) (<-chan time.Time, func()) {
	t := time.NewTicker(d)
	return t.C, t.Stop
}

// Fake is a manually advanced clock for tests. Tickers fire when Advance
// crosses their next deadline.
type Fake struct {
	mu      sync.Mutex
	now     time.Time
	tickers []*fakeTicker
}

type fakeTicker struct {
	ch       chan time.Time
	interval time.Duration
	next     time.Time
	stopped  bool
}

func NewFake(start time.Time) *Fake { return &Fake{now: start} }

func (f *Fake) Now() time.Time {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.now
}

func (f *Fake) Since(t time.Time) time.Duration { return f.Now().Sub(t) }

// Sleep on a fake clock advances time rather than blocking, so a test never
// waits on wall time.
func (f *Fake) Sleep(d time.Duration) { f.Advance(d) }

func (f *Fake) NewTicker(d time.Duration) (<-chan time.Time, func()) {
	f.mu.Lock()
	defer f.mu.Unlock()
	t := &fakeTicker{
		// Buffered so Advance never blocks on a receiver that is busy.
		ch:       make(chan time.Time, 1),
		interval: d,
		next:     f.now.Add(d),
	}
	f.tickers = append(f.tickers, t)
	return t.ch, func() {
		f.mu.Lock()
		defer f.mu.Unlock()
		t.stopped = true
	}
}

// Advance moves the clock forward and fires any ticker whose deadline passed.
// A ticker with a full buffer drops the tick, exactly as time.Ticker does.
func (f *Fake) Advance(d time.Duration) {
	f.mu.Lock()
	f.now = f.now.Add(d)
	now := f.now
	tickers := make([]*fakeTicker, len(f.tickers))
	copy(tickers, f.tickers)
	f.mu.Unlock()

	for _, t := range tickers {
		f.mu.Lock()
		for !t.stopped && !t.next.After(now) {
			select {
			case t.ch <- t.next:
			default:
			}
			t.next = t.next.Add(t.interval)
		}
		f.mu.Unlock()
	}
}

var _ Clock = Real{}
var _ Clock = (*Fake)(nil)
