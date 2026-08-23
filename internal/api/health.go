package api

import (
	"context"
	"net/http"
	"sort"
	"time"

	"github.com/TechXTT/reelay/internal/buildinfo"
)

// Health status values. Ordered by severity in statusRank.
const (
	StatusOK       = "ok"
	StatusDegraded = "degraded"
	StatusDown     = "down"
	StatusSkipped  = "skipped"
)

func statusRank(s string) int {
	switch s {
	case StatusDown:
		return 3
	case StatusDegraded:
		return 2
	case StatusSkipped:
		return 1
	default:
		return 0
	}
}

// CheckResult is what one component reports.
type CheckResult struct {
	Status string         `json:"status"`
	Detail string         `json:"detail,omitempty"`
	Extra  map[string]any `json:"extra,omitempty"`
}

// Checker is implemented by anything that appears on the health endpoint.
// Phase 1 registers the database and the hardlink probes; the indexer breaker
// and the download client register themselves in phases 3 and 5 without any
// change here.
type Checker interface {
	CheckName() string
	// CheckKind groups components in the UI: database, filesystem, indexer,
	// downloader.
	CheckKind() string
	// CheckCritical marks a component whose failure means Reelay cannot work
	// at all, as opposed to working in a degraded way.
	CheckCritical() bool
	Check(ctx context.Context) CheckResult
}

// FuncChecker adapts a closure, so trivial checks need no new type.
type FuncChecker struct {
	Name     string
	Kind     string
	Critical bool
	Fn       func(ctx context.Context) CheckResult
}

func (f FuncChecker) CheckName() string                     { return f.Name }
func (f FuncChecker) CheckKind() string                     { return f.Kind }
func (f FuncChecker) CheckCritical() bool                   { return f.Critical }
func (f FuncChecker) Check(ctx context.Context) CheckResult { return f.Fn(ctx) }

// componentDTO is one row of the health response.
type componentDTO struct {
	Name     string         `json:"name"`
	Kind     string         `json:"kind"`
	Status   string         `json:"status"`
	Critical bool           `json:"critical"`
	Detail   string         `json:"detail,omitempty"`
	Extra    map[string]any `json:"extra,omitempty"`
}

type healthDTO struct {
	Status        string         `json:"status"`
	Version       buildinfo.Info `json:"version"`
	SchemaVersion int            `json:"schema_version"`
	StartedAt     string         `json:"started_at"`
	UptimeSeconds int64          `json:"uptime_seconds"`
	Components    []componentDTO `json:"components"`
}

// checkTimeout bounds the whole health sweep. A wedged indexer must not turn
// the health endpoint itself into a hang.
const checkTimeout = 3 * time.Second

func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) error {
	ctx, cancel := context.WithTimeout(r.Context(), checkTimeout)
	defer cancel()

	s.mu.RLock()
	checkers := make([]Checker, len(s.checkers))
	copy(checkers, s.checkers)
	s.mu.RUnlock()

	components := make([]componentDTO, 0, len(checkers))
	overall := StatusOK
	for _, c := range checkers {
		res := c.Check(ctx)
		if res.Status == "" {
			res.Status = StatusDown
		}
		components = append(components, componentDTO{
			Name:     c.CheckName(),
			Kind:     c.CheckKind(),
			Status:   res.Status,
			Critical: c.CheckCritical(),
			Detail:   res.Detail,
			Extra:    res.Extra,
		})

		// A critical component that is down takes the whole service down.
		// Anything else failing is degraded: searches may not run, but the
		// API and the library are intact.
		switch {
		case res.Status == StatusDown && c.CheckCritical():
			overall = StatusDown
		case statusRank(res.Status) > 0 && overall != StatusDown:
			overall = StatusDegraded
		}
	}

	sort.SliceStable(components, func(i, j int) bool {
		if components[i].Kind != components[j].Kind {
			return components[i].Kind < components[j].Kind
		}
		return components[i].Name < components[j].Name
	})

	schemaVersion := s.schemaVersion(ctx)

	code := http.StatusOK
	if overall == StatusDown {
		code = http.StatusServiceUnavailable
	}
	writeJSON(w, s.logFor(r), code, healthDTO{
		Status:        overall,
		Version:       buildinfo.Get(),
		SchemaVersion: schemaVersion,
		StartedAt:     s.startedAt.UTC().Format(time.RFC3339),
		UptimeSeconds: int64(s.clock.Since(s.startedAt).Seconds()),
		Components:    components,
	})
	return nil
}

// handlePing is the unauthenticated liveness probe. It deliberately reveals
// nothing: no version, no paths, no component list.
func (s *Server) handlePing(w http.ResponseWriter, _ *http.Request) error {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte("ok\n"))
	return nil
}
