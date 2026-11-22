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
	"github.com/TechXTT/reelay/internal/indexer"
	"github.com/TechXTT/reelay/internal/model"
	"github.com/TechXTT/reelay/internal/store"
)

type upgradeFixture struct {
	ctx      context.Context
	clk      *clock.Fake
	db       *store.Store
	eng      *Engine
	index    *fakeIndexer
	client   *stallDownloader
	upgrade  model.QualityProfile // upgrade_until 1080p
	noCutoff model.QualityProfile // no upgrade_until
}

func newUpgradeFixture(t *testing.T, mutate func(*config.Config)) *upgradeFixture {
	t.Helper()
	fx := &upgradeFixture{ctx: context.Background(), index: &fakeIndexer{}, client: &stallDownloader{},
		clk: clock.NewFake(time.Date(2026, 10, 3, 12, 0, 0, 0, time.UTC))}
	db, err := store.Open(fx.ctx, store.Options{Path: filepath.Join(t.TempDir(), "upgrade.db"),
		CacheKB: 512, Now: fx.clk.Now})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	fx.db = db
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	if err := store.Migrate(fx.ctx, db, logger); err != nil {
		t.Fatal(err)
	}
	base := model.QualityProfile{AllowedResolutions: []string{"1080p", "720p"}, AllowedSources: []string{"webdl"},
		MinSizeMB: 100, MaxSizeMB: 10000, MinSeeders: 1, PreferredGroups: map[string]int{}}
	base.Name, base.IsDefault, base.UpgradeUntil = "upgrade", true, "1080p"
	if fx.upgrade, err = db.Profiles().Create(fx.ctx, base); err != nil {
		t.Fatal(err)
	}
	base.Name, base.IsDefault, base.UpgradeUntil = "no cutoff", false, ""
	if fx.noCutoff, err = db.Profiles().Create(fx.ctx, base); err != nil {
		t.Fatal(err)
	}
	cfg := testConfig()
	cfg.Schedules.UpgradeInterval = config.Dur(168 * time.Hour)
	cfg.Downloader.MaxUpgradeGrabs = 2
	if mutate != nil {
		mutate(cfg)
	}
	fx.eng, err = New(Options{Store: db, Config: cfg, Indexers: []indexer.Indexer{fx.index},
		Downloader: fx.client, Clock: fx.clk, Logger: logger,
		FreeSpace: func(string) (uint64, error) { return 100 << 30, nil }})
	if err != nil {
		t.Fatal(err)
	}
	return fx
}

func (fx *upgradeFixture) movie(t *testing.T, title string, profile model.QualityProfile, quality string) model.Movie {
	t.Helper()
	movie, err := fx.db.Movies().Create(fx.ctx, model.Movie{Title: title, Year: 2016, ProfileID: profile.ID,
		RootFolder: t.TempDir(), State: model.StateImported, ImportedPath: "/library/" + title + ".mkv",
		ImportedQuality: quality}, "fixture")
	if err != nil {
		t.Fatal(err)
	}
	return movie
}

