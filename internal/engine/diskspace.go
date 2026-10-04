package engine

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/TechXTT/reelay/internal/model"
	"github.com/TechXTT/reelay/internal/store"
)

const bytesPerMiB = 1 << 20

// LowDiskError reports that a grab was held because a volume lacks space for
// the release plus the configured reserve, or because free space is unknown.
type LowDiskError struct{ Reason, Detail string }

func (e *LowDiskError) Error() string { return e.Detail }

func (e *Engine) libraryRoot(subject model.SubjectType) string {
	if subject == model.SubjectMovie {
		return e.cfg.Library.MovieRoot
	}
	return e.cfg.Library.TVRoot
}

// checkSpace verifies that the library root and the local view of the download
// save path each have room for sizeBytes plus the reserve. A zero sizeBytes
// checks the reserve alone. It returns nil when the release fits.
func (e *Engine) checkSpace(sizeBytes int64, savePath, libraryRoot string) *LowDiskError {
	return e.checkPaths(sizeBytes, e.spacePaths(savePath, libraryRoot))
}

// cycleSpaceCheck is the reserve-only check across both libraries and both
// download save paths, run once per search cycle before any indexer query.
func (e *Engine) cycleSpaceCheck() *LowDiskError {
	paths := e.spacePaths(e.cfg.Downloader.SavePathTV, e.cfg.Library.TVRoot)
	paths = append(paths, e.spacePaths(e.cfg.Downloader.SavePathMovies, e.cfg.Library.MovieRoot)...)
	return e.checkPaths(0, paths)
}

func (e *Engine) spacePaths(savePath, libraryRoot string) []string {
	paths := make([]string, 0, 2)
	if libraryRoot != "" {
		paths = append(paths, libraryRoot)
	}
	if local, ok := e.localDownloadPath(savePath); ok {
		paths = append(paths, local)
	}
	return paths
}

// localDownloadPath translates the client-side save path to this host. Without
// a mapping the client path is only usable when it exists locally; otherwise
// the check is skipped, with one warning per process.
func (e *Engine) localDownloadPath(savePath string) (string, bool) {
	local := e.pathMapper.Local(savePath)
	if e.pathMapper == nil || local == savePath {
		if _, err := os.Stat(local); err != nil {
			e.spaceWarnOnce.Do(func() {
				e.log.Warn("download path is not visible on this host; skipping its free-space check (configure downloader.path_mappings)",
					"save_path", savePath)
			})
			return "", false
		}
		return local, true
	}
	// The client creates the save path on first use, so measure the nearest
	// existing ancestor. If the mount itself is missing, that ancestor may be
	// the host's own disk; the library-root check and setup checks catch that.
	for {
		if _, err := os.Stat(local); err == nil {
			return local, true
		}
		parent := filepath.Dir(local)
		if parent == local {
			return local, true
		}
		local = parent
	}
}

func (e *Engine) checkPaths(sizeBytes int64, paths []string) *LowDiskError {
	reserve := uint64(e.cfg.Downloader.MinFreeSpaceMB) * bytesPerMiB
	size := uint64(max(sizeBytes, 0))
	for _, path := range paths {
		free, err := e.freeSpace(path)
		if err != nil {
			return &LowDiskError{Reason: "disk_space_unknown",
				Detail: fmt.Sprintf("cannot read free space on %s: %v", path, err)}
		}
		if free < size+reserve {
			return &LowDiskError{Reason: "low_disk_space",
				Detail: fmt.Sprintf("need %s + %s reserve on %s, %s free", gib(size), gib(reserve), path, gib(free))}
		}
	}
	return nil
}

func gib(n uint64) string { return fmt.Sprintf("%.1f GiB", float64(n)/(1<<30)) }

// holdForSpace returns the searching item to its waiting state for one search
// interval without counting a failed attempt, so low disk space never gives up
// on the item.
func (e *Engine) holdForSpace(ctx context.Context, lock *store.ItemLock, subject model.SubjectType, id int64, lowDisk *LowDiskError) error {
	next := e.clock.Now().UTC().Add(e.cfg.Schedules.SearchInterval.Duration)
	transition, err := e.store.Transitions().HoldLocked(ctx, lock, next, lowDisk.Reason, lowDisk.Detail)
	if err != nil {
		return err
	}
	e.log.Warn("grab held: disk space", "subject", subject, "id", id,
		"reason", lowDisk.Reason, "detail", lowDisk.Detail, "retry_at", next.Format(time.RFC3339))
	e.publish("state_transition", subject, id, map[string]any{"state": transition.To, "reason": lowDisk.Reason})
	return nil
}
