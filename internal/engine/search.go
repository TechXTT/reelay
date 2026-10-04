package engine

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"sync"
	"time"

	"github.com/TechXTT/reelay/internal/downloader"
	"github.com/TechXTT/reelay/internal/indexer"
	"github.com/TechXTT/reelay/internal/model"
	"github.com/TechXTT/reelay/internal/parser"
	"github.com/TechXTT/reelay/internal/scoring"
	"github.com/TechXTT/reelay/internal/store"
)

func (e *Engine) SearchOnce(ctx context.Context) error { return e.search(ctx, false) }
func (e *Engine) RecentOnce(ctx context.Context) error { return e.search(ctx, true) }

func (e *Engine) search(ctx context.Context, recent bool) error {
	targets, err := e.dueTargets(ctx)
	if err != nil || len(targets) == 0 {
		return err
	}
	groups := make(map[string][]searchTarget)
	for _, target := range targets {
		key := targetKey(target)
		groups[key] = append(groups[key], target)
	}

	var recentReleases []indexer.Release
	if recent {
		recentReleases, err = e.queryIndexers(ctx, indexer.Query{
			Recent: true, Categories: indexer.VideoCategories(),
		})
		if errors.Is(err, indexer.ErrUnsupported) {
			e.log.Info("recent indexer query unsupported; loop disabled for this run")
			return nil
		}
		if err != nil {
			return err
		}
	}

	var wg sync.WaitGroup
	groupErrs := make([]error, len(groups))
	index := 0
	for _, group := range groups {
		slot := &groupErrs[index]
		index++
		wg.Add(1)
		go func() {
			defer wg.Done()
			*slot = e.searchGroup(ctx, group, recent, recentReleases)
		}()
	}
	wg.Wait()
	return errors.Join(groupErrs...)
}

