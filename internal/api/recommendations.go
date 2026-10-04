package api

import (
	"errors"
	"net/http"
	"strconv"
	"strings"

	"github.com/TechXTT/reelay/internal/engine"
	"github.com/TechXTT/reelay/internal/metadata"
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
		if s.engine != nil {
			userIDs := make([]string, 0, len(req.Users))
			for _, user := range req.Users {
				userIDs = append(userIDs, user.UserID)
			}
			s.engine.Events().Publish(engine.Event{Type: "requests_updated", At: s.clock.Now(), Data: map[string]any{
				"server_id": req.ServerID, "user_ids": userIDs,
			}})
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

func (s *Server) handleRecommendationPreview(w http.ResponseWriter, r *http.Request) error {
	var id, err = pathID(r)
	var provider, supported = s.discovery.(metadata.PreviewProvider)
	var response struct {
		Title          string                  `json:"title"`
		Year           int                     `json:"year"`
		MediaType      string                  `json:"media_type"`
		Overview       string                  `json:"overview"`
		PosterURL      string                  `json:"poster_url"`
		Genres         []string                `json:"genres"`
		People         []string                `json:"people"`
		RuntimeMinutes int                     `json:"runtime_minutes"`
		VoteAverage    float64                 `json:"vote_average"`
		VoteCount      int                     `json:"vote_count"`
		Videos         []metadata.PreviewVideo `json:"videos"`
		Seasons        []int                   `json:"seasons"`
	}

	if err != nil {
		return err
	}
	rec, err := s.store.Recommendations().Get(r.Context(), id)
	if errors.Is(err, store.ErrNotFound) {
		return NotFound("recommendation %d not found", id)
	}
	if err != nil {
		return err
	}
	if !supported {
		return Unavailable("recommendation previews are unavailable")
	}
	detail, err := provider.DiscoveryPreview(r.Context(), rec.MediaType, rec.TMDBID)
	if err != nil {
		return Unavailable("preview could not be loaded from TMDB").WithCause(err)
	}
	response.Title, response.Year, response.MediaType = detail.Title, detail.Year, rec.MediaType
	response.Overview, response.PosterURL = detail.Overview, detail.PosterURL
	response.Genres, response.People, response.RuntimeMinutes = detail.Genres, detail.People, detail.RuntimeMinutes
	response.VoteAverage, response.VoteCount, response.Videos = detail.VoteAverage, detail.VoteCount, detail.Videos
	response.Seasons = detail.Seasons
	writeJSON(w, s.logFor(r), http.StatusOK, response)
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
		ActionID    string `json:"action_id"`
		Action      string `json:"action"`
		Rating      int    `json:"rating"`
		MonitorMode string `json:"monitor_mode"`
	}
	if err := decodeBody(r, &req); err != nil {
		return err
	}
	if strings.TrimSpace(req.ActionID) == "" {
		return BadRequest("action_id is required")
	}
	if req.Action != "request" && req.Action != "dismiss" && req.Action != "rate" && req.Action != "undo" {
		return BadRequest("action must be request, dismiss, rate, or undo")
	}
	if req.Action != "request" && req.MonitorMode != "" {
		return BadRequest("monitor_mode is only valid for request actions")
	}
	if req.Action == "rate" && (req.Rating < 1 || req.Rating > 5) {
		return BadRequest("rating must be from 1 to 5")
	}
	if req.MonitorMode != "" && req.MonitorMode != string(model.MonitorLatestSeason) && req.MonitorMode != string(model.MonitorAll) && req.MonitorMode != string(model.MonitorFutureOnly) {
		return BadRequest("monitor_mode must be latest_season, all, or future_only")
	}
	rec, err := s.store.Recommendations().Get(r.Context(), id)
	if err != nil {
		return NotFound("recommendation %d not found", id)
	}
	if rec.MediaType != "series" && req.MonitorMode != "" {
		return BadRequest("monitor_mode is only valid for series requests")
	}
	var subject any
	metadataChanged := false
	if req.Action == "request" {
		recorded, checkErr := s.store.Requests().ActionRecorded(r.Context(), req.ActionID, rec.ServerID, rec.UserID, rec.MediaType, rec.TMDBID)
		if checkErr != nil {
			if errors.Is(checkErr, store.ErrActionIDConflict) {
				return Conflict("action_id was already used for another request").WithCause(checkErr)
			}
			return checkErr
		}
		requestedMonitorMode := req.MonitorMode
		if rec.MediaType == "series" && requestedMonitorMode == "" {
			requestedMonitorMode = string(model.MonitorFutureOnly)
		}
		monitorMode := requestedMonitorMode
		if recorded {
			monitorMode = ""
			requestedMonitorMode = ""
		}
		subject, metadataChanged, err = s.requestRecommendation(r, rec, monitorMode, !recorded)
		if err != nil {
			return Conflict("recommendation could not be requested").WithCause(err)
		}
		request := model.MediaRequest{ServerID: rec.ServerID, UserID: rec.UserID, MediaType: rec.MediaType, TMDBID: rec.TMDBID,
			Title: rec.Title, Year: rec.Year, RequestedAt: s.clock.Now(), SubjectType: rec.MediaType}
		if rec.MediaType == "series" {
			request.MonitorMode = requestedMonitorMode
		}
		switch value := subject.(type) {
		case model.Movie:
			request.Title, request.Year, request.SubjectID = value.Title, value.Year, value.ID
		case model.Series:
			request.Title, request.Year, request.SubjectID = value.Title, value.Year, value.ID
		default:
			return Conflict("recommendation returned an unsupported subject")
		}
		if _, err = s.store.Requests().Create(r.Context(), request); err != nil {
			return Conflict("recommendation could not be tracked").WithCause(err)
		}
	}
	inserted := true
	if req.Action == "undo" {
		err = s.store.Recommendations().UndoDismissal(r.Context(), id)
	} else if req.Action == "rate" {
		_, err = s.store.Recommendations().RecordRating(r.Context(), id, req.ActionID, req.Rating)
	} else {
		_, inserted, err = s.store.Recommendations().RecordAction(r.Context(), id, req.ActionID, req.Action)
	}
	if err != nil {
		return err
	}
	if s.engine != nil {
		if req.Action == "request" {
			if metadataChanged {
				_ = s.engine.MetadataOnce(r.Context())
			}
			_ = s.engine.Trigger("search")
			s.engine.Events().Publish(engine.Event{Type: "requests_updated", At: s.clock.Now(), Data: map[string]any{
				"server_id": rec.ServerID, "user_id": rec.UserID,
			}})
		} else if req.Action == "rate" {
			_ = s.engine.Trigger("recommendations")
		}
	}
	writeJSON(w, s.logFor(r), http.StatusOK, map[string]any{"recommendation": rec, "subject": subject, "created": inserted, "rating": req.Rating})
	return nil
}

func (s *Server) requestRecommendation(r *http.Request, rec model.Recommendation, monitorMode string, createIfMissing bool) (any, bool, error) {
	profile, err := s.store.Profiles().Default(r.Context())
	if err != nil {
		return nil, false, err
	}
	if rec.MediaType == "movie" {
		if existing, err := s.store.Movies().GetByTMDBID(r.Context(), rec.TMDBID); err == nil {
			return existing, false, nil
		} else if !errors.Is(err, store.ErrNotFound) {
			return nil, false, err
		}
		if !createIfMissing {
			return nil, false, store.ErrNotFound
		}
		detail, err := s.movies.MovieDetails(r.Context(), rec.TMDBID)
		if err != nil {
			return nil, false, err
		}
		created, err := s.store.Movies().Create(r.Context(), model.Movie{Title: detail.Title, Year: detail.Year, TMDBID: detail.TMDBID, IMDBID: detail.IMDBID, RuntimeMinutes: detail.RuntimeMinutes, ProfileID: profile.ID, RootFolder: s.cfg.Library.MovieRoot, State: model.StateWanted}, "requested from Jellyfin recommendation")
		return created, false, err
	}
	if existing, err := s.store.Series().GetByTMDBID(r.Context(), rec.TMDBID); err == nil {
		changed := false
		widenLatestSeason := monitorMode == string(model.MonitorLatestSeason) && (existing.MonitorMode == model.MonitorFutureOnly || existing.MonitorMode == model.MonitorNone)
		widenAll := monitorMode == string(model.MonitorAll) && existing.MonitorMode != model.MonitorAll
		if existing.Status == model.SeriesFollowing && (widenLatestSeason || widenAll) {
			existing.MonitorMode = model.MonitorMode(monitorMode)
			existing, err = s.store.Series().Update(r.Context(), existing)
			changed = err == nil
		}
		return existing, changed, err
	} else if !errors.Is(err, store.ErrNotFound) {
		return nil, false, err
	}
	if !createIfMissing {
		return nil, false, store.ErrNotFound
	}
	if s.discovery == nil || s.externalSeries == nil {
		return nil, false, errors.New("series recommendation providers are unavailable")
	}
	detail, err := s.discovery.DiscoveryDetails(r.Context(), rec.MediaType, rec.TMDBID)
	if err != nil {
		return nil, false, err
	}
	series, err := s.externalSeries.LookupSeries(r.Context(), detail.TVDBID, detail.IMDBID)
	if err != nil {
		return nil, false, err
	}
	mode := model.MonitorFutureOnly
	if monitorMode != "" {
		mode = model.MonitorMode(monitorMode)
	}
	created, err := s.store.Series().Create(r.Context(), model.Series{Title: series.Title, Year: series.Year, TVmazeID: series.TVmazeID, TMDBID: rec.TMDBID, IMDBID: series.IMDBID, Aliases: series.Aliases, MonitorMode: mode, Status: model.SeriesFollowing, ProfileID: profile.ID, RootFolder: s.cfg.Library.TVRoot, RuntimeMinutes: series.RuntimeMinutes})
	return created, err == nil, err
}
