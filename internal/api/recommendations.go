package api

import (
	"context"
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

type jellyfinEventsRequest struct {
	Events []model.JellyfinActivity `json:"events"`
}

func (s *Server) handleJellyfinSync(w http.ResponseWriter, r *http.Request) error {
	req, err := decodeJSON[jellyfinSyncRequest](r)
	if err != nil {
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
	return reply(w, r, http.StatusOK, map[string]any{"users": len(req.Users), "items": len(req.Items), "removed": removed})
}

func (s *Server) handleJellyfinEvents(w http.ResponseWriter, r *http.Request) error {
	req, err := decodeJSON[jellyfinEventsRequest](r)
	if err != nil {
		return err
	}
	if len(req.Events) == 0 || len(req.Events) > 500 {
		return BadRequest("events must contain between 1 and 500 entries")
	}
	inserted, err := s.store.Recommendations().AddActivities(r.Context(), req.Events)
	if err != nil {
		return BadRequest("invalid activity batch").WithCause(err)
	}
	return reply(w, r, http.StatusOK, map[string]any{"accepted": inserted, "duplicates": len(req.Events) - inserted})
}

func (s *Server) handleRecommendations(w http.ResponseWriter, r *http.Request) error {
	serverID, userID, err := userScope(r)
	if err != nil {
		return err
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
	return reply(w, r, http.StatusOK, map[string]any{"items": values, "limit": limit, "offset": offset})
}

type recommendationPreviewResponse struct {
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

func (s *Server) handleRecommendationPreview(w http.ResponseWriter, r *http.Request, id int64) error {
	rec, err := s.store.Recommendations().Get(r.Context(), id)
	if errors.Is(err, store.ErrNotFound) {
		return NotFound("recommendation %d not found", id)
	}
	if err != nil {
		return err
	}
	provider, supported := s.discovery.(metadata.PreviewProvider)
	if !supported {
		return Unavailable("recommendation previews are unavailable")
	}
	detail, err := provider.DiscoveryPreview(r.Context(), rec.MediaType, rec.TMDBID)
	if err != nil {
		return Unavailable("preview could not be loaded from TMDB").WithCause(err)
	}
	return reply(w, r, http.StatusOK, recommendationPreviewResponse{
		Title:          detail.Title,
		Year:           detail.Year,
		MediaType:      rec.MediaType,
		Overview:       detail.Overview,
		PosterURL:      detail.PosterURL,
		Genres:         detail.Genres,
		People:         detail.People,
		RuntimeMinutes: detail.RuntimeMinutes,
		VoteAverage:    detail.VoteAverage,
		VoteCount:      detail.VoteCount,
		Videos:         detail.Videos,
		Seasons:        detail.Seasons,
	})
}

type recommendationGenerateRequest struct {
	ServerID  string `json:"server_id"`
	UserID    string `json:"user_id"`
	MediaType string `json:"media_type"`
}

func (s *Server) handleRecommendationGenerate(w http.ResponseWriter, r *http.Request) error {
	if s.recommendations == nil || !s.cfg.Recommendations.Enabled {
		return Unavailable("recommendations are disabled")
	}
	req, err := decodeJSON[recommendationGenerateRequest](r)
	if err != nil {
		return err
	}
	if req.ServerID == "" || req.UserID == "" || (req.MediaType != "movie" && req.MediaType != "series") {
		return BadRequest("server_id, user_id, and a valid media_type are required")
	}
	if err := s.recommendations.Generate(r.Context(), req.ServerID, req.UserID, req.MediaType); err != nil {
		return Unavailable("recommendations could not be generated").WithCause(err)
	}
	return reply(w, r, http.StatusOK, map[string]any{"generated": true})
}

type recommendationActionRequest struct {
	ActionID    string `json:"action_id"`
	Action      string `json:"action"`
	Rating      int    `json:"rating"`
	MonitorMode string `json:"monitor_mode"`
	Seasons     []int  `json:"seasons"`
	Trial       string `json:"trial"`
}

// validate checks the request shape before any state is read.
func (req recommendationActionRequest) validate() error {
	if strings.TrimSpace(req.ActionID) == "" {
		return BadRequest("action_id is required")
	}
	if req.Trial != "" && (req.Action != "request" || req.MonitorMode != "" || len(req.Seasons) > 0 || (req.Trial != "one_episode" && req.Trial != "three_episodes" && req.Trial != "first_season")) {
		return BadRequest("trial must be one_episode, three_episodes, or first_season on a series request without another scope")
	}
	if req.Action != "request" && req.Action != "dismiss" && req.Action != "rate" && req.Action != "undo" {
		return BadRequest("action must be request, dismiss, rate, or undo")
	}
	if req.Action != "request" && req.MonitorMode != "" {
		return BadRequest("monitor_mode is only valid for request actions")
	}
	if len(req.Seasons) > 100 || (len(req.Seasons) > 0 && (req.Action != "request" || req.MonitorMode != "")) {
		return BadRequest("specific seasons require a request action with no monitor_mode, at most 100 seasons")
	}
	for _, season := range req.Seasons {
		if season < 0 || season > 999 {
			return BadRequest("season must be between 0 and 999")
		}
	}
	if req.Action == "rate" && (req.Rating < 1 || req.Rating > 5) {
		return BadRequest("rating must be from 1 to 5")
	}
	if req.MonitorMode != "" && req.MonitorMode != string(model.MonitorLatestSeason) && req.MonitorMode != string(model.MonitorAll) && req.MonitorMode != string(model.MonitorFutureOnly) {
		return BadRequest("monitor_mode must be latest_season, all, or future_only")
	}
	return nil
}

func (s *Server) handleRecommendationAction(w http.ResponseWriter, r *http.Request, id int64) error {
	req, err := decodeJSON[recommendationActionRequest](r)
	if err != nil {
		return err
	}
	if err := req.validate(); err != nil {
		return err
	}
	rec, err := s.store.Recommendations().Get(r.Context(), id)
	if err != nil {
		return NotFound("recommendation %d not found", id)
	}
	if rec.MediaType != "series" && (req.MonitorMode != "" || len(req.Seasons) > 0 || req.Trial != "") {
		return BadRequest("monitor_mode is only valid for series requests")
	}

	var subject any
	metadataChanged := false
	if req.Action == "request" {
		subject, metadataChanged, err = s.applyRequestAction(r.Context(), rec, req)
		if err != nil {
			return err
		}
	}

	inserted := true
	switch req.Action {
	case "undo":
		err = s.store.Recommendations().UndoDismissal(r.Context(), id)
	case "rate":
		_, err = s.store.Recommendations().RecordRating(r.Context(), id, req.ActionID, req.Rating)
	default:
		_, inserted, err = s.store.Recommendations().RecordAction(r.Context(), id, req.ActionID, req.Action)
	}
	if err != nil {
		return err
	}

	switch req.Action {
	case "request":
		if s.engine != nil && metadataChanged {
			_ = s.engine.MetadataOnce(r.Context())
		}
		s.trigger("search")
		s.publishRequestsUpdated(rec.ServerID, rec.UserID)
	case "rate":
		s.trigger("recommendations")
	}
	return reply(w, r, http.StatusOK, map[string]any{"recommendation": rec, "subject": subject, "created": inserted, "rating": req.Rating})
}

// applyRequestAction turns a recommendation into a tracked request: it enforces
// the trial rules, finds or creates the library subject, and records the
// request. A repeated action_id reuses the recorded request and creates nothing.
func (s *Server) applyRequestAction(ctx context.Context, rec model.Recommendation, req recommendationActionRequest) (any, bool, error) {
	recorded, err := s.store.Requests().ActionRecorded(ctx, req.ActionID, rec.ServerID, rec.UserID, rec.MediaType, rec.TMDBID)
	if err != nil {
		if errors.Is(err, store.ErrActionIDConflict) {
			return nil, false, Conflict("action_id was already used for another request").WithCause(err)
		}
		return nil, false, err
	}
	if !recorded && rec.MediaType == "series" {
		if err := s.checkTrialRules(ctx, rec, req.Trial); err != nil {
			return nil, false, err
		}
	}

	requestedMonitorMode := req.MonitorMode
	if rec.MediaType == "series" && requestedMonitorMode == "" && len(req.Seasons) == 0 && req.Trial == "" {
		requestedMonitorMode = string(model.MonitorFutureOnly)
	}
	monitorMode := requestedMonitorMode
	if len(req.Seasons) > 0 || req.Trial != "" {
		monitorMode = string(model.MonitorNone)
	}
	if recorded {
		monitorMode = ""
		requestedMonitorMode = ""
	}
	subject, metadataChanged, err := s.requestRecommendation(ctx, rec, monitorMode, !recorded)
	if err != nil {
		return nil, false, Conflict("recommendation could not be requested").WithCause(err)
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
		return nil, false, Conflict("recommendation returned an unsupported subject")
	}
	if _, err = s.store.Requests().CreateForAction(ctx, request, !recorded); err != nil {
		return nil, false, Conflict("recommendation could not be tracked").WithCause(err)
	}
	if recorded {
		return subject, metadataChanged, nil
	}
	if len(req.Seasons) > 0 {
		if err := s.store.Requests().AddSeasons(ctx, rec.ServerID, rec.UserID, rec.TMDBID, request.SubjectID, req.Seasons); err != nil {
			return nil, false, err
		}
		metadataChanged = true
	}
	if req.Trial != "" {
		if err := s.store.Requests().StartTrial(ctx, rec.ServerID, rec.UserID, rec.TMDBID, req.Trial); err != nil {
			return nil, false, Conflict("trial could not be started").WithCause(err)
		}
		metadataChanged = true
	}
	return subject, metadataChanged, nil
}

// checkTrialRules rejects a series request that would bypass an open trial or
// start a trial on a series that already finished one.
func (s *Server) checkTrialRules(ctx context.Context, rec model.Recommendation, requestedTrial string) error {
	trial, err := s.store.Requests().TrialForTitle(ctx, rec.ServerID, rec.UserID, rec.TMDBID)
	if err != nil {
		return err
	}
	if trial == nil {
		return nil
	}
	if trial.Decision == "" && (requestedTrial == "" || requestedTrial != trial.Scope) {
		return Conflict("vote on the existing trial before requesting more episodes")
	}
	if requestedTrial != "" && trial.Decision != "" {
		return Conflict("this series already has a completed trial")
	}
	return nil
}

func (s *Server) requestRecommendation(ctx context.Context, rec model.Recommendation, monitorMode string, createIfMissing bool) (any, bool, error) {
	profile, err := s.store.Profiles().Default(ctx)
	if err != nil {
		return nil, false, err
	}
	if rec.MediaType == "movie" {
		return s.requestMovie(ctx, rec, profile, createIfMissing)
	}
	return s.requestSeries(ctx, rec, profile, monitorMode, createIfMissing)
}

func (s *Server) requestMovie(ctx context.Context, rec model.Recommendation, profile model.QualityProfile, createIfMissing bool) (any, bool, error) {
	existing, err := s.store.Movies().GetByTMDBID(ctx, rec.TMDBID)
	if err == nil {
		return existing, false, nil
	}
	if !errors.Is(err, store.ErrNotFound) {
		return nil, false, err
	}
	if !createIfMissing {
		return nil, false, store.ErrNotFound
	}
	detail, err := s.movies.MovieDetails(ctx, rec.TMDBID)
	if err != nil {
		return nil, false, err
	}
	created, err := s.store.Movies().Create(ctx, model.Movie{Title: detail.Title, Year: detail.Year, TMDBID: detail.TMDBID, IMDBID: detail.IMDBID, RuntimeMinutes: detail.RuntimeMinutes, ProfileID: profile.ID, RootFolder: s.cfg.Library.MovieRoot, State: model.StateWanted}, "requested from Jellyfin recommendation")
	return created, false, err
}

func (s *Server) requestSeries(ctx context.Context, rec model.Recommendation, profile model.QualityProfile, monitorMode string, createIfMissing bool) (any, bool, error) {
	existing, err := s.store.Series().GetByTMDBID(ctx, rec.TMDBID)
	if err == nil {
		changed := false
		widenLatestSeason := monitorMode == string(model.MonitorLatestSeason) && (existing.MonitorMode == model.MonitorFutureOnly || existing.MonitorMode == model.MonitorNone)
		widenAll := monitorMode == string(model.MonitorAll) && existing.MonitorMode != model.MonitorAll
		widenFuture := monitorMode == string(model.MonitorFutureOnly) && existing.MonitorMode == model.MonitorNone
		if existing.Status == model.SeriesFollowing && (widenLatestSeason || widenAll || widenFuture) {
			existing.MonitorMode = model.MonitorMode(monitorMode)
			existing, err = s.store.Series().Update(ctx, existing)
			changed = err == nil
		}
		return existing, changed, err
	}
	if !errors.Is(err, store.ErrNotFound) {
		return nil, false, err
	}
	if !createIfMissing {
		return nil, false, store.ErrNotFound
	}
	if s.discovery == nil || s.externalSeries == nil {
		return nil, false, errors.New("series recommendation providers are unavailable")
	}
	detail, err := s.discovery.DiscoveryDetails(ctx, rec.MediaType, rec.TMDBID)
	if err != nil {
		return nil, false, err
	}
	series, err := s.externalSeries.LookupSeries(ctx, detail.TVDBID, detail.IMDBID)
	if err != nil {
		return nil, false, err
	}
	mode := model.MonitorFutureOnly
	if monitorMode != "" {
		mode = model.MonitorMode(monitorMode)
	}
	created, err := s.store.Series().Create(ctx, model.Series{Title: series.Title, Year: series.Year, TVmazeID: series.TVmazeID, TMDBID: rec.TMDBID, IMDBID: series.IMDBID, Aliases: series.Aliases, MonitorMode: mode, Status: model.SeriesFollowing, ProfileID: profile.ID, RootFolder: s.cfg.Library.TVRoot, RuntimeMinutes: series.RuntimeMinutes})
	return created, err == nil, err
}
