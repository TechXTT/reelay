package store

import (
	"context"
	"testing"

	"github.com/TechXTT/reelay/internal/model"
)

func TestTrialVotesAreScopedAndStopPreservesSharedMonitoring(t *testing.T) {
	var ctx = context.Background()
	var db = migratedDomainStore(t)
	var profile, err = db.Profiles().Default(ctx)

	if err != nil {
		t.Fatal(err)
	}
	series, err := db.Series().Create(ctx, model.Series{Title: "Shared", TMDBID: 51, MonitorMode: model.MonitorAll, Status: model.SeriesFollowing, ProfileID: profile.ID, RootFolder: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	for _, user := range []string{"alice", "bob"} {
		if err := db.Recommendations().UpsertUser(ctx, model.JellyfinUser{ServerID: "server", UserID: user, DisplayName: user, Enabled: true}); err != nil {
			t.Fatal(err)
		}
		if _, err := db.Requests().Create(ctx, model.MediaRequest{ServerID: "server", UserID: user, MediaType: "series", TMDBID: 51, Title: "Shared", SubjectType: "series", SubjectID: series.ID}); err != nil {
			t.Fatal(err)
		}
		if err := db.Requests().StartTrial(ctx, "server", user, 51, "one_episode"); err != nil {
			t.Fatal(err)
		}
	}
	for number := 1; number <= 4; number++ {
		if _, _, err := db.Episodes().UpsertMetadata(ctx, model.Episode{SeriesID: series.ID, Season: 1, Number: number}, model.StateWanted, "fixture"); err != nil {
			t.Fatal(err)
		}
	}
	if err := db.Requests().PrepareTrialEpisodes(ctx, series.ID); err != nil {
		t.Fatal(err)
	}
	alice, err := db.Requests().TrialForTitle(ctx, "server", "alice", 51)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Requests().MarkTrialWatched(ctx, alice.RequestID, "server", "bob", 1, 1); err == nil {
		t.Fatal("another user changed Alice's watch state")
	}
	if err := db.Requests().MarkTrialWatched(ctx, alice.RequestID, "server", "alice", 1, 4); err == nil {
		t.Fatal("an episode outside the trial was accepted")
	}
	if err := db.Requests().MarkTrialWatched(ctx, alice.RequestID, "server", "alice", 1, 1); err != nil {
		t.Fatal(err)
	}
	if err := db.Requests().MarkTrialWatched(ctx, alice.RequestID, "server", "alice", 1, 1); err != nil {
		t.Fatal(err)
	}
	bob, err := db.Requests().TrialForTitle(ctx, "server", "bob", 51)
	if err != nil || bob.WatchedEpisodes != 0 {
		t.Fatalf("Bob's watch progress changed: %+v %v", bob, err)
	}
	page, err := db.Requests().List(ctx, "server", "alice")
	if err != nil || page[0].TotalEpisodes != 1 {
		t.Fatalf("trial summary counted shared episodes: %+v %v", page, err)
	}
	if err := db.Requests().VoteTrial(ctx, alice.RequestID, "stop", 2); err != nil {
		t.Fatal(err)
	}
	if err := db.Requests().VoteTrial(ctx, alice.RequestID, "stop", 2); err != nil {
		t.Fatal("stop retry must be idempotent", err)
	}
	if err := db.Requests().VoteTrial(ctx, alice.RequestID, "continue", 5); err == nil {
		t.Fatal("reversed vote was accepted")
	}
	series, err = db.Series().Get(ctx, series.ID)
	if err != nil || series.MonitorMode != model.MonitorAll {
		t.Fatalf("shared series was narrowed: %+v %v", series, err)
	}
	request, err := db.Requests().Get(ctx, alice.RequestID)
	if err != nil || request.CancelledAt == nil || request.Trial.Decision != "stop" {
		t.Fatalf("stop did not persist: %+v %v", request, err)
	}
	ratings, err := db.Recommendations().Ratings(ctx, "server", "alice", "series")
	if err != nil || len(ratings) != 1 || ratings[0].Rating != 2 {
		t.Fatalf("trial rating was not saved: %+v %v", ratings, err)
	}
}

func TestShortSeriesTrialCanBeCompleted(t *testing.T) {
	var ctx = context.Background()
	var db = migratedDomainStore(t)
	var profile, _ = db.Profiles().Default(ctx)

	if err := db.Recommendations().UpsertUser(ctx, model.JellyfinUser{ServerID: "server", UserID: "user", DisplayName: "User", Enabled: true}); err != nil {
		t.Fatal(err)
	}
	series, err := db.Series().Create(ctx, model.Series{Title: "Two episodes", TMDBID: 81, MonitorMode: model.MonitorNone, Status: model.SeriesFollowing, ProfileID: profile.ID, RootFolder: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Requests().Create(ctx, model.MediaRequest{ServerID: "server", UserID: "user", MediaType: "series", TMDBID: 81, Title: series.Title, SubjectType: "series", SubjectID: series.ID}); err != nil {
		t.Fatal(err)
	}
	if err := db.Requests().StartTrial(ctx, "server", "user", 81, "three_episodes"); err != nil {
		t.Fatal(err)
	}
	for number := 1; number <= 2; number++ {
		if _, _, err := db.Episodes().UpsertMetadata(ctx, model.Episode{SeriesID: series.ID, Season: 1, Number: number}, model.StateImported, "fixture"); err != nil {
			t.Fatal(err)
		}
	}
	if err := db.Requests().PrepareTrialEpisodes(ctx, series.ID); err != nil {
		t.Fatal(err)
	}
	trial, err := db.Requests().TrialForTitle(ctx, "server", "user", 81)
	if err != nil || trial.EpisodeLimit != 2 {
		t.Fatalf("short trial target %+v %v", trial, err)
	}
	for number := 1; number <= 2; number++ {
		if err := db.Requests().MarkTrialWatched(ctx, trial.RequestID, "server", "user", 1, number); err != nil {
			t.Fatal(err)
		}
	}
	if err := db.Requests().VoteTrial(ctx, trial.RequestID, "continue", 4); err != nil {
		t.Fatal("short series must not wait forever for a third episode", err)
	}
}
