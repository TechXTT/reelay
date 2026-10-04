package engine

import (
	"bytes"
	"context"
	"crypto/sha1"
	"encoding/hex"
	"errors"
	"io"
	"log/slog"
	"path/filepath"
	"testing"
	"time"

	"github.com/TechXTT/reelay/internal/clock"
	"github.com/TechXTT/reelay/internal/indexer"
	"github.com/TechXTT/reelay/internal/model"
	"github.com/TechXTT/reelay/internal/store"
)

const fileTestInfo = "d6:lengthi12345e4:name8:test.mkv12:piece lengthi16384e6:pieces20:01234567890123456789e"

// fileTestTorrent returns a minimal .torrent file and its info hash.
func fileTestTorrent() ([]byte, string) {
	var sum = sha1.Sum([]byte(fileTestInfo))

	return []byte("d4:info" + fileTestInfo + "e"), hex.EncodeToString(sum[:])
}

// fetchIndexer serves a release that only has a download link.
type fetchIndexer struct {
	fakeIndexer
	payload indexer.TorrentPayload
	err     error
	fetched []string
}

func (f *fetchIndexer) FetchTorrent(_ context.Context, downloadURL string) (indexer.TorrentPayload, error) {
	f.fetched = append(f.fetched, downloadURL)
	return f.payload, f.err
}

const fileTestURL = "http://indexer.test/dl/1.torrent?file=a"

type fileFixture struct {
	db      *store.Store
	eng     *Engine
	fetcher *fetchIndexer
	client  *fakeDownloader
	movie   model.Movie
	standIn string
	hash    string
}

func newFileFixture(t *testing.T, fetchErr error) *fileFixture {
	t.Helper()
	var ctx = context.Background()
	var now = time.Date(2026, 8, 23, 12, 0, 0, 0, time.UTC)
	var clk = clock.NewFake(now)
	var file, hash = fileTestTorrent()
	var standIn = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	var logger = slog.New(slog.NewTextHandler(io.Discard, nil))

	db, err := store.Open(ctx, store.Options{Path: filepath.Join(t.TempDir(), "engine.db"), CacheKB: 512, Now: clk.Now})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
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
		ProfileID: profile.ID, RootFolder: t.TempDir(), State: model.StateWanted}, "fixture wanted")
	if err != nil {
		t.Fatal(err)
	}
	var fetcher = &fetchIndexer{payload: indexer.TorrentPayload{File: file}, err: fetchErr}
	fetcher.releases = []indexer.Release{{Title: "Arrival 2016 1080p WEB-DL x264-GROUP", InfoHash: standIn,
		DownloadURL: fileTestURL, SizeBytes: 4 << 30, Seeders: 25, Indexer: "fixture",
		Category: indexer.CatMoviesHD, PublishedAt: now.Add(-time.Hour)}}
	var client = &fakeDownloader{hash: hash}
	eng, err := New(Options{Store: db, Config: testConfig(), Indexers: []indexer.Indexer{fetcher},
		Downloader: client, Importer: fakeImporter{store: db}, Clock: clk, Logger: logger})
	if err != nil {
		t.Fatal(err)
	}
	return &fileFixture{db: db, eng: eng, fetcher: fetcher, client: client, movie: movie, standIn: standIn, hash: hash}
}

func TestSearchGrabsDownloadURLReleaseAsTorrentFile(t *testing.T) {
	var ctx = context.Background()
	var fx = newFileFixture(t, nil)
	var file, _ = fileTestTorrent()

	if err := fx.eng.SearchOnce(ctx); err != nil {
		t.Fatalf("search: %v", err)
	}
	if len(fx.fetcher.fetched) != 1 || fx.fetcher.fetched[0] != fileTestURL {
		t.Fatalf("fetched = %v", fx.fetcher.fetched)
	}
	if fx.client.addCalls != 1 || !bytes.Equal(fx.client.lastAdd.TorrentFile, file) || fx.client.lastAdd.Magnet != "" {
		t.Fatalf("add = %+v", fx.client.lastAdd)
	}
	if fx.client.lastAdd.Category != "reelay-movies" {
		t.Fatalf("category = %q", fx.client.lastAdd.Category)
	}
	grabs, err := fx.db.Grabs().BySubject(ctx, model.SubjectMovie, fx.movie.ID)
	if err != nil || len(grabs) != 1 || grabs[0].TorrentHash != fx.hash {
		t.Fatalf("grabs = %+v err=%v, want real hash %s", grabs, err, fx.hash)
	}
	stored, err := fx.db.Releases().Get(ctx, grabs[0].ReleaseID)
	if err != nil || stored.DownloadURL != fileTestURL || stored.InfoHash != fx.standIn || stored.Magnet != "" {
		t.Fatalf("stored release = %+v err=%v", stored, err)
	}
}

