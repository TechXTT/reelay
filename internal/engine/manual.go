package engine

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"time"

	"github.com/TechXTT/reelay/internal/downloader"
	"github.com/TechXTT/reelay/internal/model"
	"github.com/TechXTT/reelay/internal/parser"
	"github.com/TechXTT/reelay/internal/store"
)

func (e *Engine) ForceSearch(ctx context.Context, subject model.SubjectType, id int64) error {
	if err := e.store.Transitions().RequestSearchNow(ctx, subject, id, "manual search requested"); err != nil {
		return err
	}
	return e.Trigger("search")
}

func (e *Engine) ManualGrab(ctx context.Context, subject model.SubjectType, id, releaseID int64) (model.Grab, error) {
	release, err := e.store.Releases().Get(ctx, releaseID)
	if err != nil {
		return model.Grab{}, err
	}
	if subject == model.SubjectEpisode {
		episode, err := e.store.Episodes().Get(ctx, id)
		if err != nil {
			return model.Grab{}, err
		}
		active, err := e.store.Episodes().SeriesHasActive(ctx, episode.SeriesID)
		if err != nil {
			return model.Grab{}, err
		}
		if active {
			return model.Grab{}, fmt.Errorf("series already has an active download: %w", store.ErrItemBusy)
		}
	}
	if err := e.store.Transitions().RequestSearchNow(ctx, subject, id, "manual release selected"); err != nil {
		return model.Grab{}, err
	}
	lock, err := e.store.Locks().Acquire(ctx, subject, id, "manual-grab", 5*time.Minute)
	if err != nil {
		return model.Grab{}, err
	}
	defer func() { _ = lock.Release(context.WithoutCancel(ctx)) }()
	if _, err := e.store.Transitions().TransitionLocked(ctx, lock, model.StateSearching,
		"manual release selected", fmt.Sprintf("release_id=%d", releaseID)); err != nil {
		return model.Grab{}, err
	}
	locks := []*store.ItemLock{lock}
	if subject == model.SubjectEpisode {
		parsed := parser.Parse(release.RawTitle)
		if release.ParsedJSON != "" {
			_ = json.Unmarshal([]byte(release.ParsedJSON), &parsed)
		}
		episode, err := e.store.Episodes().Get(ctx, id)
		if err != nil {
			return model.Grab{}, err
		}
		episodes, err := e.store.Episodes().ListBySeries(ctx, episode.SeriesID)
		if err != nil {
			return model.Grab{}, err
		}
		sort.Slice(episodes, func(i, j int) bool { return episodes[i].ID < episodes[j].ID })
		for _, covered := range episodes {
			if covered.ID == id || covered.State != model.StateWanted ||
				!parsed.CoversEpisode(covered.Season, covered.Number) {
				continue
			}
			coveredLock, lockErr := e.store.Locks().Acquire(ctx, model.SubjectEpisode,
				covered.ID, "manual-season-pack", 5*time.Minute)
			if lockErr != nil {
				releaseItemLocks(locks[1:])
				_, _ = e.store.Transitions().SearchRetryLocked(ctx, lock,
					e.clock.Now().Add(15*time.Minute), "manual grab coordination failed", lockErr.Error(), false)
				return model.Grab{}, lockErr
			}
			locks = append(locks, coveredLock)
		}
		defer releaseItemLocks(locks[1:])
	}
	category, savePath := e.cfg.Downloader.CategoryTV, e.cfg.Downloader.SavePathTV
	if subject == model.SubjectMovie {
		category, savePath = e.cfg.Downloader.CategoryMovies, e.cfg.Downloader.SavePathMovies
	}
	hash, err := e.downloader.Add(ctx, downloader.AddRequest{Magnet: release.Magnet,
		Category: category, SavePath: savePath, Paused: e.cfg.Downloader.AddPaused})
	if err != nil {
		_, _ = e.store.Transitions().SearchRetryLocked(ctx, lock, e.clock.Now().Add(15*time.Minute),
			"manual grab failed", err.Error(), false)
		return model.Grab{}, err
	}
	grab, err := e.store.Grabs().CreateGrabbedFor(ctx, locks, model.Grab{SubjectType: subject,
		SubjectID: id, ReleaseID: releaseID, TorrentHash: hash, Category: category}, "manual release selected")
	if err != nil {
		_ = e.downloader.Remove(context.WithoutCancel(ctx), hash, false)
	}
	return grab, err
}
