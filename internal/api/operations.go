package api

import (
	"context"
	"errors"
	"net/http"
	"strconv"
	"strings"

	"github.com/TechXTT/reelay/internal/downloader"
	"github.com/TechXTT/reelay/internal/engine"
	"github.com/TechXTT/reelay/internal/indexer"
	"github.com/TechXTT/reelay/internal/model"
	"github.com/TechXTT/reelay/internal/parser"
	"github.com/TechXTT/reelay/internal/scoring"
)

type episodePatchRequest struct {
	Monitored *bool `json:"monitored"`
}

func (s *Server) handleEpisodePatch(w http.ResponseWriter, r *http.Request, id int64) error {
	req, err := decodeJSON[episodePatchRequest](r)
	if err != nil || req.Monitored == nil {
		return BadRequest("monitored is required")
	}
	if *req.Monitored {
		err = s.store.Transitions().RequestSearchNow(r.Context(), model.SubjectEpisode, id, "episode monitored")
	} else {
		_, err = s.store.Transitions().Transition(r.Context(), model.SubjectEpisode, id,
			model.StateUnmonitored, "episode unmonitored", "API request")
	}
	if err != nil {
		return Conflict("episode monitoring state could not change").WithCause(err)
	}
	item, _ := s.store.Episodes().Get(r.Context(), id)
	return reply(w, r, http.StatusOK, item)
}

// forceSearch queues an immediate search for the {id} subject.
func (s *Server) forceSearch(subject model.SubjectType, noun string) handler {
	return withID(func(w http.ResponseWriter, r *http.Request, id int64) error {
		if s.engine == nil {
			return Unavailable("engine is unavailable")
		}
		if err := s.engine.ForceSearch(r.Context(), subject, id); err != nil {
			return Conflict("%s cannot be searched now", noun).WithCause(err)
		}
		return reply(w, r, http.StatusAccepted, map[string]any{"queued": true})
	})
}

func (s *Server) handleCandidates(w http.ResponseWriter, r *http.Request, id int64) error {
	values, err := s.store.Decisions().Candidates(r.Context(), model.SubjectEpisode, id)
	if err != nil {
		return err
	}
	releaseIDs := make([]int64, len(values))
	for i, value := range values {
		releaseIDs[i] = value.ReleaseID
	}
	releases, err := s.store.Releases().GetMany(r.Context(), releaseIDs)
	if err != nil {
		return err
	}
	out := make([]diagnosticCandidate, 0, len(values))
	for _, value := range values {
		if release, found := releases[value.ReleaseID]; found {
			out = append(out, diagnosticCandidate{value, release})
		}
	}
	return reply(w, r, http.StatusOK, map[string]any{"items": out})
}

type episodeGrabRequest struct {
	ReleaseID int64 `json:"release_id"`
}

func (s *Server) handleEpisodeGrab(w http.ResponseWriter, r *http.Request, id int64) error {
	req, err := decodeJSON[episodeGrabRequest](r)
	if err != nil || req.ReleaseID <= 0 {
		return BadRequest("release_id is required")
	}
	grab, err := s.engine.ManualGrab(r.Context(), model.SubjectEpisode, id, req.ReleaseID)
	var lowDisk *engine.LowDiskError
	if errors.As(err, &lowDisk) {
		return InsufficientStorage("%s", lowDisk.Detail).WithCause(err)
	}
	if err != nil {
		return Conflict("release could not be grabbed").WithCause(err)
	}
	return reply(w, r, http.StatusCreated, grab)
}

func (s *Server) handleQueue(w http.ResponseWriter, r *http.Request) error {
	values, err := s.store.Grabs().Active(r.Context())
	if err != nil {
		return err
	}
	paused := false
	if s.engine != nil {
		paused, err = s.engine.DownloadsPaused(r.Context())
		if err != nil {
			return err
		}
	}
	return reply(w, r, http.StatusOK, map[string]any{"items": values, "paused": paused})
}

// setQueuePaused pauses or resumes every download.
func (s *Server) setQueuePaused(paused bool) handler {
	return func(w http.ResponseWriter, r *http.Request) error {
		if s.engine == nil {
			return Unavailable("engine is unavailable")
		}
		count, err := s.engine.SetDownloadsPaused(r.Context(), paused)
		if err != nil {
			return Conflict("download pause state could not be changed").WithCause(err)
		}
		return reply(w, r, http.StatusOK, map[string]any{"paused": paused, "count": count})
	}
}

func (s *Server) handleQueueDelete(w http.ResponseWriter, r *http.Request, id int64) error {
	grab, err := s.store.Grabs().Get(r.Context(), id)
	if err != nil {
		return NotFound("grab %d not found", id)
	}
	deleteData, err := queryBool(r.URL.Query().Get("deleteData"), "deleteData", true)
	if err != nil {
		return err
	}
	blacklist, err := queryBool(r.URL.Query().Get("blacklist"), "blacklist", true)
	if err != nil {
		return err
	}
	if s.downloader == nil {
		return Unavailable("download client is unavailable")
	}
	var covered []model.Episode
	if grab.SubjectType == model.SubjectEpisode {
		covered, err = s.store.Episodes().ActiveByRelease(r.Context(), grab.ReleaseID)
		if err != nil {
			return err
		}
	}
	if err := s.downloader.Remove(r.Context(), grab.TorrentHash, deleteData); err != nil &&
		!errors.Is(err, downloader.ErrNotFound) {
		return Conflict("torrent could not be removed").WithCause(err)
	}
	if blacklist {
		s.blacklistGrab(r.Context(), grab, covered)
	}
	grab.State = model.GrabRemoved
	if err := s.store.Grabs().Update(r.Context(), grab); err != nil {
		return err
	}
	if err := s.returnToWanted(r.Context(), grab, covered); err != nil {
		return err
	}
	w.WriteHeader(http.StatusNoContent)
	return nil
}

