package engine

import (
	"context"
	"testing"
	"time"

	"github.com/TechXTT/reelay/internal/clock"
	"github.com/TechXTT/reelay/internal/metadata"
	"github.com/TechXTT/reelay/internal/model"
)

func TestTrialCheckpointsStayBoundedUntilContinueVote(t *testing.T) {
	for _, scope := range []string{"one_episode", "three_episodes", "first_season"} {
		t.Run(scope, func(t *testing.T) {
			var ctx = context.Background()
			var clk = clock.NewFake(time.Date(2026, 10, 4, 12, 0, 0, 0, time.UTC))
			var db, log, profile = journeyStore(t, clk.Now)
			var series, err = db.Series().Create(ctx, model.Series{Title: "Trial", TMDBID: 12, TVmazeID: 12, MonitorMode: model.MonitorNone, Status: model.SeriesFollowing, ProfileID: profile.ID, RootFolder: t.TempDir()})

			if err != nil {
				t.Fatal(err)
			}
			if _, err := db.Requests().Create(ctx, model.MediaRequest{ServerID: "server", UserID: "user", MediaType: "series", TMDBID: 12, Title: "Trial", SubjectType: "series", SubjectID: series.ID}); err != nil {
				t.Fatal(err)
			}
			if err := db.Requests().StartTrial(ctx, "server", "user", 12, scope); err != nil {
				t.Fatal(err)
			}
			aired, future := clk.Now().Add(-24*time.Hour), clk.Now().Add(24*time.Hour)
			provider := metadataEpisodeFixture{episodes: []metadata.Episode{{Season: 2, Number: 1, AirDate: &aired}, {Season: 1, Number: 4, AirDate: &future}, {Season: 0, Number: 1, AirDate: &aired}, {Season: 1, Number: 3, AirDate: &aired}, {Season: 1, Number: 1, AirDate: &aired}, {Season: 1, Number: 2, AirDate: &aired}}}
			eng, err := New(Options{Store: db, Config: testConfig(), Downloader: &fakeDownloader{}, TVmaze: provider, Clock: clk, Logger: log})
			if err != nil {
				t.Fatal(err)
			}
			if err := eng.MetadataOnce(ctx); err != nil {
				t.Fatal(err)
			}
			trial, err := db.Requests().TrialForTitle(ctx, "server", "user", 12)
			if err != nil {
				t.Fatal(err)
			}
			limit := map[string]int{"one_episode": 1, "three_episodes": 3, "first_season": 4}[scope]
			if len(trial.Episodes) != limit || trial.EpisodeLimit != limit {
				t.Fatalf("incorrect selection: %+v", trial)
			}
			for _, item := range provider.episodes {
				ep, err := db.Episodes().BySeriesNumber(ctx, series.ID, item.Season, item.Number)
				wanted := item.Season == 1 && item.Number <= limit && item.Number < 4
				if err != nil || (ep.State == model.StateWanted) != wanted {
					t.Fatalf("S%dE%d state=%s error=%v", item.Season, item.Number, ep.State, err)
				}
			}
			if err := db.Requests().VoteTrial(ctx, trial.RequestID, "continue", 5); err == nil {
				t.Fatal("unwatched trial continued")
			}
			clk.Advance(48 * time.Hour)
			if err := eng.MetadataOnce(ctx); err != nil {
				t.Fatal(err)
			}
			for _, episode := range trial.Episodes {
				if err := db.Requests().MarkTrialWatched(ctx, trial.RequestID, "server", "user", episode.Season, episode.Number); err != nil {
					t.Fatal(err)
				}
			}
			if err := db.Requests().VoteTrial(ctx, trial.RequestID, "continue", 5); err != nil {
				t.Fatal(err)
			}
			if err := eng.MetadataOnce(ctx); err != nil {
				t.Fatal(err)
			}
			rest, err := db.Episodes().BySeriesNumber(ctx, series.ID, 2, 1)
			if err != nil || rest.State != model.StateWanted {
				t.Fatalf("continue failed to monitor next season: %+v %v", rest, err)
			}
			if err := db.Requests().VoteTrial(ctx, trial.RequestID, "continue", 5); err != nil {
				t.Fatal("vote retry must be idempotent", err)
			}
		})
	}
}