func TestSearchFetchFailureRetriesWithoutGrab(t *testing.T) {
	var ctx = context.Background()
	var fx = newFileFixture(t, errors.New("boom"))

	if err := fx.eng.SearchOnce(ctx); err != nil {
		t.Fatalf("search: %v", err)
	}
	if fx.client.addCalls != 0 {
		t.Fatal("downloader was called after a failed fetch")
	}
	movie, err := fx.db.Movies().Get(ctx, fx.movie.ID)
	if err != nil || movie.State != model.StateWanted {
		t.Fatalf("movie = %+v err=%v", movie, err)
	}
	history, err := fx.db.Transitions().History(ctx, model.SubjectMovie, fx.movie.ID, 20)
	if err != nil {
		t.Fatal(err)
	}
	for _, transition := range history {
		if transition.Reason == "grab_failed" {
			return
		}
	}
	t.Fatalf("no grab_failed transition in %+v", history)
}

func TestManualGrabFetchesDownloadURLRelease(t *testing.T) {
	var ctx = context.Background()
	var fx = newFileFixture(t, nil)
	var file, _ = fileTestTorrent()

	stored, err := fx.db.Releases().Upsert(ctx, model.StoredRelease{Indexer: "fixture", InfoHash: fx.standIn,
		RawTitle: "Arrival 2016 1080p WEB-DL x264-GROUP", DownloadURL: fileTestURL, SizeBytes: 4 << 30, Seeders: 25})
	if err != nil {
		t.Fatal(err)
	}
	grab, err := fx.eng.ManualGrab(ctx, model.SubjectMovie, fx.movie.ID, stored.ID)
	if err != nil {
		t.Fatal(err)
	}
	if grab.TorrentHash != fx.hash || !bytes.Equal(fx.client.lastAdd.TorrentFile, file) {
		t.Fatalf("grab=%+v add=%+v", grab, fx.client.lastAdd)
	}
}

func TestManualGrabMagnetRedirectUsesMagnet(t *testing.T) {
	var ctx = context.Background()
	var fx = newFileFixture(t, nil)
	var magnet = "magnet:?xt=urn:btih:" + fx.hash

	fx.fetcher.payload = indexer.TorrentPayload{Magnet: magnet}
	stored, err := fx.db.Releases().Upsert(ctx, model.StoredRelease{Indexer: "fixture", InfoHash: fx.standIn,
		RawTitle: "Arrival 2016 1080p WEB-DL x264-GROUP", DownloadURL: fileTestURL, SizeBytes: 4 << 30, Seeders: 25})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := fx.eng.ManualGrab(ctx, model.SubjectMovie, fx.movie.ID, stored.ID); err != nil {
		t.Fatal(err)
	}
	if fx.client.lastAdd.Magnet != magnet || len(fx.client.lastAdd.TorrentFile) != 0 {
		t.Fatalf("add = %+v", fx.client.lastAdd)
	}
}

func TestAddRequestForErrors(t *testing.T) {
	var ctx = context.Background()
	var fx = newFileFixture(t, nil)

	if _, err := fx.eng.addRequestFor(ctx, "missing", "", fileTestURL); err == nil {
		t.Fatal("unknown indexer accepted")
	}
	fx.eng.indexers = []indexer.Indexer{&fakeIndexer{}}
	if _, err := fx.eng.addRequestFor(ctx, "fixture", "", fileTestURL); err == nil {
		t.Fatal("indexer without TorrentFetcher accepted")
	}
	request, err := fx.eng.addRequestFor(ctx, "anything", "magnet:?xt=urn:btih:x", "")
	if err != nil || request.Magnet != "magnet:?xt=urn:btih:x" {
		t.Fatalf("request=%+v err=%v", request, err)
	}
}

func TestFailGrabBlacklistsStandInAndRealHash(t *testing.T) {
	var fx = newFallbackFixture(t, fakeImporter{}, []fallbackCandidate{{true, 900}})
	var realHash = "ABCDEFABCDEFABCDEFABCDEFABCDEFABCDEFABCD"

	grabs, err := fx.db.Grabs().BySubject(fx.ctx, model.SubjectMovie, fx.movie.ID)
	if err != nil || len(grabs) != 1 {
		t.Fatalf("grabs = %+v err=%v", grabs, err)
	}
	grabs[0].TorrentHash = realHash
	if err := fx.eng.failGrab(fx.ctx, grabs[0], "stalled", false); err != nil {
		t.Fatal(err)
	}
	blacklist, err := fx.db.Decisions().BlacklistFor(fx.ctx, model.SubjectMovie, fx.movie.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !blacklist[fx.releases[0].InfoHash] || !blacklist["abcdefabcdefabcdefabcdefabcdefabcdefabcd"] || len(blacklist) != 2 {
		t.Fatalf("blacklist = %v", blacklist)
	}
}
