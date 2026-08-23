package importer

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"github.com/TechXTT/reelay/internal/config"
	"github.com/TechXTT/reelay/internal/model"
	"github.com/TechXTT/reelay/internal/parser"
	"github.com/TechXTT/reelay/internal/store"
)

func TestLandHardlinkAndExistingSkip(t *testing.T) {
	dir := t.TempDir()
	source := filepath.Join(dir, "source.mkv")
	writeSized(t, source, 1024)
	root := filepath.Join(dir, "library")
	if err := os.Mkdir(root, 0o755); err != nil {
		t.Fatal(err)
	}
	service := &Service{cfg: importerConfig(root, root), link: os.Link}
	dest := filepath.Join(root, "Movie", "movie.mkv")
	method, _, skipped, err := service.land(source, dest, root)
	if err != nil || method != "hardlink" || skipped {
		t.Fatalf("land method=%s skipped=%v err=%v", method, skipped, err)
	}
	srcInfo, _ := os.Stat(source)
	dstInfo, _ := os.Stat(dest)
	if !os.SameFile(srcInfo, dstInfo) {
		t.Fatal("destination is not the same hardlinked file")
	}
	_, _, skipped, err = service.land(source, dest, root)
	if err != nil || !skipped {
		t.Fatalf("existing destination skipped=%v err=%v", skipped, err)
	}
}

func TestLandEXDEVFallsBackToVerifiedCopy(t *testing.T) {
	dir := t.TempDir()
	source := filepath.Join(dir, "source.mkv")
	writeSized(t, source, 2048)
	root := filepath.Join(dir, "library")
	_ = os.Mkdir(root, 0o755)
	service := &Service{cfg: importerConfig(root, root), link: func(string, string) error {
		return &os.LinkError{Op: "link", Err: syscall.EXDEV}
	}}
	dest := filepath.Join(root, "movie.mkv")
	method, _, _, err := service.land(source, dest, root)
	if err != nil || method != "copy" {
		t.Fatalf("method=%s err=%v", method, err)
	}
	want, _ := os.ReadFile(source)
	got, _ := os.ReadFile(dest)
	if string(want) != string(got) {
		t.Fatal("copied content differs")
	}
	if _, err := os.Stat(source); err != nil {
		t.Fatal("copy fallback removed source")
	}
}

