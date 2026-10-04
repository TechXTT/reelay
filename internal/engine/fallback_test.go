package engine

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/TechXTT/reelay/internal/clock"
	"github.com/TechXTT/reelay/internal/downloader"
	"github.com/TechXTT/reelay/internal/indexer"
	"github.com/TechXTT/reelay/internal/model"
	"github.com/TechXTT/reelay/internal/store"
)

// stallDownloader derives each torrent hash from the added magnet and reports
// every torrent as downloading with no progress, or complete when set.
type stallDownloader struct {
	adds     []downloader.AddRequest
	complete bool
}

func (d *stallDownloader) Add(_ context.Context, req downloader.AddRequest) (string, error) {
	d.adds = append(d.adds, req)
	return strings.TrimPrefix(req.Magnet, "magnet:?xt=urn:btih:"), nil
}

func (d *stallDownloader) Status(_ context.Context, hashes []string) ([]downloader.TorrentStatus, error) {
	out := make([]downloader.TorrentStatus, len(hashes))
	for i, hash := range hashes {
		out[i] = downloader.TorrentStatus{Hash: hash, State: downloader.StateDownloading}
		if d.complete {
			out[i].State, out[i].Progress = downloader.StateCompleted, 1
		}
	}
	return out, nil
}

func (*stallDownloader) Remove(context.Context, string, bool) error      { return nil }
func (*stallDownloader) SetPaused(context.Context, []string, bool) error { return nil }
func (*stallDownloader) Healthy(context.Context) error                   { return nil }

type failingImporter struct{}

func (failingImporter) ImportCompleted(context.Context, int64) error {
	return errors.New("import boom")
}

type fallbackCandidate struct {
	accepted bool
	score    int
}

type fallbackFixture struct {
	ctx      context.Context
	clk      *clock.Fake
	db       *store.Store
	eng      *Engine
	client   *stallDownloader
	free     atomic.Uint64
	movie    model.Movie
	releases []model.StoredRelease
}

func fallbackHash(i int) string { return fmt.Sprintf("%040x", i+1) }

// newFallbackFixture creates a downloading movie whose active grab is the
// release of specs[0], with every spec stored as a candidate evaluation.
func newFallbackFixture(t *testing.T, imp Importer, specs []fallbackCandidate) *fallbackFixture {
	t.Helper()
	ctx := context.Background()
	fx := &fallbackFixture{ctx: ctx, client: &stallDownloader{},
		clk: clock.NewFake(time.Date(2026, 10, 3, 12, 0, 0, 0, time.UTC))}
	fx.free.Store(100 << 30)
	db, err := store.Open(ctx, store.Options{Path: filepath.Join(t.TempDir(), "fallback.db"),
		CacheKB: 512, Now: fx.clk.Now})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	fx.db = db
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
		ProfileID: profile.ID, RootFolder: t.TempDir(), State: model.StateDownloading}, "fixture")
	if err != nil {
		t.Fatal(err)
	}
	evaluations := make([]model.CandidateEvaluation, len(specs))
	for i, spec := range specs {
		release, err := db.Releases().Upsert(ctx, model.StoredRelease{Indexer: "fixture",
			InfoHash: fallbackHash(i), RawTitle: fmt.Sprintf("Arrival 2016 1080p WEB-DL x264-G%d", i),
			Magnet: "magnet:?xt=urn:btih:" + fallbackHash(i), SizeBytes: 4 << 30, Seeders: 25})
		if err != nil {
			t.Fatal(err)
		}
		fx.releases = append(fx.releases, release)
		evaluations[i] = model.CandidateEvaluation{SubjectType: model.SubjectMovie, SubjectID: fx.movie.ID,
			ReleaseID: release.ID, Accepted: spec.accepted, Score: spec.score}
	}
	if err := db.Decisions().ReplaceCandidates(ctx, model.SubjectMovie, fx.movie.ID, evaluations); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Grabs().Create(ctx, model.Grab{SubjectType: model.SubjectMovie, SubjectID: fx.movie.ID,
		ReleaseID: fx.releases[0].ID, TorrentHash: fx.releases[0].InfoHash, Category: "reelay-movies",
		State: model.GrabDownloading}); err != nil {
		t.Fatal(err)
	}
	cfg := testConfig()
	cfg.Library.TVRoot, cfg.Library.MovieRoot = t.TempDir(), t.TempDir()
	cfg.Downloader.MinFreeSpaceMB = 2048
	fx.eng, err = New(Options{Store: db, Config: cfg, Downloader: fx.client, Importer: imp,
		Clock: fx.clk, Logger: logger,
		PathMapper: downloader.NewPathMapper([]downloader.Mapping{
			{DownloaderPrefix: "/downloads", LocalPrefix: t.TempDir()}}),
		FreeSpace: func(string) (uint64, error) { return fx.free.Load(), nil }})
	if err != nil {
		t.Fatal(err)
	}
	return fx
}