func (fx *upgradeFixture) getMovie(t *testing.T, id int64) model.Movie {
	t.Helper()
	movie, err := fx.db.Movies().Get(fx.ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	return movie
}

func (fx *upgradeFixture) release(title, hash string) indexer.Release {
	return indexer.Release{Title: title, InfoHash: hash, Magnet: "magnet:?xt=urn:btih:" + hash,
		SizeBytes: 4 << 30, Seeders: 25, Indexer: "fixture", Category: indexer.CatMoviesHD,
		PublishedAt: fx.clk.Now().Add(-time.Hour)}
}

func TestUpgradeOncePromotesOnlyItemsBelowTheirProfileCutoff(t *testing.T) {
	fx := newUpgradeFixture(t, nil)
	below := fx.movie(t, "Below", fx.upgrade, "720p webdl")
	atCutoff := fx.movie(t, "AtCutoff", fx.upgrade, "1080p webdl")
	noCutoff := fx.movie(t, "NoCutoff", fx.noCutoff, "720p webdl")
	garbage := fx.movie(t, "Garbage", fx.upgrade, "unknown")
	blank := fx.movie(t, "Blank", fx.upgrade, "")
	series, err := fx.db.Series().Create(fx.ctx, model.Series{Title: "Show", ProfileID: fx.upgrade.ID,
		RootFolder: t.TempDir(), MonitorMode: model.MonitorAll})
	if err != nil {
		t.Fatal(err)
	}
	episode, err := fx.db.Episodes().Create(fx.ctx, model.Episode{SeriesID: series.ID, Season: 1, Number: 2,
		State: model.StateImported, ImportedPath: "/library/show.mkv", ImportedQuality: "720p hdtv"}, "fixture")
	if err != nil {
		t.Fatal(err)
	}

	if err := fx.eng.UpgradeOnce(fx.ctx); err != nil {
		t.Fatal(err)
	}
	if got := fx.getMovie(t, below.ID); got.State != model.StateWanted || got.ImportedPath == "" || got.NextSearchAt != nil {
		t.Errorf("below-cutoff movie = %s path=%q next=%v, want wanted with its file and due now", got.State, got.ImportedPath, got.NextSearchAt)
	}
	got, err := fx.db.Episodes().Get(fx.ctx, episode.ID)
	if err != nil || got.State != model.StateWanted || got.ImportedPath == "" {
		t.Errorf("below-cutoff episode = %+v, %v; want wanted with its file", got, err)
	}
	for name, id := range map[string]int64{"at cutoff": atCutoff.ID, "no cutoff": noCutoff.ID, "unparseable": garbage.ID, "blank": blank.ID} {
		movie := fx.getMovie(t, id)
		if movie.State != model.StateImported {
			t.Errorf("%s movie state = %s, want imported", name, movie.State)
		}
		if movie.UpgradeCheckedAt == nil {
			t.Errorf("%s movie was examined but not stamped", name)
		}
	}
	history, err := fx.db.Transitions().History(fx.ctx, model.SubjectMovie, below.ID, 1)
	if err != nil || len(history) != 1 || history[0].Reason != "upgrade_search" ||
		history[0].Detail != "imported 720p webdl below cutoff 1080p" {
		t.Errorf("history = %+v, %v", history, err)
	}
}

func TestUpgradeOnceRespectsTheConcurrentUpgradeCap(t *testing.T) {
	fx := newUpgradeFixture(t, nil)
	ids := make([]int64, 3)
	for i, title := range []string{"One", "Two", "Three"} {
		ids[i] = fx.movie(t, title, fx.upgrade, "720p webdl").ID
		fx.clk.Advance(time.Minute)
	}
	settled := fx.movie(t, "Settled", fx.upgrade, "1080p webdl")

	if err := fx.eng.UpgradeOnce(fx.ctx); err != nil {
		t.Fatal(err)
	}
	wanted, waiting := 0, []int64{}
	for _, id := range ids {
		if fx.getMovie(t, id).State == model.StateWanted {
			wanted++
		} else {
			waiting = append(waiting, id)
		}
	}
	if wanted != 2 || len(waiting) != 1 {
		t.Fatalf("promoted %d, waiting %d; want 2 promoted and 1 waiting", wanted, len(waiting))
	}
	if got := fx.getMovie(t, waiting[0]); got.UpgradeCheckedAt != nil {
		t.Errorf("over-cap item was stamped %v; it must keep its old check time", got.UpgradeCheckedAt)
	}
	if got := fx.getMovie(t, settled.ID); got.UpgradeCheckedAt == nil {
		t.Error("an item found not below its cutoff must be stamped even when the cap is full")
	}
	// Two upgrades are in flight, so the next pass starts nothing.
	fx.clk.Advance(200 * time.Hour)
	if err := fx.eng.UpgradeOnce(fx.ctx); err != nil {
		t.Fatal(err)
	}
	if got := fx.getMovie(t, waiting[0]); got.State != model.StateImported || got.UpgradeCheckedAt != nil {
		t.Errorf("waiting item = %s checked=%v; want untouched while the cap is full", got.State, got.UpgradeCheckedAt)
	}
	// A freed slot goes to the item that has waited longest.
	finished := fx.getMovie(t, ids[0])
	if finished.State != model.StateWanted {
		finished = fx.getMovie(t, ids[1])
	}
	if _, err := fx.db.Transitions().Transition(fx.ctx, model.SubjectMovie, finished.ID, model.StateUnmonitored, "test", ""); err != nil {
		t.Fatal(err)
	}
	if err := fx.eng.UpgradeOnce(fx.ctx); err != nil {
		t.Fatal(err)
	}
	if got := fx.getMovie(t, waiting[0]); got.State != model.StateWanted {
		t.Errorf("waiting item = %s, want wanted once a slot freed", got.State)
	}
}

func TestUpgradeOnceSpacesChecksByTheConfiguredInterval(t *testing.T) {
	fx := newUpgradeFixture(t, nil)
	settled := fx.movie(t, "Settled", fx.upgrade, "1080p webdl")
	if err := fx.eng.UpgradeOnce(fx.ctx); err != nil {
		t.Fatal(err)
	}
	first := fx.getMovie(t, settled.ID).UpgradeCheckedAt
	if first == nil {
		t.Fatal("item was not stamped on its first check")
	}
	fx.clk.Advance(167 * time.Hour)
	if err := fx.eng.UpgradeOnce(fx.ctx); err != nil {
		t.Fatal(err)
	}
	if got := fx.getMovie(t, settled.ID).UpgradeCheckedAt; !got.Equal(*first) {
		t.Errorf("checked again before the interval: %v -> %v", first, got)
	}
	// The profile now sets a cutoff the item misses; it is noticed only once due.
	profile := fx.upgrade
	profile.AllowedResolutions = []string{"2160p", "1080p", "720p"}
	profile.UpgradeUntil = "2160p"
	if _, err := fx.db.Profiles().Update(fx.ctx, profile); err != nil {
		t.Fatal(err)
	}
	if err := fx.eng.UpgradeOnce(fx.ctx); err != nil {
		t.Fatal(err)
	}
	if got := fx.getMovie(t, settled.ID); got.State != model.StateImported {
		t.Fatalf("item promoted before its check was due: %s", got.State)
	}
	fx.clk.Advance(2 * time.Hour)
	if err := fx.eng.UpgradeOnce(fx.ctx); err != nil {
		t.Fatal(err)
	}
	if got := fx.getMovie(t, settled.ID); got.State != model.StateWanted {
		t.Errorf("item = %s after its check came due, want wanted", got.State)
	}
}

func TestUpgradeSearchWithoutABetterReleaseReturnsToImported(t *testing.T) {
	for _, tc := range []struct {
		name  string
		delay time.Duration
	}{{"within give-up window", time.Minute}, {"past give-up window", 40 * 24 * time.Hour}} {
		t.Run(tc.name, func(t *testing.T) {
			fx := newUpgradeFixture(t, nil)
			movie := fx.movie(t, "Arrival", fx.upgrade, "720p webdl")
			fx.index.releases = []indexer.Release{fx.release("Arrival 2016 720p WEB-DL x264-GROUP", "0123456789abcdef0123456789abcdef01234567")}
			if err := fx.eng.UpgradeOnce(fx.ctx); err != nil {
				t.Fatal(err)
			}
			fx.clk.Advance(tc.delay)
			if err := fx.eng.SearchOnce(fx.ctx); err != nil {
				t.Fatal(err)
			}
			got := fx.getMovie(t, movie.ID)
			if got.State != model.StateImported || got.ImportedPath != movie.ImportedPath ||
				got.ImportedQuality != "720p webdl" || got.NextSearchAt != nil {
				t.Fatalf("after fruitless upgrade search: state=%s path=%q quality=%q next=%v",
					got.State, got.ImportedPath, got.ImportedQuality, got.NextSearchAt)
			}
			if len(fx.client.adds) != 0 {
				t.Errorf("a non-upgrade release was grabbed: %+v", fx.client.adds)
			}
		})
	}
}

func TestStalledUpgradeGrabEndsBackAtImported(t *testing.T) {
	fx := newUpgradeFixture(t, nil)
	movie := fx.movie(t, "Arrival", fx.upgrade, "720p webdl")
	fx.index.releases = []indexer.Release{fx.release("Arrival 2016 1080p WEB-DL x264-GROUP", "0123456789abcdef0123456789abcdef01234567")}
	if err := fx.eng.UpgradeOnce(fx.ctx); err != nil {
		t.Fatal(err)
	}
	if err := fx.eng.SearchOnce(fx.ctx); err != nil {
		t.Fatal(err)
	}
	if got := fx.getMovie(t, movie.ID); got.State != model.StateGrabbed || got.ImportedPath != movie.ImportedPath {
		t.Fatalf("after search: state=%s path=%q, want grabbed with the old file kept", got.State, got.ImportedPath)
	}
	if n, err := fx.eng.upgradeInflight(fx.ctx); err != nil || n != 1 {
		t.Fatalf("in-flight upgrades = %d, %v; want 1", n, err)
	}
	fx.clk.Advance(testConfig().Downloader.StallTimeout.Duration)
	if err := fx.eng.StatusOnce(fx.ctx); err != nil {
		t.Fatal(err)
	}
	if got := fx.getMovie(t, movie.ID); got.State != model.StateWanted || got.ImportedPath != movie.ImportedPath {
		t.Fatalf("after stall: state=%s path=%q, want wanted with the old file kept", got.State, got.ImportedPath)
	}
	// The stalled release is blacklisted, so the next search finds nothing.
	if err := fx.eng.SearchOnce(fx.ctx); err != nil {
		t.Fatal(err)
	}
	if got := fx.getMovie(t, movie.ID); got.State != model.StateImported || got.ImportedPath != movie.ImportedPath {
		t.Fatalf("after second search: state=%s path=%q, want imported with the old file kept", got.State, got.ImportedPath)
	}
	if len(fx.client.adds) != 1 {
		t.Errorf("downloads added = %d, want 1", len(fx.client.adds))
	}
}

func TestStartUpgradeRefusalsAndSuccess(t *testing.T) {
	fx := newUpgradeFixture(t, func(c *config.Config) { c.Downloader.MaxUpgradeGrabs = 1 })
	below := fx.movie(t, "Below", fx.upgrade, "720p webdl")
	other := fx.movie(t, "Other", fx.upgrade, "720p webdl")
	atCutoff := fx.movie(t, "AtCutoff", fx.upgrade, "1080p webdl")
	noCutoff := fx.movie(t, "NoCutoff", fx.noCutoff, "720p webdl")

	var notUpgradable *NotUpgradableError
	for name, id := range map[string]int64{"at cutoff": atCutoff.ID, "no cutoff": noCutoff.ID} {
		if err := fx.eng.StartUpgrade(fx.ctx, model.SubjectMovie, id); !errors.As(err, &notUpgradable) {
			t.Errorf("%s: err = %v, want NotUpgradableError", name, err)
		}
	}
	if err := fx.eng.StartUpgrade(fx.ctx, model.SubjectMovie, 9999); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("unknown item: err = %v, want ErrNotFound", err)
	}
	if err := fx.eng.StartUpgrade(fx.ctx, model.SubjectMovie, below.ID); err != nil {
		t.Fatal(err)
	}
	if got := fx.getMovie(t, below.ID); got.State != model.StateWanted || got.UpgradeCheckedAt == nil {
		t.Errorf("started item = %s checked=%v", got.State, got.UpgradeCheckedAt)
	}
	if err := fx.eng.StartUpgrade(fx.ctx, model.SubjectMovie, below.ID); !errors.As(err, &notUpgradable) {
		t.Errorf("already upgrading: err = %v, want NotUpgradableError", err)
	}
	if err := fx.eng.StartUpgrade(fx.ctx, model.SubjectMovie, other.ID); !errors.Is(err, ErrUpgradeLimit) {
		t.Errorf("over the cap: err = %v, want ErrUpgradeLimit", err)
	}

	disabled := newUpgradeFixture(t, func(c *config.Config) { c.Schedules.UpgradeInterval = config.Dur(0) })
	item := disabled.movie(t, "Below", disabled.upgrade, "720p webdl")
	if err := disabled.eng.StartUpgrade(disabled.ctx, model.SubjectMovie, item.ID); !errors.Is(err, ErrUpgradesDisabled) {
		t.Errorf("disabled: err = %v, want ErrUpgradesDisabled", err)
	}
}

