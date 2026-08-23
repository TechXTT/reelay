package indexer

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/TechXTT/reelay/internal/clock"
)

func newTestBreaker(threshold int, cooldown time.Duration) (*Breaker, *clock.Fake) {
	clk := clock.NewFake(time.Date(2026, 8, 23, 12, 0, 0, 0, time.UTC))
	b := NewBreaker(BreakerOptions{
		Name:      "test",
		Threshold: threshold,
		Cooldown:  cooldown,
		Clock:     clk,
	})
	return b, clk
}

func TestBreakerOpensOnlyAtTheThreshold(t *testing.T) {
	b, _ := newTestBreaker(3, 15*time.Minute)
	boom := errors.New("boom")

	for i := 1; i < 3; i++ {
		b.Failure(boom)
		if err := b.Allow(); err != nil {
			t.Fatalf("breaker opened after %d of 3 failures: %v", i, err)
		}
	}

	b.Failure(boom)
	err := b.Allow()
	if !errors.Is(err, ErrUnhealthy) {
		t.Fatalf("Allow() = %v, want ErrUnhealthy at the threshold", err)
	}
	// The message has to be actionable: which indexer, how long, and why.
	for _, want := range []string{"test", "3 consecutive failures", "retrying in", "boom"} {
		if !contains(err.Error(), want) {
			t.Errorf("error %q should mention %q", err, want)
		}
	}
}

func TestBreakerClosesAfterCooldown(t *testing.T) {
	b, clk := newTestBreaker(2, 15*time.Minute)
	b.Failure(errors.New("x"))
	b.Failure(errors.New("x"))

	if b.Allow() == nil {
		t.Fatal("breaker should be open")
	}
	clk.Advance(15 * time.Minute)
	// Exactly at the boundary the breaker is still open; one tick past it is
	// not. Asserting the boundary keeps an off-by-one from turning a 15 minute
	// cooldown into a permanent outage.
	if b.Allow() == nil {
		t.Error("breaker should still be open exactly at the deadline")
	}
	clk.Advance(time.Second)
	if err := b.Allow(); err != nil {
		t.Errorf("breaker should have closed: %v", err)
	}
}

func TestBreakerSuccessResetsTheStreak(t *testing.T) {
	b, _ := newTestBreaker(3, time.Minute)
	b.Failure(errors.New("x"))
	b.Failure(errors.New("x"))
	b.Success()

	// The streak is what matters: two more failures after a success must not
	// reach a threshold of three.
	b.Failure(errors.New("x"))
	b.Failure(errors.New("x"))
	if err := b.Allow(); err != nil {
		t.Errorf("a success should have reset the streak: %v", err)
	}

	b.Failure(errors.New("x"))
	if b.Allow() == nil {
		t.Error("the third consecutive failure should open the breaker")
	}
}

func TestBreakerStateSnapshot(t *testing.T) {
	b, clk := newTestBreaker(2, 10*time.Minute)

	st := b.State()
	if !st.Healthy || st.Failures != 0 || st.Trips != 0 {
		t.Errorf("a fresh breaker should be clean, got %+v", st)
	}

	b.Success()
	if st := b.State(); st.LastSuccess.IsZero() {
		t.Error("Success should record when it happened")
	}

	b.Failure(errors.New("gateway down"))
	b.Failure(errors.New("gateway down"))
	st = b.State()
	if st.Healthy {
		t.Error("state should report unhealthy")
	}
	if st.Failures != 2 {
		t.Errorf("consecutive_failures = %d, want 2", st.Failures)
	}
	if st.Trips != 1 {
		t.Errorf("trips = %d, want 1", st.Trips)
	}
	if st.OpenUntil.IsZero() {
		t.Error("open_until should be set while the breaker is open")
	}
	if st.LastError != "gateway down" {
		t.Errorf("last_error = %q", st.LastError)
	}

	// Trips accumulate across cycles: a breaker that opens weekly is a
	// different problem from one that opened once.
	clk.Advance(11 * time.Minute)
	b.Success()
	b.Failure(errors.New("again"))
	b.Failure(errors.New("again"))
	if st := b.State(); st.Trips != 2 {
		t.Errorf("trips = %d, want 2 after a second cycle", st.Trips)
	}
}

func TestBreakerDefaults(t *testing.T) {
	// A zero-value options struct must produce something usable rather than a
	// breaker that opens on the first failure or never closes.
	b := NewBreaker(BreakerOptions{Name: "defaults"})
	for i := 0; i < 4; i++ {
		b.Failure(errors.New("x"))
		if err := b.Allow(); err != nil {
			t.Fatalf("default threshold opened after %d failures", i+1)
		}
	}
	b.Failure(errors.New("x"))
	if b.Allow() == nil {
		t.Error("default threshold should be 5")
	}
}

func TestBreakerIsConcurrencySafe(t *testing.T) {
	b, _ := newTestBreaker(100, time.Minute)
	done := make(chan struct{})
	for i := 0; i < 8; i++ {
		go func() {
			for j := 0; j < 200; j++ {
				b.Failure(errors.New("x"))
				_ = b.Allow()
				b.Success()
				_ = b.State()
			}
			done <- struct{}{}
		}()
	}
	for i := 0; i < 8; i++ {
		<-done
	}
}

func TestIsVideoCategory(t *testing.T) {
	// 212 is not in the published category table but appears in live TV
	// searches, which is exactly why this is a range and not an allowlist.
	for _, cat := range []int{200, 201, 205, 207, 208, 209, 212, 299} {
		if !IsVideoCategory(cat) {
			t.Errorf("category %d should count as video", cat)
		}
	}
	for _, cat := range []int{0, 101, 104, 199, 300, 301, 401, 505, 601} {
		if IsVideoCategory(cat) {
			t.Errorf("category %d should not count as video", cat)
		}
	}
}

func TestVideoCategoriesCoversTheWholeRange(t *testing.T) {
	cats := VideoCategories()
	if len(cats) != 100 {
		t.Fatalf("VideoCategories() has %d entries, want 100 (200-299)", len(cats))
	}
	if cats[0] != 200 || cats[len(cats)-1] != 299 {
		t.Errorf("range is %d..%d, want 200..299", cats[0], cats[len(cats)-1])
	}
}

func TestHumanSize(t *testing.T) {
	cases := map[int64]string{
		0:           "0 B",
		512:         "512 B",
		1024:        "1.0 KiB",
		1536:        "1.5 KiB",
		1073741824:  "1.0 GiB",
		23598867291: "22.0 GiB",
	}
	for in, want := range cases {
		if got := HumanSize(in); got != want {
			t.Errorf("HumanSize(%d) = %q, want %q", in, got, want)
		}
	}
}

func TestReleaseSizeMB(t *testing.T) {
	r := Release{SizeBytes: 5 * 1024 * 1024}
	if got := r.SizeMB(); got != 5 {
		t.Errorf("SizeMB() = %d, want 5", got)
	}
}

func TestQueryString(t *testing.T) {
	if got := (Query{Recent: true}).String(); got != "<recent>" {
		t.Errorf("recent query renders as %q", got)
	}
	if got := (Query{Term: "the expanse"}).String(); got != "the expanse" {
		t.Errorf("term query renders as %q", got)
	}
}

func contains(haystack, needle string) bool { return strings.Contains(haystack, needle) }
