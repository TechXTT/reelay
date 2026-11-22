package api

import (
	"bytes"
	"context"
	"net/http"
	"os"
	"runtime"
	"time"

	"github.com/TechXTT/reelay/internal/buildinfo"
	"github.com/TechXTT/reelay/internal/indexer"
	"github.com/TechXTT/reelay/internal/metrics"
	"github.com/TechXTT/reelay/internal/model"
	"github.com/TechXTT/reelay/internal/store"
)

// grabStates lists every value of the grabs.state CHECK constraint, so a state
// without rows is still exported as 0 and its series does not disappear.
var grabStates = []model.GrabState{model.GrabPending, model.GrabDownloading, model.GrabCompleted,
	model.GrabImporting, model.GrabImported, model.GrabStalled, model.GrabRemoved, model.GrabFailed}

// breakerReporter is implemented by indexer clients that run a circuit breaker.
type breakerReporter interface{ BreakerState() indexer.BreakerState }

// handleMetrics renders the Prometheus text exposition format. Gauges are read
// at scrape time; the counters live in the engine. The body is buffered so a
// failed read returns the normal error instead of a truncated scrape.
func (s *Server) handleMetrics(w http.ResponseWriter, r *http.Request) error {
	var ctx, cancel = context.WithTimeout(r.Context(), 10*time.Second)
	var buf bytes.Buffer
	var out = metrics.NewWriter(&buf)

	defer cancel()
	if s.engine != nil {
		s.engine.Metrics().Write(out)
	}
	if err := s.writeStoreMetrics(ctx, out); err != nil {
		return err
	}
	s.writeIndexerMetrics(ctx, out)
	s.writeProcessMetrics(out)
	if out.Err() != nil {
		return Internal(out.Err())
	}
	w.Header().Set("Content-Type", metrics.ContentType)
	w.Header().Set("Cache-Control", "no-store")
	_, _ = w.Write(buf.Bytes())
	return nil
}

func (s *Server) writeStoreMetrics(ctx context.Context, out *metrics.Writer) error {
	var episodes, err = s.store.Episodes().CountByState(ctx)

	if err != nil {
		return err
	}
	movies, err := s.store.Movies().CountByState(ctx)
	if err != nil {
		return err
	}
	grabs, err := s.store.Grabs().CountByState(ctx)
	if err != nil {
		return err
	}
	pending, err := s.store.Requests().PendingNotifications(ctx)
	if err != nil {
		return err
	}
	userPending, err := s.store.Requests().PendingUserNotifications(ctx)
	if err != nil {
		return err
	}
	incidents, err := s.store.Incidents().Open(ctx)
	if err != nil {
		return err
	}
	out.Family("reelay_items", "Episodes and movies by state.", "gauge")
	for _, state := range model.AllItemStates {
		out.Sample("reelay_items", float64(episodes[state]), "subject", string(model.SubjectEpisode), "state", string(state))
		out.Sample("reelay_items", float64(movies[state]), "subject", string(model.SubjectMovie), "state", string(state))
	}
	out.Family("reelay_grabs", "Grabs by state.", "gauge")
	for _, state := range grabStates {
		out.Sample("reelay_grabs", float64(grabs[state]), "state", string(state))
	}
	out.Gauge("reelay_notifications_pending", "Availability notifications waiting for delivery.", float64(pending))
	out.Gauge("reelay_user_notifications_pending", "Per-user availability notifications waiting for delivery.", float64(userPending))
	out.Family("reelay_incidents_open", "Open operator incidents by kind.", "gauge")
	for _, kind := range store.IncidentKinds {
		var count = 0

		for _, incident := range incidents {
			if incident.Kind == kind {
				count++
			}
		}
		out.Sample("reelay_incidents_open", float64(count), "kind", kind)
	}
	return nil
}

func (s *Server) writeIndexerMetrics(ctx context.Context, out *metrics.Writer) {
	var reporters []breakerReporter

	if len(s.indexers) == 0 {
		return
	}
	out.Family("reelay_indexer_up", "1 when the indexer circuit breaker permits requests.", "gauge")
	for _, source := range s.indexers {
		var up = 0.0

		if source.Healthy(ctx) == nil {
			up = 1
		}
		out.Sample("reelay_indexer_up", up, "indexer", source.Name())
		if reporter, ok := source.(breakerReporter); ok {
			reporters = append(reporters, reporter)
		}
	}
	if len(reporters) == 0 {
		return
	}
	out.Family("reelay_indexer_breaker_failures", "Consecutive failures counted by the indexer circuit breaker.", "gauge")
	for _, reporter := range reporters {
		var state = reporter.BreakerState()

		out.Sample("reelay_indexer_breaker_failures", float64(state.Failures), "indexer", state.Name)
	}
	out.Family("reelay_indexer_breaker_trips_total", "Times the indexer circuit breaker has opened.", "counter")
	for _, reporter := range reporters {
		var state = reporter.BreakerState()

		out.Sample("reelay_indexer_breaker_trips_total", float64(state.Trips), "indexer", state.Name)
	}
}

func (s *Server) writeProcessMetrics(out *metrics.Writer) {
	var mem runtime.MemStats
	var path = s.store.Path()

	if s.engine != nil {
		out.Gauge("reelay_sse_clients", "Connected live-update clients.", float64(s.engine.Events().Subscribers()))
		if backup := s.engine.BackupStatus(); backup.Enabled && !backup.Newest.IsZero() {
			out.Gauge("reelay_backup_newest_timestamp_seconds", "Unix time of the newest scheduled backup.",
				float64(backup.Newest.Unix()))
		}
	}
	out.Family("reelay_database_bytes", "Size of the database file and its write-ahead log.", "gauge")
	for _, file := range [][2]string{{"db", path}, {"wal", path + "-wal"}} {
		var size = 0.0

		if info, err := os.Stat(file[1]); err == nil {
			size = float64(info.Size())
		}
		out.Sample("reelay_database_bytes", size, "file", file[0])
	}
	runtime.ReadMemStats(&mem)
	out.Gauge("go_goroutines", "Number of goroutines.", float64(runtime.NumGoroutine()))
	out.Gauge("go_memstats_heap_alloc_bytes", "Bytes of allocated heap objects.", float64(mem.HeapAlloc))
	out.Gauge("go_memstats_sys_bytes", "Bytes obtained from the system.", float64(mem.Sys))
	out.Gauge("reelay_build_info", "Build information; the value is always 1.", 1,
		"version", buildinfo.Version, "commit", buildinfo.Commit)
}
