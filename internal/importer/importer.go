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

type importVideo struct {
	source string
	moved  model.ImportRecord
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
	sources, err := collectVideos(grab.ContentPath, s.cfg.Library)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	missingContent := err
	moves, err := s.store.Imports().MovedByGrab(ctx, grab.ID)
	if err != nil {
		return err
	}
	var videos = make([]importVideo, 0, len(sources)+len(moves))
	var seen = make(map[string]bool, len(sources)+len(moves))

	for _, source := range sources {
		videos = append(videos, importVideo{source: source})
		seen[source] = true
	}
	for _, move := range moves {
		if !seen[move.SourcePath] {
			videos = append(videos, importVideo{source: move.SourcePath, moved: move})
			seen[move.SourcePath] = true
		}
	}
	if len(videos) == 0 {
		if missingContent != nil {
			return missingContent
		}
		return fmt.Errorf("no qualifying video files under %s", grab.ContentPath)
	}
	if grab.SubjectType == model.SubjectEpisode {
		return s.importEpisodeGrab(ctx, grab, release, parsed, videos)
	}

	return s.importMovieGrab(ctx, grab, parsed, videos)
}

func (s *Service) importMovieGrab(ctx context.Context, grab model.Grab, fallback parser.Parsed, videos []importVideo) error {
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
	for _, video := range videos {
		var source = video.source
		var fileParsed = parseSource(source, fallback)
		var values = templateValues(fileParsed, strings.ToLower(filepath.Ext(source)))

		values["Title"], values["Year"] = movie.Title, number(movie.Year, 4)
		dest, err := destination(source, s.cfg.Library.MovieRoot,
			s.cfg.Library.MovieFolderTemplate, s.cfg.Library.MovieFileTemplate,
			s.cfg.Library.MaxPathLength, values)
		if err != nil {
			return err
		}
		dest, err = s.finishVideo(ctx, grab, grab.SubjectID, video, dest)
		if err != nil {
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

func (s *Service) importEpisodeGrab(ctx context.Context, grab model.Grab,
	release model.StoredRelease, fallback parser.Parsed, videos []importVideo) error {
	type episodeImportResult struct {
		path    string
		quality string
		err     error
	}
	var failures []error
	var primary string

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
	for _, video := range videos {
		var source = video.source
		var fileParsed = parseSource(source, fallback)
		var episode, ok = matchingEpisode(fileParsed, episodes, !fallback.IsSeasonPack && len(fallback.Episodes) <= 1)

		if !ok {
			// A pack may contain specials or unmonitored episodes. Downloading a
			// pack never grants permission to add those files to the library.
			continue
		}
		if video.moved.DestPath != "" && video.moved.SubjectID != episode.ID {
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
		if err == nil {
			dest, err = s.finishVideo(ctx, grab, episode.ID, video, dest)
		}
		if err != nil {
			results[episode.ID] = episodeImportResult{err: err}
			continue
		}
		if results[episode.ID].err == nil {
			results[episode.ID] = episodeImportResult{path: dest, quality: qualityLabel(fileParsed)}
		}
	}
	for _, episode := range episodes {
		result := results[episode.ID]
		switch {
		case result.err != nil:
			failures = append(failures, fmt.Errorf("S%02dE%02d: %w", episode.Season, episode.Number, result.err))
		case result.path == "":
			failures = append(failures, fmt.Errorf("download contains no matching file for S%02dE%02d", episode.Season, episode.Number))
		default:
			if err := s.store.Transitions().MarkImported(ctx, model.SubjectEpisode,
				episode.ID, result.path, result.quality, "media imported from shared grab"); err != nil {
				failures = append(failures, err)
			} else if primary == "" || episode.ID == grab.SubjectID {
				primary = result.path
			}
		}
	}
	if primary != "" {
		s.postWebhook(grab, primary)
	}
	return errors.Join(failures...)
}

func matchingEpisode(parsed parser.Parsed, episodes []model.Episode, allowSingleFallback bool) (model.Episode, bool) {
	if parsed.FirstEpisode() > 0 {
		for _, episode := range episodes {
			if parsed.CoversEpisode(episode.Season, episode.Number) {
				return episode, true
			}
		}
		return model.Episode{}, false
	}
	if parsed.AbsoluteEp > 0 {
		for _, episode := range episodes {
			if episode.AbsoluteNumber == parsed.AbsoluteEp {
				return episode, true
			}
		}
		return model.Episode{}, false
	}
	if allowSingleFallback && len(episodes) == 1 {
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

func (s *Service) finishVideo(ctx context.Context, grab model.Grab, subjectID int64, video importVideo, dest string) (string, error) {
	if video.moved.DestPath == "" {
		return dest, s.importFile(ctx, grab, subjectID, video.source, dest)
	}
	var root = rootFor(s.cfg, grab.SubjectType)

	dest = video.moved.DestPath
	if video.moved.SubjectID != subjectID {
		return "", fmt.Errorf("recorded move for %s belongs to a different subject", video.source)
	}
	if err := ensureInside(root, dest); err != nil {
		return "", err
	}
	info, err := os.Lstat(dest)
	if err != nil {
		return "", fmt.Errorf("inspect recorded move %s: %w", dest, err)
	}
	if !info.Mode().IsRegular() || info.Size() != video.moved.SizeBytes {
		return "", fmt.Errorf("recorded move has changed: %s", dest)
	}
	return dest, s.carrySubtitles(video.source, dest, root)
}

func (s *Service) importFile(ctx context.Context, grab model.Grab, subjectID int64, source, dest string) error {
	var root = rootFor(s.cfg, grab.SubjectType)
	var method, replaced, skipped, err = s.land(source, dest, root)

	if err != nil {
		return err
	}
	if skipped {
		return s.carrySubtitles(source, dest, root)
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
