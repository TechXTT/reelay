package indexer

import (
	"fmt"
	"sync"
	"time"

	"github.com/TechXTT/reelay/internal/clock"
)

// Breaker is a consecutive-failure circuit breaker.
//
// After threshold consecutive failures it opens for cooldown, and every call
// during that window is refused without touching the network. One success
// closes it. The point is not to protect the indexer — it is to stop the
// search loop spending its entire budget retrying something that is down, and
// to give the operator a health signal instead of a log full of timeouts.
type Breaker struct {
	name      string
	threshold int
	cooldown  time.Duration
	clock     clock.Clock

	mu          sync.Mutex
	failures    int
	openUntil   time.Time
	lastErr     error
	lastSuccess time.Time
	// trips counts how many times the breaker has opened, which is the number
	// worth showing an operator: a breaker that opens weekly is a different
	// problem from one that opened once.
	trips int
}

type BreakerOptions struct {
	Name      string
	Threshold int
	Cooldown  time.Duration
	Clock     clock.Clock
}

func NewBreaker(opt BreakerOptions) *Breaker {
	if opt.Threshold < 1 {
		opt.Threshold = 5
	}
	if opt.Cooldown <= 0 {
		opt.Cooldown = 15 * time.Minute
	}
	if opt.Clock == nil {
		opt.Clock = clock.Real{}
	}
	return &Breaker{
		name:      opt.Name,
		threshold: opt.Threshold,
		cooldown:  opt.Cooldown,
		clock:     opt.Clock,
	}
}

// Allow reports whether a request may proceed. It returns an error wrapping
// ErrUnhealthy while the breaker is open, so callers can distinguish "we did
// not try" from "we tried and it failed".
func (b *Breaker) Allow() error {
	b.mu.Lock()
	defer b.mu.Unlock()

	if b.openUntil.IsZero() || b.clock.Now().After(b.openUntil) {
		return nil
	}
	remaining := b.openUntil.Sub(b.clock.Now()).Round(time.Second)
	return fmt.Errorf("%s: %w (%d consecutive failures, retrying in %s; last error: %v)",
		b.name, ErrUnhealthy, b.failures, remaining, b.lastErr)
}

// Success closes the breaker and clears the failure count.
func (b *Breaker) Success() {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.failures = 0
	b.openUntil = time.Time{}
	b.lastErr = nil
	b.lastSuccess = b.clock.Now()
}

// Failure records a failed request and opens the breaker once the threshold is
// reached.
func (b *Breaker) Failure(err error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.failures++
	b.lastErr = err
	if b.failures >= b.threshold {
		b.openUntil = b.clock.Now().Add(b.cooldown)
		b.trips++
	}
}

// BreakerState is the snapshot the health endpoint and the UI render.
type BreakerState struct {
	Name        string    `json:"name"`
	Healthy     bool      `json:"healthy"`
	Failures    int       `json:"consecutive_failures"`
	OpenUntil   time.Time `json:"open_until,omitempty"`
	LastError   string    `json:"last_error,omitempty"`
	LastSuccess time.Time `json:"last_success,omitempty"`
	Trips       int       `json:"trips"`
}

func (b *Breaker) State() BreakerState {
	b.mu.Lock()
	defer b.mu.Unlock()

	open := !b.openUntil.IsZero() && !b.clock.Now().After(b.openUntil)
	st := BreakerState{
		Name:        b.name,
		Healthy:     !open,
		Failures:    b.failures,
		LastSuccess: b.lastSuccess,
		Trips:       b.trips,
	}
	if open {
		st.OpenUntil = b.openUntil
	}
	if b.lastErr != nil {
		st.LastError = b.lastErr.Error()
	}
	return st
}
