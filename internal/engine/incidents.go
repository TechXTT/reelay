package engine

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/TechXTT/reelay/internal/config"
	"github.com/TechXTT/reelay/internal/indexer"
	"github.com/TechXTT/reelay/internal/model"
	"github.com/TechXTT/reelay/internal/store"
)

const (
	// incidentTouchInterval bounds how often an unchanged open incident has
	// its last_seen_at rewritten, so a long outage costs one write per cause
	// per interval rather than one per affected item or loop pass.
	incidentTouchInterval = 5 * time.Minute

	downloadClientIncidentKey = "download_client"
	tvmazeIncidentKey         = "tvmaze"
)

type incidentCause struct {
	kind string
	key  string
}

// incidentObservation accumulates what one loop pass saw of a cause. A failure
// observed in the same pass as a success wins.
type incidentObservation struct {
	failed  bool
	healthy bool
	detail  string
	items   map[string]bool
}

type openIncident struct {
	items     map[string]bool
	affected  int
	writtenAt time.Time
}

// incidentTracker gathers observations in memory and writes them to the
// store once per loop pass. The zero value is ready to use.
type incidentTracker struct {
	mu      sync.Mutex
	pending map[incidentCause]*incidentObservation
	open    map[incidentCause]*openIncident
}

// IncidentCheckInterval is how often Reelay checks a cause of the given kind
// again, or 0 when nothing retries it automatically.
func IncidentCheckInterval(cfg *config.Config, kind string) time.Duration {
	switch kind {
	case store.IncidentIndexer, store.IncidentStorage:
		return cfg.Schedules.SearchInterval.Duration
	case store.IncidentDownloadClient:
		return cfg.Schedules.StatusInterval.Duration
	case store.IncidentMetadata:
		return cfg.Schedules.MetadataInterval.Duration
	}
	return 0
}

func itemKey(subject model.SubjectType, id int64) string { return fmt.Sprintf("%s:%d", subject, id) }

// observation returns the pending observation of a cause. Callers hold mu.
func (t *incidentTracker) observation(kind, key string) *incidentObservation {
	var cause = incidentCause{kind: kind, key: key}

	if t.pending == nil {
		t.pending = make(map[incidentCause]*incidentObservation)
	}
	obs := t.pending[cause]
	if obs == nil {
		obs = &incidentObservation{items: make(map[string]bool)}
		t.pending[cause] = obs
	}
	return obs
}

// incidentFailed records that a cause is failing, with the items it affects.
func (e *Engine) incidentFailed(kind, key, detail string, items ...string) {
	e.incidents.mu.Lock()
	defer e.incidents.mu.Unlock()
	obs := e.incidents.observation(kind, key)
	obs.failed = true
	obs.detail = detail
	for _, item := range items {
		obs.items[item] = true
	}
}

// incidentAffects adds an item to every cause of kind already observed failing
// in this pass, for callers that only see the combined failure.
func (e *Engine) incidentAffects(kind, item string) {
	e.incidents.mu.Lock()
	defer e.incidents.mu.Unlock()
	for cause, obs := range e.incidents.pending {
		if cause.kind == kind && obs.failed {
			obs.items[item] = true
		}
	}
}

// incidentHealthy records a positive observation that a cause works.
func (e *Engine) incidentHealthy(kind, key string) {
	e.incidents.mu.Lock()
	defer e.incidents.mu.Unlock()
	e.incidents.observation(kind, key).healthy = true
}

// incidentOpen reports whether a cause has an open incident, as known since
// the last flush.
func (e *Engine) incidentOpen(kind, key string) bool {
	e.incidents.mu.Lock()
	defer e.incidents.mu.Unlock()
	_, open := e.incidents.open[incidentCause{kind: kind, key: key}]
	return open
}

// openIncidentKeys returns the keys of the open incidents of a kind, as known
// since the last flush.
func (e *Engine) openIncidentKeys(ctx context.Context, kind string) ([]string, error) {
	var keys []string

	e.incidents.mu.Lock()
	defer e.incidents.mu.Unlock()
	if err := e.loadOpenIncidents(ctx); err != nil {
		return nil, err
	}
	for cause := range e.incidents.open {
		if cause.kind == kind {
			keys = append(keys, cause.key)
		}
	}
	return keys, nil
}

