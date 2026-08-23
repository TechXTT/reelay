package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"time"

	"github.com/TechXTT/reelay/internal/config"
	"github.com/TechXTT/reelay/internal/downloader"
	"github.com/TechXTT/reelay/internal/downloader/qbittorrent"
	"github.com/TechXTT/reelay/internal/indexer"
)

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

// runGrab implements --grab: hand one magnet to the download client and follow
// it to completion.
//
// The point of the flag is to exercise the whole handoff — add, key by the hash
// we computed ourselves, poll, detect completion or stall — without the engine,
// the database or the UI in the way.
func runGrab(ctx context.Context, cfg *config.Config, log *slog.Logger, magnet, category string) error {
	if category == "" {
		category = cfg.Downloader.CategoryTV
	}

	dl, err := buildDownloader(cfg, log)
	if err != nil {
		return err
	}
	if err := dl.Healthy(ctx); err != nil {
		return fmt.Errorf("download client is not usable: %w", err)
	}
	if err := ensureCategories(ctx, dl, cfg, log); err != nil {
		return err
	}

	savePath := cfg.Downloader.SavePathTV
	if category == cfg.Downloader.CategoryMovies {
		savePath = cfg.Downloader.SavePathMovies
	}

	fmt.Fprintf(os.Stdout, "adding to %s as category %q (save path %s)\n",
		cfg.Downloader.Type, category, savePath)

	hash, err := dl.Add(ctx, downloader.AddRequest{
		Magnet:   magnet,
		Category: category,
		SavePath: savePath,
		Paused:   cfg.Downloader.AddPaused,
	})
	if err != nil {
		return err
	}
	// The client's add endpoint returns nothing, so this hash came from the
	// magnet. If it were wrong, every poll below would report "not in client".
	fmt.Fprintf(os.Stdout, "added, tracking hash %s\n\n", hash)

	return followGrab(ctx, dl, hash,
		cfg.Downloader.StallTimeout.Duration, pathMapperFor(cfg))
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

// followGrab polls until the torrent completes, stalls or fails.
func followGrab(
	ctx context.Context,
	dl downloader.Downloader,
	hash string,
	stallTimeout time.Duration,
	mapper *downloader.PathMapper,
) error {
	const pollInterval = 3 * time.Second

	ticker := time.NewTicker(pollInterval)
	defer ticker.Stop()

	started := time.Now()
	var lastProgress float64
	progressedAt := started
	unseen := 0

	for {
		statuses, err := dl.Status(ctx, []string{hash})
		if err != nil {
			return err
		}

		if len(statuses) == 0 {
			// Either the client has not registered it yet, or our computed
			// hash does not match what it stored. A magnet takes a moment to
			// resolve, so allow a few cycles before calling it.
			unseen++
			if unseen > 5 {
				return fmt.Errorf(
					"the client has no torrent with hash %s after %s.\n"+
						"Either the magnet was rejected, or it carries a category other than ours "+
						"(Reelay only ever sees its own categories)", hash, time.Since(started).Round(time.Second))
			}
			fmt.Fprintf(os.Stdout, "  waiting for the client to register the torrent (%d/5)\n", unseen)
		} else {
			unseen = 0
			s := statuses[0]

			if s.Progress > lastProgress {
				lastProgress = s.Progress
				progressedAt = time.Now()
			}

			eta := "-"
			if s.ETA > 0 {
				eta = s.ETA.Round(time.Second).String()
			}
			fmt.Fprintf(os.Stdout, "  %-12s %5.1f%%  %s / %s  %d seeders  eta %s\n",
				s.State, s.Progress*100,
				indexer.HumanSize(s.DownloadedBytes), indexer.HumanSize(s.TotalBytes),
				s.Seeders, eta)

			switch {
			case s.Complete():
				fmt.Fprintf(os.Stdout, "\ncomplete after %s\n", time.Since(started).Round(time.Second))
				fmt.Fprintf(os.Stdout, "content path (as the client sees it): %s\n", s.ContentPath)
				if local := mapper.Local(s.ContentPath); local != s.ContentPath {
					fmt.Fprintf(os.Stdout, "content path (mapped for Reelay):    %s\n", local)
				}
				return nil

			case s.Failed():
				return fmt.Errorf("the client reports this torrent as failed: %s", s.ErrorMessage)
			}

			// A grab that never gets going is worse than one that fails: it
			// occupies a slot indefinitely. The engine will remove and
			// blacklist it; here we just report it.
			if lastProgress < 0.01 && time.Since(progressedAt) > stallTimeout {
				return fmt.Errorf(
					"stalled: no progress in %s (stall timeout is downloader.stall_timeout=%s).\n"+
						"The engine would remove this torrent, blacklist the release for this item, "+
						"and search again", time.Since(progressedAt).Round(time.Second), stallTimeout)
			}
		}

		select {
		case <-ctx.Done():
			fmt.Fprintln(os.Stdout, "\ninterrupted; the torrent is left in the client")
			return nil
		case <-ticker.C:
		}
	}
}

// grabCategoryFor resolves a human-friendly category argument.
func grabCategoryFor(cfg *config.Config, kind string) (string, error) {
	switch strings.ToLower(strings.TrimSpace(kind)) {
	case "", "tv", "series", "episode":
		return cfg.Downloader.CategoryTV, nil
	case "movie", "movies", "film":
		return cfg.Downloader.CategoryMovies, nil
	default:
		return "", errors.New(`--grab-type must be "tv" or "movie"`)
	}
}
