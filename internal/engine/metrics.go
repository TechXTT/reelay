package engine

import "github.com/TechXTT/reelay/internal/metrics"

// Metrics are the in-process counters behind GET /api/v1/metrics. They reset
// on restart, which Prometheus handles as a counter reset.
type Metrics struct {
	searchOutcomes  *metrics.CounterVec
	grabsStarted    *metrics.CounterVec
	grabAddFailures *metrics.Counter
	grabFailures    *metrics.CounterVec
	imports         *metrics.CounterVec
	importDuration  *metrics.Histogram
}

func newMetrics() *Metrics {
	return &Metrics{
		searchOutcomes: metrics.NewCounterVec("reelay_search_outcomes_total",
			"Search evaluations by outcome.", "outcome", "grabbed", "no_match", "held", "error"),
		grabsStarted: metrics.NewCounterVec("reelay_grabs_started_total",
			"Grabs handed to the download client by origin.", "source", "search", "manual", "fallback"),
		grabAddFailures: metrics.NewCounter("reelay_grab_add_failures_total",
			"Releases the download client or indexer refused at grab time."),
		grabFailures: metrics.NewCounterVec("reelay_grab_failures_total",
			"Grabs failed by the status loop, by reason.", "reason", "stalled", "client_error", "vanished"),
		imports: metrics.NewCounterVec("reelay_imports_total",
			"Import attempts by result.", "result", "ok", "failed"),
		importDuration: metrics.NewHistogram("reelay_import_duration_seconds",
			"Import duration in seconds.", 1, 5, 15, 60, 300, 900, 3600),
	}
}

// Write emits every engine metric in the text exposition format.
func (m *Metrics) Write(w *metrics.Writer) {
	w.CounterVec(m.searchOutcomes)
	w.CounterVec(m.grabsStarted)
	w.Counter(m.grabAddFailures)
	w.CounterVec(m.grabFailures)
	w.CounterVec(m.imports)
	w.Histogram(m.importDuration)
}

// Metrics returns the engine counters for the metrics endpoint.
func (e *Engine) Metrics() *Metrics { return e.metrics }

// searchFailed counts a search target that ended in an unexpected error and
// returns that error. A busy item lock is not an outcome and is not counted.
func (e *Engine) searchFailed(err error) error {
	e.metrics.searchOutcomes.Inc("error")
	return err
}
