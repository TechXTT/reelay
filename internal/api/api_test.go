package api

import (
	"compress/gzip"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/TechXTT/reelay/internal/clock"
	"github.com/TechXTT/reelay/internal/config"
	"github.com/TechXTT/reelay/internal/store"
)

const testToken = "0123456789abcdef-token"

func newTestServer(t *testing.T, mutate func(*config.Config)) (*Server, *store.Store) {
	t.Helper()

	st, err := store.Open(context.Background(), store.Options{
		Path:      filepath.Join(t.TempDir(), "reelay.db"),
		CacheKB:   512,
		ReadConns: 2,
	})
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })

	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	if err := store.Migrate(context.Background(), st, log); err != nil {
		t.Fatalf("Migrate: %v", err)
	}

	cfg := &config.Config{}
	cfg.Server.Bind = "127.0.0.1"
	cfg.Server.Port = 7878
	cfg.Server.AuthToken = testToken
	cfg.Server.ReadTimeout = config.Dur(15 * time.Second)
	cfg.Server.ShutdownTimeout = config.Dur(5 * time.Second)
	if mutate != nil {
		mutate(cfg)
	}

	return New(Options{
		Config: cfg,
		Store:  st,
		Logger: log,
		Clock:  clock.NewFake(time.Date(2026, 8, 23, 12, 0, 0, 0, time.UTC)),
	}), st
}

func do(t *testing.T, h http.Handler, method, path string, headers map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, path, nil)
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func authed() map[string]string { return map[string]string{"Authorization": "Bearer " + testToken} }

func TestHealthReportsComponents(t *testing.T) {
	srv, _ := newTestServer(t, nil)

	rec := do(t, srv.Handler(), http.MethodGet, "/api/v1/health", authed())
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}

	var got healthDTO
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v (body %s)", err, rec.Body.String())
	}
	if got.Status != StatusOK {
		t.Errorf("status = %q, want ok", got.Status)
	}
	if got.SchemaVersion < 1 {
		t.Errorf("schema_version = %d, want >= 1", got.SchemaVersion)
	}
	if len(got.Components) == 0 {
		t.Fatal("no components reported")
	}
	var found bool
	for _, c := range got.Components {
		if c.Name == "sqlite" {
			found = true
			if c.Status != StatusOK {
				t.Errorf("sqlite component status = %q", c.Status)
			}
			if !c.Critical {
				t.Error("sqlite should be marked critical")
			}
		}
	}
	if !found {
		t.Errorf("sqlite component missing from %+v", got.Components)
	}
	if rec.Header().Get("X-Request-ID") == "" {
		t.Error("X-Request-ID not set on the response")
	}
}

// A critical component down must flip the endpoint to 503, so a container
// health check or a monitoring probe actually notices.
func TestHealthReturns503WhenCriticalComponentIsDown(t *testing.T) {
	srv, st := newTestServer(t, nil)
	if err := st.Close(); err != nil {
		t.Logf("close (expected to make the ping fail): %v", err)
	}

	rec := do(t, srv.Handler(), http.MethodGet, "/api/v1/health", authed())
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503; body = %s", rec.Code, rec.Body.String())
	}
	var got healthDTO
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got.Status != StatusDown {
		t.Errorf("overall status = %q, want down", got.Status)
	}
}

// Non-critical components degrade rather than take the service down: an
// unsupported hardlink means slow imports, not a broken Reelay.
func TestNonCriticalFailureIsDegradedNot503(t *testing.T) {
	srv, _ := newTestServer(t, nil)
	srv.Register(FuncChecker{
		Name: "hardlink:tv", Kind: "filesystem", Critical: false,
		Fn: func(context.Context) CheckResult {
			return CheckResult{Status: StatusDegraded, Detail: "cross-device link"}
		},
	})

	rec := do(t, srv.Handler(), http.MethodGet, "/api/v1/health", authed())
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	var got healthDTO
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got.Status != StatusDegraded {
		t.Errorf("overall status = %q, want degraded", got.Status)
	}
}

func TestAuthRequired(t *testing.T) {
	srv, _ := newTestServer(t, nil)
	h := srv.Handler()

	cases := []struct {
		name    string
		headers map[string]string
		want    int
	}{
		{"no header", nil, http.StatusUnauthorized},
		{"wrong token", map[string]string{"Authorization": "Bearer nope"}, http.StatusUnauthorized},
		{"wrong scheme", map[string]string{"Authorization": "Basic " + testToken}, http.StatusUnauthorized},
		{"bearer", authed(), http.StatusOK},
		{"lowercase bearer", map[string]string{"Authorization": "bearer " + testToken}, http.StatusOK},
		{"api key header", map[string]string{"X-Api-Key": testToken}, http.StatusOK},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := do(t, h, http.MethodGet, "/api/v1/health", tc.headers)
			if rec.Code != tc.want {
				t.Errorf("status = %d, want %d (body %s)", rec.Code, tc.want, rec.Body.String())
			}
		})
	}
}