func TestUpgradesListsItemsBelowCutoffWithUpgradingFirst(t *testing.T) {
	fx := newUpgradeFixture(t, nil)
	fx.movie(t, "Waiting", fx.upgrade, "720p webdl")
	fx.movie(t, "AtCutoff", fx.upgrade, "1080p webdl")
	fx.movie(t, "NoCutoff", fx.noCutoff, "720p webdl")
	running := fx.movie(t, "Running", fx.upgrade, "720p webdl")
	if err := fx.eng.StartUpgrade(fx.ctx, model.SubjectMovie, running.ID); err != nil {
		t.Fatal(err)
	}
	status, err := fx.eng.Upgrades(fx.ctx)
	if err != nil {
		t.Fatal(err)
	}
	if !status.Enabled || status.MaxGrabs != 2 || status.Inflight != 1 || len(status.Items) != 2 {
		t.Fatalf("status = %+v", status)
	}
	if first := status.Items[0]; first.Title != "Running" || !first.Upgrading || first.Cutoff != "1080p" ||
		first.Profile != "upgrade" || first.Quality != "720p webdl" {
		t.Errorf("first = %+v", first)
	}
	if second := status.Items[1]; second.Title != "Waiting" || second.Upgrading || second.State != model.StateImported {
		t.Errorf("second = %+v", second)
	}
}
