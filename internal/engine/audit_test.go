package engine

import (
	"context"
	"io"
	"log/slog"
	"path/filepath"
	"testing"
	"time"

	"github.com/TechXTT/reelay/internal/clock"
	"github.com/TechXTT/reelay/internal/config"
	"github.com/TechXTT/reelay/internal/model"
	"github.com/TechXTT/reelay/internal/store"
)

func TestPruneAuditOnceDeletesOldTransitions(t *testing.T) {
	ctx := context.Background()
	clk := clock.NewFake(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC))
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	db, err := store.Open(ctx, store.Options{Path: filepath.Join(t.TempDir(), "engine.db"), CacheKB: 512, Now: clk.Now})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := store.Migrate(ctx, db, logger); err != nil {
		t.Fatal(err)
	}
	cfg := testConfig()
	cfg.Runtime.AuditRetention = config.Dur(30 * 24 * time.Hour)
	eng, err := New(Options{Store: db, Config: cfg, Downloader: &fakeDownloader{}, Clock: clk, Logger: logger})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Profiles().Seed(ctx, []model.QualityProfile{{Name: "test", IsDefault: true,
		AllowedResolutions: []string{"1080p"}, AllowedSources: []string{"webdl"},
		MinSizeMB: 100, MaxSizeMB: 10000, MinSeeders: 1, PreferredGroups: map[string]int{}}}); err != nil {
		t.Fatal(err)
	}
	profile, err := db.Profiles().Default(ctx)
	if err != nil {
		t.Fatal(err)
	}
	movie, err := db.Movies().Create(ctx, model.Movie{Title: "Primer", Year: 2004,
		ProfileID: profile.ID, RootFolder: "/movies", State: model.StateWanted}, "added")
	if err != nil {
		t.Fatal(err)
	}
	for _, to := range []model.ItemState{model.StateSearching, model.StateWanted} {
		if _, err := db.Transitions().Transition(ctx, model.SubjectMovie, movie.ID, to, "test", ""); err != nil {
			t.Fatal(err)
		}
	}
	clk.Advance(60 * 24 * time.Hour)
	if _, err := db.Transitions().Transition(ctx, model.SubjectMovie, movie.ID, model.StateSearching, "test", ""); err != nil {
		t.Fatal(err)
	}

	if err := eng.PruneAuditOnce(ctx); err != nil {
		t.Fatal(err)
	}
	hist, err := db.Transitions().History(ctx, model.SubjectMovie, movie.ID, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(hist) != 1 || hist[0].To != model.StateSearching {
		t.Fatalf("history after prune = %+v, want only the recent transition", hist)
	}
}
