package engine

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/TechXTT/reelay/internal/downloader"
	"github.com/TechXTT/reelay/internal/indexer"
	"github.com/TechXTT/reelay/internal/model"
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
		groups[targetKey(target)] = append(groups[targetKey(target)], target)
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
		if err != nil && len(recentReleases) == 0 {
			return err
		}
	}

	var wg sync.WaitGroup
	errCh := make(chan error, len(groups))
	for _, group := range groups {
		group := group
		wg.Add(1)
		go func() {
			defer wg.Done()
			select {
			case e.searchSem <- struct{}{}:
				defer func() { <-e.searchSem }()
			case <-ctx.Done():
				errCh <- ctx.Err()
				return
			}
			releases := recentReleases
			queryErr := err
			if !recent {
				minSeeders := group[0].profile.MinSeeders
				for _, target := range group[1:] {
					if target.profile.MinSeeders < minSeeders {
						minSeeders = target.profile.MinSeeders
					}
				}
				releases, queryErr = e.queryIndexers(ctx, indexer.Query{
					Term: targetQuery(group[0]), Categories: targetCategories(group[0]),
					MinSeeders: minSeeders,
				})
			}
			for _, target := range group {
				if processErr := e.processSearchTarget(ctx, target, releases, queryErr); processErr != nil {
					errCh <- processErr
				}
			}
		}()
	}
	wg.Wait()
	close(errCh)
	var errs []error
	for loopErr := range errCh {
		if loopErr != nil && !errors.Is(loopErr, store.ErrLocked) {
			errs = append(errs, loopErr)
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
		Blacklist: blacklist, RuntimeMinutes: target.runtime})
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
	hash, err := e.downloader.Add(ctx, downloader.AddRequest{Magnet: best.Release.Magnet,
		Category: target.category, SavePath: target.savePath, Paused: e.cfg.Downloader.AddPaused})
	if err != nil {
		return e.retrySearch(ctx, lock, target, "grab_failed", err.Error())
	}
	grab, err := e.store.Grabs().CreateGrabbed(ctx, lock, model.Grab{SubjectType: target.subject,
		SubjectID: target.id, ReleaseID: stored.ID, TorrentHash: hash, Category: target.category},
		fmt.Sprintf("selected %s with score %d", best.Release.Title, best.Score))
	if err != nil {
		_ = e.downloader.Remove(context.WithoutCancel(ctx), hash, false)
		return fmt.Errorf("record grab: %w", err)
	}
	e.publish("state_transition", target, map[string]any{"state": model.StateGrabbed,
		"grab_id": grab.ID, "release": best.Release.Title, "score": best.Score})
	return nil
}

func (e *Engine) persistCandidates(ctx context.Context, target searchTarget, result scoring.Result) error {
	evaluations := make([]model.CandidateEvaluation, 0, result.Considered())
	all := append(append([]scoring.Candidate{}, result.Accepted...), result.Rejected...)
	for _, candidate := range all {
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
		if terminal {
			state = model.StateFailed
		}
		e.publish("state_transition", target, map[string]any{"state": state, "reason": reason})
	}
	return err
}
