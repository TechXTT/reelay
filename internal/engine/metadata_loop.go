package engine

import (
	"context"
	"errors"
	"fmt"
	"time"

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
	var errs []error
	for _, item := range series {
		if item.TVmazeID <= 0 || item.MonitorMode == model.MonitorNone {
			continue
		}
		episodes, err := e.tvmaze.SeriesEpisodes(ctx, item.TVmazeID)
		if err != nil {
			errs = append(errs, fmt.Errorf("refresh %s: %w", item.Title, err))
			continue
		}
		latestSeason := 0
		for _, episode := range episodes {
			if episode.Season > latestSeason {
				latestSeason = episode.Season
			}
		}
		for _, episode := range episodes {
			wanted := e.monitorEpisode(item, episode.Season, episode.AirDate, latestSeason)
			initial := model.StateUnmonitored
			if wanted {
				initial = model.StateWanted
			}
			stored, created, err := e.store.Episodes().UpsertMetadata(ctx, model.Episode{
				SeriesID: item.ID, Season: episode.Season, Number: episode.Number,
				AbsoluteNumber: episode.AbsoluteNumber, Title: episode.Title, AirDate: episode.AirDate,
			}, initial, "episode announced by TVmaze")
			if err != nil {
				errs = append(errs, fmt.Errorf("store %s S%02dE%02d: %w",
					item.Title, episode.Season, episode.Number, err))
				continue
			}
			if !created && wanted && stored.State == model.StateUnmonitored {
				lock, lockErr := e.store.Locks().Acquire(ctx, model.SubjectEpisode, stored.ID,
					"metadata-refresh", time.Minute)
				if lockErr != nil {
					if !errors.Is(lockErr, store.ErrLocked) {
						errs = append(errs, lockErr)
					}
					continue
				}
				_, transitionErr := e.store.Transitions().TransitionLocked(ctx, lock,
					model.StateWanted, "episode aired", "air date plus grace has passed")
				_ = lock.Release(context.WithoutCancel(ctx))
				if transitionErr != nil {
					errs = append(errs, transitionErr)
				}
			}
		}
		if err := e.store.Series().MarkRefreshed(ctx, item.ID, e.clock.Now().UTC()); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
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
