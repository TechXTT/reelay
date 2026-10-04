package engine

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/TechXTT/reelay/internal/downloader"
	"github.com/TechXTT/reelay/internal/model"
)

func (e *Engine) StatusOnce(ctx context.Context) error {
	grabs, err := e.store.Grabs().Active(ctx)
	if err != nil || len(grabs) == 0 {
		return err
	}
	hashes := make([]string, len(grabs))
	for i, grab := range grabs {
		hashes[i] = grab.TorrentHash
	}
	statuses, err := e.downloader.Status(ctx, hashes)
	if err != nil {
		return fmt.Errorf("poll downloader: %w", err)
	}
	downloadsPaused, err := e.DownloadsPaused(ctx)
	if err != nil {
		return fmt.Errorf("read download pause state: %w", err)
	}
	covered, err := e.coveredEpisodesByRelease(ctx, grabs)
	if err != nil {
		return err
	}
	byHash := make(map[string]downloader.TorrentStatus, len(statuses))
	for _, status := range statuses {
		byHash[strings.ToLower(status.Hash)] = status
	}
	var errs []error
	for _, grab := range grabs {
		var grabErr error

		status, ok := byHash[strings.ToLower(grab.TorrentHash)]
		if ok {
			if e.pathMapper != nil {
				status.ContentPath = e.pathMapper.Local(status.ContentPath)
			}
			grabErr = e.updateGrabStatus(ctx, grab, covered[grab.ReleaseID], status, downloadsPaused)
		} else {
			grabErr = e.handleMissingTorrent(ctx, grab, downloadsPaused)
		}
		if grabErr != nil {
			errs = append(errs, fmt.Errorf("grab %d: %w", grab.ID, grabErr))
		}
	}
	return errors.Join(errs...)
}

// coveredEpisodesByRelease loads the active episodes of every episode grab in
// one query, so the per-grab stages do not each look them up.
func (e *Engine) coveredEpisodesByRelease(ctx context.Context, grabs []model.Grab) (map[int64][]model.Episode, error) {
	var releaseIDs []int64
	for _, grab := range grabs {
		if grab.SubjectType == model.SubjectEpisode && !slices.Contains(releaseIDs, grab.ReleaseID) {
			releaseIDs = append(releaseIDs, grab.ReleaseID)
		}
	}
	return e.store.Episodes().ActiveByReleases(ctx, releaseIDs)
}

// handleMissingTorrent gives a torrent absent from the client a full stall
// timeout since the last observation before recovering the grab.
func (e *Engine) handleMissingTorrent(ctx context.Context, grab model.Grab, downloadsPaused bool) error {
	now := e.clock.Now().UTC()
	if downloadsPaused {
		// Persist the pause observation so a torrent that vanished while
		// paused gets a full disappearance grace after the operator resumes.
		grab.ProgressedAt = now
		return e.store.Grabs().Update(ctx, grab)
	}
	lastSeen := grab.UpdatedAt
	if lastSeen.IsZero() {
		lastSeen = grab.CreatedAt
	}
	if now.Sub(lastSeen) < e.cfg.Downloader.StallTimeout.Duration {
		return nil
	}
	if grab.State == model.GrabCompleted || grab.State == model.GrabImporting {
		return e.completeGrab(ctx, grab)
	}
	return e.failGrab(ctx, grab,
		"torrent disappeared from download client before completion", true)
}

func (e *Engine) updateGrabStatus(ctx context.Context, grab model.Grab, covered []model.Episode,
	status downloader.TorrentStatus, downloadsPaused bool) error {
	now := e.clock.Now().UTC()
	queuedOrMaintenance := status.State == downloader.StateMaintenance || status.State == downloader.StateQueued
	idle := queuedOrMaintenance || status.State == downloader.StatePaused
	if status.Progress > grab.Progress {
		grab.ProgressedAt = now
	}
	grab.Progress = status.Progress
	grab.ContentPath = status.ContentPath
	if status.Failed() && !downloadsPaused {
		return e.failGrab(ctx, grab, "download client error: "+status.ErrorMessage, true)
	}
	if idle || downloadsPaused {
		// These states can legitimately leave the byte count unchanged. Refresh
		// the timer so recovery starts only after the torrent can transfer again.
		grab.ProgressedAt = now
	}
	if !idle && !downloadsPaused && !status.Complete() && status.Progress < 1 &&
		now.Sub(grab.ProgressedAt) >= e.cfg.Downloader.StallTimeout.Duration {
		return e.failGrab(ctx, grab, "torrent made no progress before stall timeout", true)
	}
	if status.Complete() {
		return e.completeGrab(ctx, grab)
	}
	if status.State == downloader.StateDownloading || status.State == downloader.StateStalled ||
		status.State == downloader.StatePaused {
		grab.State = model.GrabDownloading
		if err := e.store.Grabs().Update(ctx, grab); err != nil {
			return err
		}
		if err := e.advanceGrabItems(ctx, grab, covered, model.StateDownloading,
			"download started", status.State); err != nil {
			return err
		}
		e.publishProgress(grab, status.State)
		return nil
	}
	if queuedOrMaintenance {
		grab.State = model.GrabDownloading
	}
	// Persist every successful status poll. UpdatedAt is the last observation
	// time used to grant a missing torrent its full disappearance grace.
	if err := e.store.Grabs().Update(ctx, grab); err != nil {
		return err
	}
	if queuedOrMaintenance {
		e.publishProgress(grab, status.State)
	}
	return nil
}

