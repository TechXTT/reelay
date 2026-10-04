package engine

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/TechXTT/reelay/internal/clock"
	"github.com/TechXTT/reelay/internal/downloader"
	"github.com/TechXTT/reelay/internal/indexer"
	"github.com/TechXTT/reelay/internal/model"
	"github.com/TechXTT/reelay/internal/store"
)

const spaceHash = "0123456789abcdef0123456789abcdef01234567"

// countingIndexer counts queries and runs onSearch so a test can change the
// world between the cycle check and the per-release check.
type countingIndexer struct {
	fakeIndexer
	searches atomic.Int32
	onSearch func()
}

func (c *countingIndexer) Search(ctx context.Context, q indexer.Query) ([]indexer.Release, error) {
	c.searches.Add(1)
	if c.onSearch != nil {
		c.onSearch()
	}
	return c.fakeIndexer.Search(ctx, q)
}

type spaceFixture struct {
	eng     *Engine
	db      *store.Store
	client  *fakeDownloader
	idx     *countingIndexer
	clk     *clock.Fake
	movie   model.Movie
	free    atomic.Uint64
	failing atomic.Bool
}

func newSpaceFixture(t *testing.T, reserveMB int, free uint64) *spaceFixture {
	t.Helper()
	ctx := context.Background()
	now := time.Date(2026, 8, 23, 12, 0, 0, 0, time.UTC)
	fx := &spaceFixture{clk: clock.NewFake(now), client: &fakeDownloader{hash: spaceHash}}
	fx.free.Store(free)
	db, err := store.Open(ctx, store.Options{Path: filepath.Join(t.TempDir(), "space.db"),
		CacheKB: 512, Now: fx.clk.Now})
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
	fx.movie, err = db.Movies().Create(ctx, model.Movie{Title: "Arrival", Year: 2016,
		ProfileID: profile.ID, RootFolder: t.TempDir(), State: model.StateWanted}, "fixture wanted")
	if err != nil {
		t.Fatal(err)
	}
	cfg := testConfig()
	cfg.Library.TVRoot, cfg.Library.MovieRoot = t.TempDir(), t.TempDir()
	cfg.Downloader.MinFreeSpaceMB = reserveMB
	fx.idx = &countingIndexer{fakeIndexer: fakeIndexer{releases: []indexer.Release{{
		Title: "Arrival 2016 1080p WEB-DL x264-GROUP", InfoHash: spaceHash,
		Magnet: "magnet:?xt=urn:btih:" + spaceHash, SizeBytes: 4 << 30,
		Seeders: 25, Indexer: "fixture", Category: indexer.CatMoviesHD,
		PublishedAt: now.Add(-time.Hour),
	}}}}
	fx.db = db
	fx.eng, err = New(Options{Store: db, Config: cfg, Indexers: []indexer.Indexer{fx.idx},
		Downloader: fx.client, Clock: fx.clk, Logger: logger,
		PathMapper: downloader.NewPathMapper([]downloader.Mapping{
			{DownloaderPrefix: "/downloads", LocalPrefix: t.TempDir()}}),
		FreeSpace: func(string) (uint64, error) {
			if fx.failing.Load() {
				return 0, errors.New("statfs failed")
			}
			return fx.free.Load(), nil
		}})
	if err != nil {
		t.Fatal(err)
	}
	return fx
}

func (fx *spaceFixture) reloadMovie(t *testing.T) model.Movie {
	t.Helper()
	movie, err := fx.db.Movies().Get(context.Background(), fx.movie.ID)
	if err != nil {
		t.Fatal(err)
	}
	return movie
}

func (fx *spaceFixture) hasTransitionReason(t *testing.T, reason string) bool {
	t.Helper()
	history, err := fx.db.Transitions().History(context.Background(), model.SubjectMovie, fx.movie.ID, 50)
	if err != nil {
		t.Fatal(err)
	}
	for _, tr := range history {
		if tr.Reason == reason {
			return true
		}
	}
	return false
}

func TestGrabProceedsWithEnoughFreeSpace(t *testing.T) {
	fx := newSpaceFixture(t, 2048, 100<<30)
	if err := fx.eng.SearchOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	if fx.client.addCalls != 1 || fx.reloadMovie(t).State != model.StateGrabbed {
		t.Fatalf("add calls=%d state=%s", fx.client.addCalls, fx.reloadMovie(t).State)
	}
}