func TestSeasonPackAndSubtitleImport(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	tvRoot := filepath.Join(dir, "tv")
	movieRoot := filepath.Join(dir, "movies")
	for _, path := range []string{tvRoot, movieRoot} {
		if err := os.Mkdir(path, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	cfg := importerConfig(tvRoot, movieRoot)
	db, err := store.Open(ctx, store.Options{Path: filepath.Join(dir, "test.db"), CacheKB: 512})
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
		MinSizeMB: 1, MaxSizeMB: 10000, MinSeeders: 1, PreferredGroups: map[string]int{}}
	if _, err := db.Profiles().Seed(ctx, []model.QualityProfile{profile}); err != nil {
		t.Fatal(err)
	}
	profile, _ = db.Profiles().Default(ctx)
	series, err := db.Series().Create(ctx, model.Series{Title: "The Expanse", Year: 2015,
		ProfileID: profile.ID, RootFolder: tvRoot, MonitorMode: model.MonitorAll,
		Status: model.SeriesFollowing})
	if err != nil {
		t.Fatal(err)
	}
	e1, err := db.Episodes().Create(ctx, model.Episode{SeriesID: series.ID, Season: 1,
		Number: 1, Title: "Dulcinea", State: model.StateImporting}, "fixture")
	if err != nil {
		t.Fatal(err)
	}
	_, err = db.Episodes().Create(ctx, model.Episode{SeriesID: series.ID, Season: 1,
		Number: 2, Title: "The Big Empty", State: model.StateWanted}, "fixture")
	if err != nil {
		t.Fatal(err)
	}
	releaseParsed := parser.Parse("The Expanse S01 1080p WEB-DL x264-GROUP")
	parsedJSON, _ := json.Marshal(releaseParsed)
	release, err := db.Releases().Upsert(ctx, model.StoredRelease{Indexer: "fixture",
		RawTitle: "The Expanse S01 1080p WEB-DL x264-GROUP", InfoHash: "abc123",
		Magnet: "magnet:?xt=urn:btih:abc123", ParsedJSON: string(parsedJSON)})
	if err != nil {
		t.Fatal(err)
	}
	download := filepath.Join(dir, "download")
	_ = os.Mkdir(download, 0o755)
	one := filepath.Join(download, "The.Expanse.S01E01.1080p.WEB-DL.x264-GROUP.mkv")
	two := filepath.Join(download, "The.Expanse.S01E02.1080p.WEB-DL.x264-GROUP.mkv")
	writeSized(t, one, 1024*1024)
	writeSized(t, two, 1024*1024)
	if err := os.WriteFile(filepath.Join(download,
		"The.Expanse.S01E01.1080p.WEB-DL.x264-GROUP.en.srt"), []byte("subtitle"), 0o644); err != nil {
		t.Fatal(err)
	}
	grab, err := db.Grabs().Create(ctx, model.Grab{SubjectType: model.SubjectEpisode,
		SubjectID: e1.ID, ReleaseID: release.ID, TorrentHash: "abc123", Category: "reelay-tv",
		State: model.GrabImporting, Progress: 1, ContentPath: download, ProgressedAt: time.Now()})
	if err != nil {
		t.Fatal(err)
	}
	service, err := New(Options{Store: db, Config: cfg, Logger: logger})
	if err != nil {
		t.Fatal(err)
	}
	if err := service.ImportCompleted(ctx, grab.ID); err != nil {
		t.Fatal(err)
	}
	wantDir := filepath.Join(tvRoot, "The Expanse (2015)", "Season 01")
	entries, err := os.ReadDir(wantDir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 3 {
		t.Fatalf("destination entries=%d, want two videos and one subtitle", len(entries))
	}
	e1, _ = db.Episodes().Get(ctx, e1.ID)
	if e1.State != model.StateImported || e1.ImportedPath == "" {
		t.Fatalf("episode state=%s path=%q", e1.State, e1.ImportedPath)
	}
}

func TestRefusesDestinationOutsideRoot(t *testing.T) {
	root := filepath.Join(t.TempDir(), "root")
	outside := filepath.Join(filepath.Dir(root), "outside.mkv")
	if err := ensureInside(root, outside); err == nil {
		t.Fatal("outside path was accepted")
	}
}

func TestReplacementMovesExistingFileToRecycle(t *testing.T) {
	dir := t.TempDir()
	root := filepath.Join(dir, "library")
	if err := os.Mkdir(root, 0o755); err != nil {
		t.Fatal(err)
	}
	source := filepath.Join(dir, "upgrade.mkv")
	dest := filepath.Join(root, "Movie", "movie.mkv")
	if err := os.Mkdir(filepath.Dir(dest), 0o755); err != nil {
		t.Fatal(err)
	}
	writeSized(t, source, 4096)
	if err := os.WriteFile(dest, []byte("old"), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg := importerConfig(root, root)
	cfg.Library.Hardlink = false
	service := &Service{cfg: cfg, link: os.Link}
	method, replaced, skipped, err := service.land(source, dest, root)
	if err != nil || method != "copy" || skipped || replaced == "" {
		t.Fatalf("method=%s replaced=%q skipped=%v err=%v", method, replaced, skipped, err)
	}
	if got, err := os.ReadFile(replaced); err != nil || string(got) != "old" {
		t.Fatalf("recycled content=%q err=%v", got, err)
	}
	if info, err := os.Stat(dest); err != nil || info.Size() != 4096 {
		t.Fatalf("upgrade size=%v err=%v", info, err)
	}
}

func TestNamingFallbackPathCapAndUnsupportedLink(t *testing.T) {
	fallback := parser.Parsed{Title: "show", TitleRaw: "Show", Year: 2020,
		Season: 2, Episodes: []int{3}, Resolution: "1080p", Source: "webdl",
		VideoCodec: "x265", ReleaseGroup: "GROUP"}
	merged := mergeParsed(parser.Parsed{}, fallback)
	if merged.Title != fallback.Title || merged.Resolution != "1080p" || merged.Season != 2 {
		t.Fatalf("mergeParsed=%+v", merged)
	}
	long := filepath.Join(t.TempDir(), "folder", "a-very-long-filename-that-needs-truncation.mkv")
	capped := capPath(long, len(filepath.Dir(long))+20)
	if len(capped) > len(filepath.Dir(long))+20 || filepath.Ext(capped) != ".mkv" {
		t.Fatalf("capPath=%q", capped)
	}
	if !isUnsupportedLink(errors.New("operation not supported")) {
		t.Fatal("unsupported link error was not recognised")
	}
}

func TestPostImportWebhook(t *testing.T) {
	called := make(chan map[string]any, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer r.Body.Close()
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Error(err)
		}
		called <- body
		w.WriteHeader(http.StatusNoContent)
	}))
	defer server.Close()
	cfg := importerConfig(t.TempDir(), t.TempDir())
	cfg.Library.PostImportWebhook = server.URL
	service := &Service{cfg: cfg, http: server.Client(), log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	service.postWebhook(model.Grab{ID: 7, SubjectType: model.SubjectMovie, SubjectID: 9}, "/library/movie.mkv")
	select {
	case body := <-called:
		if body["event"] != "imported" || body["path"] != "/library/movie.mkv" {
			t.Fatalf("webhook body=%v", body)
		}
	case <-time.After(time.Second):
		t.Fatal("webhook was not delivered")
	}
}

func importerConfig(tv, movies string) *config.Config {
	return &config.Config{Library: config.Library{TVRoot: tv, MovieRoot: movies,
		Hardlink: true, RecycleDir: ".recycle", MinVideoSizeMB: 1,
		VideoExtensions: []string{".mkv", ".mp4"}, SubtitleExtensions: []string{".srt", ".ass"},
		IgnorePatterns: []string{"sample", "extras"}, MaxPathLength: 240,
		TVFolderTemplate:    "{Title} ({Year})/Season {Season}",
		TVFileTemplate:      "{Title} - S{Season}E{Episode} - {EpisodeTitle} [{Resolution} {Source} {VideoCodec}]-{Group}",
		AnimeFileTemplate:   "{Title} - S{Season}E{Episode} - {Absolute} - {EpisodeTitle} [{Resolution} {Source} {VideoCodec}]-{Group}",
		MovieFolderTemplate: "{Title} ({Year})", MovieFileTemplate: "{Title} ({Year}) [{Resolution} {Source} {VideoCodec}]-{Group}"}}
}

func writeSized(t *testing.T, path string, size int64) {
	t.Helper()
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.Truncate(size); err != nil {
		_ = f.Close()
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
}