// The liveness probe stays open so Docker and systemd can check it without a
// credential, and it must not leak anything.
func TestPingIsUnauthenticatedAndSilent(t *testing.T) {
	srv, _ := newTestServer(t, nil)
	rec := do(t, srv.Handler(), http.MethodGet, "/api/v1/ping", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	body := rec.Body.String()
	if strings.TrimSpace(body) != "ok" {
		t.Errorf("body = %q, want \"ok\"", body)
	}
}

func TestStaticUIShellIsReachableBeforeTokenEntry(t *testing.T) {
	srv, _ := newTestServer(t, nil)
	rec := do(t, srv.Handler(), http.MethodGet, "/", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "<title>Reelay</title>") {
		t.Fatalf("response is not the embedded UI: %s", rec.Body.String())
	}
}

func TestUnauthorizedUsesErrorEnvelope(t *testing.T) {
	srv, _ := newTestServer(t, nil)
	rec := do(t, srv.Handler(), http.MethodGet, "/api/v1/health", nil)

	var env struct {
		Error struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
		t.Fatalf("decode: %v (body %s)", err, rec.Body.String())
	}
	if env.Error.Code != CodeUnauthorized {
		t.Errorf("code = %q, want %q", env.Error.Code, CodeUnauthorized)
	}
	if got := rec.Header().Get("WWW-Authenticate"); !strings.Contains(got, "Bearer") {
		t.Errorf("WWW-Authenticate = %q", got)
	}
}

func TestUnknownAPIPathIsJSON404(t *testing.T) {
	srv, _ := newTestServer(t, nil)
	rec := do(t, srv.Handler(), http.MethodGet, "/api/v1/nonsense", authed())
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
		t.Errorf("Content-Type = %q, want JSON", ct)
	}
	var env errorEnvelope
	if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
		t.Fatal(err)
	}
	if env.Error.Code != CodeNotFound {
		t.Errorf("code = %q", env.Error.Code)
	}
}

func TestNoAuthConfiguredSkipsTheCheck(t *testing.T) {
	srv, _ := newTestServer(t, func(c *config.Config) { c.Server.AuthToken = "" })
	rec := do(t, srv.Handler(), http.MethodGet, "/api/v1/health", nil)
	if rec.Code != http.StatusOK {
		t.Errorf("status = %d, want 200 with auth disabled", rec.Code)
	}
}

func TestGzipWhenRequested(t *testing.T) {
	srv, _ := newTestServer(t, nil)
	h := authed()
	h["Accept-Encoding"] = "gzip"

	rec := do(t, srv.Handler(), http.MethodGet, "/api/v1/health", h)
	if enc := rec.Header().Get("Content-Encoding"); enc != "gzip" {
		t.Fatalf("Content-Encoding = %q, want gzip", enc)
	}
	zr, err := gzip.NewReader(rec.Body)
	if err != nil {
		t.Fatalf("gzip reader: %v", err)
	}
	body, err := io.ReadAll(zr)
	if err != nil {
		t.Fatalf("gunzip: %v", err)
	}
	if !strings.Contains(string(body), `"components"`) {
		t.Errorf("decompressed body looks wrong: %s", body)
	}
}

func TestPanicBecomesJSON500(t *testing.T) {
	srv, _ := newTestServer(t, nil)
	srv.Register(FuncChecker{
		Name: "explodes", Kind: "test", Critical: false,
		Fn: func(context.Context) CheckResult { panic("boom") },
	})

	rec := do(t, srv.Handler(), http.MethodGet, "/api/v1/health", authed())
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", rec.Code)
	}
	var env errorEnvelope
	if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
		t.Fatalf("decode: %v (body %s)", err, rec.Body.String())
	}
	if env.Error.Code != CodeInternal {
		t.Errorf("code = %q, want internal", env.Error.Code)
	}
	// A panic message must never reach the client.
	if strings.Contains(rec.Body.String(), "boom") {
		t.Error("panic detail leaked into the response body")
	}
}

func TestCORSOnlyForConfiguredOrigins(t *testing.T) {
	srv, _ := newTestServer(t, func(c *config.Config) {
		c.Server.CORSOrigins = []string{"http://allowed.local"}
	})
	h := srv.Handler()

	rec := do(t, h, http.MethodGet, "/api/v1/health",
		map[string]string{"Authorization": "Bearer " + testToken, "Origin": "http://allowed.local"})
	if got := rec.Header().Get("Access-Control-Allow-Origin"); got != "http://allowed.local" {
		t.Errorf("allowed origin header = %q", got)
	}

	rec = do(t, h, http.MethodGet, "/api/v1/health",
		map[string]string{"Authorization": "Bearer " + testToken, "Origin": "http://evil.local"})
	if got := rec.Header().Get("Access-Control-Allow-Origin"); got != "" {
		t.Errorf("unexpected CORS header for a disallowed origin: %q", got)
	}
}

func TestSecurityHeaders(t *testing.T) {
	srv, _ := newTestServer(t, nil)
	rec := do(t, srv.Handler(), http.MethodGet, "/api/v1/ping", nil)
	for k, want := range map[string]string{
		"X-Content-Type-Options": "nosniff",
		"X-Frame-Options":        "DENY",
	} {
		if got := rec.Header().Get(k); got != want {
			t.Errorf("%s = %q, want %q", k, got, want)
		}
	}
}

// Serve must bind synchronously and return on context cancellation, which is
// what makes graceful shutdown observable rather than hopeful.
func TestServeShutsDownOnContextCancel(t *testing.T) {
	srv, _ := newTestServer(t, func(c *config.Config) { c.Server.Port = 0 })

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- srv.Serve(ctx) }()

	// Give Serve a moment to bind, then ask it to stop.
	time.Sleep(50 * time.Millisecond)
	cancel()

	select {
	case err := <-done:
		if err != nil {
			t.Errorf("Serve returned %v, want nil after cancellation", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Serve did not return within 10s of cancellation")
	}
}