func TestLowDiskHoldsWithoutCountingAttemptsOrFailing(t *testing.T) {
	ctx := context.Background()
	// 4 GiB release + 2 GiB reserve needs 6 GiB; 3 GiB passes the cycle's
	// reserve-only check but not the per-release check.
	fx := newSpaceFixture(t, 2048, 3<<30)
	held := fx.clk.Now()
	if err := fx.eng.SearchOnce(ctx); err != nil {
		t.Fatal(err)
	}
	movie := fx.reloadMovie(t)
	if fx.client.addCalls != 0 || movie.State != model.StateWanted || movie.SearchAttempts != 0 {
		t.Fatalf("add calls=%d state=%s attempts=%d", fx.client.addCalls, movie.State, movie.SearchAttempts)
	}
	if !fx.hasTransitionReason(t, "low_disk_space") {
		t.Fatal("no low_disk_space transition recorded")
	}
	want := held.Add(15 * time.Minute)
	if movie.NextSearchAt == nil || movie.NextSearchAt.Sub(want).Abs() > time.Second {
		t.Fatalf("next_search_at=%v, want about %v", movie.NextSearchAt, want)
	}
	if movie.LastError == "" {
		t.Fatal("last_error not set to the hold detail")
	}

	// Repeated holds well past search_give_up_after must never fail the item.
	for range 3 {
		fx.clk.Advance(31 * 24 * time.Hour)
		if err := fx.eng.SearchOnce(ctx); err != nil {
			t.Fatal(err)
		}
		movie = fx.reloadMovie(t)
		if movie.State != model.StateWanted || movie.SearchAttempts != 0 {
			t.Fatalf("after long hold state=%s attempts=%d", movie.State, movie.SearchAttempts)
		}
	}
	if fx.client.addCalls != 0 {
		t.Fatalf("add calls=%d, want 0", fx.client.addCalls)
	}

	// Space returns: the next due search grabs.
	fx.free.Store(100 << 30)
	fx.clk.Advance(time.Hour)
	if err := fx.eng.SearchOnce(ctx); err != nil {
		t.Fatal(err)
	}
	if fx.client.addCalls != 1 {
		t.Fatalf("add calls=%d after space returned, want 1", fx.client.addCalls)
	}
}

func TestCycleReserveShortageSkipsIndexerQueries(t *testing.T) {
	fx := newSpaceFixture(t, 2048, 1<<30)
	if err := fx.eng.SearchOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	if fx.idx.searches.Load() != 0 || fx.client.addCalls != 0 {
		t.Fatalf("searches=%d add calls=%d, want none", fx.idx.searches.Load(), fx.client.addCalls)
	}
	movie := fx.reloadMovie(t)
	if movie.State != model.StateWanted || movie.SearchAttempts != 0 || fx.hasTransitionReason(t, "search_started") {
		t.Fatalf("item touched during skipped cycle: %+v", movie)
	}
}

func TestUnknownFreeSpaceSkipsCycleOrHolds(t *testing.T) {
	ctx := context.Background()
	fx := newSpaceFixture(t, 0, 100<<30)
	fx.failing.Store(true)
	if err := fx.eng.SearchOnce(ctx); err != nil {
		t.Fatal(err)
	}
	if fx.idx.searches.Load() != 0 {
		t.Fatalf("searches=%d, want cycle skipped on FreeSpace error", fx.idx.searches.Load())
	}

	// The error appears only after the cycle check, so the per-release check holds.
	fx.failing.Store(false)
	fx.idx.onSearch = func() { fx.failing.Store(true) }
	if err := fx.eng.SearchOnce(ctx); err != nil {
		t.Fatal(err)
	}
	movie := fx.reloadMovie(t)
	if fx.client.addCalls != 0 || movie.State != model.StateWanted || movie.SearchAttempts != 0 ||
		!fx.hasTransitionReason(t, "disk_space_unknown") {
		t.Fatalf("add calls=%d state=%s attempts=%d", fx.client.addCalls, movie.State, movie.SearchAttempts)
	}
}

func TestManualGrabLowDiskReturnsTypedErrorAndHolds(t *testing.T) {
	ctx := context.Background()
	fx := newSpaceFixture(t, 2048, 3<<30)
	release, err := fx.db.Releases().Upsert(ctx, model.StoredRelease{Indexer: "fixture",
		RawTitle: "Arrival 2016 1080p WEB-DL x264-GROUP", InfoHash: spaceHash,
		Magnet: "magnet:?xt=urn:btih:" + spaceHash, SizeBytes: 4 << 30, Seeders: 25})
	if err != nil {
		t.Fatal(err)
	}
	_, err = fx.eng.ManualGrab(ctx, model.SubjectMovie, fx.movie.ID, release.ID)
	var lowDisk *LowDiskError
	if !errors.As(err, &lowDisk) || lowDisk.Reason != "low_disk_space" || lowDisk.Detail == "" {
		t.Fatalf("ManualGrab error = %v, want *LowDiskError", err)
	}
	movie := fx.reloadMovie(t)
	if fx.client.addCalls != 0 || movie.State != model.StateWanted || movie.SearchAttempts != 0 ||
		!fx.hasTransitionReason(t, "low_disk_space") {
		t.Fatalf("add calls=%d state=%s attempts=%d", fx.client.addCalls, movie.State, movie.SearchAttempts)
	}
}
