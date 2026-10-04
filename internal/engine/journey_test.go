package engine

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"github.com/TechXTT/reelay/internal/clock"
	"github.com/TechXTT/reelay/internal/metadata"
	"github.com/TechXTT/reelay/internal/model"
	"github.com/TechXTT/reelay/internal/store"
)

func journeyStore(t *testing.T, now func() time.Time) (*store.Store, *slog.Logger, model.QualityProfile) {
	t.Helper()
	ctx := context.Background()
	db, err := store.Open(ctx, store.Options{Path: filepath.Join(t.TempDir(), "journey.db"), Now: now})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	if err := store.Migrate(ctx, db, log); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Profiles().Seed(ctx, []model.QualityProfile{{Name: "test", IsDefault: true, AllowedResolutions: []string{"1080p"}, AllowedSources: []string{"webdl"}, MinSeeders: 1}}); err != nil {
		t.Fatal(err)
	}
	profile, err := db.Profiles().Default(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Recommendations().UpsertUser(ctx, model.JellyfinUser{ServerID: "server", UserID: "user", DisplayName: "User", Enabled: true}); err != nil {
		t.Fatal(err)
	}
	return db, log, profile
}

func TestSelectedSeasonsRespectAirDateAndGrace(t *testing.T) {
	ctx := context.Background()
	clk := clock.NewFake(time.Date(2026, 10, 4, 12, 0, 0, 0, time.UTC))
	db, log, profile := journeyStore(t, clk.Now)
	series, err := db.Series().Create(ctx, model.Series{Title: "Selected", TVmazeID: 1, TMDBID: 1, MonitorMode: model.MonitorNone, Status: model.SeriesFollowing, ProfileID: profile.ID, RootFolder: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Requests().Create(ctx, model.MediaRequest{ServerID: "server", UserID: "user", MediaType: "series", TMDBID: 1, Title: "Selected", SubjectType: "series", SubjectID: series.ID}); err != nil {
		t.Fatal(err)
	}
	if err := db.Requests().AddSeasons(ctx, "server", "user", 1, series.ID, []int{0, 2}); err != nil {
		t.Fatal(err)
	}
	aired, recent, future := clk.Now().Add(-24*time.Hour), clk.Now().Add(-30*time.Minute), clk.Now().Add(24*time.Hour)
	provider := metadataEpisodeFixture{episodes: []metadata.Episode{
		{Season: 0, Number: 1, Title: "Special", AirDate: &aired},
		{Season: 1, Number: 1, Title: "Unselected", AirDate: &aired},
		{Season: 2, Number: 1, Title: "Aired", AirDate: &aired},
		{Season: 2, Number: 2, Title: "Grace", AirDate: &recent},
		{Season: 2, Number: 3, Title: "Future", AirDate: &future},
		{Season: 2, Number: 4, Title: "Unknown date"},
	}}
	eng, err := New(Options{Store: db, Config: testConfig(), Downloader: &fakeDownloader{}, TVmaze: provider, Clock: clk, Logger: log})
	if err != nil {
		t.Fatal(err)
	}
	if err := eng.MetadataOnce(ctx); err != nil {
		t.Fatal(err)
	}
	for _, episode := range provider.episodes {
		stored, err := db.Episodes().BySeriesNumber(ctx, series.ID, episode.Season, episode.Number)
		wanted := episode.Season != 1 && episode.Number == 1
		if err != nil || (stored.State == model.StateWanted) != wanted {
			t.Fatalf("%s: state=%s error=%v", episode.Title, stored.State, err)
		}
	}
	clk.Advance(time.Hour)
	if err := eng.MetadataOnce(ctx); err != nil {
		t.Fatal(err)
	}
	stored, err := db.Episodes().BySeriesNumber(ctx, series.ID, 2, 2)
	if err != nil || stored.State != model.StateWanted {
		t.Fatalf("grace did not expire: %+v %v", stored, err)
	}
}

func TestAvailabilityWebhookRetriesWithStableEventID(t *testing.T) {
	ctx := context.Background()
	clk := clock.NewFake(time.Date(2026, 10, 4, 12, 0, 0, 0, time.UTC))
	db, log, profile := journeyStore(t, clk.Now)
	movie, err := db.Movies().Create(ctx, model.Movie{Title: "Available", TMDBID: 1, ProfileID: profile.ID, RootFolder: t.TempDir(), State: model.StateWanted}, "fixture")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Requests().Create(ctx, model.MediaRequest{ServerID: "server", UserID: "user", MediaType: "movie", TMDBID: 1, Title: "Available", SubjectType: "movie", SubjectID: movie.ID}); err != nil {
		t.Fatal(err)
	}
	if err := db.Recommendations().UpsertItems(ctx, []model.JellyfinItem{{ServerID: "server", ItemID: "playable", MediaType: "movie", TMDBID: 1, Title: "Available", Present: true}}, "sync"); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Recommendations().CompleteSync(ctx, "server", "sync"); err != nil {
		t.Fatal(err)
	}
	var eventIDs []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		eventIDs = append(eventIDs, r.Header.Get("X-Reelay-Event-ID"))
		if r.Method != "POST" || r.Header.Get("Content-Type") != "application/json" {
			t.Error("incorrect webhook request")
		}
		if len(eventIDs) == 1 {
			w.WriteHeader(503)
		} else {
			w.WriteHeader(204)
		}
	}))
	defer server.Close()
	cfg := testConfig()
	cfg.Availability.WebhookURL = server.URL
	eng, err := New(Options{Store: db, Config: cfg, Downloader: &fakeDownloader{}, Clock: clk, Logger: log})
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		if err := eng.NotificationsOnce(ctx); err != nil {
			t.Fatal(err)
		}
	}
	if len(eventIDs) != 1 {
		t.Fatalf("retried before due: %v", eventIDs)
	}
	clk.Advance(time.Minute)
	if err := eng.NotificationsOnce(ctx); err != nil {
		t.Fatal(err)
	}
	if len(eventIDs) != 2 || eventIDs[0] == "" || eventIDs[0] != eventIDs[1] {
		t.Fatalf("event ID changed: %v", eventIDs)
	}
	if _, err := db.Requests().ClaimNotification(ctx); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("delivered event still pending: %v", err)
	}
}

