package main

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/TechXTT/reelay/internal/clock"
	"github.com/TechXTT/reelay/internal/config"
	"github.com/TechXTT/reelay/internal/downloader"
	"github.com/TechXTT/reelay/internal/downloader/qbittorrent"
	"github.com/TechXTT/reelay/internal/indexer"
	"github.com/TechXTT/reelay/internal/indexer/torznab"
	"github.com/TechXTT/reelay/internal/indexer/tpb"
)

// buildIndexers constructs one client per enabled indexer.
//
// This is the only place concrete indexer types are named; everything
// downstream sees indexer.Indexer.
func buildIndexers(cfg *config.Config, log *slog.Logger, clk clock.Clock) ([]indexer.Indexer, error) {
	var out []indexer.Indexer
	for _, ix := range cfg.EnabledIndexers() {
		switch ix.Type {
		case "piratebay":
			c, err := tpb.New(ix, tpb.Options{Clock: clk, Logger: log})
			if err != nil {
				return nil, err
			}
			out = append(out, c)
		case "torznab":
			c, err := torznab.New(ix, torznab.Options{Clock: clk})
			if err != nil {
				return nil, fmt.Errorf("indexer %q: %w", ix.Name, err)
			}
			out = append(out, c)
		default:
			// Config validation already rejects unknown types; reaching here
			// means the two lists drifted apart.
			return nil, fmt.Errorf("indexer %q has unsupported type %q", ix.Name, ix.Type)
		}
	}
	return out, nil
}

// buildDownloader constructs the configured download client.
//
// The only place a concrete client type is named. Everything downstream sees
// downloader.Downloader, which is what will let a Transmission implementation
// drop in for topology B without an engine change.
func buildDownloader(cfg *config.Config, log *slog.Logger) (downloader.Downloader, error) {
	switch cfg.Downloader.Type {
	case "qbittorrent":
		return qbittorrent.New(cfg.Downloader, qbittorrent.Options{Logger: log})
	default:
		return nil, fmt.Errorf("unsupported download client type %q", cfg.Downloader.Type)
	}
}

// ensureCategories creates Reelay's categories in the client if they are
// missing.
//
// Done at startup rather than at first grab: the category is the safety
// boundary, and discovering it does not exist at the moment we are trying to
// add a torrent means either failing the grab or — much worse — adding it
// uncategorised, where Reelay could never see it again and could not tell it
// apart from the operator's own torrents.
func ensureCategories(ctx context.Context, dl downloader.Downloader, cfg *config.Config, log *slog.Logger) error {
	type ensurer interface {
		EnsureCategory(ctx context.Context, name, savePath string) error
	}
	e, ok := dl.(ensurer)
	if !ok {
		return nil
	}
	pairs := []struct{ name, path string }{
		{cfg.Downloader.CategoryTV, cfg.Downloader.SavePathTV},
		{cfg.Downloader.CategoryMovies, cfg.Downloader.SavePathMovies},
	}
	for _, p := range pairs {
		if err := e.EnsureCategory(ctx, p.name, p.path); err != nil {
			return fmt.Errorf("ensure category %q: %w", p.name, err)
		}
		log.Debug("category ready", "category", p.name, "save_path", p.path)
	}
	return nil
}

// pathMapperFor builds the client-path translator from config.
func pathMapperFor(cfg *config.Config) *downloader.PathMapper {
	mappings := make([]downloader.Mapping, 0, len(cfg.Downloader.PathMappings))
	for _, m := range cfg.Downloader.PathMappings {
		mappings = append(mappings, downloader.Mapping{
			DownloaderPrefix: m.DownloaderPrefix,
			LocalPrefix:      m.LocalPrefix,
		})
	}
	return downloader.NewPathMapper(mappings)
}
