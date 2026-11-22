package api

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/TechXTT/reelay/internal/engine"
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
	incidents, err := s.store.Incidents().Open(r.Context())
	if err != nil {
		return err
	}
	now := s.clock.Now().UTC()
	member := principalFrom(r).Role != roleOperator
	for i := range values {
		s.describeRequestWait(&values[i], incidents, member, now)
		if base := s.cfg.Availability.JellyfinServers[values[i].ServerID]; base != "" && values[i].JellyfinItemID != "" {
			values[i].JellyfinURL = strings.TrimRight(base, "/") + "/web/index.html#!/details?id=" + url.QueryEscape(values[i].JellyfinItemID)
		}
	}
	return reply(w, r, http.StatusOK, map[string]any{"items": values, "limit": model.MediaRequestListLimit, "offset": offset, "has_more": len(values) == model.MediaRequestListLimit})
}

// requestWaitText is the viewer-safe copy for each wait kind. It never names
// hosts, paths, trackers or raw errors; incident detail stays operator-only.
var requestWaitText = map[string]string{
	model.WaitSearchService:  "Waiting for the search service to respond.",
	model.WaitDownloadClient: "Waiting for the download service to respond.",
	model.WaitStorage:        "Waiting for free storage space on the server.",
	model.WaitMetadata:       "Waiting for the TV listings service to respond.",
	model.WaitImportRetry:    "Waiting for you to retry the import.",
	model.WaitSearchRetry:    "Waiting for you to retry the search.",
}

// serviceWaitKinds wait on the service rather than the viewer.
var serviceWaitKinds = []string{model.WaitSearchService, model.WaitDownloadClient, model.WaitDownloadFiles,
	model.WaitStorage, model.WaitMetadata}

// describeRequestWait fills the viewer fields that say what a request waits
// for, when Reelay checks again and whether the viewer needs to act. Open
// incidents only choose the copy; their detail is never copied. During a
// service wait the item's last error is the raw service error, so members
// get the wait text instead.
func (s *Server) describeRequestWait(request *model.MediaRequest, incidents []store.Incident, member bool, now time.Time) {
	var searching = request.State == string(model.StateWanted) || request.State == string(model.StateSearching)
	var due = request.NextSearchAt == nil || !request.NextSearchAt.After(now)

	if request.CancelledAt != nil {
		return
	}
	switch {
	case request.DownloadWait != "":
		observed := request.WaitObservedAt.Add(s.cfg.Schedules.StatusInterval.Duration)
		request.WaitingFor = request.DownloadWait
		request.NextCheckAt = nextCheck(&observed, s.cfg.Schedules.StatusInterval.Duration, now)
	case request.WaitKind != "":
		request.WaitingFor = requestWaitText[request.WaitKind]
		request.NextCheckAt = nextCheck(request.NextSearchAt, s.cfg.Schedules.SearchInterval.Duration, now)
	default:
		// A storage incident skips whole search passes without holding each
		// item, and a metadata outage leaves a series without episodes.
		for _, incident := range incidents {
			kind := ""
			if incident.Kind == store.IncidentStorage && searching && due {
				kind = model.WaitStorage
			} else if incident.Kind == store.IncidentMetadata && request.State == "waiting_metadata" {
				kind = model.WaitMetadata
			}
			if kind != "" {
				request.WaitKind, request.WaitingFor = kind, requestWaitText[kind]
				request.NextCheckAt = nextCheck(incident.NextCheckAt, engine.IncidentCheckInterval(s.cfg, incident.Kind), now)
				break
			}
		}
	}
	switch request.State {
	case string(model.StateFailed), string(model.StateImportFailed), "attention_needed":
		request.NeedsAction = true
	}
	switch {
	case request.CanRetryImport:
		request.NeedsAction = true
		request.ActionHint = "Use Retry import to add the finished download to the library."
		if request.WaitKind == "" {
			request.WaitKind, request.WaitingFor = model.WaitImportRetry, requestWaitText[model.WaitImportRetry]
		}
	case request.NeedsAction:
		request.ActionHint = "Use Retry to search for another release."
		if request.WaitKind == "" {
			request.WaitKind, request.WaitingFor = model.WaitSearchRetry, requestWaitText[model.WaitSearchRetry]
		}
	case request.WaitingFor != "":
		request.ActionHint = "Nothing to do; Reelay will retry automatically."
	}
	if member && slices.Contains(serviceWaitKinds, request.WaitKind) {
		request.LastError = ""
	}
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
	if errors.Is(err, store.ErrNotFound) || (err == nil && !ownsRecord(r, request.ServerID, request.UserID)) {
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
	case "retry_import":
		if s.engine == nil {
			return Unavailable("engine is unavailable")
		}
		if request.CancelledAt != nil {
			return Conflict("request was withdrawn")
		}
		retried, err = s.store.Requests().RetryImports(r.Context(), request)
		if err != nil {
			return requestActionFailed(err)
		}
		if retried == 0 {
			return Conflict("no failed imports with a recorded completed download can be retried")
		}
		if err := s.engine.Trigger("status"); err != nil {
			return err
		}
	default:
		return BadRequest("action must be cancel, retry, or retry_import")
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