func (e *Engine) completeGrab(ctx context.Context, grab model.Grab) error {
	var subjectIDs = []int64{grab.SubjectID}
	var imported bool

	if grab.SubjectType == model.SubjectEpisode {
		covered, err := e.store.Episodes().ActiveByRelease(ctx, grab.ReleaseID)
		if err != nil {
			return err
		}
		if len(covered) == 0 {
			state, getErr := e.itemState(ctx, grab.SubjectType, grab.SubjectID)
			if getErr != nil || state != model.StateImported {
				return fmt.Errorf("completed episode grab has no active covered episodes")
			}
			imported = true
		} else {
			if err := e.advanceGrabItems(ctx, grab, covered, model.StateImporting,
				"download completed", grab.ContentPath); err != nil {
				return err
			}
			subjectIDs = episodeIDs(covered)
		}
	} else {
		state, err := e.itemState(ctx, grab.SubjectType, grab.SubjectID)
		if err != nil {
			return err
		}
		if state == model.StateGrabbed {
			if _, err := e.store.Transitions().Transition(ctx, grab.SubjectType, grab.SubjectID,
				model.StateDownloading, "download completed", "completion observed before prior status poll"); err != nil {
				return err
			}
			state = model.StateDownloading
		}
		if state == model.StateDownloading {
			if _, err := e.store.Transitions().Transition(ctx, grab.SubjectType, grab.SubjectID,
				model.StateImporting, "download completed", grab.ContentPath); err != nil {
				return err
			}
			state = model.StateImporting
		}
		if state != model.StateImporting && state != model.StateImported {
			return fmt.Errorf("completed grab cannot be imported while item is %s", state)
		}
		imported = state == model.StateImported
	}

	// Completed grabs observed after a restart must not import the same files again.
	if !imported {
		grab.State = model.GrabImporting
		if err := e.store.Grabs().Update(ctx, grab); err != nil {
			return err
		}
		if e.importer == nil {
			return errors.New("no importer configured")
		}
		if err := e.importer.ImportCompleted(ctx, grab.ID); err != nil {
			cleanup := context.WithoutCancel(ctx)

			grab.State, grab.LastError = model.GrabFailed, err.Error()
			_ = e.store.Grabs().Update(cleanup, grab)
			for _, id := range subjectIDs {
				state, stateErr := e.itemState(cleanup, grab.SubjectType, id)
				if stateErr == nil && state == model.StateImporting {
					_, _ = e.store.Transitions().Transition(cleanup, grab.SubjectType,
						id, model.StateImportFailed, "import failed", err.Error())
				}
			}
			e.publishGrabState(grab, subjectIDs, string(model.GrabFailed))
			return err
		}
	}
	grab.State, grab.Progress, grab.LastError = model.GrabImported, 1, ""
	if err := e.store.Grabs().Update(ctx, grab); err != nil {
		return err
	}
	e.publishGrabState(grab, subjectIDs, string(model.GrabImported))
	return nil
}

func episodeIDs(episodes []model.Episode) []int64 {
	ids := make([]int64, len(episodes))
	for i, episode := range episodes {
		ids[i] = episode.ID
	}
	return ids
}

func (e *Engine) publishGrabState(grab model.Grab, subjectIDs []int64, state string) {
	for _, id := range subjectIDs {
		e.publish("state_transition", grab.SubjectType, id,
			map[string]any{"grab_id": grab.ID, "state": state})
	}
}

