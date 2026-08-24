package api

import (
	"errors"
	"net/http"
	"strconv"
	"strings"

	"github.com/TechXTT/reelay/internal/model"
	"github.com/TechXTT/reelay/internal/store"
)

type jellyfinSyncRequest struct {
	ServerID  string               `json:"server_id"`
	SyncToken string               `json:"sync_token"`
	Complete  bool                 `json:"complete"`
	Users     []model.JellyfinUser `json:"users"`
	Items     []model.JellyfinItem `json:"items"`
}

func (s *Server) handleJellyfinSync(w http.ResponseWriter, r *http.Request) error {
	var req jellyfinSyncRequest
	if err := decodeBody(r, &req); err != nil {
		return err
	}
	if len(req.Users) > 100 || len(req.Items) > 500 {
		return BadRequest("sync batch exceeds 100 users or 500 items")
	}
	if strings.TrimSpace(req.ServerID) == "" || strings.TrimSpace(req.SyncToken) == "" {
		return BadRequest("server_id and sync_token are required")
	}
	for i := range req.Users {
		if req.Users[i].ServerID != req.ServerID {
			return BadRequest("user at index %d belongs to a different server", i)
		}
		if req.Users[i].LastSynced.IsZero() {
			req.Users[i].LastSynced = s.clock.Now()
		}
		if err := s.store.Recommendations().UpsertUser(r.Context(), req.Users[i]); err != nil {
			return BadRequest("invalid user at index %d", i).WithCause(err)
		}
	}
	for i := range req.Items {
		if req.Items[i].ServerID != req.ServerID {
			return BadRequest("item at index %d belongs to a different server", i)
		}
	}
	if err := s.store.Recommendations().UpsertItems(r.Context(), req.Items, req.SyncToken); err != nil {
		return BadRequest("invalid item batch").WithCause(err)
	}
	removed := 0
	if req.Complete {
		var err error
		removed, err = s.store.Recommendations().CompleteSync(r.Context(), req.ServerID, req.SyncToken)
		if err != nil {
			return BadRequest("invalid sync completion").WithCause(err)
		}
	}
	writeJSON(w, s.logFor(r), http.StatusOK, map[string]any{"users": len(req.Users), "items": len(req.Items), "removed": removed})
	return nil
}

func (s *Server) handleJellyfinEvents(w http.ResponseWriter, r *http.Request) error {
	var req struct {
		Events []model.JellyfinActivity `json:"events"`
	}
	if err := decodeBody(r, &req); err != nil {
		return err
	}
	if len(req.Events) == 0 || len(req.Events) > 500 {
		return BadRequest("events must contain between 1 and 500 entries")
	}
	inserted, err := s.store.Recommendations().AddActivities(r.Context(), req.Events)
	if err != nil {
		return BadRequest("invalid activity batch").WithCause(err)
	}
	writeJSON(w, s.logFor(r), http.StatusOK, map[string]any{"accepted": inserted, "duplicates": len(req.Events) - inserted})
	return nil
}

func (s *Server) handleJellyfinUsers(w http.ResponseWriter, r *http.Request) error {
	values, err := s.store.Recommendations().Users(r.Context())
	if err != nil {
		return err
	}
	writeJSON(w, s.logFor(r), http.StatusOK, map[string]any{"items": values})
	return nil
}

func (s *Server) handleRecommendations(w http.ResponseWriter, r *http.Request) error {
	serverID, userID := strings.TrimSpace(r.URL.Query().Get("server_id")), strings.TrimSpace(r.URL.Query().Get("user_id"))
	if serverID == "" || userID == "" {
		return BadRequest("server_id and user_id are required")
	}
	mediaType := r.URL.Query().Get("media_type")
	if mediaType != "" && mediaType != "movie" && mediaType != "series" {
		return BadRequest("media_type must be movie or series")
	}
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	offset, _ := strconv.Atoi(r.URL.Query().Get("offset"))
	values, err := s.store.Recommendations().List(r.Context(), serverID, userID, mediaType, r.URL.Query().Get("status"), limit, offset)
	if err != nil {
		return err
	}
	writeJSON(w, s.logFor(r), http.StatusOK, map[string]any{"items": values, "limit": limit, "offset": offset})
	return nil
}

