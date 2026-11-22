package store

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/TechXTT/reelay/internal/model"
)

func sessionFixture(hash string, now time.Time, ttl time.Duration) Session {
	return Session{TokenHash: hash, Role: "member", ServerID: "server", UserID: "user", DisplayName: "User",
		CreatedAt: now, ExpiresAt: now.Add(ttl), LastSeenAt: now}
}

func TestSessionsCreateGetExpireDelete(t *testing.T) {
	ctx := context.Background()
	s := migratedDomainStore(t)
	now := time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)

	if err := s.Sessions().Create(ctx, sessionFixture("live", now, time.Hour)); err != nil {
		t.Fatal(err)
	}
	if err := s.Sessions().Create(ctx, sessionFixture("old", now.Add(-2*time.Hour), time.Hour)); err != nil {
		t.Fatal(err)
	}
	if err := s.Sessions().Create(ctx, sessionFixture("live", now, time.Hour)); err == nil {
		t.Error("duplicate token hash accepted")
	}
	bad := sessionFixture("bad-role", now, time.Hour)
	bad.Role = "root"
	if err := s.Sessions().Create(ctx, bad); err == nil {
		t.Error("unknown role accepted")
	}

	got, err := s.Sessions().Get(ctx, "live", now)
	if err != nil || got.Role != "member" || got.UserID != "user" || !got.ExpiresAt.Equal(now.Add(time.Hour)) {
		t.Fatalf("Get live = %+v, %v", got, err)
	}
	if _, err := s.Sessions().Get(ctx, "old", now); !errors.Is(err, ErrNotFound) {
		t.Errorf("expired session err = %v, want ErrNotFound", err)
	}
	if _, err := s.Sessions().Get(ctx, "live", now.Add(time.Hour)); !errors.Is(err, ErrNotFound) {
		t.Errorf("session at its expiry instant err = %v, want ErrNotFound", err)
	}
	if _, err := s.Sessions().Get(ctx, "missing", now); !errors.Is(err, ErrNotFound) {
		t.Errorf("unknown session err = %v, want ErrNotFound", err)
	}

	deleted, err := s.Sessions().DeleteExpired(ctx, now)
	if err != nil || deleted != 1 {
		t.Fatalf("DeleteExpired = %d, %v, want 1", deleted, err)
	}
	if err := s.Sessions().Delete(ctx, "live"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Sessions().Get(ctx, "live", now); !errors.Is(err, ErrNotFound) {
		t.Errorf("deleted session err = %v, want ErrNotFound", err)
	}
	if err := s.Sessions().Delete(ctx, "live"); err != nil {
		t.Errorf("deleting a missing session = %v, want nil", err)
	}
}

func TestSessionTouchWritesAtMostOncePerMinute(t *testing.T) {
	ctx := context.Background()
	s := migratedDomainStore(t)
	now := time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)

	if err := s.Sessions().Create(ctx, sessionFixture("token", now, time.Hour)); err != nil {
		t.Fatal(err)
	}
	for _, step := range []struct {
		at   time.Duration
		want time.Duration
	}{{30 * time.Second, 0}, {2 * time.Minute, 2 * time.Minute}, {2*time.Minute + 20*time.Second, 2 * time.Minute}} {
		if err := s.Sessions().Touch(ctx, "token", now.Add(step.at)); err != nil {
			t.Fatal(err)
		}
		got, err := s.Sessions().Get(ctx, "token", now)
		if err != nil || !got.LastSeenAt.Equal(now.Add(step.want)) {
			t.Fatalf("after touch at +%s last_seen = %s (%v), want +%s", step.at, got.LastSeenAt, err, step.want)
		}
	}
}

func TestCountRequestedSinceAndRequestExists(t *testing.T) {
	ctx := context.Background()
	s := migratedDomainStore(t)
	now := time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)
	p, _ := s.Profiles().Default(ctx)

	for _, id := range []string{"user", "other"} {
		if err := s.Recommendations().UpsertUser(ctx, model.JellyfinUser{ServerID: "server", UserID: id, DisplayName: id, Enabled: true}); err != nil {
			t.Fatal(err)
		}
	}
	movie := func(tmdb int) int64 {
		m, err := s.Movies().Create(ctx, model.Movie{Title: "Movie", TMDBID: tmdb, ProfileID: p.ID, RootFolder: "/movies", State: model.StateWanted}, "test")
		if err != nil {
			t.Fatal(err)
		}
		return m.ID
	}
	request := func(user string, tmdb int, at time.Time) {
		_, err := s.Requests().Create(ctx, model.MediaRequest{ServerID: "server", UserID: user, MediaType: "movie", TMDBID: tmdb,
			Title: "Movie", RequestedAt: at, SubjectType: "movie", SubjectID: movie(tmdb)})
		if err != nil {
			t.Fatal(err)
		}
	}
	request("user", 1, now.Add(-30*time.Hour))
	request("user", 2, now.Add(-20*time.Hour))
	request("user", 3, now.Add(-time.Hour))
	request("other", 4, now.Add(-time.Hour))

	count, oldest, err := s.Requests().CountRequestedSince(ctx, "server", "user", now.Add(-24*time.Hour))
	if err != nil || count != 2 || !oldest.Equal(now.Add(-20*time.Hour)) {
		t.Fatalf("count = %d oldest = %s err = %v, want 2 and -20h", count, oldest, err)
	}
	if err := s.Requests().Cancel(ctx, 2); err != nil {
		t.Fatal(err)
	}
	if count, _, _ = s.Requests().CountRequestedSince(ctx, "server", "user", now.Add(-24*time.Hour)); count != 2 {
		t.Errorf("cancelled request count = %d, want it to still count", count)
	}
	count, oldest, err = s.Requests().CountRequestedSince(ctx, "server", "user", now)
	if err != nil || count != 0 || !oldest.IsZero() {
		t.Errorf("empty window = %d, %s, %v", count, oldest, err)
	}

	exists, err := s.Requests().RequestExists(ctx, "server", "user", "movie", 1)
	if err != nil || !exists {
		t.Errorf("RequestExists(existing) = %v, %v", exists, err)
	}
	if exists, _ = s.Requests().RequestExists(ctx, "server", "user", "movie", 4); exists {
		t.Error("another user's request reported as existing")
	}
}