func (e *Engine) publishProgress(grab model.Grab, clientState string) {
	e.publish("progress", grab.SubjectType, grab.SubjectID, map[string]any{"grab_id": grab.ID,
		"progress": grab.Progress, "state": clientState})
}

func (e *Engine) advanceGrabItems(ctx context.Context, grab model.Grab, covered []model.Episode,
	to model.ItemState, reason, detail string) error {
	if grab.SubjectType == model.SubjectMovie {
		state, err := e.itemState(ctx, grab.SubjectType, grab.SubjectID)
		if err != nil || state == to || state == model.StateImported {
			return err
		}
		if to == model.StateImporting && state == model.StateGrabbed {
			if _, err := e.store.Transitions().Transition(ctx, model.SubjectMovie, grab.SubjectID,
				model.StateDownloading, reason, detail); err != nil {
				return err
			}
		}
		_, err = e.store.Transitions().Transition(ctx, model.SubjectMovie, grab.SubjectID, to, reason, detail)
		return err
	}
	for _, episode := range covered {
		state := episode.State
		if state == to {
			continue
		}
		if to == model.StateImporting && state == model.StateGrabbed {
			if _, err := e.store.Transitions().Transition(ctx, model.SubjectEpisode, episode.ID,
				model.StateDownloading, reason, detail); err != nil {
				return err
			}
			state = model.StateDownloading
		}
		if to == model.StateDownloading && state != model.StateGrabbed {
			continue
		}
		if to == model.StateImporting && state != model.StateDownloading {
			continue
		}
		if _, err := e.store.Transitions().Transition(ctx, model.SubjectEpisode,
			episode.ID, to, reason, detail); err != nil {
			return err
		}
	}
	return nil
}

func (e *Engine) failGrab(ctx context.Context, grab model.Grab, reason string, deleteData bool) error {
	if err := e.downloader.Remove(ctx, grab.TorrentHash, deleteData); err != nil {
		if errors.Is(err, downloader.ErrNotOurs) {
			reason = "torrent is no longer owned by Reelay; left untouched: " + reason
		} else if !errors.Is(err, downloader.ErrNotFound) {
			return err
		}
	}
	release, err := e.store.Releases().Get(ctx, grab.ReleaseID)
	if err != nil {
		return err
	}
	grab.State, grab.LastError = model.GrabStalled, reason
	if err := e.store.Grabs().Update(ctx, grab); err != nil {
		return err
	}
	if grab.SubjectType == model.SubjectEpisode {
		covered, err := e.store.Episodes().ActiveByRelease(ctx, grab.ReleaseID)
		if err != nil {
			return err
		}
		for _, episode := range covered {
			if err := e.store.Decisions().Blacklist(ctx, model.SubjectEpisode, episode.ID,
				release.InfoHash, reason); err != nil {
				return err
			}
			if err := e.store.Transitions().RetryNow(ctx, model.SubjectEpisode,
				episode.ID, "grab stalled"); err != nil {
				return err
			}
		}
		subjectIDs := []int64{grab.SubjectID}
		if len(covered) > 0 {
			subjectIDs = episodeIDs(covered)
		}
		e.publishGrabState(grab, subjectIDs, string(model.GrabStalled))
		e.fallbackGrab(ctx, grab.SubjectType, grab.SubjectID, reason)
		return nil
	}
	if err := e.store.Decisions().Blacklist(ctx, grab.SubjectType, grab.SubjectID,
		release.InfoHash, reason); err != nil {
		return err
	}
	state, err := e.itemState(ctx, grab.SubjectType, grab.SubjectID)
	if err != nil {
		return err
	}
	if state == model.StateGrabbed || state == model.StateDownloading {
		_, err = e.store.Transitions().Transition(ctx, grab.SubjectType, grab.SubjectID,
			model.StateWanted, "grab stalled", reason)
	}
	if err != nil {
		return err
	}
	e.publishGrabState(grab, []int64{grab.SubjectID}, string(model.GrabStalled))
	e.fallbackGrab(ctx, grab.SubjectType, grab.SubjectID, reason)
	return nil
}

func (e *Engine) itemState(ctx context.Context, subject model.SubjectType, id int64) (model.ItemState, error) {
	if subject == model.SubjectMovie {
		item, err := e.store.Movies().Get(ctx, id)
		return item.State, err
	}
	item, err := e.store.Episodes().Get(ctx, id)
	return item.State, err
}
