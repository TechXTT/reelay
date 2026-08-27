package api

import (
	"context"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/TechXTT/reelay/internal/downloader"
	"github.com/TechXTT/reelay/internal/engine"
	"github.com/TechXTT/reelay/internal/model"
	"github.com/TechXTT/reelay/internal/store"
)

type removalCall struct {
	hash       string
	deleteData bool
}

type removalDownloader struct {
	calls        []removalCall
	pauseCalls   int
	paused       bool
	pausedHashes []string
}

func (f *removalDownloader) Add(context.Context, downloader.AddRequest) (string, error) {
	return "", errors.New("not implemented")
}
func (f *removalDownloader) Status(context.Context, []string) ([]downloader.TorrentStatus, error) {
	return nil, nil
}
func (f *removalDownloader) Remove(_ context.Context, hash string, deleteData bool) error {
	f.calls = append(f.calls, removalCall{hash: hash, deleteData: deleteData})
	return nil
}
func (f *removalDownloader) SetPaused(_ context.Context, hashes []string, paused bool) error {
	f.pauseCalls++
	f.paused = paused
	f.pausedHashes = append([]string(nil), hashes...)
	return nil
}
func (f *removalDownloader) Healthy(context.Context) error { return nil }

func TestDeleteManagedFileStaysInsideRootAndRemovesSubtitles(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "Movie (2026)")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	video := filepath.Join(dir, "Movie (2026).mkv")
	subtitle := filepath.Join(dir, "Movie (2026).en.srt")
	unrelated := filepath.Join(dir, "poster.jpg")
	for _, path := range []string{video, subtitle, unrelated} {
		if err := os.WriteFile(path, []byte("fixture"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := deleteManagedFile(video, root, []string{".srt", ".ass"}); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{video, subtitle} {
		if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("managed file still exists: %s", path)
		}
	}
	if _, err := os.Stat(unrelated); err != nil {
		t.Fatalf("unrelated artwork was removed: %v", err)
	}

	outside := filepath.Join(t.TempDir(), "outside.mkv")
	if err := os.WriteFile(outside, []byte("fixture"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := deleteManagedFile(outside, root, nil); err == nil {
		t.Fatal("outside-root deletion was accepted")
	}
	if _, err := os.Stat(outside); err != nil {
		t.Fatalf("outside file was touched: %v", err)
	}
}

func TestCollectionDeleteRequiresExplicitActiveDownloadRemoval(t *testing.T) {
	srv, st := newTestServer(t, nil)
	client := &removalDownloader{}
	srv.downloader = client
	movie, grab := managedMovieFixture(t, st, model.StateDownloading, model.GrabDownloading)

	rec := do(t, srv.Handler(), "DELETE", "/api/v1/movies/"+itoa(movie.ID), authed())
	if rec.Code != 409 {
		t.Fatalf("delete active collection status=%d body=%s", rec.Code, rec.Body.String())
	}
	if _, err := st.Movies().Get(context.Background(), movie.ID); err != nil {
		t.Fatalf("rejected delete removed movie: %v", err)
	}

	rec = do(t, srv.Handler(), "DELETE",
		"/api/v1/movies/"+itoa(movie.ID)+"?deleteDownloads=true", authed())
	if rec.Code != 204 {
		t.Fatalf("explicit delete status=%d body=%s", rec.Code, rec.Body.String())
	}
	if len(client.calls) != 1 || !client.calls[0].deleteData {
		t.Fatalf("downloader removals=%+v", client.calls)
	}
	got, err := st.Grabs().Get(context.Background(), grab.ID)
	if err != nil || got.State != model.GrabRemoved {
		t.Fatalf("grab after collection delete=%+v err=%v", got, err)
	}
}

func TestCollectionDeleteRejectsMovieLockedBySearch(t *testing.T) {
	srv, st := newTestServer(t, nil)
	srv.downloader = &removalDownloader{}
	movie, _ := managedMovieFixture(t, st, model.StateWanted, model.GrabRemoved)
	lock, err := st.Locks().Acquire(context.Background(), model.SubjectMovie, movie.ID,
		"engine-search", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = lock.Release(context.Background()) }()

	rec := do(t, srv.Handler(), "DELETE", "/api/v1/movies/"+itoa(movie.ID), authed())
	if rec.Code != 409 {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	if _, err := st.Movies().Get(context.Background(), movie.ID); err != nil {
		t.Fatalf("locked delete removed movie: %v", err)
	}
}

func TestQueueCancelCanKeepDataAndSkipBlacklist(t *testing.T) {
	srv, st := newTestServer(t, nil)
	client := &removalDownloader{}
	srv.downloader = client
	movie, grab := managedMovieFixture(t, st, model.StateDownloading, model.GrabDownloading)

	rec := do(t, srv.Handler(), "DELETE", "/api/v1/queue/"+itoa(grab.ID)+
		"?deleteData=false&blacklist=false", authed())
	if rec.Code != 204 {
		t.Fatalf("cancel status=%d body=%s", rec.Code, rec.Body.String())
	}
	if len(client.calls) != 1 || client.calls[0].deleteData {
		t.Fatalf("downloader removals=%+v", client.calls)
	}
	movie, err := st.Movies().Get(context.Background(), movie.ID)
	if err != nil || movie.State != model.StateWanted {
		t.Fatalf("movie after cancel=%+v err=%v", movie, err)
	}
	blacklisted, err := st.Decisions().IsBlacklisted(context.Background(), model.SubjectMovie,
		movie.ID, "0123456789abcdef0123456789abcdef01234567")
	if err != nil || blacklisted {
		t.Fatalf("blacklist=%v err=%v", blacklisted, err)
	}
}

func TestQueuePauseAndResumeAll(t *testing.T) {
	srv, st := newTestServer(t, nil)
	client := &removalDownloader{}
	eng, err := engine.New(engine.Options{Store: st, Config: srv.cfg, Downloader: client})
	if err != nil {
		t.Fatal(err)
	}
	srv.engine = eng
	_, grab := managedMovieFixture(t, st, model.StateDownloading, model.GrabDownloading)

	rec := do(t, srv.Handler(), http.MethodPost, "/api/v1/queue/pause", authed())
	if rec.Code != http.StatusOK {
		t.Fatalf("pause status=%d body=%s", rec.Code, rec.Body.String())
	}
	if client.pauseCalls != 1 || !client.paused || len(client.pausedHashes) != 1 || client.pausedHashes[0] != grab.TorrentHash {
		t.Fatalf("pause call=%d paused=%t hashes=%v", client.pauseCalls, client.paused, client.pausedHashes)
	}
	rec = do(t, srv.Handler(), http.MethodGet, "/api/v1/queue", authed())
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"paused":true`) {
		t.Fatalf("queue after pause status=%d body=%s", rec.Code, rec.Body.String())
	}
	rec = do(t, srv.Handler(), http.MethodPost, "/api/v1/queue/resume", authed())
	if rec.Code != http.StatusOK || client.pauseCalls != 2 || client.paused {
		t.Fatalf("resume status=%d calls=%d paused=%t body=%s", rec.Code, client.pauseCalls, client.paused, rec.Body.String())
	}
}

func TestQueueCancelReturnsEveryCoveredEpisodeToWanted(t *testing.T) {
	srv, st := newTestServer(t, nil)
	client := &removalDownloader{}
	srv.downloader = client
	ctx := context.Background()
	if _, err := st.Profiles().Seed(ctx, []model.QualityProfile{{Name: "Test", IsDefault: true,
		AllowedResolutions: []string{"1080p"}, AllowedSources: []string{"webdl"},
		MinSizeMB: 1, MaxSizeMB: 12000, MinSeeders: 1,
		PreferredGroups: map[string]int{}}}); err != nil {
		t.Fatal(err)
	}
	profile, _ := st.Profiles().Default(ctx)
	series, err := st.Series().Create(ctx, model.Series{Title: "Fixture Show", Year: 2020,
		ProfileID: profile.ID, RootFolder: t.TempDir(), MonitorMode: model.MonitorAll,
		Status: model.SeriesFollowing})
	if err != nil {
		t.Fatal(err)
	}
	first, _ := st.Episodes().Create(ctx, model.Episode{SeriesID: series.ID, Season: 1,
		Number: 1, State: model.StateSearching}, "fixture")
	second, _ := st.Episodes().Create(ctx, model.Episode{SeriesID: series.ID, Season: 1,
		Number: 2, State: model.StateWanted}, "fixture")
	release, err := st.Releases().Upsert(ctx, model.StoredRelease{Indexer: "fixture",
		RawTitle: "Fixture.Show.S01.1080p.WEB-DL.x265-GROUP",
		InfoHash: "1123456789abcdef0123456789abcdef01234567",
		Magnet:   "magnet:?xt=urn:btih:1123456789abcdef0123456789abcdef01234567"})
	if err != nil {
		t.Fatal(err)
	}
	lock1, _ := st.Locks().Acquire(ctx, model.SubjectEpisode, first.ID, "fixture", time.Minute)
	lock2, _ := st.Locks().Acquire(ctx, model.SubjectEpisode, second.ID, "fixture", time.Minute)
	grab, err := st.Grabs().CreateGrabbedFor(ctx, []*store.ItemLock{lock1, lock2}, model.Grab{
		SubjectType: model.SubjectEpisode, SubjectID: first.ID, ReleaseID: release.ID,
		TorrentHash: release.InfoHash, Category: "reelay-tv"}, "fixture pack")
	if err != nil {
		t.Fatal(err)
	}
	_ = lock1.Release(ctx)
	_ = lock2.Release(ctx)
	for _, id := range []int64{first.ID, second.ID} {
		if _, err := st.Transitions().Transition(ctx, model.SubjectEpisode, id,
			model.StateDownloading, "fixture", ""); err != nil {
			t.Fatal(err)
		}
	}
	grab.State = model.GrabDownloading
	if err := st.Grabs().Update(ctx, grab); err != nil {
		t.Fatal(err)
	}

	rec := do(t, srv.Handler(), "DELETE", "/api/v1/queue/"+itoa(grab.ID)+
		"?deleteData=false&blacklist=false", authed())
	if rec.Code != http.StatusNoContent {
		t.Fatalf("cancel status=%d body=%s", rec.Code, rec.Body.String())
	}
	for _, id := range []int64{first.ID, second.ID} {
		episode, err := st.Episodes().Get(ctx, id)
		if err != nil || episode.State != model.StateWanted {
			t.Fatalf("episode %d after shared cancel=%+v err=%v", id, episode, err)
		}
	}
}

func managedMovieFixture(t *testing.T, st *store.Store, itemState model.ItemState, grabState model.GrabState) (model.Movie, model.Grab) {
	t.Helper()
	ctx := context.Background()
	_, err := st.Profiles().Seed(ctx, []model.QualityProfile{{Name: "Test", IsDefault: true,
		AllowedResolutions: []string{"1080p"}, AllowedSources: []string{"webdl"},
		MinSizeMB: 1, MaxSizeMB: 12000, MinSeeders: 1, PreferredGroups: map[string]int{}}})
	if err != nil {
		t.Fatal(err)
	}
	profile, err := st.Profiles().Default(ctx)
	if err != nil {
		t.Fatal(err)
	}
	movie, err := st.Movies().Create(ctx, model.Movie{Title: "Fixture", Year: 2026,
		ProfileID: profile.ID, RootFolder: t.TempDir(), State: itemState}, "fixture")
	if err != nil {
		t.Fatal(err)
	}
	release, err := st.Releases().Upsert(ctx, model.StoredRelease{Indexer: "fixture",
		RawTitle:  "Fixture.2026.1080p.WEB-DL.x265-GROUP",
		InfoHash:  "0123456789abcdef0123456789abcdef01234567",
		Magnet:    "magnet:?xt=urn:btih:0123456789abcdef0123456789abcdef01234567",
		SizeBytes: 1024, Seeders: 10, ParsedJSON: `{}`})
	if err != nil {
		t.Fatal(err)
	}
	grab, err := st.Grabs().Create(ctx, model.Grab{SubjectType: model.SubjectMovie,
		SubjectID: movie.ID, ReleaseID: release.ID, TorrentHash: release.InfoHash,
		Category: "reelay-movies", State: grabState})
	if err != nil {
		t.Fatal(err)
	}
	return movie, grab
}

func itoa(value int64) string { return strconv.FormatInt(value, 10) }
