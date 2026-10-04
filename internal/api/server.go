// Package api serves the REST API and, from phase 9, the embedded web UI.
//
// It depends on store and (later) engine. Nothing in store or engine imports
// this package.
package api

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"mime"
	"net"
	"net/http"
	"path"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/TechXTT/reelay/internal/clock"
	"github.com/TechXTT/reelay/internal/config"
	"github.com/TechXTT/reelay/internal/downloader"
	"github.com/TechXTT/reelay/internal/engine"
	"github.com/TechXTT/reelay/internal/indexer"
	"github.com/TechXTT/reelay/internal/metadata"
	"github.com/TechXTT/reelay/internal/model"
	"github.com/TechXTT/reelay/internal/recommendation"
	"github.com/TechXTT/reelay/internal/store"
	webui "github.com/TechXTT/reelay/web"
)

// Route paths referenced by middleware. Keep in sync with routes().
const (
	pathPing   = "/api/v1/ping"
	pathEvents = "/api/v1/events"
)

type Server struct {
	cfg             *config.Config
	store           *store.Store
	log             *slog.Logger
	clock           clock.Clock
	engine          *engine.Engine
	movies          metadata.MovieProvider
	series          metadata.SeriesProvider
	indexers        []indexer.Indexer
	downloader      downloader.Downloader
	recommendations *recommendation.Service
	externalSeries  metadata.ExternalSeriesProvider
	discovery       metadata.RecommendationProvider
	static          fs.FS

	http      *http.Server
	startedAt time.Time

	mu       sync.RWMutex
	checkers []Checker
}

type Options struct {
	Config          *config.Config
	Store           *store.Store
	Logger          *slog.Logger
	Clock           clock.Clock
	Engine          *engine.Engine
	Movies          metadata.MovieProvider
	Series          metadata.SeriesProvider
	Indexers        []indexer.Indexer
	Downloader      downloader.Downloader
	Recommendations *recommendation.Service
	ExternalSeries  metadata.ExternalSeriesProvider
	Discovery       metadata.RecommendationProvider
}

func New(opt Options) *Server {
	if opt.Clock == nil {
		opt.Clock = clock.Real{}
	}
	s := &Server{
		cfg:             opt.Config,
		store:           opt.Store,
		log:             opt.Logger,
		clock:           opt.Clock,
		startedAt:       opt.Clock.Now(),
		engine:          opt.Engine,
		movies:          opt.Movies,
		series:          opt.Series,
		indexers:        opt.Indexers,
		downloader:      opt.Downloader,
		recommendations: opt.Recommendations,
		externalSeries:  opt.ExternalSeries,
		discovery:       opt.Discovery,
	}
	s.static, _ = fs.Sub(webui.Dist, "dist")

	s.Register(FuncChecker{
		Name:     "sqlite",
		Kind:     "database",
		Critical: true,
		Fn: func(ctx context.Context) CheckResult {
			if err := s.store.Ping(ctx); err != nil {
				return CheckResult{Status: StatusDown, Detail: err.Error()}
			}
			return CheckResult{
				Status: StatusOK,
				Extra:  map[string]any{"path": s.store.Path()},
			}
		},
	})

	s.http = &http.Server{
		Addr:              opt.Config.Addr(),
		Handler:           s.handler(),
		ReadHeaderTimeout: opt.Config.Server.ReadTimeout.Duration,
		ReadTimeout:       opt.Config.Server.ReadTimeout.Duration,
		// Zero means no write deadline, which SSE requires. Validation warns
		// rather than forbids, so respect whatever is configured.
		WriteTimeout: opt.Config.Server.WriteTimeout.Duration,
		IdleTimeout:  120 * time.Second,
		ErrorLog:     slog.NewLogLogger(opt.Logger.Handler(), slog.LevelWarn),
	}
	return s
}

// Register adds a health component. Safe to call while serving, which is how
// later phases attach indexers and the download client.
func (s *Server) Register(c Checker) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.checkers = append(s.checkers, c)
}

