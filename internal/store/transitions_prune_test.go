package store

import (
	"context"
	"database/sql"
	"testing"
	"time"

	"github.com/TechXTT/reelay/internal/model"
)

func seedTransitions(t *testing.T, s *Store, subject model.SubjectType, id int64, ats ...time.Time) {
	t.Helper()
	err := s.InTx(context.Background(), func(tx *sql.Tx) error {
		for _, at := range ats {
			if err := insertTransition(context.Background(), tx, subject, id,
				model.StateWanted, model.StateSearching, "test", "", at); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

func countTransitions(t *testing.T, s *Store, subject model.SubjectType, id int64) int {
	t.Helper()
	var n int
	err := s.Reader().QueryRow(`SELECT COUNT(*) FROM state_transitions
 WHERE subject_type = ? AND subject_id = ?`, subject, id).Scan(&n)
	if err != nil {
		t.Fatal(err)
	}
	return n
}

func TestPruneBeforeKeepsLatestTransitionPerItem(t *testing.T) {
	ctx := context.Background()
	s := migratedDomainStore(t)
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	cutoff := base.Add(100 * 24 * time.Hour)
	day := func(n int) time.Time { return base.Add(time.Duration(n) * 24 * time.Hour) }

	// Movie 1: two old rows and one recent row; only the recent one must survive
	// alongside nothing else old, since the latest is not older than the cutoff.
	seedTransitions(t, s, model.SubjectMovie, 1, day(1), day(2), day(150))
	// Movie 2: every row is older than the cutoff; the latest one is kept.
	seedTransitions(t, s, model.SubjectMovie, 2, day(3), day(4), day(5))
	// Episode 1 shares an id with movie 1 and must be tracked separately.
	seedTransitions(t, s, model.SubjectEpisode, 1, day(6))

	deleted, err := s.Transitions().PruneBefore(ctx, cutoff)
	if err != nil {
		t.Fatal(err)
	}
	if deleted != 4 {
		t.Fatalf("deleted = %d, want 4", deleted)
	}
	if got := countTransitions(t, s, model.SubjectMovie, 1); got != 1 {
		t.Fatalf("movie 1 rows = %d, want 1", got)
	}
	hist, err := s.Transitions().History(ctx, model.SubjectMovie, 2, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(hist) != 1 || !hist[0].At.Equal(day(5)) {
		t.Fatalf("movie 2 history = %+v, want only the day 5 row", hist)
	}
	if got := countTransitions(t, s, model.SubjectEpisode, 1); got != 1 {
		t.Fatalf("episode 1 rows = %d, want 1", got)
	}

	deleted, err = s.Transitions().PruneBefore(ctx, cutoff)
	if err != nil || deleted != 0 {
		t.Fatalf("second prune = %d, %v; want 0, nil", deleted, err)
	}
}

func TestPruneBeforeDeletesAcrossBatches(t *testing.T) {
	ctx := context.Background()
	s := migratedDomainStore(t)
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	total := 2*pruneBatchSize + 5
	ats := make([]time.Time, total)
	for i := range ats {
		ats[i] = base.Add(time.Duration(i) * time.Second)
	}
	seedTransitions(t, s, model.SubjectMovie, 7, ats...)

	deleted, err := s.Transitions().PruneBefore(ctx, base.Add(24*time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if deleted != int64(total-1) {
		t.Fatalf("deleted = %d, want %d", deleted, total-1)
	}
	if got := countTransitions(t, s, model.SubjectMovie, 7); got != 1 {
		t.Fatalf("rows left = %d, want 1", got)
	}
}

func TestPruneBeforeHonoursCancelledContext(t *testing.T) {
	s := migratedDomainStore(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	if _, err := s.Transitions().PruneBefore(ctx, time.Now()); err == nil {
		t.Fatal("PruneBefore with cancelled context succeeded")
	}
}
