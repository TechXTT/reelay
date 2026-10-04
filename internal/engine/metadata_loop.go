package engine

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/TechXTT/reelay/internal/metadata"
	"github.com/TechXTT/reelay/internal/model"
	"github.com/TechXTT/reelay/internal/store"
)

func (e *Engine) MetadataOnce(ctx context.Context) error {
	if e.tvmaze == nil {
		return nil
	}
	series, err := e.store.Series().ListFollowing(ctx, 500)
	if err != nil {
		return err
	}
	seriesIDs := make([]int64, len(series))
	for i, item := range series {
		seriesIDs[i] = item.ID
	}
	selectedSeasonsBySeries, err := e.store.Requests().SelectedSeasonsBySeries(ctx, seriesIDs)
	if err != nil {
		return err
	}
	trialLimits, err := e.store.Requests().TrialEpisodeLimits(ctx, seriesIDs)
	if err != nil {
		return err
	}
	var errs []error
	for _, item := range series {
		selectedSeasons := selectedSeasonsBySeries[item.ID]
		trialLimit := trialLimits[item.ID]
		if item.TVmazeID <= 0 || (item.MonitorMode == model.MonitorNone && len(selectedSeasons) == 0 && trialLimit == 0) {
			continue
		}
		episodes, err := e.tvmaze.SeriesEpisodes(ctx, item.TVmazeID)
		if err != nil {
			errs = append(errs, fmt.Errorf("refresh %s: %w", item.Title, err))
			continue
		}
		var trialEpisodes []int64
		if trialLimit > 0 {
			trialEpisodes, err = e.prepareTrialEpisodes(ctx, item.ID, episodes)
			if err != nil {
				return err
			}
		}
		errs = append(errs, e.storeEpisodes(ctx, item, episodes, selectedSeasons, trialEpisodes)...)
		if err := e.store.Series().MarkRefreshed(ctx, item.ID, e.clock.Now().UTC()); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// prepareTrialEpisodes stores every announced episode as unmonitored so trial
// selection can see the whole series, then returns the selected episode IDs.
func (e *Engine) prepareTrialEpisodes(ctx context.Context, seriesID int64, episodes []metadata.Episode) ([]int64, error) {
	for _, episode := range episodes {
		if _, _, err := e.store.Episodes().UpsertMetadata(ctx, episodeModel(seriesID, episode),
			model.StateUnmonitored, "trial episode metadata"); err != nil {
			return nil, err
		}
	}
	if err := e.store.Requests().PrepareTrialEpisodes(ctx, seriesID); err != nil {
		return nil, err
	}
	return e.store.Requests().TrialEpisodeIDs(ctx, seriesID)
}

// storeEpisodes upserts the provider episodes and promotes unmonitored ones
// that are now wanted. Failures are collected so one bad episode does not
// block the rest of the series.
func (e *Engine) storeEpisodes(ctx context.Context, item model.Series, episodes []metadata.Episode,
	selectedSeasons []int, trialEpisodes []int64) []error {
	now := e.clock.Now().UTC()
	pastGrace := func(airDate *time.Time) bool {
		return airDate != nil && !now.Before(airDate.Add(e.cfg.Schedules.AirGrace.Duration))
	}
	latestSeason := latestAiredSeason(episodes, now)
	var errs []error
	for _, episode := range episodes {
		wanted := e.monitorEpisode(item, episode.Season, episode.AirDate, latestSeason)
		if slices.Contains(selectedSeasons, episode.Season) && pastGrace(episode.AirDate) {
			wanted = true
		}
		initial := model.StateUnmonitored
		if wanted {
			initial = model.StateWanted
		}
		stored, created, err := e.store.Episodes().UpsertMetadata(ctx, episodeModel(item.ID, episode),
			initial, "episode announced by TVmaze")
		if err != nil {
			errs = append(errs, fmt.Errorf("store %s S%02dE%02d: %w",
				item.Title, episode.Season, episode.Number, err))
			continue
		}
		if slices.Contains(trialEpisodes, stored.ID) && pastGrace(episode.AirDate) {
			wanted = true
		}
		if created || !wanted || stored.State != model.StateUnmonitored {
			continue
		}
		if err := e.markEpisodeAired(ctx, stored.ID); err != nil && !errors.Is(err, store.ErrLocked) {
			errs = append(errs, err)
		}
	}
	return errs
}

func (e *Engine) markEpisodeAired(ctx context.Context, episodeID int64) error {
	lock, err := e.store.Locks().Acquire(ctx, model.SubjectEpisode, episodeID, "metadata-refresh", time.Minute)
	if err != nil {
		return err
	}
	defer func() { _ = lock.Release(context.WithoutCancel(ctx)) }()
	_, err = e.store.Transitions().TransitionLocked(ctx, lock,
		model.StateWanted, "episode aired", "air date plus grace has passed")
	return err
}

// latestAiredSeason is the newest season with an aired episode, falling back
// to the newest announced season when nothing has aired yet.
func latestAiredSeason(episodes []metadata.Episode, now time.Time) int {
	latestKnown, latestAired := 0, 0
	for _, episode := range episodes {
		if episode.Season <= 0 {
			continue
		}
		latestKnown = max(latestKnown, episode.Season)
		if episode.AirDate != nil && !episode.AirDate.After(now) {
			latestAired = max(latestAired, episode.Season)
		}
	}
	if latestAired == 0 {
		return latestKnown
	}
	return latestAired
}

func episodeModel(seriesID int64, episode metadata.Episode) model.Episode {
	return model.Episode{SeriesID: seriesID, Season: episode.Season, Number: episode.Number,
		AbsoluteNumber: episode.AbsoluteNumber, Title: episode.Title, AirDate: episode.AirDate}
}

func (e *Engine) monitorEpisode(series model.Series, season int, airDate *time.Time, latestSeason int) bool {
	if series.MonitorMode == model.MonitorNone || airDate == nil {
		return false
	}
	now := e.clock.Now().UTC()
	if now.Before(airDate.Add(e.cfg.Schedules.AirGrace.Duration)) {
		return false
	}
	switch series.MonitorMode {
	case model.MonitorAll:
		return true
	case model.MonitorFutureOnly:
		return !airDate.Before(series.AddedAt)
	case model.MonitorLatestSeason:
		return season == latestSeason
	default:
		return false
	}
}