// blacklistGrab best-effort blacklists the grabbed release for every subject it served.
func (s *Server) blacklistGrab(ctx context.Context, grab model.Grab, covered []model.Episode) {
	release, err := s.store.Releases().Get(ctx, grab.ReleaseID)
	if err != nil {
		return
	}
	if grab.SubjectType != model.SubjectEpisode {
		_ = s.store.Decisions().Blacklist(ctx, grab.SubjectType, grab.SubjectID, release.InfoHash, "removed from queue")
		return
	}
	for _, episode := range covered {
		_ = s.store.Decisions().Blacklist(ctx, model.SubjectEpisode, episode.ID, release.InfoHash, "removed from queue")
	}
}

// returnToWanted sends every subject the removed grab served back to the search queue.
func (s *Server) returnToWanted(ctx context.Context, grab model.Grab, covered []model.Episode) error {
	if grab.SubjectType != model.SubjectEpisode {
		if err := s.store.Transitions().RetryNow(ctx, grab.SubjectType, grab.SubjectID, "grab removed from queue"); err != nil {
			return Conflict("item cannot return to wanted").WithCause(err)
		}
		return nil
	}
	for _, episode := range covered {
		if err := s.store.Transitions().RetryNow(ctx, model.SubjectEpisode, episode.ID, "shared grab removed from queue"); err != nil {
			return Conflict("covered episode cannot return to wanted").WithCause(err)
		}
	}
	return nil
}

func (s *Server) handleHistory(w http.ResponseWriter, r *http.Request) error {
	page, _ := strconv.Atoi(r.URL.Query().Get("page"))
	if page < 1 {
		page = 1
	}
	values, err := s.store.Grabs().History(r.Context(), 50, (page-1)*50)
	if err != nil {
		return err
	}
	return reply(w, r, http.StatusOK, map[string]any{"items": values, "page": page})
}

func (s *Server) handleTrigger(w http.ResponseWriter, r *http.Request) error {
	if s.engine == nil {
		return Unavailable("engine is unavailable")
	}
	loop := r.PathValue("loop")
	if err := s.engine.Trigger(loop); err != nil {
		return BadRequest("%v", err)
	}
	return reply(w, r, http.StatusAccepted, map[string]any{"triggered": loop})
}

func (s *Server) handleSettings(w http.ResponseWriter, r *http.Request) error {
	indexers := make([]map[string]any, 0, len(s.cfg.Indexers))
	for _, item := range s.cfg.Indexers {
		indexers = append(indexers, map[string]any{"name": item.Name, "type": item.Type,
			"base_url": item.BaseURL, "enabled": item.Enabled, "rate_limit_per_second": item.RateLimitPerSecond})
	}
	return reply(w, r, http.StatusOK, map[string]any{
		"server": map[string]any{"bind": s.cfg.Server.Bind, "port": s.cfg.Server.Port,
			"auth_enabled": s.cfg.Server.AuthToken != ""},
		"indexers": indexers,
		"downloader": map[string]any{"type": s.cfg.Downloader.Type, "url": s.cfg.Downloader.URL,
			"username": s.cfg.Downloader.Username, "password": "[redacted]"},
		"library": s.cfg.Library, "schedules": s.cfg.Schedules, "runtime": s.cfg.Runtime,
		"recommendations": map[string]any{"enabled": s.cfg.Recommendations.Enabled,
			"refresh_interval": s.cfg.Recommendations.RefreshInterval.String(),
			"result_limit":     s.cfg.Recommendations.ResultLimit},
	})
}

func (s *Server) handleLiveSearch(w http.ResponseWriter, r *http.Request) error {
	term := strings.TrimSpace(r.URL.Query().Get("q"))
	if term == "" {
		return BadRequest("q is required")
	}
	typeName := r.URL.Query().Get("type")
	categories := []int{indexer.CatTVShows, indexer.CatTVShowsHD, indexer.CatVideoOther}
	if typeName == "movie" {
		categories = []int{indexer.CatMovies, indexer.CatMoviesDVDR, indexer.CatMoviesHD}
	}
	var releases []indexer.Release
	var failures []string
	for _, client := range s.indexers {
		values, err := client.Search(r.Context(), indexer.Query{Term: term, Categories: categories})
		if err != nil {
			failures = append(failures, client.Name()+": "+err.Error())
			continue
		}
		releases = append(releases, values...)
	}
	profile, err := s.store.Profiles().Default(r.Context())
	if err != nil {
		return err
	}
	result := scoring.Evaluate(scoring.Input{Releases: releases, Profile: profile,
		Weights: s.cfg.Scoring, Now: s.clock.Now()})
	return reply(w, r, http.StatusOK, map[string]any{"result": result,
		"failures": failures, "parsed_query": parser.Parse(term)})
}
