package api

import (
	"errors"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"

	"github.com/TechXTT/reelay/internal/engine"
	"github.com/TechXTT/reelay/internal/model"
	"github.com/TechXTT/reelay/internal/store"
)

func (s *Server) handleRequests(w http.ResponseWriter, r *http.Request) error {
	serverID := strings.TrimSpace(r.URL.Query().Get("server_id"))
	userID := strings.TrimSpace(r.URL.Query().Get("user_id"))
	if serverID == "" || userID == "" {
		return BadRequest("server_id and user_id are required")
	}
	offset := 0
	if raw := r.URL.Query().Get("offset"); raw != "" {
		var err error
		offset, err = strconv.Atoi(raw)
		if err != nil {
			return BadRequest("offset must be an integer")
		}
	}
	if offset < 0 || offset > 1000000 {
		return BadRequest("offset must be between 0 and 1000000")
	}
	values, err := s.store.Requests().ListPage(r.Context(), serverID, userID, offset, r.URL.Query().Get("attention") == "true")
	if err != nil {
		return err
	}
	for i := range values {
		if base := s.cfg.Availability.JellyfinServers[values[i].ServerID]; base != "" && values[i].JellyfinItemID != "" {
			values[i].JellyfinURL = strings.TrimRight(base, "/") + "/web/index.html#!/details?id=" + url.QueryEscape(values[i].JellyfinItemID)
		}
	}
	writeJSON(w, s.logFor(r), http.StatusOK, map[string]any{"items": values, "limit": model.MediaRequestListLimit, "offset": offset, "has_more": len(values) == model.MediaRequestListLimit})
	return nil
}

func (s *Server) handleRequestAction(w http.ResponseWriter, r *http.Request) error {
	var id, err = pathID(r)
	var body struct {
		Action string `json:"action"`
	}
	var retried int

	if err != nil {
		return err
	}
	if err := decodeBody(r, &body); err != nil {
		return err
	}
	request, err := s.store.Requests().Get(r.Context(), id)
	if errors.Is(err, store.ErrNotFound) {
		return NotFound("request not found")
	}
	if err != nil {
		return err
	}
	switch body.Action {
	case "cancel":
		err = s.store.Requests().Cancel(r.Context(), id)
	case "retry":
		if s.engine == nil {
			return Unavailable("engine is unavailable")
		}
		if request.MediaType == "movie" {
			movie, lookupErr := s.store.Movies().Get(r.Context(), request.SubjectID)
			if lookupErr != nil || movie.TMDBID != request.TMDBID {
				return Conflict("requested movie is missing; request the title again")
			}
			if movie.State == model.StateImported {
				return Conflict("movie is already imported")
			}
			err = s.engine.ForceSearch(r.Context(), model.SubjectMovie, movie.ID)
			if err == nil {
				retried++
			}
		} else {
			series, lookupErr := s.store.Series().Get(r.Context(), request.SubjectID)
			if lookupErr != nil || series.TMDBID != request.TMDBID {
				return Conflict("requested series is missing; request the title again")
			}
			if series.Status != model.SeriesFollowing {
				return Conflict("resume the series before retrying")
			}
			episodes, lookupErr := s.store.Episodes().ListBySeries(r.Context(), series.ID)
			if lookupErr != nil {
				return lookupErr
			}
			for _, episode := range episodes {
				if request.MonitorMode == "" && len(request.Seasons) > 0 && !slices.Contains(request.Seasons, episode.Season) {
					continue
				}
				if episode.State != model.StateWanted && episode.State != model.StateFailed && episode.State != model.StateImportFailed {
					continue
				}
				if retryErr := s.engine.ForceSearch(r.Context(), model.SubjectEpisode, episode.ID); retryErr != nil {
					return Conflict("episode cannot be retried now").WithCause(retryErr)
				}
				retried++
			}
			if retried == 0 {
				return Conflict("no failed or waiting episodes can be retried")
			}
		}
		if err == nil {
			err = s.store.Requests().Reactivate(r.Context(), id)
		}
	default:
		return BadRequest("action must be cancel or retry")
	}
	if err != nil {
		return Conflict("request action could not be completed").WithCause(err)
	}
	if s.engine != nil {
		s.engine.Events().Publish(engine.Event{Type: "requests_updated", At: s.clock.Now(), Data: map[string]any{"server_id": request.ServerID, "user_id": request.UserID}})
	}
	writeJSON(w, s.logFor(r), http.StatusOK, map[string]any{"action": body.Action, "retried": retried})
	return nil
}
