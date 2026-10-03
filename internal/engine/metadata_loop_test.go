package engine

import (
	"context"
	"io"
	"log/slog"
	"path/filepath"
	"testing"
	"time"

	"github.com/TechXTT/reelay/internal/clock"
	"github.com/TechXTT/reelay/internal/config"
	"github.com/TechXTT/reelay/internal/metadata"
	"github.com/TechXTT/reelay/internal/model"
	"github.com/TechXTT/reelay/internal/store"
)

type metadataEpisodeFixture struct{ episodes []metadata.Episode }

func (metadataEpisodeFixture) SearchSeries(context.Context, string) ([]metadata.Series, error) {
	return nil, nil
}
func (f metadataEpisodeFixture) SeriesEpisodes(context.Context, int) ([]metadata.Episode, error) {
	return f.episodes, nil
}

func TestMetadataLatestSeasonUsesMostRecentAiredNumberedSeason(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 10, 3, 12, 0, 0, 0, time.UTC)
	db, err := store.Open(ctx, store.Options{Path: filepath.Join(t.TempDir(), "metadata-latest-season.db"), CacheKB: 512})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	if err := store.Migrate(ctx, db, logger); err != nil {
		t.Fatal(err)
	}
	_, err = db.Profiles().Seed(ctx, []model.QualityProfile{{Name: "test", IsDefault: true, AllowedResolutions: []string{"1080p"}, AllowedSources: []string{"webdl"}, MinSeeders: 1}})
	if err != nil {
		t.Fatal(err)
	}
	profile, err := db.Profiles().Default(ctx)
	if err != nil {
		t.Fatal(err)
	}
	series, err := db.Series().Create(ctx, model.Series{Title: "Seasoned", TVmazeID: 7, TMDBID: 7, MonitorMode: model.MonitorLatestSeason,
		Status: model.SeriesFollowing, ProfileID: profile.ID, RootFolder: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	aired := now.AddDate(0, -4, 0)
	specialAirDate := now.AddDate(0, -6, 0)
	future := now.AddDate(0, 4, 0)
	provider := metadataEpisodeFixture{episodes: []metadata.Episode{
		{Season: 0, Number: 1, Title: "Special", AirDate: &specialAirDate},
		{Season: 1, Number: 1, Title: "Earlier season", AirDate: &aired},
		{Season: 2, Number: 1, Title: "Upcoming season", AirDate: &future},
	}}
	clk := clock.NewFake(now)
	cfg := testConfig()
	cfg.Schedules.AirGrace = config.Dur(time.Hour)
	eng, err := New(Options{Store: db, Config: cfg, Downloader: &fakeDownloader{hash: "0123456789abcdef0123456789abcdef01234567"},
		TVmaze: provider, Clock: clk, Logger: logger})
	if err != nil {
		t.Fatal(err)
	}
	if err := eng.MetadataOnce(ctx); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		season int
		want   model.ItemState
	}{
		{season: 0, want: model.StateUnmonitored},
		{season: 1, want: model.StateWanted},
		{season: 2, want: model.StateUnmonitored},
	} {
		episode, err := db.Episodes().BySeriesNumber(ctx, series.ID, tc.season, 1)
		if err != nil || episode.State != tc.want {
			t.Fatalf("season %d state=%s err=%v, want %s", tc.season, episode.State, err, tc.want)
		}
	}
}
