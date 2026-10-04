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
	"github.com/TechXTT/reelay/internal/importer"
	"github.com/TechXTT/reelay/internal/indexer"
	"github.com/TechXTT/reelay/internal/metadata"
	"github.com/TechXTT/reelay/internal/model"
	"github.com/TechXTT/reelay/internal/recommendation"
	"github.com/TechXTT/reelay/internal/store"
)

type cliFlags struct {
	configPath   string
	dev          bool
	showVersion  bool
	checkOnly    bool
	backupPath   string
	restorePath  string
	searchTerm   string
	searchRecent bool
	grabMagnet   string
	grabType     string
	domain       domainCLIOptions
}

// clients are the external systems Reelay talks to, built once from config.
type clients struct {
	indexers   []indexer.Indexer
	downloader downloader.Downloader
}

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
	flags := parseFlags()
	if flags.showVersion {
		fmt.Println(buildinfo.Get())
		return nil
	}

	cfg, log, err := loadConfig(flags)
	if err != nil {
		return err
	}

	// SIGINT/SIGTERM cancel the root context; every loop and the HTTP server
	// hang off it.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	if handled, err := runStorelessMode(ctx, cfg, log, flags); handled {
		return err
	}

	st, err := openStore(ctx, cfg, log)
	if err != nil {
		return err
	}
	defer func() {
		if err := st.Close(); err != nil {
			log.Error("closing database", "error", err)
		}
	}()

	if flags.backupPath != "" {
		return st.Backup(ctx, flags.backupPath)
	}
	if err := seedProfiles(ctx, st, cfg, log); err != nil {
		return err
	}
	if flags.domain.Active() {
		return runDomainCLI(ctx, st, cfg, flags.domain)
	}

	cl, err := buildClients(ctx, cfg, log)
	if err != nil {
		return err
	}
	if flags.checkOnly {
		log.Info("configuration and schema are valid")
		return nil
	}
	return serve(ctx, cfg, log, st, cl)
}

func parseFlags() cliFlags {
	var f cliFlags

	flag.StringVar(&f.configPath, "config", "config.yaml", "path to the configuration file")
	flag.BoolVar(&f.dev, "dev", false, "human-readable text logs at debug level")
	flag.BoolVar(&f.showVersion, "version", false, "print version and exit")
	flag.BoolVar(&f.checkOnly, "check", false, "validate the config and the schema, then exit")
	flag.StringVar(&f.backupPath, "backup", "", "write a consistent SQLite backup to a new file, then exit")
	flag.StringVar(&f.restorePath, "restore", "", "restore a backup into the configured database path, which must not exist; stop the service first")
	flag.StringVar(&f.searchTerm, "search", "", "run a one-shot live indexer search, print the parsed and scored results, and exit")
	flag.BoolVar(&f.searchRecent, "search-recent", false, "with --search: fetch the indexer's newest listing instead of searching a term")
	flag.StringVar(&f.grabMagnet, "grab", "", "hand one magnet to the download client and follow it to completion, then exit")
	flag.StringVar(&f.grabType, "grab-type", "tv", `with --grab: "tv" or "movie", selecting which category and save path to use`)
	flag.StringVar(&f.domain.AddMovie, "add-movie", "", "persist a wanted movie and exit")
	flag.IntVar(&f.domain.MovieYear, "movie-year", 0, "with --add-movie: release year, or 0 if unknown")
	flag.StringVar(&f.domain.AddSeries, "add-series", "", "persist a followed series and exit")
	flag.StringVar(&f.domain.MonitorMode, "monitor-mode", "future_only", "with --add-series: all, future_only, latest_season, or none")
	flag.StringVar(&f.domain.AddEpisode, "add-episode", "", "persist a wanted episode as <series-id>:SxxEyy and exit")
	flag.StringVar(&f.domain.EpisodeTitle, "episode-title", "", "with --add-episode: optional episode title")
	flag.StringVar(&f.domain.AirDate, "air-date", "", "with --add-episode: optional YYYY-MM-DD air date")
	flag.BoolVar(&f.domain.ListItems, "list-items", false, "list persisted series, episodes, and movies, then exit")
	flag.StringVar(&f.domain.Transition, "transition", "", "transition an item as <episode|movie>:<id>:<state> and exit")
	flag.StringVar(&f.domain.Reason, "reason", "manual CLI action", "reason recorded for --transition")
	flag.StringVar(&f.domain.History, "history", "", "show transition history for <episode|movie>:<id> and exit")
	flag.Parse()

	return f
}

