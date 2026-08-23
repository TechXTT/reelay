package store

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/TechXTT/reelay/internal/model"
)

func testProfile() model.QualityProfile {
	return model.QualityProfile{
		Name: "Test 1080p", IsDefault: true,
		AllowedResolutions: []string{"1080p", "720p"},
		AllowedSources:     []string{"webdl", "bluray"},
		MinSizeMB:          50, MaxSizeMB: 20000, MinSeeders: 2,
		RequiredTerms: []string{}, BannedTerms: []string{"cam"},
		PreferredGroups: map[string]int{"NTb": 50},
		LanguagePrefs:   []string{"en"}, HDRPrefs: []string{},
	}
}

func migratedDomainStore(t *testing.T) *Store {
	t.Helper()
	s := openTestStore(t)
	if err := Migrate(context.Background(), s, testLogger()); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Profiles().Seed(context.Background(), []model.QualityProfile{testProfile()}); err != nil {
		t.Fatal(err)
	}
	return s
}

func TestProfileSeedOnlyRunsOnceAndRoundTripsJSON(t *testing.T) {
	ctx := context.Background()
	s := openTestStore(t)
	if err := Migrate(ctx, s, testLogger()); err != nil {
		t.Fatal(err)
	}
	seeded, err := s.Profiles().Seed(ctx, []model.QualityProfile{testProfile()})
	if err != nil || !seeded {
		t.Fatalf("first seed = %v, %v; want true, nil", seeded, err)
	}
	seeded, err = s.Profiles().Seed(ctx, []model.QualityProfile{{Name: "Must Not Replace"}})
	if err != nil || seeded {
		t.Fatalf("second seed = %v, %v; want false, nil", seeded, err)
	}
	p, err := s.Profiles().Default(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if p.Name != "Test 1080p" || len(p.AllowedResolutions) != 2 ||
		p.PreferredGroups["NTb"] != 50 || len(p.BannedTerms) != 1 {
		t.Fatalf("profile did not round trip: %+v", p)
	}
}

func TestSeriesEpisodeMovieRoundTripAndInitialAudit(t *testing.T) {
	ctx := context.Background()
	s := migratedDomainStore(t)
	p, _ := s.Profiles().Default(ctx)
	series, err := s.Series().Create(ctx, model.Series{
		Title: "The Expanse", SortTitle: "expanse, the", Year: 2015,
		Aliases: []string{"expanse"}, MonitorMode: model.MonitorFutureOnly,
		Status: model.SeriesFollowing, ProfileID: p.ID, RootFolder: "/tv",
	})
	if err != nil {
		t.Fatal(err)
	}
	air := time.Date(2026, 8, 20, 0, 0, 0, 0, time.UTC)
	ep, err := s.Episodes().Create(ctx, model.Episode{
		SeriesID: series.ID, Season: 1, Number: 1, Title: "Dulcinea",
		AirDate: &air, State: model.StateWanted,
	}, "aired episode added")
	if err != nil {
		t.Fatal(err)
	}
	movie, err := s.Movies().Create(ctx, model.Movie{
		Title: "Arrival", SortTitle: "arrival", Year: 2016,
		ProfileID: p.ID, RootFolder: "/movies", State: model.StateWanted,
	}, "queued by test")
	if err != nil {
		t.Fatal(err)
	}

	gotSeries, err := s.Series().Get(ctx, series.ID)
	if err != nil || len(gotSeries.Aliases) != 1 || gotSeries.Aliases[0] != "expanse" {
		t.Fatalf("series round trip = %+v, %v", gotSeries, err)
	}
	gotEpisode, err := s.Episodes().Get(ctx, ep.ID)
	if err != nil || gotEpisode.FirstWantedAt == nil || gotEpisode.AirDate == nil {
		t.Fatalf("episode round trip = %+v, %v", gotEpisode, err)
	}
	gotMovie, err := s.Movies().Get(ctx, movie.ID)
	if err != nil || gotMovie.State != model.StateWanted || gotMovie.FirstWantedAt == nil {
		t.Fatalf("movie round trip = %+v, %v", gotMovie, err)
	}
	for subject, id := range map[model.SubjectType]int64{
		model.SubjectEpisode: ep.ID, model.SubjectMovie: movie.ID,
	} {
		history, err := s.Transitions().History(ctx, subject, id, 10)
		if err != nil || len(history) != 1 || history[0].From != "" || history[0].To != model.StateWanted {
			t.Fatalf("initial history for %s:%d = %+v, %v", subject, id, history, err)
		}
	}
}

func TestTransitionIsValidatedLockedAndAuditedAtomically(t *testing.T) {
	ctx := context.Background()
	s := migratedDomainStore(t)
	p, _ := s.Profiles().Default(ctx)
	movie, err := s.Movies().Create(ctx, model.Movie{
		Title: "Primer", Year: 2004, ProfileID: p.ID, RootFolder: "/movies",
		State: model.StateWanted,
	}, "added")
	if err != nil {
		t.Fatal(err)
	}
	tr, err := s.Transitions().Transition(ctx, model.SubjectMovie, movie.ID,
		model.StateSearching, "search loop selected item", "attempt 1")
	if err != nil || tr.From != model.StateWanted || tr.To != model.StateSearching {
		t.Fatalf("transition = %+v, %v", tr, err)
	}
	_, err = s.Transitions().Transition(ctx, model.SubjectMovie, movie.ID,
		model.StateImported, "skip the lifecycle", "")
	if !errors.Is(err, ErrInvalidTransition) {
		t.Fatalf("illegal transition = %v, want ErrInvalidTransition", err)
	}
	got, _ := s.Movies().Get(ctx, movie.ID)
	if got.State != model.StateSearching {
		t.Fatalf("illegal transition changed state to %s", got.State)
	}
	history, err := s.Transitions().History(ctx, model.SubjectMovie, movie.ID, 10)
	if err != nil || len(history) != 2 {
		t.Fatalf("history = %+v, %v; illegal edge must not be audited", history, err)
	}
}

func TestItemLockContentionReleaseAndExpiry(t *testing.T) {
	ctx := context.Background()
	s := migratedDomainStore(t)
	now := time.Date(2026, 8, 23, 12, 0, 0, 0, time.UTC)
	s.now = func() time.Time { return now }

	first, err := s.Locks().Acquire(ctx, model.SubjectMovie, 42, "worker-a", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Locks().Acquire(ctx, model.SubjectMovie, 42, "worker-b", time.Minute); !errors.Is(err, ErrLocked) {
		t.Fatalf("contending lock = %v, want ErrLocked", err)
	}
	if err := first.Release(ctx); err != nil {
		t.Fatal(err)
	}
	second, err := s.Locks().Acquire(ctx, model.SubjectMovie, 42, "worker-b", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	now = now.Add(2 * time.Minute)
	third, err := s.Locks().Acquire(ctx, model.SubjectMovie, 42, "worker-c", time.Minute)
	if err != nil {
		t.Fatalf("expired lease was not reclaimed: %v", err)
	}
	if err := second.Release(ctx); !errors.Is(err, ErrLocked) {
		t.Fatalf("stale owner release = %v, want ErrLocked", err)
	}
	if err := third.Release(ctx); err != nil {
		t.Fatal(err)
	}
}

func TestReleaseDecisionsGrabBlacklistAndImport(t *testing.T) {
	ctx := context.Background()
	s := migratedDomainStore(t)
	p, _ := s.Profiles().Default(ctx)
	movie, _ := s.Movies().Create(ctx, model.Movie{
		Title: "Moon", Year: 2009, ProfileID: p.ID, RootFolder: "/movies",
		State: model.StateWanted,
	}, "added")
	release, err := s.Releases().Upsert(ctx, model.StoredRelease{
		Indexer: "tpb", RawTitle: "Moon.2009.1080p.BluRay.x264-GROUP",
		InfoHash: "ABCDEF0123456789", Magnet: "magnet:?xt=urn:btih:ABCDEF0123456789",
		SizeBytes: 1000, Seeders: 20, ParsedJSON: `{"title":"moon"}`, Score: 123,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Decisions().ReplaceCandidates(ctx, model.SubjectMovie, movie.ID,
		[]model.CandidateEvaluation{{ReleaseID: release.ID, Accepted: true, Score: 123}}); err != nil {
		t.Fatal(err)
	}
	candidates, _ := s.Decisions().Candidates(ctx, model.SubjectMovie, movie.ID)
	if len(candidates) != 1 || !candidates[0].Accepted {
		t.Fatalf("candidates = %+v", candidates)
	}
	if err := s.Decisions().Blacklist(ctx, model.SubjectMovie, movie.ID, release.InfoHash, "stalled"); err != nil {
		t.Fatal(err)
	}
	blacklisted, err := s.Decisions().IsBlacklisted(ctx, model.SubjectMovie, movie.ID, release.InfoHash)
	if err != nil || !blacklisted {
		t.Fatalf("blacklisted = %v, %v", blacklisted, err)
	}
	grab, err := s.Grabs().Create(ctx, model.Grab{
		SubjectType: model.SubjectMovie, SubjectID: movie.ID, ReleaseID: release.ID,
		TorrentHash: release.InfoHash, Category: "reelay-movies", State: model.GrabDownloading,
	})
	if err != nil {
		t.Fatal(err)
	}
	active, _ := s.Grabs().Active(ctx)
	if len(active) != 1 || active[0].ID != grab.ID {
		t.Fatalf("active grabs = %+v", active)
	}
	if _, err := s.Imports().Create(ctx, model.ImportRecord{
		GrabID: grab.ID, SubjectType: model.SubjectMovie, SubjectID: movie.ID,
		SourcePath: "/downloads/moon.mkv", DestPath: "/movies/Moon/moon.mkv",
		Method: "hardlink", SizeBytes: 1000,
	}); err != nil {
		t.Fatal(err)
	}
}

func TestDomainRowsPersistAcrossReopen(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "reelay.db")
	open := func() *Store {
		s, err := Open(ctx, Options{Path: path, CacheKB: 512, ReadConns: 1})
		if err != nil {
			t.Fatal(err)
		}
		if err := Migrate(ctx, s, testLogger()); err != nil {
			t.Fatal(err)
		}
		return s
	}

	first := open()
	if _, err := first.Profiles().Seed(ctx, []model.QualityProfile{testProfile()}); err != nil {
		t.Fatal(err)
	}
	p, _ := first.Profiles().Default(ctx)
	movie, err := first.Movies().Create(ctx, model.Movie{
		Title: "Coherence", Year: 2013, ProfileID: p.ID, RootFolder: "/movies",
		State: model.StateWanted,
	}, "added before restart")
	if err != nil {
		t.Fatal(err)
	}
	if err := first.Close(); err != nil {
		t.Fatal(err)
	}

	second := open()
	defer second.Close()
	got, err := second.Movies().Get(ctx, movie.ID)
	if err != nil || got.Title != "Coherence" || got.State != model.StateWanted {
		t.Fatalf("after reopen = %+v, %v", got, err)
	}
	if _, err := second.Transitions().Transition(ctx, model.SubjectMovie, movie.ID,
		model.StateSearching, "continued after restart", ""); err != nil {
		t.Fatal(err)
	}
}

func TestMetadataCacheTTL(t *testing.T) {
	ctx := context.Background()
	s := openTestStore(t)
	if err := Migrate(ctx, s, testLogger()); err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 8, 23, 12, 0, 0, 0, time.UTC)
	if err := s.Metadata().Put(ctx, "tvmaze", "episodes:1", []byte(`[{"id":1}]`), now, time.Hour); err != nil {
		t.Fatal(err)
	}
	if _, hit, err := s.Metadata().Get(ctx, "tvmaze", "episodes:1", now.Add(59*time.Minute)); err != nil || !hit {
		t.Fatalf("before expiry hit=%v err=%v", hit, err)
	}
	if _, hit, err := s.Metadata().Get(ctx, "tvmaze", "episodes:1", now.Add(time.Hour)); err != nil || hit {
		t.Fatalf("at expiry hit=%v err=%v", hit, err)
	}
	deleted, err := s.Metadata().DeleteExpired(ctx, now.Add(time.Hour), 10)
	if err != nil || deleted != 1 {
		t.Fatalf("DeleteExpired = %d, %v", deleted, err)
	}
}
