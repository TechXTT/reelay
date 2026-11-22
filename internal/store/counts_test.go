package store

import (
	"context"
	"testing"

	"github.com/TechXTT/reelay/internal/model"
)

func TestCountByStateGroupsItemsAndGrabs(t *testing.T) {
	ctx := context.Background()
	s := migratedDomainStore(t)
	p, _ := s.Profiles().Default(ctx)

	empty, err := s.Episodes().CountByState(ctx)
	if err != nil || len(empty) != 0 {
		t.Fatalf("empty episode counts = %v, %v", empty, err)
	}
	series, err := s.Series().Create(ctx, model.Series{Title: "The Expanse", SortTitle: "expanse, the",
		Year: 2015, MonitorMode: model.MonitorFutureOnly, Status: model.SeriesFollowing,
		ProfileID: p.ID, RootFolder: "/tv"})
	if err != nil {
		t.Fatal(err)
	}
	for number, state := range []model.ItemState{model.StateWanted, model.StateWanted, model.StateImported} {
		if _, err := s.Episodes().Create(ctx, model.Episode{SeriesID: series.ID, Season: 1,
			Number: number + 1, State: state}, "test"); err != nil {
			t.Fatal(err)
		}
	}
	movie, err := s.Movies().Create(ctx, model.Movie{Title: "Moon", Year: 2009, ProfileID: p.ID,
		RootFolder: "/movies", State: model.StateWanted}, "test")
	if err != nil {
		t.Fatal(err)
	}
	release, err := s.Releases().Upsert(ctx, model.StoredRelease{Indexer: "tpb",
		RawTitle: "Moon.2009.1080p.BluRay.x264-GROUP", InfoHash: "ABCDEF0123456789",
		Magnet: "magnet:?xt=urn:btih:ABCDEF0123456789", SizeBytes: 1000})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Grabs().Create(ctx, model.Grab{SubjectType: model.SubjectMovie, SubjectID: movie.ID,
		ReleaseID: release.ID, TorrentHash: release.InfoHash, Category: "reelay-movies",
		State: model.GrabDownloading}); err != nil {
		t.Fatal(err)
	}

	episodes, err := s.Episodes().CountByState(ctx)
	if err != nil || len(episodes) != 2 || episodes[model.StateWanted] != 2 || episodes[model.StateImported] != 1 {
		t.Fatalf("episode counts = %v, %v", episodes, err)
	}
	movies, err := s.Movies().CountByState(ctx)
	if err != nil || len(movies) != 1 || movies[model.StateWanted] != 1 {
		t.Fatalf("movie counts = %v, %v", movies, err)
	}
	grabs, err := s.Grabs().CountByState(ctx)
	if err != nil || len(grabs) != 1 || grabs[model.GrabDownloading] != 1 {
		t.Fatalf("grab counts = %v, %v", grabs, err)
	}
}

func TestPendingNotificationsCountsOnlyUndeliveredLiveRequests(t *testing.T) {
	ctx := context.Background()
	s := migratedDomainStore(t)
	p, _ := s.Profiles().Default(ctx)
	movie, err := s.Movies().Create(ctx, model.Movie{Title: "Moon", Year: 2009, ProfileID: p.ID,
		RootFolder: "/movies", TMDBID: 99, State: model.StateWanted}, "test")
	if err != nil {
		t.Fatal(err)
	}
	for _, user := range []string{"alice", "bob"} {
		if err := s.Recommendations().UpsertUser(ctx, model.JellyfinUser{ServerID: "server", UserID: user,
			DisplayName: user, Enabled: true}); err != nil {
			t.Fatal(err)
		}
		if _, err := s.Requests().Create(ctx, model.MediaRequest{ServerID: "server", UserID: user,
			MediaType: "movie", TMDBID: 99, Title: "Moon", SubjectType: "movie", SubjectID: movie.ID}); err != nil {
			t.Fatal(err)
		}
	}
	if count, err := s.Requests().PendingNotifications(ctx); err != nil || count != 0 {
		t.Fatalf("pending before outbox = %d, %v", count, err)
	}
	_, err = s.Writer().ExecContext(ctx, `INSERT INTO availability_outbox(request_id,item_id,payload,created_at,next_attempt_at,delivered_at)
 SELECT id,'item','{}','2026-01-01T00:00:00Z','2026-01-01T00:00:00Z',CASE user_id WHEN 'bob' THEN '2026-01-02T00:00:00Z' END FROM media_requests`)
	if err != nil {
		t.Fatal(err)
	}
	if count, err := s.Requests().PendingNotifications(ctx); err != nil || count != 1 {
		t.Fatalf("pending = %d, %v; want 1 (bob already delivered)", count, err)
	}
}
