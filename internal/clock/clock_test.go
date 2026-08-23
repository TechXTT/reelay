package clock

import (
	"testing"
	"time"
)

func TestFakeAdvancesWithoutWallTime(t *testing.T) {
	start := time.Date(2026, 8, 23, 12, 0, 0, 0, time.UTC)
	f := NewFake(start)

	if !f.Now().Equal(start) {
		t.Fatalf("Now() = %s, want %s", f.Now(), start)
	}
	f.Advance(90 * time.Minute)
	if got := f.Now(); !got.Equal(start.Add(90 * time.Minute)) {
		t.Errorf("Now() = %s after advancing 90m", got)
	}
	if got := f.Since(start); got != 90*time.Minute {
		t.Errorf("Since = %s, want 90m", got)
	}
}

func TestFakeTickerFiresOnAdvance(t *testing.T) {
	f := NewFake(time.Date(2026, 8, 23, 12, 0, 0, 0, time.UTC))
	ch, stop := f.NewTicker(15 * time.Minute)
	defer stop()

	select {
	case <-ch:
		t.Fatal("ticker fired before the interval elapsed")
	default:
	}

	f.Advance(15 * time.Minute)
	select {
	case <-ch:
	default:
		t.Fatal("ticker did not fire after advancing one interval")
	}
}

// A stopped ticker must go quiet, or a cancelled engine loop keeps waking up.
func TestFakeTickerStop(t *testing.T) {
	f := NewFake(time.Now())
	ch, stop := f.NewTicker(time.Minute)
	stop()

	f.Advance(10 * time.Minute)
	select {
	case tick := <-ch:
		t.Errorf("stopped ticker fired at %s", tick)
	default:
	}
}

// Advancing past several intervals must not block on an unread channel, the
// same coalescing behaviour time.Ticker has.
func TestFakeTickerCoalescesMissedTicks(t *testing.T) {
	f := NewFake(time.Now())
	ch, stop := f.NewTicker(time.Minute)
	defer stop()

	f.Advance(10 * time.Minute)

	got := 0
	for {
		select {
		case <-ch:
			got++
			continue
		default:
		}
		break
	}
	if got != 1 {
		t.Errorf("drained %d ticks, want 1 (buffered channel coalesces)", got)
	}
}

func TestRealClock(t *testing.T) {
	var c Clock = Real{}
	before := time.Now()
	got := c.Now()
	if got.Before(before.Add(-time.Second)) || got.After(time.Now().Add(time.Second)) {
		t.Errorf("Real.Now() = %s, out of range", got)
	}

	ch, stop := c.NewTicker(5 * time.Millisecond)
	defer stop()
	select {
	case <-ch:
	case <-time.After(2 * time.Second):
		t.Error("real ticker never fired")
	}
}
