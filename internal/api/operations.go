package api

import (
	"errors"
	"net/http"
	"strconv"
	"strings"

	"github.com/TechXTT/reelay/internal/downloader"
	"github.com/TechXTT/reelay/internal/indexer"
	"github.com/TechXTT/reelay/internal/model"
	"github.com/TechXTT/reelay/internal/parser"
	"github.com/TechXTT/reelay/internal/scoring"
)

func (s *Server) handleEpisodePatch(w http.ResponseWriter, r *http.Request) error {
	id, err := pathID(r)
	if err != nil {
		return err
	}
	var req struct {
		Monitored *bool `json:"monitored"`
	}
	if err := decodeBody(r, &req); err != nil || req.Monitored == nil {
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
	writeJSON(w, s.logFor(r), http.StatusOK, item)
	return nil
}

func (s *Server) handleEpisodeSearch(w http.ResponseWriter, r *http.Request) error {
	id, err := pathID(r)
	if err != nil {
		return err
	}
	if s.engine == nil {
		return Unavailable("engine is unavailable")
	}
	if err := s.engine.ForceSearch(r.Context(), model.SubjectEpisode, id); err != nil {
		return Conflict("episode cannot be searched now").WithCause(err)
	}
	writeJSON(w, s.logFor(r), http.StatusAccepted, map[string]any{"queued": true})
	return nil
}

func (s *Server) handleCandidates(w http.ResponseWriter, r *http.Request) error {
	id, err := pathID(r)
	if err != nil {
		return err
	}
	values, err := s.store.Decisions().Candidates(r.Context(), model.SubjectEpisode, id)
	if err != nil {
		return err
	}
	type candidate struct {
		Evaluation model.CandidateEvaluation `json:"evaluation"`
		Release    model.StoredRelease       `json:"release"`
	}
	out := make([]candidate, 0, len(values))
	for _, value := range values {
		release, err := s.store.Releases().Get(r.Context(), value.ReleaseID)
		if err == nil {
			out = append(out, candidate{value, release})
		}
	}
	writeJSON(w, s.logFor(r), http.StatusOK, map[string]any{"items": out})
	return nil
}

func (s *Server) handleEpisodeGrab(w http.ResponseWriter, r *http.Request) error {
	id, err := pathID(r)
	if err != nil {
		return err
	}
	var req struct {
		ReleaseID int64 `json:"release_id"`
	}
	if err := decodeBody(r, &req); err != nil || req.ReleaseID <= 0 {
		return BadRequest("release_id is required")
	}
	grab, err := s.engine.ManualGrab(r.Context(), model.SubjectEpisode, id, req.ReleaseID)
	if err != nil {
		return Conflict("release could not be grabbed").WithCause(err)
	}
	writeJSON(w, s.logFor(r), http.StatusCreated, grab)
	return nil
}

func (s *Server) handleQueue(w http.ResponseWriter, r *http.Request) error {
	values, err := s.store.Grabs().Active(r.Context())
	if err != nil {
		return err
	}
	writeJSON(w, s.logFor(r), http.StatusOK, map[string]any{"items": values})
	return nil
}

func (s *Server) handleQueueDelete(w http.ResponseWriter, r *http.Request) error {
	id, err := pathID(r)
	if err != nil {
		return err
	}
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
	if err := s.downloader.Remove(r.Context(), grab.TorrentHash, deleteData); err != nil &&
		!errors.Is(err, downloader.ErrNotFound) {
		return Conflict("torrent could not be removed").WithCause(err)
	}
	if blacklist {
		release, releaseErr := s.store.Releases().Get(r.Context(), grab.ReleaseID)
		if releaseErr == nil {
			_ = s.store.Decisions().Blacklist(r.Context(), grab.SubjectType, grab.SubjectID,
				release.InfoHash, "removed from queue")
		}
	}
	grab.State = model.GrabRemoved
	if err := s.store.Grabs().Update(r.Context(), grab); err != nil {
		return err
	}
	if err := s.store.Transitions().RetryNow(r.Context(), grab.SubjectType, grab.SubjectID,
		"grab removed from queue"); err != nil {
		return Conflict("item cannot return to wanted").WithCause(err)
	}
	w.WriteHeader(http.StatusNoContent)
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
	writeJSON(w, s.logFor(r), http.StatusOK, map[string]any{"items": values, "page": page})
	return nil
}

func (s *Server) handleTrigger(w http.ResponseWriter, r *http.Request) error {
	if s.engine == nil {
		return Unavailable("engine is unavailable")
	}
	if err := s.engine.Trigger(r.PathValue("loop")); err != nil {
		return BadRequest("%v", err)
	}
	writeJSON(w, s.logFor(r), http.StatusAccepted, map[string]any{"triggered": r.PathValue("loop")})
	return nil
}

func (s *Server) handleSettings(w http.ResponseWriter, r *http.Request) error {
	indexers := make([]map[string]any, 0, len(s.cfg.Indexers))
	for _, item := range s.cfg.Indexers {
		indexers = append(indexers, map[string]any{"name": item.Name, "type": item.Type,
			"base_url": item.BaseURL, "enabled": item.Enabled, "rate_limit_per_second": item.RateLimitPerSecond})
	}
	writeJSON(w, s.logFor(r), http.StatusOK, map[string]any{
		"server": map[string]any{"bind": s.cfg.Server.Bind, "port": s.cfg.Server.Port,
			"auth_enabled": s.cfg.Server.AuthToken != ""},
		"indexers": indexers,
		"downloader": map[string]any{"type": s.cfg.Downloader.Type, "url": s.cfg.Downloader.URL,
			"username": s.cfg.Downloader.Username, "password": "[redacted]"},
		"library": s.cfg.Library, "schedules": s.cfg.Schedules, "runtime": s.cfg.Runtime,
	})
	return nil
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
	writeJSON(w, s.logFor(r), http.StatusOK, map[string]any{"result": result,
		"failures": failures, "parsed_query": parser.Parse(term)})
	return nil
}