// loadOpenIncidents reads the open incidents once per process. An incident
// whose cause is no longer configured resolves with a note instead, since
// nothing would observe it again. Callers hold mu.
func (e *Engine) loadOpenIncidents(ctx context.Context) error {
	var t = &e.incidents
	var now = e.clock.Now().UTC()

	if t.open != nil {
		return nil
	}
	open, err := e.store.Incidents().Open(ctx)
	if err != nil {
		return err
	}
	loaded := make(map[incidentCause]*openIncident, len(open))
	for _, incident := range open {
		cause := incidentCause{kind: incident.Kind, key: incident.Key}
		if !e.incidentCauseConfigured(cause) {
			if err := e.store.Incidents().Resolve(ctx, cause.kind, cause.key, store.IncidentCauseRemoved, now); err != nil {
				return err
			}
			e.log.Info("incident resolved: cause removed from configuration", "kind", cause.kind, "cause", cause.key)
			continue
		}
		loaded[cause] = &openIncident{items: make(map[string]bool), affected: incident.Affected, writtenAt: incident.LastSeenAt}
	}
	t.open = loaded
	return nil
}

// incidentCauseConfigured reports whether the configuration still has the
// cause, so that something can observe it again.
func (e *Engine) incidentCauseConfigured(cause incidentCause) bool {
	switch cause.kind {
	case store.IncidentIndexer:
		return slices.ContainsFunc(e.indexers, func(client indexer.Indexer) bool { return client.Name() == cause.key })
	case store.IncidentDownloadClient:
		return cause.key == downloadClientIncidentKey
	case store.IncidentStorage:
		if cause.key == e.cfg.Library.MovieRoot || cause.key == e.cfg.Library.TVRoot {
			return true
		}
		// A free-space check measures the local download path or, while it
		// does not exist yet, its nearest existing ancestor.
		for _, savePath := range []string{e.cfg.Downloader.SavePathTV, e.cfg.Downloader.SavePathMovies} {
			rel, err := filepath.Rel(cause.key, e.pathMapper.Local(savePath))
			if err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
				return true
			}
		}
		return false
	case store.IncidentLibrary:
		return cause.key == e.cfg.Library.MovieRoot || cause.key == e.cfg.Library.TVRoot
	case store.IncidentMetadata:
		return cause.key == tvmazeIncidentKey && e.tvmaze != nil
	case store.IncidentNotifications:
		return cause.key == userNtfyIncidentKey || (cause.key == webhookIncidentKey && e.cfg.Availability.WebhookURL != "")
	}
	panic(fmt.Sprintf("unknown incident kind %q", cause.kind))
}

// flushIncidents writes the pending observations of the given kinds: a failing
// cause opens or refreshes its incident, a cause seen only healthy resolves
// its open incident, and an unobserved cause is left alone. Without kinds it
// only loads the open incidents.
func (e *Engine) flushIncidents(ctx context.Context, kinds ...string) error {
	var t = &e.incidents
	var now = e.clock.Now().UTC()
	var errs []error

	t.mu.Lock()
	defer t.mu.Unlock()
	if err := e.loadOpenIncidents(ctx); err != nil {
		return err
	}
	for cause, obs := range t.pending {
		if !slices.Contains(kinds, cause.kind) {
			continue
		}
		delete(t.pending, cause)
		current, open := t.open[cause]
		if !obs.failed {
			if !obs.healthy || !open {
				continue
			}
			if err := e.store.Incidents().Resolve(ctx, cause.kind, cause.key, "", now); err != nil {
				errs = append(errs, err)
				continue
			}
			delete(t.open, cause)
			e.log.Info("incident resolved", "kind", cause.kind, "cause", cause.key)
			continue
		}
		if !open {
			current = &openIncident{items: make(map[string]bool)}
		}
		for item := range obs.items {
			current.items[item] = true
		}
		affected := max(current.affected, len(current.items))
		if open && affected == current.affected && now.Sub(current.writtenAt) < incidentTouchInterval {
			continue
		}
		incident := store.Incident{Kind: cause.kind, Key: cause.key, Detail: obs.detail, Affected: affected}
		if interval := IncidentCheckInterval(e.cfg, cause.kind); interval > 0 {
			next := now.Add(interval)
			incident.NextCheckAt = &next
		}
		if err := e.store.Incidents().Observe(ctx, incident, now); err != nil {
			errs = append(errs, err)
			continue
		}
		if !open {
			e.log.Warn("incident opened", "kind", cause.kind, "cause", cause.key, "detail", obs.detail)
		}
		current.affected, current.writtenAt = affected, now
		t.open[cause] = current
	}
	return errors.Join(errs...)
}
