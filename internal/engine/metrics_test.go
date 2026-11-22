package engine

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/TechXTT/reelay/internal/clock"
	"github.com/TechXTT/reelay/internal/downloader"
	"github.com/TechXTT/reelay/internal/indexer"
	"github.com/TechXTT/reelay/internal/metrics"
	"github.com/TechXTT/reelay/internal/model"
	"github.com/TechXTT/reelay/internal/store"
)

func TestSearchGrabAndImportIncrementMetrics(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 8, 23, 12, 0, 0, 0, time.UTC)
	clk := clock.NewFake(now)
	db, err := store.Open(ctx, store.Options{Path: filepath.Join(t.TempDir(), "metrics.db"),
		CacheKB: 512, Now: clk.Now})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	if err := store.Migrate(ctx, db, logger); err != nil {
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
	if _, err := db.Movies().Create(ctx, model.Movie{Title: "Arrival", Year: 2016,
		ProfileID: profile.ID, RootFolder: t.TempDir(), State: model.StateWanted}, "fixture wanted"); err != nil {
		t.Fatal(err)
	}
	hash := "0123456789abcdef0123456789abcdef01234567"
	client := &fakeDownloader{hash: hash}
	eng, err := New(Options{Store: db, Config: testConfig(),
		Indexers: []indexer.Indexer{&fakeIndexer{releases: []indexer.Release{{
			Title: "Arrival 2016 1080p WEB-DL x264-GROUP", InfoHash: hash,
			Magnet: "magnet:?xt=urn:btih:" + hash, SizeBytes: 4 << 30,
			Seeders: 25, Indexer: "fixture", Category: indexer.CatMoviesHD,
			PublishedAt: now.Add(-time.Hour),
		}}}}, Downloader: client, Importer: fakeImporter{store: db}, Clock: clk, Logger: logger})
	if err != nil {
		t.Fatal(err)
	}

	if err := eng.SearchOnce(ctx); err != nil {
		t.Fatalf("search: %v", err)
	}
	m := eng.Metrics()
	if got := m.searchOutcomes.Value("grabbed"); got != 1 {
		t.Fatalf("search outcome grabbed = %d, want 1", got)
	}
	if got := m.grabsStarted.Value("search"); got != 1 {
		t.Fatalf("grabs started search = %d, want 1", got)
	}
	if m.searchOutcomes.Value("no_match")+m.searchOutcomes.Value("error") != 0 {
		t.Fatal("unexpected non-grab search outcome")
	}

	client.complete = true
	clk.Advance(30 * time.Second)
	if err := eng.StatusOnce(ctx); err != nil {
		t.Fatalf("status: %v", err)
	}
	if got := m.imports.Value("ok"); got != 1 {
		t.Fatalf("imports ok = %d, want 1", got)
	}
	var buf bytes.Buffer
	m.Write(metrics.NewWriter(&buf))
	for _, want := range []string{
		`reelay_search_outcomes_total{outcome="grabbed"} 1`,
		`reelay_grabs_started_total{source="search"} 1`,
		`reelay_imports_total{result="ok"} 1`,
		`reelay_import_duration_seconds_bucket{le="+Inf"} 1`,
		`reelay_import_duration_seconds_count 1`,
	} {
		if !strings.Contains(buf.String(), want) {
			t.Errorf("metrics output missing %q:\n%s", want, buf.String())
		}
	}
}

func TestStallIncrementsGrabFailureMetric(t *testing.T) {
	f := newRecoveryFixture(t, model.GrabDownloading, model.StateDownloading, 0.4)
	f.client.statuses = []downloader.TorrentStatus{{Hash: f.grab.TorrentHash,
		State: downloader.StateDownloading, Progress: 0.4}}
	f.clock.Advance(testConfig().Downloader.StallTimeout.Duration)

	if err := f.engine.StatusOnce(f.ctx); err != nil {
		t.Fatal(err)
	}
	if got := f.engine.Metrics().grabFailures.Value("stalled"); got != 1 {
		t.Fatalf("grab failures stalled = %d, want 1", got)
	}
	if got := f.engine.Metrics().grabFailures.Value("vanished"); got != 0 {
		t.Fatalf("grab failures vanished = %d, want 0", got)
	}
}
