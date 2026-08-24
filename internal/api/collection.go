package api

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/TechXTT/reelay/internal/downloader"
	"github.com/TechXTT/reelay/internal/model"
	"github.com/TechXTT/reelay/internal/store"
)

type collectionTarget struct {
	root  string
	paths []string
	grabs []model.Grab
}

func queryBool(rValue, name string, fallback bool) (bool, error) {
	if strings.TrimSpace(rValue) == "" {
		return fallback, nil
	}
	value, err := strconv.ParseBool(rValue)
	if err != nil {
		return false, BadRequest("%s must be true or false", name)
	}
	return value, nil
}

func (s *Server) deleteMovieCollection(ctx context.Context, id int64, deleteFiles, deleteDownloads bool) error {
	movie, err := s.store.Movies().Get(ctx, id)
	if err != nil {
		return NotFound("movie %d not found", id)
	}
	lock, err := s.store.Locks().Acquire(ctx, model.SubjectMovie, id, "api-delete", 5*time.Minute)
	if errors.Is(err, store.ErrLocked) {
		return Conflict("movie is currently being processed; retry deletion shortly")
	}
	if err != nil {
		return err
	}
	defer func() { _ = lock.Release(context.WithoutCancel(ctx)) }()

	grabs, err := s.store.Grabs().BySubject(ctx, model.SubjectMovie, id)
	if err != nil {
		return err
	}
	target := collectionTarget{root: movie.RootFolder, grabs: grabs}
	if movie.ImportedPath != "" {
		target.paths = append(target.paths, movie.ImportedPath)
	}
	if err := s.prepareCollectionDelete(ctx, target, deleteFiles, deleteDownloads); err != nil {
		return err
	}
	return s.store.Movies().Delete(ctx, id)
}

func (s *Server) deleteSeriesCollection(ctx context.Context, id int64, deleteFiles, deleteDownloads bool) error {
	series, err := s.store.Series().Get(ctx, id)
	if err != nil {
		return NotFound("series %d not found", id)
	}
	episodes, err := s.store.Episodes().ListBySeries(ctx, id)
	if err != nil {
		return err
	}
	locks := make([]*store.ItemLock, 0, len(episodes))
	releaseLocks := func() {
		for _, lock := range locks {
			_ = lock.Release(context.WithoutCancel(ctx))
		}
	}
	for _, episode := range episodes {
		lock, lockErr := s.store.Locks().Acquire(ctx, model.SubjectEpisode, episode.ID,
			"api-delete-series", 5*time.Minute)
		if errors.Is(lockErr, store.ErrLocked) {
			releaseLocks()
			return Conflict("series has an episode currently being processed; retry deletion shortly")
		}
		if lockErr != nil {
			releaseLocks()
			return lockErr
		}
		locks = append(locks, lock)
	}
	defer releaseLocks()

	target := collectionTarget{root: series.RootFolder}
	for _, episode := range episodes {
		if episode.ImportedPath != "" {
			target.paths = append(target.paths, episode.ImportedPath)
		}
		grabs, err := s.store.Grabs().BySubject(ctx, model.SubjectEpisode, episode.ID)
		if err != nil {
			return err
		}
		target.grabs = append(target.grabs, grabs...)
	}
	if err := s.prepareCollectionDelete(ctx, target, deleteFiles, deleteDownloads); err != nil {
		return err
	}
	return s.store.Series().Delete(ctx, id)
}

func (s *Server) prepareCollectionDelete(ctx context.Context, target collectionTarget, deleteFiles, deleteDownloads bool) error {
	for _, grab := range target.grabs {
		if activeGrab(grab.State) && !deleteDownloads {
			return Conflict("collection has an active download; select download-data deletion to cancel it")
		}
	}

	paths := make([]string, 0, len(target.paths))
	if deleteFiles {
		for _, path := range target.paths {
			clean, err := managedPath(target.root, path)
			if err != nil {
				return Conflict("refusing to delete a library path outside its configured root").WithCause(err)
			}
			paths = append(paths, clean)
		}
	}

	if deleteDownloads {
		if s.downloader == nil {
			return Unavailable("download client is unavailable")
		}
		removed := make(map[string]bool)
		for _, grab := range target.grabs {
			hash := strings.ToLower(grab.TorrentHash)
			if !removed[hash] {
				if err := s.downloader.Remove(ctx, hash, true); err != nil &&
					!errors.Is(err, downloader.ErrNotFound) {
					return Conflict("download data could not be removed").WithCause(err)
				}
				removed[hash] = true
			}
			if grab.State != model.GrabRemoved {
				grab.State = model.GrabRemoved
				grab.LastError = "removed with collection"
				if err := s.store.Grabs().Update(ctx, grab); err != nil {
					return err
				}
			}
		}
	}

	for _, path := range paths {
		if err := deleteManagedFile(path, target.root, s.cfg.Library.SubtitleExtensions); err != nil {
			return Conflict("library file could not be deleted").WithCause(err)
		}
	}
	return nil
}

func activeGrab(state model.GrabState) bool {
	switch state {
	case model.GrabPending, model.GrabDownloading, model.GrabCompleted, model.GrabImporting:
		return true
	default:
		return false
	}
}

func managedPath(root, path string) (string, error) {
	root, err := filepath.Abs(filepath.Clean(root))
	if err != nil {
		return "", fmt.Errorf("resolve root: %w", err)
	}
	path, err = filepath.Abs(filepath.Clean(path))
	if err != nil {
		return "", fmt.Errorf("resolve path: %w", err)
	}
	rel, err := filepath.Rel(root, path)
	if err != nil {
		return "", fmt.Errorf("compare path to root: %w", err)
	}
	if rel == "." || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("path %q is outside root %q", path, root)
	}
	return path, nil
}

func deleteManagedFile(path, root string, subtitleExtensions []string) error {
	path, err := managedPath(root, path)
	if err != nil {
		return err
	}
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if info.IsDir() {
		return fmt.Errorf("refusing to delete directory %q as an imported file", path)
	}

	dir := filepath.Dir(path)
	base := strings.TrimSuffix(filepath.Base(path), filepath.Ext(path))
	allowed := make(map[string]bool, len(subtitleExtensions))
	for _, ext := range subtitleExtensions {
		allowed[strings.ToLower(ext)] = true
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return err
	}
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasPrefix(name, base+".") ||
			!allowed[strings.ToLower(filepath.Ext(name))] {
			continue
		}
		sidecar, err := managedPath(root, filepath.Join(dir, name))
		if err != nil {
			return err
		}
		if err := os.Remove(sidecar); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
	}
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	removeEmptyParents(dir, root)
	return nil
}

func removeEmptyParents(dir, root string) {
	root, err := filepath.Abs(filepath.Clean(root))
	if err != nil {
		return
	}
	for {
		dir, err = filepath.Abs(filepath.Clean(dir))
		if err != nil || dir == root {
			return
		}
		if _, err := managedPath(root, dir); err != nil || os.Remove(dir) != nil {
			return
		}
		dir = filepath.Dir(dir)
	}
}
