// Command reelay is the whole service: one binary, one config file.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/TechXTT/reelay/internal/api"
	"github.com/TechXTT/reelay/internal/buildinfo"
	"github.com/TechXTT/reelay/internal/clock"
	"github.com/TechXTT/reelay/internal/config"
	"github.com/TechXTT/reelay/internal/downloader"
	"github.com/TechXTT/reelay/internal/engine"
	"github.com/TechXTT/reelay/internal/fsprobe"
	"github.com/TechXTT/reelay/internal/importer"
	"github.com/TechXTT/reelay/internal/indexer"
	"github.com/TechXTT/reelay/internal/indexer/tpb"
	"github.com/TechXTT/reelay/internal/metadata"
	"github.com/TechXTT/reelay/internal/model"
	"github.com/TechXTT/reelay/internal/store"
)

func main() {
	if err := run(); err != nil {
		// Config problems are multi-line and already formatted for a human;
		// they go to stderr rather than through the structured logger, which
		// may not exist yet.
		fmt.Fprintf(os.Stderr, "reelay: %v\n", err)
		os.Exit(1)
	}
}

func run() error {
	var (
		configPath   = flag.String("config", "config.yaml", "path to the configuration file")
		dev          = flag.Bool("dev", false, "human-readable text logs at debug level")
		showVersion  = flag.Bool("version", false, "print version and exit")
		checkOnly    = flag.Bool("check", false, "validate the config and the schema, then exit")
		searchTerm   = flag.String("search", "", "run a one-shot live indexer search, print the parsed and scored results, and exit")
		searchRecent = flag.Bool("search-recent", false, "with --search: fetch the indexer's newest listing instead of searching a term")
		grabMagnet   = flag.String("grab", "", "hand one magnet to the download client and follow it to completion, then exit")
		grabType     = flag.String("grab-type", "tv", `with --grab: "tv" or "movie", selecting which category and save path to use`)
		addMovie     = flag.String("add-movie", "", "persist a wanted movie and exit")
		movieYear    = flag.Int("movie-year", 0, "with --add-movie: release year, or 0 if unknown")
		addSeries    = flag.String("add-series", "", "persist a followed series and exit")
		monitorMode  = flag.String("monitor-mode", "future_only", "with --add-series: all, future_only, latest_season, or none")
		addEpisode   = flag.String("add-episode", "", "persist a wanted episode as <series-id>:SxxEyy and exit")
		episodeTitle = flag.String("episode-title", "", "with --add-episode: optional episode title")
		airDate      = flag.String("air-date", "", "with --add-episode: optional YYYY-MM-DD air date")
		listItems    = flag.Bool("list-items", false, "list persisted series, episodes, and movies, then exit")
		transition   = flag.String("transition", "", "transition an item as <episode|movie>:<id>:<state> and exit")
		reason       = flag.String("reason", "manual CLI action", "reason recorded for --transition")
		history      = flag.String("history", "", "show transition history for <episode|movie>:<id> and exit")
	)
	flag.Parse()

	if *showVersion {
		fmt.Println(buildinfo.Get())
		return nil
	}

	cfg, warnings, err := config.Load(*configPath)
	if err != nil {
		return err
	}

	log := newLogger(cfg, *dev)
	slog.SetDefault(log)

	log.Info("starting reelay",
		"version", buildinfo.Version,
		"commit", buildinfo.Commit,
		"platform", buildinfo.Get().Platform,
		"config", *configPath)

	// Warnings are conditions we allow but the operator must see. They are
	// logged after the logger exists, which is why Load returns rather than
	// prints them.
	for _, w := range warnings {
		log.Warn("configuration", "warning", w)
	}

	// SIGINT/SIGTERM cancel the root context; every loop and the HTTP server
	// hang off it.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	// --search and --grab are diagnostics: no database, no server, no state.
	// They run before the store is opened so they work against a config whose
	// database path is not writable.
	if *searchTerm != "" || *searchRecent {
		return runSearch(ctx, cfg, log, *searchTerm, *searchRecent)
	}
	if *grabMagnet != "" {
		category, err := grabCategoryFor(cfg, *grabType)
		if err != nil {
			return err
		}
		return runGrab(ctx, cfg, log, *grabMagnet, category)
	}

	st, err := store.Open(ctx, store.Options{
		Path:      cfg.Database.Path,
		CacheKB:   cfg.Runtime.SQLiteCacheKB,
		ReadConns: cfg.Runtime.SearchConcurrency + 1,
	})
	if err != nil {
		return err
	}
	defer func() {
		if err := st.Close(); err != nil {
			log.Error("closing database", "error", err)
		}
	}()
	log.Info("database open", "path", st.Path(), "cache_kb", cfg.Runtime.SQLiteCacheKB)

	if err := store.Migrate(ctx, st, log); err != nil {
		return err
	}
	seedProfiles := make([]model.QualityProfile, 0, len(cfg.Profiles))
	for _, p := range cfg.Profiles {
		seedProfiles = append(seedProfiles, p.ToModel())
	}
	seeded, err := st.Profiles().Seed(ctx, seedProfiles)
	if err != nil {
		return err
	}
	if seeded {
		log.Info("seeded quality profiles", "count", len(seedProfiles))
	}

	domainCLI := domainCLIOptions{
		AddMovie: *addMovie, MovieYear: *movieYear,
		AddSeries: *addSeries, MonitorMode: *monitorMode,
		AddEpisode: *addEpisode, EpisodeTitle: *episodeTitle, AirDate: *airDate,
		ListItems: *listItems, Transition: *transition, Reason: *reason, History: *history,
	}
	if domainCLI.Active() {
		return runDomainCLI(ctx, st, cfg, domainCLI)
	}

	indexers, err := buildIndexers(cfg, log, clock.Real{})
	if err != nil {
		return err
	}
	log.Info("indexers ready", "count", len(indexers))

	dl, err := buildDownloader(cfg, log)
	if err != nil {
		return err
	}
	// Create the categories now rather than at first grab. A missing category
	// at grab time means either failing the grab or adding the torrent
	// uncategorised — and an uncategorised torrent is one Reelay can never see
	// again and cannot tell apart from the operator's own.
	if err := ensureCategories(ctx, dl, cfg, log); err != nil {
		log.Warn("could not prepare download client categories; grabs will fail until this is fixed",
			"error", err)
	}

	if *checkOnly {
		log.Info("configuration and schema are valid")
		return nil
	}

	tmdb, err := metadata.NewTMDB(metadata.TMDBOptions{BaseURL: cfg.Metadata.TMDBBaseURL,
		APIKey: cfg.Metadata.TMDBAPIKey, Timeout: cfg.Metadata.RequestTimeout.Duration,
		Cache: st.Metadata(), CacheTTL: cfg.Metadata.CacheTTL.Duration})
	if err != nil {
		return fmt.Errorf("create TMDB client: %w", err)
	}
	tvmaze, err := metadata.NewTVmaze(metadata.TVmazeOptions{BaseURL: cfg.Metadata.TVmazeBaseURL,
		Timeout: cfg.Metadata.RequestTimeout.Duration, Cache: st.Metadata(),
		CacheTTL: cfg.Metadata.CacheTTL.Duration})
	if err != nil {
		return fmt.Errorf("create TVmaze client: %w", err)
	}
	mediaImporter, err := importer.New(importer.Options{Store: st, Config: cfg, Logger: log})
	if err != nil {
		return err
	}
	eng, err := engine.New(engine.Options{Store: st, Config: cfg, Indexers: indexers,
		Downloader: dl, TVmaze: tvmaze, Importer: mediaImporter, Clock: clock.Real{},
		Logger: log, PathMapper: pathMapperFor(cfg)})
	if err != nil {
		return err
	}
	srv := api.New(api.Options{Config: cfg, Store: st, Logger: log, Clock: clock.Real{},
		Engine: eng, Movies: tmdb, Series: tvmaze, Indexers: indexers, Downloader: dl})
	registerHardlinkProbes(srv, cfg, log)
	registerIndexerHealth(srv, indexers)
	registerDownloaderHealth(srv, dl, cfg)

	runCtx, cancelRun := context.WithCancel(ctx)
	defer cancelRun()
	engineDone := make(chan error, 1)
	go func() { engineDone <- eng.Run(runCtx) }()

	err = srv.Serve(runCtx)
	cancelRun()
	if err != nil && !errors.Is(err, context.Canceled) {
		return err
	}
	select {
	case engineErr := <-engineDone:
		if engineErr != nil && !errors.Is(engineErr, context.Canceled) {
			return engineErr
		}
	case <-time.After(10 * time.Second):
		return errors.New("engine did not stop within 10 seconds")
	}
	log.Info("shutdown complete")
	return nil
}

