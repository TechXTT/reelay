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
	"path/filepath"
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

	movie, err := s.store.Movies().Get(ctx, grab.SubjectID)
	if err != nil {
		return err
	}
	var primaryPath, quality string
	for _, source := range videos {
		var fileParsed = parseSource(source, parsed)
		var values = templateValues(fileParsed, strings.ToLower(filepath.Ext(source)))

		values["Title"], values["Year"] = movie.Title, number(movie.Year, 4)
		dest, err := destination(source, s.cfg.Library.MovieRoot,
			s.cfg.Library.MovieFolderTemplate, s.cfg.Library.MovieFileTemplate,
			s.cfg.Library.MaxPathLength, values)
		if err != nil {
			return err
		}
		if err := s.importFile(ctx, grab, grab.SubjectID, source, dest); err != nil {
			return err
		}
		primaryPath = dest
		quality = qualityLabel(fileParsed)
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
	series, err := s.store.Series().Get(ctx, episodes[0].SeriesID)
	if err != nil {
		return err
	}
	results := make(map[int64]episodeImportResult, len(episodes))
	for _, source := range videos {
		var fileParsed = parseSource(source, fallback)
		var episode, ok = matchingEpisode(fileParsed, episodes)

		if !ok {
			// A pack may contain specials or unmonitored episodes. Downloading a
			// pack never grants permission to add those files to the library.
			continue
		}
		values := templateValues(fileParsed, strings.ToLower(filepath.Ext(source)))
		values["Title"], values["Year"] = series.Title, number(series.Year, 4)
		values["Season"], values["Episode"] = number(episode.Season, 2), number(episode.Number, 2)
		values["EpisodeTitle"], values["Absolute"] = episode.Title, number(episode.AbsoluteNumber, 3)
		fileTemplate := s.cfg.Library.TVFileTemplate
		if series.IsAnime && episode.AbsoluteNumber > 0 {
			fileTemplate = s.cfg.Library.AnimeFileTemplate
		}
		dest, err := destination(source, s.cfg.Library.TVRoot, s.cfg.Library.TVFolderTemplate,
			fileTemplate, s.cfg.Library.MaxPathLength, values)
		if err != nil {
			return err
		}
		if err := s.importFile(ctx, grab, episode.ID, source, dest); err != nil {
			return err
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

func (s *Service) importFile(ctx context.Context, grab model.Grab, subjectID int64, source, dest string) error {
	var root = rootFor(s.cfg, grab.SubjectType)
	var method, replaced, skipped, err = s.land(source, dest, root)

	if err != nil || skipped {
		return err
	}
	info, err := os.Stat(dest)
	if err != nil {
		return err
	}
	if _, err := s.store.Imports().Create(ctx, model.ImportRecord{GrabID: grab.ID,
		SubjectType: grab.SubjectType, SubjectID: subjectID, SourcePath: source,
		DestPath: dest, Method: method, SizeBytes: info.Size(), ReplacedPath: replaced}); err != nil {
		return err
	}
	return s.carrySubtitles(source, dest, root)
}