func (s *Server) handler() http.Handler {
	mux := http.NewServeMux()
	s.routes(mux)

	// Outermost first. recoverPanic sits inside requestContext so the panic
	// log carries a request id, and inside compress so the error body is
	// encoded consistently.
	return chain(mux,
		requestContext(s.log),
		accessLog,
		securityHeaders,
		cors(s.cfg.Server.CORSOrigins),
		compress,
		recoverPanic,
		bearerAuth(s.cfg.Server.AuthToken),
	)
}

func (s *Server) routes(mux *http.ServeMux) {
	// Go 1.22 pattern routing: method and wildcards in the pattern itself.
	routes := []struct {
		pattern string
		handler handler
	}{
		{"GET " + pathPing, s.handlePing},
		{"GET /api/v1/health", s.handleHealth},
		{"GET " + pathEvents, s.handleEvents},
		{"GET /api/v1/search", s.handleLiveSearch},
		{"GET /api/v1/metadata/search", s.handleMetadataSearch},

		{"GET /api/v1/series", listHandler(s.store.Series().List)},
		{"POST /api/v1/series", s.handleSeriesCreate},
		{"GET /api/v1/series/{id}", withID(s.handleSeriesGet)},
		{"PATCH /api/v1/series/{id}", withID(s.handleSeriesPatch)},
		{"DELETE /api/v1/series/{id}", collectionDelete(s.deleteSeriesCollection)},
		{"POST /api/v1/series/{id}/search", withID(s.handleSeriesSearch)},

		{"GET /api/v1/movies", listHandler(s.store.Movies().List)},
		{"POST /api/v1/movies", s.handleMovieCreate},
		{"GET /api/v1/movies/{id}", withID(s.handleMovieGet)},
		{"PATCH /api/v1/movies/{id}", withID(s.handleMoviePatch)},
		{"DELETE /api/v1/movies/{id}", collectionDelete(s.deleteMovieCollection)},
		{"POST /api/v1/movies/{id}/search", s.forceSearch(model.SubjectMovie, "movie")},

		{"PATCH /api/v1/episodes/{id}", withID(s.handleEpisodePatch)},
		{"POST /api/v1/episodes/{id}/search", s.forceSearch(model.SubjectEpisode, "episode")},
		{"GET /api/v1/episodes/{id}/candidates", withID(s.handleCandidates)},
		{"POST /api/v1/episodes/{id}/grab", withID(s.handleEpisodeGrab)},

		{"GET /api/v1/queue", s.handleQueue},
		{"POST /api/v1/queue/pause", s.setQueuePaused(true)},
		{"POST /api/v1/queue/resume", s.setQueuePaused(false)},
		{"DELETE /api/v1/queue/{id}", withID(s.handleQueueDelete)},
		{"GET /api/v1/history", s.handleHistory},

		{"GET /api/v1/profiles", listHandler(s.store.Profiles().List)},
		{"POST /api/v1/profiles", s.handleProfileCreate},
		{"PATCH /api/v1/profiles/{id}", withID(s.handleProfilePatch)},
		{"DELETE /api/v1/profiles/{id}", withID(s.handleProfileDelete)},

		{"GET /api/v1/settings", s.handleSettings},
		{"GET /api/v1/setup", s.handleSetup},
		{"POST /api/v1/database/backup", s.handleDatabaseBackup},
		{"POST /api/v1/system/trigger/{loop}", s.handleTrigger},

		{"POST /api/v1/integrations/jellyfin/sync", s.handleJellyfinSync},
		{"POST /api/v1/integrations/jellyfin/events", s.handleJellyfinEvents},
		{"GET /api/v1/integrations/jellyfin/users", listHandler(s.store.Recommendations().Users)},
		{"POST /api/v1/integrations/jellyfin/trial-playback", s.handleTrialPlayback},

		{"GET /api/v1/recommendations", s.handleRecommendations},
		{"GET /api/v1/recommendations/preferences", s.handleRecommendationPreferences},
		{"PUT /api/v1/recommendations/preferences", s.handleRecommendationPreferences},
		{"GET /api/v1/recommendations/history", s.handleRecommendationHistory},
		{"GET /api/v1/recommendations/{id}/preview", withID(s.handleRecommendationPreview)},
		{"POST /api/v1/recommendations/generate", s.handleRecommendationGenerate},
		{"POST /api/v1/recommendations/{id}/actions", withID(s.handleRecommendationAction)},

		{"GET /api/v1/requests", s.handleRequests},
		{"POST /api/v1/requests/{id}/actions", withID(s.handleRequestAction)},
		{"GET /api/v1/requests/{id}/diagnostics", withID(s.handleRequestDiagnostics)},
		{"POST /api/v1/requests/{id}/grab", withID(s.handleRequestGrab)},

		{"GET /api/v1/trials", s.handleTrials},
		{"POST /api/v1/trials/{id}/vote", withID(s.handleTrialVote)},
	}
	for _, route := range routes {
		mux.HandleFunc(route.pattern, s.wrap(route.handler))
	}

	// Everything under /api that has no route yet gets a JSON 404 rather than
	// the stdlib's text/plain "404 page not found".
	mux.HandleFunc("/api/", s.wrap(func(w http.ResponseWriter, r *http.Request) error {
		return NotFound("no such endpoint: %s %s", r.Method, r.URL.Path)
	}))

	// Registered without a method: Go 1.22's mux rejects a method-qualified
	// "GET /" alongside the method-less "/api/" above, because the more
	// specific method on the more general path is ambiguous.
	mux.HandleFunc("/", s.wrap(func(w http.ResponseWriter, r *http.Request) error {
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			return BadRequest("method %s is not allowed", r.Method)
		}
		return s.serveSPA(w, r)
	}))
}