func newLogger(cfg *config.Config, dev bool) *slog.Logger {
	level := slog.LevelInfo
	switch cfg.Logging.Level {
	case "debug":
		level = slog.LevelDebug
	case "warn":
		level = slog.LevelWarn
	case "error":
		level = slog.LevelError
	}

	format := cfg.Logging.Format
	if dev {
		format = "text"
		level = slog.LevelDebug
	}

	opts := &slog.HandlerOptions{Level: level}
	if format == "text" {
		return slog.New(slog.NewTextHandler(os.Stderr, opts))
	}
	return slog.New(slog.NewJSONHandler(os.Stderr, opts))
}

// registerHardlinkProbes runs one probe per (download path, library root) pair
// and freezes the result. Filesystem capability does not change under a running
// process, so re-probing on every health request would only add I/O.
func registerHardlinkProbes(srv *api.Server, cfg *config.Config, log *slog.Logger) {
	pairs := []struct {
		name string
		from string
		to   string
	}{
		{"hardlink:tv", cfg.Downloader.SavePathTV, cfg.Library.TVRoot},
		{"hardlink:movies", cfg.Downloader.SavePathMovies, cfg.Library.MovieRoot},
	}

	// The download paths are what the CLIENT reports, so they go through the
	// path mapper before the probe touches the filesystem — the same
	// translation the importer will apply.
	mapper := pathMapperFor(cfg)

	for _, p := range pairs {
		res := fsprobe.Hardlink(mapper.Local(p.from), p.to)

		switch res.Support {
		case fsprobe.Supported:
			log.Info("hardlink probe", "pair", p.name, "result", "supported",
				"from", res.From, "to", res.To)
		case fsprobe.Unsupported:
			log.Warn("hardlink probe", "pair", p.name, "result", "unsupported",
				"from", res.From, "to", res.To,
				"cross_device", res.CrossDevice, "detail", res.Detail)
		default:
			log.Warn("hardlink probe", "pair", p.name, "result", "unknown",
				"detail", res.Detail)
		}

		status := api.StatusOK
		switch res.Support {
		case fsprobe.Unsupported:
			// Not down: imports still work, they just copy. Degraded is the
			// honest description.
			status = api.StatusDegraded
		case fsprobe.Unknown:
			status = api.StatusSkipped
		}
		if !cfg.Library.Hardlink {
			status = api.StatusSkipped
			res.Detail = "library.hardlink is disabled in config; every import copies"
		}

		frozen := res
		frozenStatus := status
		srv.Register(api.FuncChecker{
			Name:     p.name,
			Kind:     "filesystem",
			Critical: false,
			Fn: func(context.Context) api.CheckResult {
				return api.CheckResult{
					Status: frozenStatus,
					Detail: frozen.Detail,
					Extra: map[string]any{
						"from":         frozen.From,
						"to":           frozen.To,
						"hardlink":     frozen.Status,
						"cross_device": frozen.CrossDevice,
					},
				}
			},
		})
	}
}