// stall lets the stall timeout elapse and runs one status poll.
func (fx *fallbackFixture) stall(t *testing.T) {
	t.Helper()
	fx.clk.Advance(testConfig().Downloader.StallTimeout.Duration)
	if err := fx.eng.StatusOnce(fx.ctx); err != nil {
		t.Fatalf("StatusOnce: %v", err)
	}
}

func (fx *fallbackFixture) movieState(t *testing.T) model.ItemState {
	t.Helper()
	movie, err := fx.db.Movies().Get(fx.ctx, fx.movie.ID)
	if err != nil {
		t.Fatal(err)
	}
	return movie.State
}

func (fx *fallbackFixture) hasReason(t *testing.T, reason string) bool {
	t.Helper()
	history, err := fx.db.Transitions().History(fx.ctx, model.SubjectMovie, fx.movie.ID, 100)
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

func TestStallFallsBackToNextAcceptedCandidate(t *testing.T) {
	fx := newFallbackFixture(t, fakeImporter{}, []fallbackCandidate{{true, 900}, {true, 700}, {true, 800}})
	fx.stall(t)

	if len(fx.client.adds) != 1 || fx.client.adds[0].Magnet != fx.releases[2].Magnet {
		t.Fatalf("adds = %+v, want one Add of the second-best magnet %s", fx.client.adds, fx.releases[2].Magnet)
	}
	if state := fx.movieState(t); state != model.StateGrabbed {
		t.Fatalf("movie state = %s, want grabbed", state)
	}
	if !fx.hasReason(t, "fallback_grab") {
		t.Fatal("no fallback_grab transition recorded")
	}
	grabs, err := fx.db.Grabs().BySubject(fx.ctx, model.SubjectMovie, fx.movie.ID)
	if err != nil || len(grabs) != 2 || grabs[0].ReleaseID != fx.releases[2].ID || grabs[1].State != model.GrabStalled {
		t.Fatalf("grabs = %+v err=%v, want new grab on the second-best release after a stalled one", grabs, err)
	}
}

func TestFallbackSkipsUnusableCandidates(t *testing.T) {
	cases := []struct {
		name  string
		specs []fallbackCandidate
		setup func(*testing.T, *fallbackFixture)
	}{
		{"rejected", []fallbackCandidate{{true, 900}, {false, 800}}, func(*testing.T, *fallbackFixture) {}},
		{"blacklisted", []fallbackCandidate{{true, 900}, {true, 800}}, func(t *testing.T, fx *fallbackFixture) {
			err := fx.db.Decisions().Blacklist(fx.ctx, model.SubjectMovie, fx.movie.ID,
				strings.ToUpper(fx.releases[1].InfoHash), "earlier failure")
			if err != nil {
				t.Fatal(err)
			}
		}},
		{"stale", []fallbackCandidate{{true, 900}, {true, 800}}, func(_ *testing.T, fx *fallbackFixture) {
			fx.clk.Advance(fallbackCandidateMaxAge)
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fx := newFallbackFixture(t, fakeImporter{}, tc.specs)
			tc.setup(t, fx)
			fx.stall(t)
			if len(fx.client.adds) != 0 || fx.movieState(t) != model.StateWanted || fx.hasReason(t, "fallback_grab") {
				t.Fatalf("adds=%d state=%s, want no Add and a wanted item", len(fx.client.adds), fx.movieState(t))
			}
		})
	}
}

func TestFallbackStopsAfterThreeFallbackGrabs(t *testing.T) {
	fx := newFallbackFixture(t, fakeImporter{}, []fallbackCandidate{
		{true, 900}, {true, 800}, {true, 700}, {true, 600}, {true, 500}})
	for i := 1; i <= 3; i++ {
		fx.stall(t)
		if len(fx.client.adds) != i || fx.client.adds[i-1].Magnet != fx.releases[i].Magnet ||
			fx.movieState(t) != model.StateGrabbed {
			t.Fatalf("fallback %d: adds=%d state=%s", i, len(fx.client.adds), fx.movieState(t))
		}
	}
	fx.stall(t)
	if len(fx.client.adds) != 3 || fx.movieState(t) != model.StateWanted {
		t.Fatalf("after the fourth failure: adds=%d state=%s, want 3 adds and wanted",
			len(fx.client.adds), fx.movieState(t))
	}
}

func TestFallbackLowDiskHoldsWithoutError(t *testing.T) {
	fx := newFallbackFixture(t, fakeImporter{}, []fallbackCandidate{{true, 900}, {true, 800}})
	// 4 GiB release + 2 GiB reserve does not fit in 3 GiB.
	fx.free.Store(3 << 30)
	fx.stall(t)

	if len(fx.client.adds) != 0 || fx.movieState(t) != model.StateWanted || !fx.hasReason(t, "low_disk_space") {
		t.Fatalf("adds=%d state=%s, want a low_disk_space hold without an Add",
			len(fx.client.adds), fx.movieState(t))
	}
}

func TestImportFailureDoesNotFallBack(t *testing.T) {
	fx := newFallbackFixture(t, failingImporter{}, []fallbackCandidate{{true, 900}, {true, 800}})
	fx.client.complete = true
	if err := fx.eng.StatusOnce(fx.ctx); err == nil {
		t.Fatal("StatusOnce = nil, want the import error")
	}
	if len(fx.client.adds) != 0 || fx.movieState(t) != model.StateImportFailed {
		t.Fatalf("adds=%d state=%s, want no Add and import_failed", len(fx.client.adds), fx.movieState(t))
	}
}

func TestPackFallbackReservesCoveredEpisodes(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 10, 3, 12, 0, 0, 0, time.UTC)
	clk := clock.NewFake(now)
	db, err := store.Open(ctx, store.Options{Path: filepath.Join(t.TempDir(), "packfallback.db"),
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
	profile, _ := db.Profiles().Default(ctx)
	series, err := db.Series().Create(ctx, model.Series{Title: "Example Show", Year: 2020,
		ProfileID: profile.ID, RootFolder: t.TempDir(), MonitorMode: model.MonitorAll,
		Status: model.SeriesFollowing})
	if err != nil {
		t.Fatal(err)
	}
	var episodeIDs []int64
	for number := 1; number <= 2; number++ {
		episode, err := db.Episodes().Create(ctx, model.Episode{SeriesID: series.ID, Season: 1,
			Number: number, State: model.StateWanted}, "fixture")
		if err != nil {
			t.Fatal(err)
		}
		episodeIDs = append(episodeIDs, episode.ID)
	}
	packs := []indexer.Release{
		{Title: "Example Show S01 1080p WEB-DL x264-BEST", InfoHash: fallbackHash(0),
			Magnet: "magnet:?xt=urn:btih:" + fallbackHash(0), SizeBytes: 8 << 30, Seeders: 50,
			Indexer: "fixture", Category: indexer.CatTVShowsHD, PublishedAt: now.Add(-time.Hour), Files: 2},
		{Title: "Example Show S01 1080p WEB-DL x264-NEXT", InfoHash: fallbackHash(1),
			Magnet: "magnet:?xt=urn:btih:" + fallbackHash(1), SizeBytes: 8 << 30, Seeders: 5,
			Indexer: "fixture", Category: indexer.CatTVShowsHD, PublishedAt: now.Add(-time.Hour), Files: 2},
	}
	client := &stallDownloader{}
	eng, err := New(Options{Store: db, Config: testConfig(),
		Indexers:   []indexer.Indexer{&fakeIndexer{releases: packs}},
		Downloader: client, Clock: clk, Logger: logger})
	if err != nil {
		t.Fatal(err)
	}
	if err := eng.SearchOnce(ctx); err != nil {
		t.Fatal(err)
	}
	if len(client.adds) != 1 || client.adds[0].Magnet != packs[0].Magnet {
		t.Fatalf("search adds = %+v, want the best pack", client.adds)
	}

	clk.Advance(testConfig().Downloader.StallTimeout.Duration)
	if err := eng.StatusOnce(ctx); err != nil {
		t.Fatalf("StatusOnce: %v", err)
	}
	if len(client.adds) != 2 || client.adds[1].Magnet != packs[1].Magnet {
		t.Fatalf("adds = %+v, want the second pack added after the stall", client.adds)
	}
	active, err := db.Grabs().Active(ctx)
	if err != nil || len(active) != 1 {
		t.Fatalf("active grabs=%d err=%v, want one shared fallback grab", len(active), err)
	}
	for _, id := range episodeIDs {
		episode, err := db.Episodes().Get(ctx, id)
		if err != nil || episode.State != model.StateGrabbed || episode.ChosenReleaseID != active[0].ReleaseID {
			t.Fatalf("episode %d = %+v err=%v, want grabbed on the fallback release", id, episode, err)
		}
	}
}
