package api

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"github.com/TechXTT/reelay/internal/model"
	"github.com/TechXTT/reelay/internal/store"
)

func (s *Server) handleRequests(w http.ResponseWriter, r *http.Request) error {
	serverID, userID, err := userScope(r)
	if err != nil {
		return err
	}
	offset := 0
	if raw := r.URL.Query().Get("offset"); raw != "" {
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
	return reply(w, r, http.StatusOK, map[string]any{"items": values, "limit": model.MediaRequestListLimit, "offset": offset, "has_more": len(values) == model.MediaRequestListLimit})
}

type requestActionRequest struct {
	Action string `json:"action"`
}

func requestActionFailed(err error) error {
	return Conflict("request action could not be completed").WithCause(err)
}

func (s *Server) handleRequestAction(w http.ResponseWriter, r *http.Request, id int64) error {
	body, err := decodeJSON[requestActionRequest](r)
	if err != nil {
		return err
	}
	request, err := s.store.Requests().Get(r.Context(), id)
	if errors.Is(err, store.ErrNotFound) {
		return NotFound("request not found")
	}
	if err != nil {
		return err
	}

	retried := 0
	switch body.Action {
	case "cancel":
		if err := s.store.Requests().Cancel(r.Context(), id); err != nil {
			return requestActionFailed(err)
		}
	case "retry":
		if s.engine == nil {
			return Unavailable("engine is unavailable")
		}
		retried, err = s.retryRequest(r.Context(), request)
		if err != nil {
			return err
		}
		if err := s.store.Requests().Reactivate(r.Context(), id); err != nil {
			return requestActionFailed(err)
		}
	default:
		return BadRequest("action must be cancel or retry")
	}
	s.publishRequestsUpdated(request.ServerID, request.UserID)
	return reply(w, r, http.StatusOK, map[string]any{"action": body.Action, "retried": retried})
}

// retryRequest forces a search for every retryable subject of the request and
// returns how many were queued. Errors are final API errors.
func (s *Server) retryRequest(ctx context.Context, request model.MediaRequest) (int, error) {
	if request.MediaType == "movie" {
		movie, err := s.store.Movies().Get(ctx, request.SubjectID)
		if err != nil || movie.TMDBID != request.TMDBID {
			return 0, Conflict("requested movie is missing; request the title again")
		}
		if movie.State == model.StateImported {
			return 0, Conflict("movie is already imported")
		}
		if err := s.engine.ForceSearch(ctx, model.SubjectMovie, movie.ID); err != nil {
			return 0, requestActionFailed(err)
		}
		return 1, nil
	}

	series, err := s.store.Series().Get(ctx, request.SubjectID)
	if err != nil || series.TMDBID != request.TMDBID {
		return 0, Conflict("requested series is missing; request the title again")
	}
	if series.Status != model.SeriesFollowing {
		return 0, Conflict("resume the series before retrying")
	}
	episodes, err := s.store.Episodes().ListBySeries(ctx, series.ID)
	if err != nil {
		return 0, err
	}
	retried := 0
	for _, episode := range episodes {
		if !request.IncludesEpisode(episode) || !isRetryable(episode.State) {
			continue
		}
		if err := s.engine.ForceSearch(ctx, model.SubjectEpisode, episode.ID); err != nil {
			return 0, Conflict("episode cannot be retried now").WithCause(err)
		}
		retried++
	}
	if retried == 0 {
		return 0, Conflict("no failed or waiting episodes can be retried")
	}
	return retried, nil
}