// loadConfig reads the config file and builds the process logger from it.
func loadConfig(flags cliFlags) (*config.Config, *slog.Logger, error) {
	cfg, warnings, err := config.Load(flags.configPath)
	if err != nil {
		return nil, nil, err
	}

	log := newLogger(cfg, flags.dev)
	slog.SetDefault(log)

	log.Info("starting reelay",
		"version", buildinfo.Version,
		"commit", buildinfo.Commit,
		"platform", buildinfo.Get().Platform,
		"config", flags.configPath)

	// Warnings are conditions we allow but the operator must see. They are
	// logged after the logger exists, which is why Load returns rather than
	// prints them.
	for _, w := range warnings {
		log.Warn("configuration", "warning", w)
	}
	return cfg, log, nil
}

// runStorelessMode handles --search, --grab and --restore. The first two are
// diagnostics: no database, no server, no state. All three run before the store
// is opened so they work against a config whose database path is not writable.
// handled reports whether one of them ran, in which case err is the result.
func runStorelessMode(ctx context.Context, cfg *config.Config, log *slog.Logger, flags cliFlags) (bool, error) {
	switch {
	case flags.searchTerm != "" || flags.searchRecent:
		return true, runSearch(ctx, cfg, log, flags.searchTerm, flags.searchRecent)
	case flags.grabMagnet != "":
		category, err := grabCategoryFor(cfg, flags.grabType)
		if err != nil {
			return true, err
		}
		return true, runGrab(ctx, cfg, log, flags.grabMagnet, category)
	case flags.restorePath != "":
		if flags.backupPath != "" {
			return true, errors.New("backup and restore cannot be combined")
		}
		return true, store.Restore(ctx, flags.restorePath, cfg.Database.Path)
	}
	return false, nil
}

func openStore(ctx context.Context, cfg *config.Config, log *slog.Logger) (*store.Store, error) {
	st, err := store.Open(ctx, store.Options{
		Path:      cfg.Database.Path,
		CacheKB:   cfg.Runtime.SQLiteCacheKB,
		ReadConns: cfg.Runtime.SearchConcurrency + 1,
	})
	if err != nil {
		return nil, err
	}
	log.Info("database open", "path", st.Path(), "cache_kb", cfg.Runtime.SQLiteCacheKB)

	if err := store.Migrate(ctx, st, log); err != nil {
		if closeErr := st.Close(); closeErr != nil {
			log.Error("closing database", "error", closeErr)
		}
		return nil, err
	}
	return st, nil
}

func seedProfiles(ctx context.Context, st *store.Store, cfg *config.Config, log *slog.Logger) error {
	profiles := make([]model.QualityProfile, 0, len(cfg.Profiles))
	for _, p := range cfg.Profiles {
		profiles = append(profiles, p.ToModel())
	}
	seeded, err := st.Profiles().Seed(ctx, profiles)
	if err != nil {
		return err
	}
	if seeded {
		log.Info("seeded quality profiles", "count", len(profiles))
	}
	return nil
}

func buildClients(ctx context.Context, cfg *config.Config, log *slog.Logger) (clients, error) {
	indexers, err := buildIndexers(cfg, log, clock.Real{})
	if err != nil {
		return clients{}, err
	}
	log.Info("indexers ready", "count", len(indexers))

	dl, err := buildDownloader(cfg, log)
	if err != nil {
		return clients{}, err
	}
	// Create the categories now rather than at first grab. A missing category
	// at grab time means either failing the grab or adding the torrent
	// uncategorised — and an uncategorised torrent is one Reelay can never see
	// again and cannot tell apart from the operator's own.
	if err := ensureCategories(ctx, dl, cfg, log); err != nil {
		log.Warn("could not prepare download client categories; grabs will fail until this is fixed",
			"error", err)
	}
	return clients{indexers: indexers, downloader: dl}, nil
}

// serve wires the engine and the HTTP API together and runs them until ctx is
// cancelled or either fails.
func serve(ctx context.Context, cfg *config.Config, log *slog.Logger, st *store.Store, cl clients) error {
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
	recommendations := recommendation.NewService(st, tmdb, cfg.Recommendations, time.Now, log)
	eng, err := engine.New(engine.Options{Store: st, Config: cfg, Indexers: cl.indexers,
		Downloader: cl.downloader, TVmaze: tvmaze, Importer: mediaImporter, Clock: clock.Real{},
		Logger: log, PathMapper: pathMapperFor(cfg), Recommendations: recommendations})
	if err != nil {
		return err
	}
	srv := api.New(api.Options{Config: cfg, Store: st, Logger: log, Clock: clock.Real{},
		Engine: eng, Movies: tmdb, Series: tvmaze, Indexers: cl.indexers, Downloader: cl.downloader,
		Recommendations: recommendations, ExternalSeries: tvmaze, Discovery: tmdb})
	registerHardlinkProbes(srv, cfg, log)
	registerIndexerHealth(srv, cl.indexers)
	registerDownloaderHealth(srv, cl.downloader, cfg)

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
