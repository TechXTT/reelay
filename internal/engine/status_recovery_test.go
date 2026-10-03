package engine

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/TechXTT/reelay/internal/clock"
	"github.com/TechXTT/reelay/internal/downloader"
	"github.com/TechXTT/reelay/internal/model"
	"github.com/TechXTT/reelay/internal/store"
)

type recoveryDownloader struct {
	statuses    []downloader.TorrentStatus
	statusErr   error
	removeErr   error
	removeCalls int
	deleteData  bool
}

func (d *recoveryDownloader) Add(context.Context, downloader.AddRequest) (string, error) {
	return "", errors.New("unexpected Add")
}

func (d *recoveryDownloader) Status(context.Context, []string) ([]downloader.TorrentStatus, error) {
	return append([]downloader.TorrentStatus(nil), d.statuses...), d.statusErr
}

func (d *recoveryDownloader) Remove(_ context.Context, _ string, deleteData bool) error {
	d.removeCalls++
	d.deleteData = deleteData
	return d.removeErr
}

func (*recoveryDownloader) SetPaused(context.Context, []string, bool) error { return nil }
func (*recoveryDownloader) Healthy(context.Context) error                   { return nil }

type recoveryFixture struct {
	ctx    context.Context
	clock  *clock.Fake
	store  *store.Store
	engine *Engine
	client *recoveryDownloader
	grab   model.Grab
	movie  model.Movie
}

