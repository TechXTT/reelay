package engine

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/TechXTT/reelay/internal/downloader"
	"github.com/TechXTT/reelay/internal/model"
)

func (e *Engine) StatusOnce(ctx context.Context) error {
	grabs, err := e.store.Grabs().Active(ctx)
	if err != nil || len(grabs) == 0 {
		return err
	}
	hashes := make([]string, 0, len(grabs))
	for _, grab := range grabs {
		hashes = append(hashes, grab.TorrentHash)
	}
	statuses, err := e.downloader.Status(ctx, hashes)
	if err != nil {
		return fmt.Errorf("poll downloader: %w", err)
	}
	byHash := make(map[string]downloader.TorrentStatus, len(statuses))
	for _, status := range statuses {
		byHash[strings.ToLower(status.Hash)] = status
	}
	var errs []error
	for _, grab := range grabs {
		status, ok := byHash[strings.ToLower(grab.TorrentHash)]
		if !ok {
			continue
		}
		if e.pathMapper != nil {
			status.ContentPath = e.pathMapper.Local(status.ContentPath)
		}
		if err := e.updateGrabStatus(ctx, grab, status); err != nil {
			errs = append(errs, fmt.Errorf("grab %d: %w", grab.ID, err))
		}
	}
	return errors.Join(errs...)
}

func (e *Engine) updateGrabStatus(ctx context.Context, grab model.Grab, status downloader.TorrentStatus) error {
	now := e.clock.Now().UTC()
	if status.Progress > grab.Progress {
		grab.ProgressedAt = now
	}
	grab.Progress = status.Progress
	grab.ContentPath = status.ContentPath
	if status.Failed() {
		return e.failGrab(ctx, grab, "download client error: "+status.ErrorMessage, true)
	}
	if status.Progress < 0.01 && !grab.ProgressedAt.IsZero() &&
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
		if state, err := e.itemState(ctx, grab); err == nil && state == model.StateGrabbed {
			if _, err := e.store.Transitions().Transition(ctx, grab.SubjectType, grab.SubjectID,
				model.StateDownloading, "download started", status.State); err != nil {
				return err
			}
		}
		e.events.Publish(Event{Type: "progress", At: now, SubjectType: string(grab.SubjectType),
			SubjectID: grab.SubjectID, Data: map[string]any{"grab_id": grab.ID,
				"progress": grab.Progress, "state": status.State}})
	}
	return nil
}

func (e *Engine) completeGrab(ctx context.Context, grab model.Grab) error {
	state, err := e.itemState(ctx, grab)
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
	}
	grab.State = model.GrabImporting
	if err := e.store.Grabs().Update(ctx, grab); err != nil {
		return err
	}
	if e.importer == nil {
		return errors.New("no importer configured")
	}
	if err := e.importer.ImportCompleted(ctx, grab.ID); err != nil {
		grab.State, grab.LastError = model.GrabFailed, err.Error()
		_ = e.store.Grabs().Update(context.WithoutCancel(ctx), grab)
		state, stateErr := e.itemState(context.WithoutCancel(ctx), grab)
		if stateErr == nil && state == model.StateImporting {
			_, _ = e.store.Transitions().Transition(context.WithoutCancel(ctx), grab.SubjectType,
				grab.SubjectID, model.StateImportFailed, "import failed", err.Error())
		}
		return err
	}
	grab.State = model.GrabImported
	grab.Progress = 1
	return e.store.Grabs().Update(ctx, grab)
}

func (e *Engine) failGrab(ctx context.Context, grab model.Grab, reason string, deleteData bool) error {
	if err := e.downloader.Remove(ctx, grab.TorrentHash, deleteData); err != nil &&
		!errors.Is(err, downloader.ErrNotFound) {
		return err
	}
	release, err := e.store.Releases().Get(ctx, grab.ReleaseID)
	if err != nil {
		return err
	}
	if err := e.store.Decisions().Blacklist(ctx, grab.SubjectType, grab.SubjectID,
		release.InfoHash, reason); err != nil {
		return err
	}
	grab.State, grab.LastError = model.GrabStalled, reason
	if err := e.store.Grabs().Update(ctx, grab); err != nil {
		return err
	}
	state, err := e.itemState(ctx, grab)
	if err != nil {
		return err
	}
	if state == model.StateGrabbed || state == model.StateDownloading {
		_, err = e.store.Transitions().Transition(ctx, grab.SubjectType, grab.SubjectID,
			model.StateWanted, "grab stalled", reason)
	}
	return err
}

func (e *Engine) itemState(ctx context.Context, grab model.Grab) (model.ItemState, error) {
	if grab.SubjectType == model.SubjectMovie {
		item, err := e.store.Movies().Get(ctx, grab.SubjectID)
		return item.State, err
	}
	item, err := e.store.Episodes().Get(ctx, grab.SubjectID)
	return item.State, err
}