// searchGroup evaluates targets that share one indexer query. A recent run
// reuses the feed already fetched; otherwise the group queries once with the
// loosest seeder floor among its profiles.
func (e *Engine) searchGroup(ctx context.Context, group []searchTarget, recent bool,
	recentReleases []indexer.Release) error {
	select {
	case e.searchSem <- struct{}{}:
		defer func() { <-e.searchSem }()
	case <-ctx.Done():
		return ctx.Err()
	}
	releases := recentReleases
	var queryErr error
	if !recent {
		minSeeders := group[0].profile.MinSeeders
		for _, target := range group[1:] {
			minSeeders = min(minSeeders, target.profile.MinSeeders)
		}
		releases, queryErr = e.queryIndexers(ctx, indexer.Query{
			Term: targetQuery(group[0]), Categories: targetCategories(group[0]),
			MinSeeders: minSeeders,
		})
	}
	var errs []error
	for _, target := range group {
		if err := e.processSearchTarget(ctx, target, releases, queryErr); err != nil && !errors.Is(err, store.ErrLocked) {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

func (e *Engine) queryIndexers(ctx context.Context, query indexer.Query) ([]indexer.Release, error) {
	var out []indexer.Release
	var errs []error
	unsupported := 0
	for _, client := range e.indexers {
		if err := client.Healthy(ctx); err != nil {
			errs = append(errs, fmt.Errorf("%s unhealthy: %w", client.Name(), err))
			continue
		}
		values, err := client.Search(ctx, query)
		if errors.Is(err, indexer.ErrUnsupported) {
			unsupported++
			continue
		}
		if err != nil {
			errs = append(errs, fmt.Errorf("%s search: %w", client.Name(), err))
			continue
		}
		out = append(out, values...)
	}
	if unsupported == len(e.indexers) && len(e.indexers) > 0 {
		return nil, indexer.ErrUnsupported
	}
	if len(out) == 0 && len(errs) > 0 {
		return nil, errors.Join(errs...)
	}
	return out, nil
}

func (e *Engine) processSearchTarget(ctx context.Context, target searchTarget, releases []indexer.Release, queryErr error) error {
	lock, err := e.store.Locks().Acquire(ctx, target.subject, target.id, "engine-search", 5*time.Minute)
	if err != nil {
		return err
	}
	defer func() { _ = lock.Release(context.WithoutCancel(ctx)) }()
	if target.subject == model.SubjectEpisode {
		active, err := e.store.Episodes().SeriesHasActive(ctx, target.seriesID)
		if err != nil {
			return err
		}
		if active {
			return nil
		}
		episode, err := e.store.Episodes().Get(ctx, target.id)
		if err != nil {
			return err
		}
		if episode.State != model.StateWanted {
			return nil
		}
	}
	if _, err := e.store.Transitions().TransitionLocked(ctx, lock, model.StateSearching,
		"search_started", fmt.Sprintf("candidates=%d", len(releases))); err != nil {
		return err
	}

	blacklist, err := e.store.Decisions().BlacklistFor(ctx, target.subject, target.id)
	if err != nil {
		return e.retrySearch(ctx, lock, target, "decision_error", err.Error())
	}
	result := scoring.Evaluate(scoring.Input{Releases: releases, Want: &target.want,
		Profile: target.profile, Weights: e.cfg.Scoring, Now: e.clock.Now().UTC(),
		Blacklist: blacklist, Imported: target.imported, RuntimeMinutes: target.runtime})
	if err := e.persistCandidates(ctx, target, result); err != nil {
		return e.retrySearch(ctx, lock, target, "candidate_store_error", err.Error())
	}
	best := result.Best()
	if best == nil {
		detail := result.Summary()
		if queryErr != nil {
			detail = queryErr.Error()
		}
		return e.retrySearch(ctx, lock, target, "no_acceptable_release", detail)
	}

	stored, err := e.store.Releases().ByIndexerHash(ctx, best.Release.Indexer, best.Release.InfoHash)
	if err != nil {
		return e.retrySearch(ctx, lock, target, "release_lookup_error", err.Error())
	}
	locks := []*store.ItemLock{lock}
	if target.subject == model.SubjectEpisode {
		packLocks, failure, packErr := e.lockPackEpisodes(ctx, target.episodes, target.id,
			best.Parsed, "engine-season-pack", true)
		if packErr != nil {
			reason := "pack_coordination_error"
			if failure == packLockBusy {
				reason = "pack_coordination_busy"
			}
			return e.retrySearch(ctx, lock, target, reason, packErr.Error())
		}
		defer releaseItemLocks(packLocks)
		locks = append(locks, packLocks...)
	}
	hash, err := e.addDownload(ctx, downloader.AddRequest{Magnet: best.Release.Magnet,
		Category: target.category, SavePath: target.savePath, Paused: e.cfg.Downloader.AddPaused})
	if err != nil {
		return e.retrySearch(ctx, lock, target, "grab_failed", err.Error())
	}
	grab, err := e.store.Grabs().CreateGrabbedFor(ctx, locks, model.Grab{SubjectType: target.subject,
		SubjectID: target.id, ReleaseID: stored.ID, TorrentHash: hash, Category: target.category},
		fmt.Sprintf("selected %s with score %d", best.Release.Title, best.Score))
	if err != nil {
		_ = e.downloader.Remove(context.WithoutCancel(ctx), hash, false)
		return fmt.Errorf("record grab: %w", err)
	}
	e.publish("state_transition", target.subject, target.id, map[string]any{"state": model.StateGrabbed,
		"grab_id": grab.ID, "release": best.Release.Title, "score": best.Score,
		"covered_items": len(locks)})
	return nil
}

type packLockFailure int

const (
	packLockLookup packLockFailure = iota + 1
	packLockBusy
)

// lockPackEpisodes locks every still-wanted episode among candidates that the
// parsed release covers, in ascending ID order, excluding primaryID which the
// caller already holds. With refresh set each episode is re-read first because
// candidates may be stale. On failure nothing stays locked.
func (e *Engine) lockPackEpisodes(ctx context.Context, candidates []model.Episode, primaryID int64,
	parsed parser.Parsed, reason string, refresh bool) ([]*store.ItemLock, packLockFailure, error) {
	covered := make([]model.Episode, 0, len(candidates))
	for _, episode := range candidates {
		if episode.ID != primaryID && parsed.CoversEpisode(episode.Season, episode.Number) {
			covered = append(covered, episode)
		}
	}
	slices.SortFunc(covered, func(a, b model.Episode) int { return cmp.Compare(a.ID, b.ID) })
	var locks []*store.ItemLock
	for _, episode := range covered {
		if refresh {
			current, err := e.store.Episodes().Get(ctx, episode.ID)
			if err != nil {
				releaseItemLocks(locks)
				return nil, packLockLookup, err
			}
			episode.State = current.State
		}
		if episode.State != model.StateWanted {
			continue
		}
		lock, err := e.store.Locks().Acquire(ctx, model.SubjectEpisode, episode.ID, reason, 5*time.Minute)
		if err != nil {
			releaseItemLocks(locks)
			return nil, packLockBusy, err
		}
		locks = append(locks, lock)
	}
	return locks, 0, nil
}

func releaseItemLocks(locks []*store.ItemLock) {
	for _, lock := range locks {
		_ = lock.Release(context.Background())
	}
}

func (e *Engine) persistCandidates(ctx context.Context, target searchTarget, result scoring.Result) error {
	evaluations := make([]model.CandidateEvaluation, 0, result.Considered())
	for _, candidates := range [][]scoring.Candidate{result.Accepted, result.Rejected} {
		for _, candidate := range candidates {
			parsed, err := json.Marshal(candidate.Parsed)
			if err != nil {
				return err
			}
			stored, err := e.store.Releases().Upsert(ctx, model.StoredRelease{Indexer: candidate.Release.Indexer,
				RawTitle: candidate.Release.Title, InfoHash: candidate.Release.InfoHash,
				Magnet: candidate.Release.Magnet, SizeBytes: candidate.Release.SizeBytes,
				Seeders: candidate.Release.Seeders, Leechers: candidate.Release.Leechers,
				PublishedAt: candidate.Release.PublishedAt, Category: candidate.Release.Category,
				ParsedJSON: string(parsed), Score: candidate.Score})
			if err != nil {
				return err
			}
			evaluations = append(evaluations, model.CandidateEvaluation{SubjectType: target.subject,
				SubjectID: target.id, ReleaseID: stored.ID, Accepted: candidate.Accepted(),
				ReasonCode: candidate.RejectedBy, Reason: candidate.Reason, Score: candidate.Score})
		}
	}
	return e.store.Decisions().ReplaceCandidates(ctx, target.subject, target.id, evaluations)
}

func (e *Engine) retrySearch(ctx context.Context, lock *store.ItemLock, target searchTarget, reason, detail string) error {
	now := e.clock.Now().UTC()
	terminal := target.firstWanted != nil && now.Sub(*target.firstWanted) >= e.cfg.Schedules.SearchGiveUpAfter.Duration
	delay := 6 * time.Hour
	backoff := e.cfg.Schedules.SearchBackoff
	if len(backoff) > 0 {
		idx := target.attempts
		if idx >= len(backoff) {
			idx = len(backoff) - 1
		}
		delay = backoff[idx].Duration
	}
	_, err := e.store.Transitions().SearchRetryLocked(ctx, lock, now.Add(delay), reason, detail, terminal)
	if err == nil {
		state := model.StateWanted
		if target.imported != nil {
			state = model.StateImported
		} else if terminal {
			state = model.StateFailed
		}
		e.publish("state_transition", target.subject, target.id, map[string]any{"state": state, "reason": reason})
	}
	return err
}
