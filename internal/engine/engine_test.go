package engine

import (
	"context"
	"errors"
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
	hash         string
	complete     bool
	added        bool
	addCalls     int
	lastAdd      downloader.AddRequest
	pauseCalls   int
	paused       bool
	pausedHashes []string
	pausedState  bool
}

func (f *fakeDownloader) Add(_ context.Context, req downloader.AddRequest) (string, error) {
	f.added = true
	f.addCalls++
	f.lastAdd = req
	return f.hash, nil
}
func (f *fakeDownloader) Status(context.Context, []string) ([]downloader.TorrentStatus, error) {
	progress, state := 0.4, downloader.StateDownloading
	if f.pausedState {
		progress, state = 0, downloader.StatePaused
	}
	if f.complete {
		progress, state = 1, downloader.StateCompleted
	}
	return []downloader.TorrentStatus{{Hash: f.hash, State: state, Progress: progress,
		Category: "reelay-movies", ContentPath: "/downloads/arrival.mkv"}}, nil
}
func (f *fakeDownloader) Remove(context.Context, string, bool) error { return nil }
func (f *fakeDownloader) SetPaused(_ context.Context, hashes []string, paused bool) error {
	f.pauseCalls++
	f.paused = paused
	f.pausedHashes = append([]string(nil), hashes...)
	return nil
}
func (f *fakeDownloader) Healthy(context.Context) error { return nil }

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
	if err := eng.ForceSearch(ctx, model.SubjectMovie, movie.ID); !errors.Is(err, store.ErrItemBusy) {
		t.Fatalf("repeat search while grabbed = %v, want ErrItemBusy", err)
	}
	client.pausedState = true
	clk.Advance(3 * time.Hour)
	if err := eng.StatusOnce(ctx); err != nil {
		t.Fatalf("paused torrent treated as stalled: %v", err)
	}
	movie, _ = db.Movies().Get(ctx, movie.ID)
	if movie.State != model.StateDownloading {
		t.Fatalf("paused torrent state=%s, want downloading", movie.State)
	}
	client.pausedState = false

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

	// Legacy databases may contain a duplicate active grab. Observing its
	// completed torrent after the subject imported must be idempotent.
	grabs, err := db.Grabs().History(ctx, 10, 0)
	if err != nil || len(grabs) != 1 {
		t.Fatalf("grab history = %+v, %v", grabs, err)
	}
	stale := grabs[0]
	stale.State = model.GrabImporting
	if err := db.Grabs().Update(ctx, stale); err != nil {
		t.Fatal(err)
	}
	if err := eng.StatusOnce(ctx); err != nil {
		t.Fatalf("idempotent completion: %v", err)
	}
	stale, _ = db.Grabs().Get(ctx, stale.ID)
	if stale.State != model.GrabImported || stale.LastError != "" {
		t.Fatalf("stale completed grab = %+v", stale)
	}
}

func TestDownloadPausePersistsAndAppliesToNewGrabs(t *testing.T) {
	ctx := context.Background()
	db, err := store.Open(ctx, store.Options{Path: filepath.Join(t.TempDir(), "pause.db"), CacheKB: 512})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	if err := store.Migrate(ctx, db, logger); err != nil {
		t.Fatal(err)
	}
	client := &fakeDownloader{hash: "0123456789abcdef0123456789abcdef01234567"}
	eng, err := New(Options{Store: db, Config: testConfig(), Downloader: client, Logger: logger})
	if err != nil {
		t.Fatal(err)
	}
	count, err := eng.SetDownloadsPaused(ctx, true)
	if err != nil || count != 0 || client.pauseCalls != 1 || !client.paused {
		t.Fatalf("pause count=%d calls=%d paused=%t err=%v", count, client.pauseCalls, client.paused, err)
	}
	paused, err := eng.DownloadsPaused(ctx)
	if err != nil || !paused {
		t.Fatalf("persisted pause=%t err=%v", paused, err)
	}
	if _, err := eng.addDownload(ctx, downloader.AddRequest{Magnet: "magnet:?xt=urn:btih:" + client.hash}); err != nil {
		t.Fatal(err)
	}
	if !client.lastAdd.Paused {
		t.Fatal("new grab did not inherit the global pause state")
	}
	if _, err := eng.SetDownloadsPaused(ctx, false); err != nil {
		t.Fatal(err)
	}
	paused, err = eng.DownloadsPaused(ctx)
	if err != nil || paused || client.paused {
		t.Fatalf("resume persisted=%t client=%t err=%v", paused, client.paused, err)
	}
}

