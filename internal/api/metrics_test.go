package api

import (
	"net/http"
	"strings"
	"testing"
)

func TestMetricsEndpointExposesPrometheusText(t *testing.T) {
	s, _ := newTestServer(t, nil)
	h := s.handler()

	if rec := do(t, h, http.MethodGet, "/api/v1/metrics", nil); rec.Code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated status = %d, want 401", rec.Code)
	}
	rec := do(t, h, http.MethodGet, "/api/v1/metrics", authed())
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body %s", rec.Code, rec.Body.String())
	}
	if got := rec.Header().Get("Content-Type"); got != "text/plain; version=0.0.4; charset=utf-8" {
		t.Fatalf("content type = %q", got)
	}
	body := rec.Body.String()
	for _, want := range []string{
		"# TYPE reelay_items gauge\n",
		`reelay_items{subject="episode",state="wanted"} 0`,
		`reelay_items{subject="movie",state="import_failed"} 0`,
		`reelay_grabs{state="stalled"} 0`,
		`reelay_grabs{state="pending"} 0`,
		"reelay_notifications_pending 0\n",
		`reelay_database_bytes{file="wal"}`,
		"go_goroutines ",
		"go_memstats_heap_alloc_bytes ",
		`reelay_build_info{version="dev",commit="none"} 1`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("metrics output missing %q", want)
		}
	}
	t.Log("\n" + body)
}
