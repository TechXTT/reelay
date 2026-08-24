package api

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"testing"

	"github.com/TechXTT/reelay/internal/downloader"
	"github.com/TechXTT/reelay/internal/model"
	"github.com/TechXTT/reelay/internal/store"
)

type removalCall struct {
	hash       string
	deleteData bool
}

type removalDownloader struct{ calls []removalCall }

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