func TestImportedQualityParsing(t *testing.T) {
	got := importedQuality("1080p bluray av1 proper")
	if got == nil || got.Resolution != "1080p" || got.Source != "bluray" || !got.Proper || got.Repack {
		t.Fatalf("imported quality = %+v", got)
	}
	if got := importedQuality(""); got != nil {
		t.Fatalf("empty quality = %+v, want nil", got)
	}
}

func TestSeasonPackCreatesOneGrabAndReservesCoveredEpisodes(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 8, 25, 12, 0, 0, 0, time.UTC)
	clk := clock.NewFake(now)
	db, err := store.Open(ctx, store.Options{Path: filepath.Join(t.TempDir(), "pack.db"),
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
		MinSizeMB: 100, MaxSizeMB: 10000, MinSeeders: 1,
		PreferredGroups: map[string]int{}}}); err != nil {
		t.Fatal(err)
	}
	profile, _ := db.Profiles().Default(ctx)
	series, err := db.Series().Create(ctx, model.Series{Title: "Example Show", Year: 2020,
		ProfileID: profile.ID, RootFolder: t.TempDir(), MonitorMode: model.MonitorAll,
		Status: model.SeriesFollowing})
	if err != nil {
		t.Fatal(err)
	}
	first, err := db.Episodes().Create(ctx, model.Episode{SeriesID: series.ID, Season: 1,
		Number: 1, State: model.StateWanted}, "fixture")
	if err != nil {
		t.Fatal(err)
	}
	second, err := db.Episodes().Create(ctx, model.Episode{SeriesID: series.ID, Season: 1,
		Number: 2, State: model.StateWanted}, "fixture")
	if err != nil {
		t.Fatal(err)
	}
	hash := "1123456789abcdef0123456789abcdef01234567"
	client := &fakeDownloader{hash: hash}
	eng, err := New(Options{Store: db, Config: testConfig(),
		Indexers: []indexer.Indexer{&fakeIndexer{releases: []indexer.Release{{
			Title: "Example Show S01 1080p WEB-DL x264-GROUP", InfoHash: hash,
			Magnet: "magnet:?xt=urn:btih:" + hash, SizeBytes: 8 << 30,
			Seeders: 25, Indexer: "fixture", Category: indexer.CatTVShowsHD,
			PublishedAt: now.Add(-time.Hour), Files: 2,
		}}}}, Downloader: client, Clock: clk, Logger: logger})
	if err != nil {
		t.Fatal(err)
	}
	if err := eng.SearchOnce(ctx); err != nil {
		t.Fatal(err)
	}
	if client.addCalls != 1 {
		t.Fatalf("downloader Add calls=%d, want 1", client.addCalls)
	}
	grabs, err := db.Grabs().Active(ctx)
	if err != nil || len(grabs) != 1 {
		t.Fatalf("active grabs=%d err=%v, want one shared grab", len(grabs), err)
	}
	for _, id := range []int64{first.ID, second.ID} {
		episode, err := db.Episodes().Get(ctx, id)
		if err != nil || episode.State != model.StateGrabbed || episode.ChosenReleaseID == 0 {
			t.Fatalf("episode %d = %+v err=%v", id, episode, err)
		}
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
