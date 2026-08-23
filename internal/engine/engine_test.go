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
	"github.com/TechXTT/reelay/internal/downloader"
	"github.com/TechXTT/reelay/internal/indexer"
	"github.com/TechXTT/reelay/internal/model"
	"github.com/TechXTT/reelay/internal/store"
)

type fakeIndexer struct{ releases []indexer.Release }

func (f *fakeIndexer) Name() string                  { return "fixture" }
func (f *fakeIndexer) Healthy(context.Context) error { return nil }
func (f *fakeIndexer) Search(context.Context, indexer.Query) ([]indexer.Release, error) {
	return append([]indexer.Release(nil), f.releases...), nil
}

type fakeDownloader struct {
	hash     string
	complete bool
	added    bool
}

func (f *fakeDownloader) Add(context.Context, downloader.AddRequest) (string, error) {
	f.added = true
	return f.hash, nil
}
func (f *fakeDownloader) Status(context.Context, []string) ([]downloader.TorrentStatus, error) {
	progress, state := 0.4, downloader.StateDownloading
	if f.complete {
		progress, state = 1, downloader.StateCompleted
	}
	return []downloader.TorrentStatus{{Hash: f.hash, State: state, Progress: progress,
		Category: "reelay-movies", ContentPath: "/downloads/arrival.mkv"}}, nil
}
func (f *fakeDownloader) Remove(context.Context, string, bool) error { return nil }
func (f *fakeDownloader) Healthy(context.Context) error              { return nil }

type fakeImporter struct{ store *store.Store }

func (f fakeImporter) ImportCompleted(ctx context.Context, grabID int64) error {
	grab, err := f.store.Grabs().Get(ctx, grabID)
	if err != nil {
		return err
	}
	_, err = f.store.Transitions().Transition(ctx, grab.SubjectType, grab.SubjectID,
		model.StateImported, "fixture import", grab.ContentPath)
	return err
}

func TestWantedToImportedCycle(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	now := time.Date(2026, 8, 23, 12, 0, 0, 0, time.UTC)
	clk := clock.NewFake(now)
	db, err := store.Open(ctx, store.Options{Path: filepath.Join(t.TempDir(), "engine.db"),
		CacheKB: 512, Now: clk.Now})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	if err := store.Migrate(ctx, db, logger); err != nil {
		t.Fatal(err)
	}
	profile := model.QualityProfile{Name: "test", IsDefault: true,
		AllowedResolutions: []string{"1080p"}, AllowedSources: []string{"webdl"},
		MinSizeMB: 100, MaxSizeMB: 10000, MinSeeders: 1,
		BannedTerms: []string{"cam", "telesync"}, PreferredGroups: map[string]int{}}
	if _, err := db.Profiles().Seed(ctx, []model.QualityProfile{profile}); err != nil {
		t.Fatal(err)
	}
	profile, err = db.Profiles().Default(ctx)
	if err != nil {
		t.Fatal(err)
	}
	movie, err := db.Movies().Create(ctx, model.Movie{Title: "Arrival", Year: 2016,
		ProfileID: profile.ID, RootFolder: t.TempDir(), State: model.StateWanted}, "fixture wanted")
	if err != nil {
		t.Fatal(err)
	}
	hash := "0123456789abcdef0123456789abcdef01234567"
	client := &fakeDownloader{hash: hash}
	cfg := testConfig()
	eng, err := New(Options{Store: db, Config: cfg,
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
	if !client.added {
		t.Fatal("search did not hand the release to downloader")
	}
	movie, err = db.Movies().Get(ctx, movie.ID)
	if err != nil || movie.State != model.StateGrabbed {
		t.Fatalf("after search state=%s err=%v", movie.State, err)
	}

	clk.Advance(30 * time.Second)
	if err := eng.StatusOnce(ctx); err != nil {
		t.Fatalf("downloading status: %v", err)
	}
	movie, _ = db.Movies().Get(ctx, movie.ID)
	if movie.State != model.StateDownloading {
		t.Fatalf("during download state=%s", movie.State)
	}

	client.complete = true
	clk.Advance(30 * time.Second)
	if err := eng.StatusOnce(ctx); err != nil {
		t.Fatalf("completed status: %v", err)
	}
	movie, _ = db.Movies().Get(ctx, movie.ID)
	if movie.State != model.StateImported {
		t.Fatalf("final state=%s, want imported", movie.State)
	}
	history, err := db.Transitions().History(ctx, model.SubjectMovie, movie.ID, 20)
	if err != nil || len(history) < 6 {
		t.Fatalf("audit history len=%d err=%v", len(history), err)
	}
}

func testConfig() *config.Config {
	return &config.Config{
		Runtime: config.Runtime{SearchConcurrency: 2, MaxSSEClients: 2},
		Downloader: config.Downloader{CategoryMovies: "reelay-movies", CategoryTV: "reelay-tv",
			SavePathMovies: "/downloads", SavePathTV: "/downloads", StallTimeout: config.Dur(2 * time.Hour)},
		Schedules: config.Schedules{SearchInterval: config.Dur(15 * time.Minute),
			StatusInterval: config.Dur(30 * time.Second), MetadataInterval: config.Dur(12 * time.Hour),
			RecentInterval: config.Dur(10 * time.Minute), AirGrace: config.Dur(time.Hour),
			SearchBackoff:     []config.Duration{config.Dur(15 * time.Minute)},
			SearchGiveUpAfter: config.Dur(30 * 24 * time.Hour)},
		Scoring: config.Scoring{ResolutionWeight: 1000, SourceWeight: 500,
			GroupWeight: 300, SeederWeightMax: 150, AgePenaltyPerDay: 1, AgePenaltyMax: 365},
	}
}
