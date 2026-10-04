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
	"github.com/TechXTT/reelay/internal/indexer"
)

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
