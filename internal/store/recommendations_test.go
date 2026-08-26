package store

import (
	"context"
	"testing"
	"time"

	"github.com/TechXTT/reelay/internal/model"
)

func TestRecommendationSyncAndEventsAreIdempotent(t *testing.T) {
	s := migratedDomainStore(t)
	ctx := context.Background()
	now := time.Date(2026, 8, 24, 12, 0, 0, 0, time.UTC)
	user := model.JellyfinUser{ServerID: "server", UserID: "user", DisplayName: "Alex", Enabled: true, LastSynced: now}
	if err := s.Recommendations().UpsertUser(ctx, user); err != nil {
		t.Fatal(err)
	}
	item := model.JellyfinItem{ServerID: "server", ItemID: "item", MediaType: "movie", TMDBID: 42, Title: "Arrival", Genres: []string{"Science Fiction"}, Present: true}
	if err := s.Recommendations().UpsertItems(ctx, []model.JellyfinItem{item}, "sync-1"); err != nil {
		t.Fatal(err)
	}
	event := model.JellyfinActivity{EventID: "event-1", ServerID: "server", UserID: "user", ItemID: "item", EventType: "completed", Progress: 1, OccurredAt: now}
	if n, err := s.Recommendations().AddActivities(ctx, []model.JellyfinActivity{event, event}); err != nil || n != 1 {
		t.Fatalf("inserted=%d err=%v", n, err)
	}
	seeds, err := s.Recommendations().PositiveSeeds(ctx, "server", "user", "movie", 12)
	if err != nil || len(seeds) != 1 || seeds[0].TMDBID != 42 {
		t.Fatalf("seeds=%+v err=%v", seeds, err)
	}
	if removed, err := s.Recommendations().CompleteSync(ctx, "server", "sync-2"); err != nil || removed != 1 {
		t.Fatalf("removed=%d err=%v", removed, err)
	}
	owned, err := s.Recommendations().OwnedTMDBIDs(ctx, "server", "movie")
	if err != nil || owned[42] {
		t.Fatalf("owned=%v err=%v", owned, err)
	}
	excluded, err := s.Recommendations().ExcludedTMDBIDs(ctx, "server", "user", "movie")
	if err != nil || !excluded[42] {
		t.Fatalf("excluded=%v err=%v", excluded, err)
	}
	rating := model.JellyfinActivity{EventID: "rating-1", ServerID: "server", UserID: "user", ItemID: "item", EventType: "rating", Progress: .2, OccurredAt: now.Add(time.Minute)}
	if _, err := s.Recommendations().AddActivities(ctx, []model.JellyfinActivity{rating}); err != nil {
		t.Fatal(err)
	}
	if seeds, err = s.Recommendations().PositiveSeeds(ctx, "server", "user", "movie", 12); err != nil || len(seeds) != 0 {
		t.Fatalf("low rating should override completed seed: seeds=%+v err=%v", seeds, err)
	}
}

func TestRecommendationActionIsIdempotent(t *testing.T) {
	s := migratedDomainStore(t)
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Second)
	_ = s.Recommendations().UpsertUser(ctx, model.JellyfinUser{ServerID: "s", UserID: "u", DisplayName: "U", Enabled: true})
	err := s.Recommendations().Replace(ctx, "s", "u", "movie", []model.Recommendation{{TMDBID: 5, Title: "Five", Score: 80, Reasons: []string{"reason"}, GeneratedAt: now, ExpiresAt: now.Add(time.Hour)}})
	if err != nil {
		t.Fatal(err)
	}
	values, _ := s.Recommendations().List(ctx, "s", "u", "movie", "active", 10, 0)
	if len(values) != 1 {
		t.Fatalf("values=%+v", values)
	}
	_, inserted, err := s.Recommendations().RecordAction(ctx, values[0].ID, "action", "dismiss")
	if err != nil || !inserted {
		t.Fatalf("first inserted=%v err=%v", inserted, err)
	}
	_, inserted, err = s.Recommendations().RecordAction(ctx, values[0].ID, "action", "dismiss")
	if err != nil || inserted {
		t.Fatalf("second inserted=%v err=%v", inserted, err)
	}
}

func TestRecommendationRatingReplacesPriorValue(t *testing.T) {
	s := migratedDomainStore(t)
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Second)
	_ = s.Recommendations().UpsertUser(ctx, model.JellyfinUser{ServerID: "s", UserID: "u", DisplayName: "U", Enabled: true})
	_ = s.Recommendations().Replace(ctx, "s", "u", "movie", []model.Recommendation{{TMDBID: 7, Title: "Seven", Score: 80, Reasons: []string{"reason"}, GeneratedAt: now, ExpiresAt: now.Add(time.Hour)}})
	values, _ := s.Recommendations().List(ctx, "s", "u", "movie", "active", 10, 0)
	if _, err := s.Recommendations().RecordRating(ctx, values[0].ID, "rating-1", 2); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Recommendations().RecordRating(ctx, values[0].ID, "rating-2", 5); err != nil {
		t.Fatal(err)
	}
	ratings, err := s.Recommendations().Ratings(ctx, "s", "u", "movie")
	if err != nil || len(ratings) != 1 || ratings[0].Rating != 5 {
		t.Fatalf("ratings=%+v err=%v", ratings, err)
	}
	excluded, err := s.Recommendations().ExcludedTMDBIDs(ctx, "s", "u", "movie")
	if err != nil || !excluded[7] {
		t.Fatalf("excluded=%+v err=%v", excluded, err)
	}
}

func TestRecommendationReplacePreservesActiveIDs(t *testing.T) {
	s := migratedDomainStore(t)
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Second)
	if err := s.Recommendations().UpsertUser(ctx, model.JellyfinUser{ServerID: "s", UserID: "u", DisplayName: "U", Enabled: true}); err != nil {
		t.Fatal(err)
	}
	recommendation := func(tmdbID int, title string, score float64) model.Recommendation {
		return model.Recommendation{TMDBID: tmdbID, Title: title, Score: score, Reasons: []string{"reason"}, GeneratedAt: now, ExpiresAt: now.Add(time.Hour)}
	}
	if err := s.Recommendations().Replace(ctx, "s", "u", "movie", []model.Recommendation{
		recommendation(1, "One", 90), recommendation(2, "Two", 80),
	}); err != nil {
		t.Fatal(err)
	}
	before, err := s.Recommendations().List(ctx, "s", "u", "movie", "active", 10, 0)
	if err != nil || len(before) != 2 {
		t.Fatalf("before=%+v err=%v", before, err)
	}
	ids := map[int]int64{}
	for _, value := range before {
		ids[value.TMDBID] = value.ID
	}
	if _, err := s.Recommendations().RecordRating(ctx, ids[1], "rating-1", 4); err != nil {
		t.Fatal(err)
	}
	if err := s.Recommendations().Replace(ctx, "s", "u", "movie", []model.Recommendation{
		recommendation(2, "Two", 85), recommendation(3, "Three", 75),
	}); err != nil {
		t.Fatal(err)
	}
	after, err := s.Recommendations().List(ctx, "s", "u", "movie", "active", 10, 0)
	if err != nil || len(after) != 2 {
		t.Fatalf("after=%+v err=%v", after, err)
	}
	if after[0].TMDBID != 2 || after[0].ID != ids[2] {
		t.Fatalf("unchanged recommendation ID changed: before=%d after=%+v", ids[2], after[0])
	}
	if _, err := s.Recommendations().RecordRating(ctx, ids[2], "rating-2", 5); err != nil {
		t.Fatalf("second action with pre-refresh ID failed: %v", err)
	}
}