func (s *Server) serveSPA(w http.ResponseWriter, r *http.Request) error {
	name := strings.TrimPrefix(path.Clean(r.URL.Path), "/")
	if name == "." || name == "" {
		name = "index.html"
	}
	data, err := fs.ReadFile(s.static, name)
	if err != nil {
		name = "index.html"
		data, err = fs.ReadFile(s.static, name)
	}
	if err != nil {
		return Internal(err)
	}
	w.Header().Set("Content-Type", mime.TypeByExtension(filepath.Ext(name)))
	http.ServeContent(w, r, name, s.startedAt, bytes.NewReader(data))
	return nil
}

// Serve binds the listener and blocks until ctx is cancelled, then drains.
//
// Binding happens synchronously so a port clash is reported by Run's caller as
// a startup failure rather than as a log line after we claimed to be healthy.
func (s *Server) Serve(ctx context.Context) error {
	ln, err := net.Listen("tcp", s.http.Addr)
	if err != nil {
		return fmt.Errorf("api: listen on %s: %w", s.http.Addr, err)
	}

	errCh := make(chan error, 1)
	go func() {
		if err := s.http.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- fmt.Errorf("api: serve: %w", err)
			return
		}
		errCh <- nil
	}()

	s.log.Info("http server listening",
		"addr", s.http.Addr,
		"auth", s.cfg.Server.AuthToken != "")

	select {
	case err := <-errCh:
		return err
	case <-ctx.Done():
	}

	shutdownCtx, cancel := context.WithTimeout(
		context.Background(), s.cfg.Server.ShutdownTimeout.Duration)
	defer cancel()

	if err := s.http.Shutdown(shutdownCtx); err != nil {
		// Shutdown returning an error means in-flight requests outlived the
		// grace period. Close hard rather than leak the listener.
		_ = s.http.Close()
		return fmt.Errorf("api: graceful shutdown timed out after %s: %w",
			s.cfg.Server.ShutdownTimeout, err)
	}
	s.log.Info("http server stopped")
	return <-errCh
}

// Handler exposes the fully wrapped handler for httptest.
func (s *Server) Handler() http.Handler { return s.http.Handler }

func (s *Server) schemaVersion(ctx context.Context) int {
	v, err := store.SchemaVersion(ctx, s.store)
	if err != nil {
		s.log.Warn("read schema version for health", "error", err)
		return 0
	}
	return v
}
