package store

import (
	"context"
	"testing"
	"time"

	"github.com/TechXTT/reelay/internal/model"
)

func TestHoldLockedDoesNotCountAttemptsAndKeepsImportedItems(t *testing.T) {
	ctx := context.Background()
	s := migratedDomainStore(t)
	p, _ := s.Profiles().Default(ctx)
	movie, err := s.Movies().Create(ctx, model.Movie{Title: "Primer", Year: 2004,
		ProfileID: p.ID, RootFolder: "/movies", State: model.StateWanted}, "added")
	if err != nil {
		t.Fatal(err)
	}
	next := time.Now().Add(time.Hour).UTC()
	search := func(hold bool) model.StateTransition {
		t.Helper()
		if _, err := s.Transitions().Transition(ctx, model.SubjectMovie, movie.ID,
			model.StateSearching, "search", ""); err != nil {
			t.Fatal(err)
		}
		lock, err := s.Locks().Acquire(ctx, model.SubjectMovie, movie.ID, "test", time.Minute)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = lock.Release(ctx) }()
		if hold {
			tr, err := s.Transitions().HoldLocked(ctx, lock, next, "low_disk_space", "need more")
			if err != nil {
				t.Fatal(err)
			}
			return tr
		}
		tr, err := s.Transitions().SearchRetryLocked(ctx, lock, next, "no_acceptable_release", "none", false)
		if err != nil {
			t.Fatal(err)
		}
		return tr
	}
	attempts := func() int {
		t.Helper()
		got, err := s.Movies().Get(ctx, movie.ID)
		if err != nil {
			t.Fatal(err)
		}
		return got.SearchAttempts
	}

	tr := search(true)
	if tr.To != model.StateWanted || tr.Reason != "low_disk_space" || attempts() != 0 {
		t.Fatalf("hold = %+v attempts=%d", tr, attempts())
	}
	got, _ := s.Movies().Get(ctx, movie.ID)
	if got.NextSearchAt == nil || got.NextSearchAt.Sub(next).Abs() > time.Second ||
		got.LastError != "need more" {
		t.Fatalf("hold schedule = next %v error %q", got.NextSearchAt, got.LastError)
	}
	search(false)
	if attempts() != 1 {
		t.Fatalf("retry attempts = %d, want 1", attempts())
	}
	search(true)
	if attempts() != 1 {
		t.Fatalf("attempts after second hold = %d, want 1", attempts())
	}

	for _, state := range []model.ItemState{model.StateSearching, model.StateGrabbed,
		model.StateDownloading, model.StateImporting} {
		if _, err := s.Transitions().Transition(ctx, model.SubjectMovie, movie.ID,
			state, "fixture lifecycle", ""); err != nil {
			t.Fatal(err)
		}
	}
	lock, err := s.Locks().Acquire(ctx, model.SubjectMovie, movie.ID, "import", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Transitions().MarkImportedLocked(ctx, lock, "/movies/Primer/movie.mkv",
		"1080p bluray x264", "fixture import"); err != nil {
		t.Fatal(err)
	}
	_ = lock.Release(ctx)
	if err := s.Transitions().RequestSearchNow(ctx, model.SubjectMovie, movie.ID, "upgrade search"); err != nil {
		t.Fatal(err)
	}
	tr = search(true)
	got, _ = s.Movies().Get(ctx, movie.ID)
	if tr.To != model.StateImported || got.State != model.StateImported ||
		got.ImportedPath == "" || got.NextSearchAt != nil || got.SearchAttempts != 0 {
		t.Fatalf("imported hold = %+v movie=%+v", tr, got)
	}
}