func (s *Server) handleRecommendationGenerate(w http.ResponseWriter, r *http.Request) error {
	if s.recommendations == nil || !s.cfg.Recommendations.Enabled {
		return Unavailable("recommendations are disabled")
	}
	var req struct {
		ServerID  string `json:"server_id"`
		UserID    string `json:"user_id"`
		MediaType string `json:"media_type"`
	}
	if err := decodeBody(r, &req); err != nil {
		return err
	}
	if req.ServerID == "" || req.UserID == "" || (req.MediaType != "movie" && req.MediaType != "series") {
		return BadRequest("server_id, user_id, and a valid media_type are required")
	}
	if err := s.recommendations.Generate(r.Context(), req.ServerID, req.UserID, req.MediaType); err != nil {
		return Unavailable("recommendations could not be generated").WithCause(err)
	}
	writeJSON(w, s.logFor(r), http.StatusOK, map[string]any{"generated": true})
	return nil
}

func (s *Server) handleRecommendationAction(w http.ResponseWriter, r *http.Request) error {
	id, err := pathID(r)
	if err != nil {
		return err
	}
	var req struct {
		ActionID string `json:"action_id"`
		Action   string `json:"action"`
	}
	if err := decodeBody(r, &req); err != nil {
		return err
	}
	if strings.TrimSpace(req.ActionID) == "" {
		return BadRequest("action_id is required")
	}
	if req.Action != "request" && req.Action != "dismiss" {
		return BadRequest("action must be request or dismiss")
	}
	rec, err := s.store.Recommendations().Get(r.Context(), id)
	if err != nil {
		return NotFound("recommendation %d not found", id)
	}
	var subject any
	if req.Action == "request" {
		subject, err = s.requestRecommendation(r, rec)
		if err != nil {
			return Conflict("recommendation could not be requested").WithCause(err)
		}
	}
	_, inserted, err := s.store.Recommendations().RecordAction(r.Context(), id, req.ActionID, req.Action)
	if err != nil {
		return err
	}
	if s.engine != nil && req.Action == "request" {
		_ = s.engine.Trigger("search")
	}
	writeJSON(w, s.logFor(r), http.StatusOK, map[string]any{"recommendation": rec, "subject": subject, "created": inserted})
	return nil
}

func (s *Server) requestRecommendation(r *http.Request, rec model.Recommendation) (any, error) {
	profile, err := s.store.Profiles().Default(r.Context())
	if err != nil {
		return nil, err
	}
	if rec.MediaType == "movie" {
		if existing, err := s.store.Movies().GetByTMDBID(r.Context(), rec.TMDBID); err == nil {
			return existing, nil
		} else if !errors.Is(err, store.ErrNotFound) {
			return nil, err
		}
		detail, err := s.movies.MovieDetails(r.Context(), rec.TMDBID)
		if err != nil {
			return nil, err
		}
		return s.store.Movies().Create(r.Context(), model.Movie{Title: detail.Title, Year: detail.Year, TMDBID: detail.TMDBID, IMDBID: detail.IMDBID, RuntimeMinutes: detail.RuntimeMinutes, ProfileID: profile.ID, RootFolder: s.cfg.Library.MovieRoot, State: model.StateWanted}, "requested from Jellyfin recommendation")
	}
	if existing, err := s.store.Series().GetByTMDBID(r.Context(), rec.TMDBID); err == nil {
		return existing, nil
	} else if !errors.Is(err, store.ErrNotFound) {
		return nil, err
	}
	if s.discovery == nil || s.externalSeries == nil {
		return nil, errors.New("series recommendation providers are unavailable")
	}
	detail, err := s.discovery.DiscoveryDetails(r.Context(), rec.MediaType, rec.TMDBID)
	if err != nil {
		return nil, err
	}
	series, err := s.externalSeries.LookupSeries(r.Context(), detail.TVDBID, detail.IMDBID)
	if err != nil {
		return nil, err
	}
	created, err := s.store.Series().Create(r.Context(), model.Series{Title: series.Title, Year: series.Year, TVmazeID: series.TVmazeID, TMDBID: rec.TMDBID, IMDBID: series.IMDBID, Aliases: series.Aliases, MonitorMode: model.MonitorFutureOnly, Status: model.SeriesFollowing, ProfileID: profile.ID, RootFolder: s.cfg.Library.TVRoot, RuntimeMinutes: series.RuntimeMinutes})
	if err == nil && s.engine != nil {
		_ = s.engine.MetadataOnce(r.Context())
	}
	return created, err
}