// registerIndexerHealth surfaces each indexer's circuit breaker on the health
// endpoint. An unhealthy indexer is degraded, not down: the rest of the service
// keeps working and the other indexers keep being searched.
func registerIndexerHealth(srv *api.Server, indexers []indexer.Indexer) {
	for _, ix := range indexers {
		ix := ix
		srv.Register(api.FuncChecker{
			Name:     "indexer:" + ix.Name(),
			Kind:     "indexer",
			Critical: false,
			Fn: func(ctx context.Context) api.CheckResult {
				res := api.CheckResult{Status: api.StatusOK}
				if err := ix.Healthy(ctx); err != nil {
					res.Status = api.StatusDegraded
					res.Detail = err.Error()
				}
				// Statser is optional: the interface does not require it, but
				// an implementation that offers counters should have them
				// shown, because the no-results ratio is how throttling
				// becomes visible.
				if s, ok := ix.(interface{ Stats() tpb.Stats }); ok {
					st := s.Stats()
					res.Extra = map[string]any{
						"searches":        st.Searches,
						"no_results":      st.NoResults,
						"failed_requests": st.FailedRequests,
						"rows_returned":   st.Rows,
						"breaker_trips":   st.Trips,
					}
					if st.Searches > 0 && st.NoResults*2 > st.Searches {
						res.Status = api.StatusDegraded
						res.Detail = fmt.Sprintf(
							"%d of %d searches returned the no-results marker; the indexer is probably throttling. Lower indexers[].rate_limit_per_second.",
							st.NoResults, st.Searches)
					}
				}
				return res
			},
		})
	}
}

// registerDownloaderHealth surfaces the download client on the health
// endpoint. Critical: with no download client nothing can be grabbed, so the
// service is genuinely not working rather than merely degraded.
func registerDownloaderHealth(srv *api.Server, dl downloader.Downloader, cfg *config.Config) {
	srv.Register(api.FuncChecker{
		Name:     "downloader:" + cfg.Downloader.Type,
		Kind:     "downloader",
		Critical: true,
		Fn: func(ctx context.Context) api.CheckResult {
			if err := dl.Healthy(ctx); err != nil {
				return api.CheckResult{Status: api.StatusDown, Detail: err.Error()}
			}
			extra := map[string]any{
				"url":        cfg.Downloader.URL,
				"categories": []string{cfg.Downloader.CategoryTV, cfg.Downloader.CategoryMovies},
			}
			// Version reporting is optional on the interface; show it when the
			// implementation offers it.
			if v, ok := dl.(interface {
				Version(context.Context) (string, error)
				APIVersion() string
			}); ok {
				if ver, err := v.Version(ctx); err == nil {
					extra["version"] = ver
				}
				extra["web_api"] = v.APIVersion()
			}
			return api.CheckResult{Status: api.StatusOK, Extra: extra}
		},
	})
}
