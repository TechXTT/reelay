package main

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/TechXTT/reelay/internal/api"
	"github.com/TechXTT/reelay/internal/config"
	"github.com/TechXTT/reelay/internal/downloader"
	"github.com/TechXTT/reelay/internal/fsprobe"
	"github.com/TechXTT/reelay/internal/indexer"
	"github.com/TechXTT/reelay/internal/indexer/tpb"
)

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

		status := api.StatusOK
		switch res.Support {
		case fsprobe.Supported:
			log.Info("hardlink probe", "pair", p.name, "result", "supported",
				"from", res.From, "to", res.To)
		case fsprobe.Unsupported:
			log.Warn("hardlink probe", "pair", p.name, "result", "unsupported",
				"from", res.From, "to", res.To,
				"cross_device", res.CrossDevice, "detail", res.Detail)
			// Not down: imports still work, they just copy. Degraded is the
			// honest description.
			status = api.StatusDegraded
		default:
			log.Warn("hardlink probe", "pair", p.name, "result", "unknown",
				"detail", res.Detail)
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