func newRecoveryFixture(t *testing.T, grabState model.GrabState, itemState model.ItemState,
	progress float64) recoveryFixture {
	t.Helper()
	ctx := context.Background()
	now := time.Date(2026, 10, 3, 12, 0, 0, 0, time.UTC)
	clk := clock.NewFake(now)
	db, err := store.Open(ctx, store.Options{Path: filepath.Join(t.TempDir(), "recovery.db"),
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
	movie, err := db.Movies().Create(ctx, model.Movie{Title: "Arrival", Year: 2016,
		ProfileID: profile.ID, RootFolder: t.TempDir(), State: itemState}, "recovery fixture")
	if err != nil {
		t.Fatal(err)
	}
	release, err := db.Releases().Upsert(ctx, model.StoredRelease{Indexer: "fixture",
		InfoHash: "0123456789abcdef0123456789abcdef01234567", RawTitle: "Arrival 2016 1080p WEB-DL",
		Magnet: "magnet:?xt=urn:btih:0123456789abcdef0123456789abcdef01234567"})
	if err != nil {
		t.Fatal(err)
	}
	grab, err := db.Grabs().Create(ctx, model.Grab{SubjectType: model.SubjectMovie, SubjectID: movie.ID,
		ReleaseID: release.ID, TorrentHash: release.InfoHash, Category: "reelay-movies",
		State: grabState, Progress: progress, ContentPath: "/downloads/arrival.mkv"})
	if err != nil {
		t.Fatal(err)
	}
	client := &recoveryDownloader{}
	cfg := testConfig()
	engine, err := New(Options{Store: db, Config: cfg, Downloader: client,
		Importer: fakeImporter{store: db}, Clock: clk, Logger: logger})
	if err != nil {
		t.Fatal(err)
	}
	return recoveryFixture{ctx: ctx, clock: clk, store: db, engine: engine, client: client,
		grab: grab, movie: movie}
}

func TestStatusRecoveryStallsAtIntermediateProgress(t *testing.T) {
	f := newRecoveryFixture(t, model.GrabDownloading, model.StateDownloading, 0.4)
	f.client.statuses = []downloader.TorrentStatus{{Hash: f.grab.TorrentHash,
		State: downloader.StateDownloading, Progress: 0.4}}
	f.clock.Advance(testConfig().Downloader.StallTimeout.Duration)

	if err := f.engine.StatusOnce(f.ctx); err != nil {
		t.Fatal(err)
	}
	grab, err := f.store.Grabs().Get(f.ctx, f.grab.ID)
	if err != nil || grab.State != model.GrabStalled {
		t.Fatalf("grab = %+v, err=%v; want stalled at 40%%", grab, err)
	}
	if f.client.removeCalls != 1 || !f.client.deleteData {
		t.Fatalf("Remove calls=%d deleteData=%t; want one owned-data cleanup", f.client.removeCalls, f.client.deleteData)
	}
}

func TestMissingHashUsesLastSuccessfulObservation(t *testing.T) {
	f := newRecoveryFixture(t, model.GrabDownloading, model.StateDownloading, 0.4)
	f.grab.ProgressedAt = f.clock.Now().Add(-3 * time.Hour)
	if err := f.store.Grabs().Update(f.ctx, f.grab); err != nil {
		t.Fatal(err)
	}

	if err := f.engine.StatusOnce(f.ctx); err != nil {
		t.Fatal(err)
	}
	grab, err := f.store.Grabs().Get(f.ctx, f.grab.ID)
	if err != nil || grab.State != model.GrabDownloading || f.client.removeCalls != 0 {
		t.Fatalf("first missing poll: grab=%+v removes=%d err=%v", grab, f.client.removeCalls, err)
	}

	f.clock.Advance(testConfig().Downloader.StallTimeout.Duration)
	if err := f.engine.StatusOnce(f.ctx); err != nil {
		t.Fatal(err)
	}
	grab, err = f.store.Grabs().Get(f.ctx, f.grab.ID)
	if err != nil || grab.State != model.GrabStalled {
		t.Fatalf("after disappearance grace: grab=%+v err=%v", grab, err)
	}
}

func TestResumeRestartsMissingHashGrace(t *testing.T) {
	f := newRecoveryFixture(t, model.GrabDownloading, model.StateDownloading, 0.4)
	if _, err := f.engine.SetDownloadsPaused(f.ctx, true); err != nil {
		t.Fatal(err)
	}
	f.clock.Advance(5 * time.Hour)
	if _, err := f.engine.SetDownloadsPaused(f.ctx, false); err != nil {
		t.Fatal(err)
	}

	if err := f.engine.StatusOnce(f.ctx); err != nil {
		t.Fatal(err)
	}
	grab, err := f.store.Grabs().Get(f.ctx, f.grab.ID)
	if err != nil || grab.State != model.GrabDownloading || f.client.removeCalls != 0 {
		t.Fatalf("immediately after resume: grab=%+v removes=%d err=%v", grab, f.client.removeCalls, err)
	}

	f.clock.Advance(testConfig().Downloader.StallTimeout.Duration - time.Second)
	if err := f.engine.StatusOnce(f.ctx); err != nil {
		t.Fatal(err)
	}
	grab, err = f.store.Grabs().Get(f.ctx, f.grab.ID)
	if err != nil || grab.State != model.GrabDownloading {
		t.Fatalf("before resumed grace expires: grab=%+v err=%v", grab, err)
	}
}

func TestMaintenanceAndQueuedStatesDoNotStall(t *testing.T) {
	for _, state := range []string{downloader.StateMaintenance, downloader.StateQueued} {
		t.Run(state, func(t *testing.T) {
			f := newRecoveryFixture(t, model.GrabDownloading, model.StateDownloading, 0.4)
			f.grab.ProgressedAt = f.clock.Now().Add(-3 * time.Hour)
			if err := f.store.Grabs().Update(f.ctx, f.grab); err != nil {
				t.Fatal(err)
			}
			f.client.statuses = []downloader.TorrentStatus{{Hash: f.grab.TorrentHash,
				State: state, Progress: 0.4}}
			f.clock.Advance(time.Second)
			if err := f.engine.StatusOnce(f.ctx); err != nil {
				t.Fatal(err)
			}
			grab, err := f.store.Grabs().Get(f.ctx, f.grab.ID)
			if err != nil || grab.State != model.GrabDownloading ||
				!grab.ProgressedAt.Equal(f.clock.Now()) || f.client.removeCalls != 0 {
				t.Fatalf("grab=%+v removes=%d err=%v", grab, f.client.removeCalls, err)
			}
		})
	}
}

func TestMetadataAcquisitionStillUsesStallTimeout(t *testing.T) {
	f := newRecoveryFixture(t, model.GrabDownloading, model.StateDownloading, 0)
	f.client.statuses = []downloader.TorrentStatus{{Hash: f.grab.TorrentHash,
		State: downloader.StateDownloading, Progress: 0}}
	f.clock.Advance(testConfig().Downloader.StallTimeout.Duration)

	if err := f.engine.StatusOnce(f.ctx); err != nil {
		t.Fatal(err)
	}
	grab, err := f.store.Grabs().Get(f.ctx, f.grab.ID)
	if err != nil || grab.State != model.GrabStalled {
		t.Fatalf("metadata acquisition grab=%+v err=%v; want timeout", grab, err)
	}
}

func TestMissingHashLeavesNonOwnedTorrentUntouched(t *testing.T) {
	f := newRecoveryFixture(t, model.GrabDownloading, model.StateDownloading, 0.4)
	f.client.removeErr = downloader.ErrNotOurs
	f.clock.Advance(testConfig().Downloader.StallTimeout.Duration)

	if err := f.engine.StatusOnce(f.ctx); err != nil {
		t.Fatal(err)
	}
	grab, err := f.store.Grabs().Get(f.ctx, f.grab.ID)
	if err != nil || grab.State != model.GrabStalled ||
		!strings.Contains(grab.LastError, "no longer owned by Reelay") {
		t.Fatalf("grab=%+v err=%v; want local failure with ownership reason", grab, err)
	}
	if f.client.removeCalls != 1 || !f.client.deleteData {
		t.Fatalf("Remove calls=%d deleteData=%t", f.client.removeCalls, f.client.deleteData)
	}
}

func TestFailedStatusPollDoesNotStartMissingRecovery(t *testing.T) {
	f := newRecoveryFixture(t, model.GrabDownloading, model.StateDownloading, 0.4)
	f.client.statusErr = errors.New("client unavailable")
	f.clock.Advance(5 * time.Hour)

	if err := f.engine.StatusOnce(f.ctx); err == nil {
		t.Fatal("StatusOnce succeeded with a failed client poll")
	}
	grab, err := f.store.Grabs().Get(f.ctx, f.grab.ID)
	if err != nil || grab.State != model.GrabDownloading || f.client.removeCalls != 0 {
		t.Fatalf("failed poll: grab=%+v removes=%d err=%v", grab, f.client.removeCalls, err)
	}
}

func TestMissingCompletedGrabRetriesImport(t *testing.T) {
	f := newRecoveryFixture(t, model.GrabImporting, model.StateImporting, 1)
	f.clock.Advance(testConfig().Downloader.StallTimeout.Duration)

	if err := f.engine.StatusOnce(f.ctx); err != nil {
		t.Fatal(err)
	}
	grab, err := f.store.Grabs().Get(f.ctx, f.grab.ID)
	if err != nil || grab.State != model.GrabImported {
		t.Fatalf("grab=%+v err=%v; want imported after recovery", grab, err)
	}
	movie, err := f.store.Movies().Get(f.ctx, f.movie.ID)
	if err != nil || movie.State != model.StateImported {
		t.Fatalf("movie=%+v err=%v; want imported", movie, err)
	}
}

func TestGlobalPauseDefersReportedClientErrors(t *testing.T) {
	f := newRecoveryFixture(t, model.GrabDownloading, model.StateDownloading, 0.4)
	if _, err := f.engine.SetDownloadsPaused(f.ctx, true); err != nil {
		t.Fatal(err)
	}
	f.client.statuses = []downloader.TorrentStatus{{Hash: f.grab.TorrentHash,
		State: downloader.StateError, ErrorMessage: "fixture failure", Progress: 0.4}}
	if err := f.engine.StatusOnce(f.ctx); err != nil {
		t.Fatal(err)
	}
	grab, err := f.store.Grabs().Get(f.ctx, f.grab.ID)
	if err != nil || grab.State != model.GrabDownloading || f.client.removeCalls != 0 {
		t.Fatalf("paused error: grab=%+v removes=%d err=%v", grab, f.client.removeCalls, err)
	}
}

var _ downloader.Downloader = (*recoveryDownloader)(nil)
