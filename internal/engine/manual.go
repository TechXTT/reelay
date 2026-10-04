package engine

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

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
	return e.grabStoredRelease(ctx, subject, id, releaseID, grabIntent{owner: "manual",
		reason: "manual release selected", detail: fmt.Sprintf("release_id=%d", releaseID)})
}

// grabIntent names why a stored release is grabbed. owner prefixes the lock
// owners and failure reasons, reason is the search transition and grab reason,
// and detail is the search transition detail.
type grabIntent struct{ owner, reason, detail string }

// grabStoredRelease grabs an already stored release for an item. Manual picks
// and failure fallbacks share it, so both keep the single active download per
// series rule, pack locks, the free-space check and the global pause behaviour.
func (e *Engine) grabStoredRelease(ctx context.Context, subject model.SubjectType, id, releaseID int64,
	intent grabIntent) (model.Grab, error) {
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
	if err := e.store.Transitions().RequestSearchNow(ctx, subject, id, intent.reason); err != nil {
		return model.Grab{}, err
	}
	lock, err := e.store.Locks().Acquire(ctx, subject, id, intent.owner+"-grab", 5*time.Minute)
	if err != nil {
		return model.Grab{}, err
	}
	defer func() { _ = lock.Release(context.WithoutCancel(ctx)) }()
	if _, err := e.store.Transitions().TransitionLocked(ctx, lock, model.StateSearching,
		intent.reason, intent.detail); err != nil {
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
		packLocks, _, packErr := e.lockPackEpisodes(ctx, episodes, id, parsed, intent.owner+"-season-pack", false)
		if packErr != nil {
			_, _ = e.store.Transitions().SearchRetryLocked(ctx, lock,
				e.clock.Now().Add(15*time.Minute), intent.owner+" grab coordination failed", packErr.Error(), false)
			return model.Grab{}, packErr
		}
		defer releaseItemLocks(packLocks)
		locks = append(locks, packLocks...)
	}
	category, savePath := e.cfg.Downloader.CategoryTV, e.cfg.Downloader.SavePathTV
	if subject == model.SubjectMovie {
		category, savePath = e.cfg.Downloader.CategoryMovies, e.cfg.Downloader.SavePathMovies
	}
	if lowDisk := e.checkSpace(release.SizeBytes, savePath, e.libraryRoot(subject)); lowDisk != nil {
		if err := e.holdForSpace(ctx, lock, subject, id, lowDisk); err != nil {
			return model.Grab{}, err
		}
		return model.Grab{}, lowDisk
	}
	var hash string

	request, err := e.addRequestFor(ctx, release.Indexer, release.Magnet, release.DownloadURL)
	if err == nil {
		request.Category, request.SavePath, request.Paused = category, savePath, e.cfg.Downloader.AddPaused
		hash, err = e.addDownload(ctx, request)
	}
	if err != nil {
		_, _ = e.store.Transitions().SearchRetryLocked(ctx, lock, e.clock.Now().Add(15*time.Minute),
			intent.owner+" grab failed", err.Error(), false)
		return model.Grab{}, err
	}
	grab, err := e.store.Grabs().CreateGrabbedFor(ctx, locks, model.Grab{SubjectType: subject,
		SubjectID: id, ReleaseID: releaseID, TorrentHash: hash, Category: category}, intent.reason)
	if err != nil {
		_ = e.downloader.Remove(context.WithoutCancel(ctx), hash, false)
	}
	return grab, err
}