func TestAvailabilityNtfyRetriesWithReadableMessage(t *testing.T) {
	ctx := context.Background()
	clk := clock.NewFake(time.Date(2026, 10, 4, 12, 0, 0, 0, time.UTC))
	db, log, profile := journeyStore(t, clk.Now)
	movie, err := db.Movies().Create(ctx, model.Movie{Title: "Available", TMDBID: 1, ProfileID: profile.ID, RootFolder: t.TempDir(), State: model.StateWanted}, "fixture")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Requests().Create(ctx, model.MediaRequest{ServerID: "server", UserID: "user", MediaType: "movie", TMDBID: 1, Title: "Available", SubjectType: "movie", SubjectID: movie.ID}); err != nil {
		t.Fatal(err)
	}
	if err := db.Recommendations().UpsertItems(ctx, []model.JellyfinItem{{ServerID: "server", ItemID: "playable", MediaType: "movie", TMDBID: 1, Title: "Available", Present: true}}, "sync"); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Recommendations().CompleteSync(ctx, "server", "sync"); err != nil {
		t.Fatal(err)
	}
	var eventIDs []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		eventIDs = append(eventIDs, r.Header.Get("X-Reelay-Event-ID"))
		body, _ := io.ReadAll(r.Body)
		if string(body) != "Available is available in Jellyfin." || r.Header.Get("Title") != "Reelay: ready to watch" {
			t.Errorf("incorrect ntfy message: %s", body)
		}
		if r.Method != "POST" || r.Header.Get("Content-Type") != "text/plain; charset=utf-8" {
			t.Error("incorrect webhook request")
		}
		if len(eventIDs) == 1 {
			w.WriteHeader(503)
		} else {
			w.WriteHeader(204)
		}
	}))
	defer server.Close()
	cfg := testConfig()
	cfg.Availability.WebhookURL = server.URL
	cfg.Availability.WebhookFormat = "ntfy"
	eng, err := New(Options{Store: db, Config: cfg, Downloader: &fakeDownloader{}, Clock: clk, Logger: log})
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		if err := eng.NotificationsOnce(ctx); err != nil {
			t.Fatal(err)
		}
	}
	if len(eventIDs) != 1 {
		t.Fatalf("retried before due: %v", eventIDs)
	}
	clk.Advance(time.Minute)
	if err := eng.NotificationsOnce(ctx); err != nil {
		t.Fatal(err)
	}
	if len(eventIDs) != 2 || eventIDs[0] == "" || eventIDs[0] != eventIDs[1] {
		t.Fatalf("event ID changed: %v", eventIDs)
	}
	if _, err := db.Requests().ClaimNotification(ctx); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("delivered event still pending: %v", err)
	}
}
