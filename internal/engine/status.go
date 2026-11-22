package engine

import (
	"context"
	"errors"
	"fmt"
	"os"
	"slices"
	"strings"

	"github.com/TechXTT/reelay/internal/downloader"
	"github.com/TechXTT/reelay/internal/model"
	"github.com/TechXTT/reelay/internal/store"
)

const downloadClientUnavailable = "download client unavailable: "

func (e *Engine) StatusOnce(ctx context.Context) (err error) {
	var filesChecked bool

	defer func() {
		// Once every grab was checked, a download-files cause that no grab
		// waited on in this pass is cleared; a wait observed in the same pass
		// wins, and an import that read the files counts as recovery.
		if filesChecked && ctx.Err() == nil {
			err = errors.Join(err, e.downloadFilesChecked(context.WithoutCancel(ctx)))
		}
		err = errors.Join(err, e.flushIncidents(context.WithoutCancel(ctx),
			store.IncidentDownloadClient, store.IncidentLibrary, downloadFilesScope))
	}()
	grabs, err := e.store.Grabs().Active(ctx)
	if err != nil {
		return err
	}
	filesChecked = true
	if len(grabs) == 0 {
		// Without grabs to poll, an open outage resolves on a direct check.
		if e.incidentOpen(store.IncidentDownloadClient, downloadClientIncidentKey) && e.downloader.Healthy(ctx) == nil {
			e.incidentHealthy(store.IncidentDownloadClient, downloadClientIncidentKey)
		}
		return nil
	}
	var pending []model.Grab
	var hashes []string
	var errs []error

	for _, grab := range grabs {
		if grab.ContentPath != "" && (grab.State == model.GrabCompleted || grab.State == model.GrabImporting) {
			if err := e.completeGrab(ctx, grab); err != nil {
				errs = append(errs, fmt.Errorf("grab %d: %w", grab.ID, err))
			}
			continue
		}
		pending = append(pending, grab)
		hashes = append(hashes, grab.TorrentHash)
	}
	if len(hashes) == 0 {
		return errors.Join(errs...)
	}
	statuses, err := e.downloader.Status(ctx, hashes)
	if err != nil {
		filesChecked = false
		errs = append(errs, fmt.Errorf("poll downloader: %w", err))
		if ctx.Err() == nil {
			items := make([]string, len(pending))
			for i, grab := range pending {
				items[i] = itemKey(grab.SubjectType, grab.SubjectID)
				errs = append(errs, e.recordDownloadWait(ctx, grab, downloadClientUnavailable+err.Error()))
			}
			e.incidentFailed(store.IncidentDownloadClient, downloadClientIncidentKey, err.Error(), items...)
		}
		return errors.Join(errs...)
	}
	e.incidentHealthy(store.IncidentDownloadClient, downloadClientIncidentKey)
	downloadsPaused, err := e.DownloadsPaused(ctx)
	if err != nil {
		return errors.Join(append(errs, fmt.Errorf("read download pause state: %w", err))...)
	}
	covered, err := e.coveredEpisodesByRelease(ctx, pending)
	if err != nil {
		return errors.Join(append(errs, err)...)
	}
	byHash := make(map[string]downloader.TorrentStatus, len(statuses))
	for _, status := range statuses {
		byHash[strings.ToLower(status.Hash)] = status
	}
	for _, grab := range pending {
		var grabErr error

		status, ok := byHash[strings.ToLower(grab.TorrentHash)]
		if ok {
			if status.ContentPath != "" && e.pathMapper != nil {
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
	if grab.State == model.GrabCompleted || grab.State == model.GrabImporting {
		return e.recordDownloadWait(ctx, grab, "download completed, but its source path is unavailable")
	}
	if strings.HasPrefix(grab.LastError, downloadClientUnavailable) {
		return e.recordDownloadWait(ctx, grab,
			"torrent was not found after the download client recovered")
	}
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
	e.metrics.grabFailures.Inc("vanished")
	return e.failGrab(ctx, grab,
		"torrent disappeared from download client before completion", true)
}

func (e *Engine) recordDownloadWait(ctx context.Context, grab model.Grab, reason string) error {
	// A completed download whose files cannot be reached waits on storage,
	// not on the client, unless the client itself is unavailable.
	if (grab.State == model.GrabCompleted || grab.State == model.GrabImporting) &&
		!strings.HasPrefix(reason, downloadClientUnavailable) {
		e.incidentFailed(store.IncidentStorage, e.downloadFilesKey(grab.SubjectType), reason,
			itemKey(grab.SubjectType, grab.SubjectID))
	}
	if err := e.store.Grabs().RecordDownloadWait(ctx, grab.ID, reason); err != nil {
		return err
	}
	if grab.LastError != reason {
		e.publishProgress(grab, string(grab.State))
	}
	return nil
}

// downloadFilesChecked clears every open download-files cause. One whose
// files an import read in this pass was already seen healthy; the rest close
// with a note, since the waiting downloads went away (cancelled, failed or
// removed) without their files being seen again.
func (e *Engine) downloadFilesChecked(ctx context.Context) error {
	keys, err := e.openIncidentKeys(ctx, store.IncidentStorage)
	if err != nil {
		return err
	}
	for _, key := range keys {
		if strings.HasPrefix(key, DownloadFilesKeyPrefix) {
			e.incidentCleared(store.IncidentStorage, key, store.IncidentNothingWaiting)
		}
	}
	return nil
}

func (e *Engine) updateGrabStatus(ctx context.Context, grab model.Grab, covered []model.Episode,
	status downloader.TorrentStatus, downloadsPaused bool) error {
	now := e.clock.Now().UTC()
	queuedOrMaintenance := status.State == downloader.StateMaintenance || status.State == downloader.StateQueued
	idle := queuedOrMaintenance || status.State == downloader.StatePaused
	transferActive := status.State == downloader.StateDownloading || status.State == downloader.StateStalled
	if status.Failed() || status.State == downloader.StateUnknown {
		detail := status.ErrorMessage
		if detail == "" {
			detail = "client reported torrent state " + status.State
		}
		return e.recordDownloadWait(ctx, grab, "download needs attention: "+detail)
	}
	if !status.Complete() && (grab.State == model.GrabCompleted || grab.State == model.GrabImporting) {
		return e.recordDownloadWait(ctx, grab, "waiting for the download client to report the completed source path")
	}
	if status.Progress > grab.Progress || grab.LastError != "" {
		grab.ProgressedAt = now
	}
	grab.LastError = ""
	grab.Progress = status.Progress
	if status.ContentPath != "" {
		grab.ContentPath = status.ContentPath
	}
	if idle || downloadsPaused {
		// These states can legitimately leave the byte count unchanged. Refresh
		// the timer so recovery starts only after the torrent can transfer again.
		grab.ProgressedAt = now
	}
	if transferActive && !downloadsPaused && status.Progress < 1 &&
		now.Sub(grab.ProgressedAt) >= e.cfg.Downloader.StallTimeout.Duration {
		e.metrics.grabFailures.Inc("stalled")
		return e.failGrab(ctx, grab, "torrent made no progress before stall timeout", true)
	}
	if status.Complete() {
		return e.completeGrab(ctx, grab)
	}
	if transferActive || status.State == downloader.StatePaused {
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
			hasImported, hasFailed, err := e.store.Episodes().ImportOutcomes(ctx, grab)
			if err != nil {
				return err
			}
			if !hasImported && !(grab.State == model.GrabImporting && hasFailed) {
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
		started := e.clock.Now()
		err := e.importer.ImportCompleted(ctx, grab.ID)
		e.metrics.importDuration.Observe(e.clock.Now().Sub(started).Seconds())
		if err != nil {
			e.metrics.imports.Inc("failed")
			cleanup := context.WithoutCancel(ctx)

			root := e.libraryRoot(grab.SubjectType)
			if probeErr := probeLibraryRoot(root); probeErr != nil {
				e.incidentFailed(store.IncidentLibrary, root, probeErr.Error()+"; import failed: "+err.Error(),
					itemKey(grab.SubjectType, grab.SubjectID))
			}
			if failErr := e.store.Grabs().FailImport(cleanup, grab, subjectIDs, err.Error()); failErr != nil {
				return errors.Join(err, fmt.Errorf("record import failure: %w", failErr))
			}
			e.publishGrabState(grab, subjectIDs, string(model.GrabFailed))
			return err
		}
		e.metrics.imports.Inc("ok")
		// The import read the completed files, so their path is reachable.
		e.incidentHealthy(store.IncidentStorage, e.downloadFilesKey(grab.SubjectType))
		e.incidentHealthy(store.IncidentLibrary, e.libraryRoot(grab.SubjectType))
	}
	grab.State, grab.Progress, grab.LastError = model.GrabImported, 1, ""
	if grab.SubjectType == model.SubjectEpisode {
		_, hasFailed, err := e.store.Episodes().ImportOutcomes(ctx, grab)
		if err != nil {
			return err
		}
		if hasFailed {
			grab.State, grab.LastError = model.GrabFailed, "some episodes still need import"
		}
	}
	if err := e.store.Grabs().Update(ctx, grab); err != nil {
		return err
	}
	e.publishGrabState(grab, subjectIDs, string(grab.State))
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

// blacklistGrabHashes blacklists the release hash and, when it differs, the
// grab's torrent hash. They differ for download-link releases, whose release
// hash is a stand-in key while the grab carries the real info hash.
func (e *Engine) blacklistGrabHashes(ctx context.Context, subject model.SubjectType, id int64,
	releaseHash, torrentHash, reason string) error {
	if err := e.store.Decisions().Blacklist(ctx, subject, id, releaseHash, reason); err != nil {
		return err
	}
	if torrentHash == "" || strings.EqualFold(torrentHash, releaseHash) {
		return nil
	}
	return e.store.Decisions().Blacklist(ctx, subject, id, torrentHash, reason)
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
			if err := e.blacklistGrabHashes(ctx, model.SubjectEpisode, episode.ID,
				release.InfoHash, grab.TorrentHash, reason); err != nil {
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
	if err := e.blacklistGrabHashes(ctx, grab.SubjectType, grab.SubjectID,
		release.InfoHash, grab.TorrentHash, reason); err != nil {
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

// probeLibraryRoot reports whether the library root exists and accepts a new
// file, so an import failure caused by the library itself is told apart from
// one caused by the release.
func probeLibraryRoot(root string) error {
	info, err := os.Stat(root)
	if err != nil {
		return fmt.Errorf("library root is unavailable: %w", err)
	}
	if !info.IsDir() {
		return fmt.Errorf("library root %s is not a directory", root)
	}
	probe, err := os.CreateTemp(root, ".reelay-probe-*")
	if err != nil {
		return fmt.Errorf("library root is not writable: %w", err)
	}
	_ = probe.Close()
	return os.Remove(probe.Name())
}
