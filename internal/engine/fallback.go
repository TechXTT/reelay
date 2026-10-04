package engine

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/TechXTT/reelay/internal/model"
)

// fallbackCandidateMaxAge bounds how old a stored evaluation may be and still
// be grabbed without a fresh indexer search.
const fallbackCandidateMaxAge = 24 * time.Hour

// maxFallbackGrabs is the number of automatic fallback grabs allowed per
// search result: the original grab plus this many fallbacks may fail.
const maxFallbackGrabs = 3

// fallbackGrab grabs the next-best accepted candidate of the item's last search
// after a grab failed or stalled, instead of waiting for the next search. It
// never fails the caller: every error is logged and the item simply stays
// wanted for the normal search.
func (e *Engine) fallbackGrab(ctx context.Context, subject model.SubjectType, id int64, reason string) {
	grab, why, err := e.startFallback(ctx, subject, id, reason)
	if err != nil {
		e.log.Warn("fallback grab failed", "subject", subject, "id", id, "error", err)
		return
	}
	if why != "" {
		e.log.Info("no fallback grab", "subject", subject, "id", id, "why", why)
		return
	}
	e.log.Info("fallback grab started", "subject", subject, "id", id, "grab_id", grab.ID)
}

// startFallback returns the new grab, or a non-empty why when nothing was done.
func (e *Engine) startFallback(ctx context.Context, subject model.SubjectType, id int64,
	reason string) (model.Grab, string, error) {
	state, err := e.itemState(ctx, subject, id)
	if err != nil {
		return model.Grab{}, "", err
	}
	if state != model.StateWanted {
		return model.Grab{}, "item is " + string(state), nil
	}
	candidates, err := e.store.Decisions().Candidates(ctx, subject, id)
	if err != nil {
		return model.Grab{}, "", err
	}
	blacklist, err := e.store.Decisions().BlacklistFor(ctx, subject, id)
	if err != nil {
		return model.Grab{}, "", err
	}
	var newest time.Time
	for _, candidate := range candidates {
		if candidate.EvaluatedAt.After(newest) {
			newest = candidate.EvaluatedAt
		}
	}
	grabs, err := e.store.Grabs().BySubject(ctx, subject, id)
	if err != nil {
		return model.Grab{}, "", err
	}
	failed := 0
	for _, grab := range grabs {
		if (grab.State == model.GrabStalled || grab.State == model.GrabFailed) && !grab.CreatedAt.Before(newest) {
			failed++
		}
	}
	if failed > maxFallbackGrabs {
		return model.Grab{}, fmt.Sprintf("%d grabs from the last search failed; fallback limit reached", failed), nil
	}
	cutoff := e.clock.Now().Add(-fallbackCandidateMaxAge)
	for _, candidate := range candidates {
		if !candidate.Accepted || candidate.EvaluatedAt.Before(cutoff) {
			continue
		}
		release, err := e.store.Releases().Get(ctx, candidate.ReleaseID)
		if err != nil {
			return model.Grab{}, "", err
		}
		if blacklist[strings.ToLower(strings.TrimSpace(release.InfoHash))] {
			continue
		}
		grab, err := e.grabStoredRelease(ctx, subject, id, candidate.ReleaseID, grabIntent{owner: "fallback",
			reason: "fallback_grab", detail: fmt.Sprintf("after %s: selected %s score %d",
				reason, release.RawTitle, candidate.Score)})
		if err != nil {
			return model.Grab{}, "", err
		}
		e.publish("state_transition", subject, id, map[string]any{"state": model.StateGrabbed,
			"grab_id": grab.ID, "release": release.RawTitle, "score": candidate.Score,
			"reason": "fallback_grab"})
		return grab, "", nil
	}
	return model.Grab{}, "no accepted, unblacklisted candidate from the last 24h", nil
}
