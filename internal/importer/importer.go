// Package importer lands completed downloads in the configured media roots.
package importer

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/TechXTT/reelay/internal/config"
	"github.com/TechXTT/reelay/internal/model"
	"github.com/TechXTT/reelay/internal/parser"
	"github.com/TechXTT/reelay/internal/store"
)

type Options struct {
	Store      *store.Store
	Config     *config.Config
	Logger     *slog.Logger
	HTTPClient *http.Client
	Link       func(string, string) error
}

type Service struct {
	store *store.Store
	cfg   *config.Config
	log   *slog.Logger
	http  *http.Client
	link  func(string, string) error
}

func New(opt Options) (*Service, error) {
	if opt.Store == nil || opt.Config == nil {
		return nil, errors.New("importer requires store and config")
	}
	if opt.Logger == nil {
		opt.Logger = slog.Default()
	}
	if opt.HTTPClient == nil {
		opt.HTTPClient = &http.Client{Timeout: 10 * time.Second}
	}
	if opt.Link == nil {
		opt.Link = os.Link
	}
	return &Service{store: opt.Store, cfg: opt.Config, log: opt.Logger,
		http: opt.HTTPClient, link: opt.Link}, nil
}

func (s *Service) ImportCompleted(ctx context.Context, grabID int64) error {
	grab, err := s.store.Grabs().Get(ctx, grabID)
	if err != nil {
		return err
	}
	if grab.State != model.GrabImporting && grab.State != model.GrabCompleted {
		return fmt.Errorf("grab %d is %s, not ready to import", grab.ID, grab.State)
	}
	release, err := s.store.Releases().Get(ctx, grab.ReleaseID)
	if err != nil {
		return err
	}
	parsed := parser.Parse(release.RawTitle)
	if release.ParsedJSON != "" {
		_ = json.Unmarshal([]byte(release.ParsedJSON), &parsed)
	}
	videos, err := collectVideos(grab.ContentPath, s.cfg.Library)
	if err != nil {
		return err
	}
	if len(videos) == 0 {
		return fmt.Errorf("no qualifying video files under %s", grab.ContentPath)
	}
	if grab.SubjectType == model.SubjectEpisode {
		return s.importEpisodeGrab(ctx, grab, release, parsed, videos)
	}

	lock, err := s.store.Locks().Acquire(ctx, grab.SubjectType, grab.SubjectID,
		"importer", 10*time.Minute)
	if err != nil {
		return err
	}
	defer func() { _ = lock.Release(context.WithoutCancel(ctx)) }()

	var primaryPath, quality string
	for _, source := range videos {
		dest, fileParsed, err := s.destination(ctx, grab, source, parsed)
		if err != nil {
			return err
		}
		method, replaced, skipped, err := s.land(source, dest, rootFor(s.cfg, grab.SubjectType))
		if err != nil {
			return err
		}
		if !skipped {
			info, err := os.Stat(dest)
			if err != nil {
				return err
			}
			if _, err := s.store.Imports().Create(ctx, model.ImportRecord{GrabID: grab.ID,
				SubjectType: grab.SubjectType, SubjectID: grab.SubjectID, SourcePath: source,
				DestPath: dest, Method: method, SizeBytes: info.Size(), ReplacedPath: replaced}); err != nil {
				return err
			}
			if err := s.carrySubtitles(source, dest, rootFor(s.cfg, grab.SubjectType)); err != nil {
				return err
			}
		}
		if primaryPath == "" || sourceMatchesSubject(ctx, s.store, grab, fileParsed) {
			primaryPath = dest
			quality = qualityLabel(fileParsed)
		}
	}
	if primaryPath == "" {
		return errors.New("import produced no destination path")
	}
	if err := s.store.Transitions().MarkImportedLocked(ctx, lock, primaryPath,
		quality, "media imported"); err != nil {
		return err
	}
	s.postWebhook(grab, primaryPath)
	return nil
}

type episodeImportResult struct {
	path    string
	quality string
}

func (s *Service) importEpisodeGrab(ctx context.Context, grab model.Grab,
	release model.StoredRelease, fallback parser.Parsed, videos []string) error {
	episodes, err := s.store.Episodes().ActiveByRelease(ctx, release.ID)
	if err != nil {
		return err
	}
	if len(episodes) == 0 {
		return errors.New("episode grab has no active covered episodes")
	}
	results := make(map[int64]episodeImportResult, len(episodes))
	for _, source := range videos {
		dest, fileParsed, err := s.destination(ctx, grab, source, fallback)
		if err != nil {
			return err
		}
		episode, ok := matchingEpisode(fileParsed, episodes)
		if !ok {
			// A pack may contain specials or unmonitored episodes. Downloading a
			// pack never grants permission to add those files to the library.
			continue
		}
		method, replaced, skipped, err := s.land(source, dest, rootFor(s.cfg, grab.SubjectType))
		if err != nil {
			return err
		}
		if !skipped {
			info, err := os.Stat(dest)
			if err != nil {
				return err
			}
			if _, err := s.store.Imports().Create(ctx, model.ImportRecord{GrabID: grab.ID,
				SubjectType: model.SubjectEpisode, SubjectID: episode.ID, SourcePath: source,
				DestPath: dest, Method: method, SizeBytes: info.Size(), ReplacedPath: replaced}); err != nil {
				return err
			}
			if err := s.carrySubtitles(source, dest, rootFor(s.cfg, grab.SubjectType)); err != nil {
				return err
			}
		}
		results[episode.ID] = episodeImportResult{path: dest, quality: qualityLabel(fileParsed)}
	}
	for _, episode := range episodes {
		if results[episode.ID].path == "" {
			return fmt.Errorf("download contains no matching file for S%02dE%02d",
				episode.Season, episode.Number)
		}
	}
	for _, episode := range episodes {
		result := results[episode.ID]
		if err := s.store.Transitions().MarkImported(ctx, model.SubjectEpisode,
			episode.ID, result.path, result.quality, "media imported from shared grab"); err != nil {
			return err
		}
	}
	primary := results[grab.SubjectID].path
	if primary == "" {
		primary = results[episodes[0].ID].path
	}
	s.postWebhook(grab, primary)
	return nil
}

func matchingEpisode(parsed parser.Parsed, episodes []model.Episode) (model.Episode, bool) {
	if parsed.Season > 0 && parsed.FirstEpisode() > 0 {
		for _, episode := range episodes {
			if parsed.CoversEpisode(episode.Season, episode.Number) {
				return episode, true
			}
		}
	}
	if len(episodes) == 1 {
		return episodes[0], true
	}
	return model.Episode{}, false
}

func rootFor(cfg *config.Config, subject model.SubjectType) string {
	if subject == model.SubjectMovie {
		return cfg.Library.MovieRoot
	}
	return cfg.Library.TVRoot
}

func qualityLabel(p parser.Parsed) string {
	parts := []string{p.Resolution, p.Source, p.VideoCodec}
	if p.Proper {
		parts = append(parts, "proper")
	}
	if p.Repack {
		parts = append(parts, "repack")
	}
	return strings.TrimSpace(strings.Join(parts, " "))
}

func sourceMatchesSubject(ctx context.Context, st *store.Store, grab model.Grab, parsed parser.Parsed) bool {
	if grab.SubjectType == model.SubjectMovie {
		return true
	}
	episode, err := st.Episodes().Get(ctx, grab.SubjectID)
	return err == nil && parsed.CoversEpisode(episode.Season, episode.Number)
}
