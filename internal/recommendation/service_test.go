package recommendation

import (
	"context"
	"io"
	"log/slog"
	"path/filepath"
	"testing"
	"time"

	"github.com/TechXTT/reelay/internal/config"
	"github.com/TechXTT/reelay/internal/metadata"
	"github.com/TechXTT/reelay/internal/model"
	"github.com/TechXTT/reelay/internal/store"
)

type recommendationProviderFake struct{}

func (recommendationProviderFake) Recommendations(context.Context, string, int) ([]metadata.DiscoveryItem, error) {
	return []metadata.DiscoveryItem{{MediaType: "movie", TMDBID: 99, Title: "Candidate", VoteAverage: 8, VoteCount: 1000}}, nil
}

func (recommendationProviderFake) Similar(context.Context, string, int) ([]metadata.DiscoveryItem, error) {
	return nil, nil
}

func (recommendationProviderFake) Discover(context.Context, string) ([]metadata.DiscoveryItem, error) {
	return nil, nil
}

func (recommendationProviderFake) DiscoveryDetails(_ context.Context, _ string, id int) (metadata.DiscoveryItem, error) {
	if id == 42 {
		return metadata.DiscoveryItem{TMDBID: 42, Genres: []string{"Science Fiction"}, Keywords: []string{"space"}, People: []string{"Director A"}, Language: "en", RuntimeMinutes: 120}, nil
	}
	return metadata.DiscoveryItem{MediaType: "movie", TMDBID: 99, Title: "Candidate", Genres: []string{"Science Fiction"}, Keywords: []string{"space"}, People: []string{"Director A"}, Language: "en", RuntimeMinutes: 118, VoteAverage: 8, VoteCount: 1000}, nil
}

func TestServiceGeneratesAndPersistsEnrichedRecommendations(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 8, 24, 12, 0, 0, 0, time.UTC)
	db, err := store.Open(ctx, store.Options{Path: filepath.Join(t.TempDir(), "recommendations.db"), Now: func() time.Time { return now }})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	if err := store.Migrate(ctx, db, log); err != nil {
		t.Fatal(err)
	}
	repo := db.Recommendations()
	if err := repo.UpsertUser(ctx, model.JellyfinUser{ServerID: "server", UserID: "user", DisplayName: "User", Enabled: true}); err != nil {
		t.Fatal(err)
	}
	if err := repo.UpsertItems(ctx, []model.JellyfinItem{{ServerID: "server", ItemID: "seed", MediaType: "movie", TMDBID: 42, Title: "Seed", Present: true}}, "sync"); err != nil {
		t.Fatal(err)
	}
	if _, err := repo.AddActivities(ctx, []model.JellyfinActivity{{EventID: "favorite", ServerID: "server", UserID: "user", ItemID: "seed", EventType: "favorite", Progress: 1, OccurredAt: now}}); err != nil {
		t.Fatal(err)
	}
	cfg := config.Recommendations{Enabled: true, CandidateLimit: 20, ResultLimit: 10, SeedLimit: 5, Expiry: config.Dur(24 * time.Hour), ProviderWeight: 25, AffinityWeight: 20, PeopleWeight: 15, MultiSeedWeight: 15, RatingWeight: 10, PreferenceWeight: 5, NoveltyWeight: 10}
	service := NewService(db, recommendationProviderFake{}, cfg, func() time.Time { return now }, log)
	if err := service.Generate(ctx, "server", "user", "movie"); err != nil {
		t.Fatal(err)
	}
	values, err := repo.List(ctx, "server", "user", "movie", "active", 10, 0)
	if err != nil || len(values) != 1 {
		t.Fatalf("values=%+v err=%v", values, err)
	}
	if values[0].TMDBID != 99 || values[0].Components["people"] == 0 || values[0].Components["affinity"] == 0 || len(values[0].Reasons) == 0 {
		t.Fatalf("recommendation was not enriched: %+v", values[0])
	}
}
