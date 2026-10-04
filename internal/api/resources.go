package api

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strconv"
	"strings"

	"github.com/TechXTT/reelay/internal/engine"
	"github.com/TechXTT/reelay/internal/model"
)

// decodeJSON reads a strict JSON request body: at most 1 MiB, no unknown fields.
func decodeJSON[T any](r *http.Request) (T, error) {
	var body T

	dec := json.NewDecoder(io.LimitReader(r.Body, 1<<20))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&body); err != nil {
		return body, BadRequest("invalid JSON body: %v", err)
	}
	return body, nil
}

// reply writes v as the JSON response; handlers end with `return reply(...)`.
func reply(w http.ResponseWriter, r *http.Request, status int, v any) error {
	writeJSON(w, loggerFrom(r), status, v)
	return nil
}

// withID parses the positive {id} path value before calling h.
func withID(h func(http.ResponseWriter, *http.Request, int64) error) handler {
	return func(w http.ResponseWriter, r *http.Request) error {
		id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
		if err != nil || id <= 0 {
			return BadRequest("invalid id %q", r.PathValue("id"))
		}
		return h(w, r, id)
	}
}

// listHandler serves {"items": list(ctx)}.
func listHandler[T any](list func(context.Context) ([]T, error)) handler {
	return func(w http.ResponseWriter, r *http.Request) error {
		values, err := list(r.Context())
		if err != nil {
			return err
		}
		return reply(w, r, http.StatusOK, map[string]any{"items": values})
	}
}

// userScope reads the required server_id and user_id query parameters.
func userScope(r *http.Request) (string, string, error) {
	serverID := strings.TrimSpace(r.URL.Query().Get("server_id"))
	userID := strings.TrimSpace(r.URL.Query().Get("user_id"))
	if serverID == "" || userID == "" {
		return "", "", BadRequest("server_id and user_id are required")
	}
	return serverID, userID, nil
}

func (s *Server) publishRequestsUpdated(serverID, userID string) {
	if s.engine == nil {
		return
	}
	s.engine.Events().Publish(engine.Event{Type: "requests_updated", At: s.clock.Now(), Data: map[string]any{"server_id": serverID, "user_id": userID}})
}

// trigger wakes the named engine loops. It does nothing without an engine,
// which is how the API runs in tests and read-only setups.
func (s *Server) trigger(loops ...string) {
	if s.engine == nil {
		return
	}
	for _, loop := range loops {
		_ = s.engine.Trigger(loop)
	}
}

type seriesCreateRequest struct {
	Query       string            `json:"query"`
	TVmazeID    int               `json:"tvmaze_id"`
	MonitorMode model.MonitorMode `json:"monitor_mode"`
	ProfileID   int64             `json:"profile_id"`
	RootFolder  string            `json:"root_folder"`
	IsAnime     bool              `json:"is_anime"`
}

func (s *Server) handleSeriesCreate(w http.ResponseWriter, r *http.Request) error {
	req, err := decodeJSON[seriesCreateRequest](r)
	if err != nil {
		return err
	}
	if s.series == nil {
		return Unavailable("series metadata provider is unavailable")
	}
	values, err := s.series.SearchSeries(r.Context(), req.Query)
	if err != nil || len(values) == 0 {
		return BadRequest("series not found").WithCause(err)
	}
	selected := values[0]
	if req.TVmazeID > 0 {
		for _, value := range values {
			if value.TVmazeID == req.TVmazeID {
				selected = value
				break
			}
		}
	}
	if req.ProfileID == 0 {
		profile, err := s.store.Profiles().Default(r.Context())
		if err != nil {
			return err
		}
		req.ProfileID = profile.ID
	}
	if req.RootFolder == "" {
		req.RootFolder = s.cfg.Library.TVRoot
	}
	if req.MonitorMode == "" {
		req.MonitorMode = model.MonitorFutureOnly
	}
	created, err := s.store.Series().Create(r.Context(), model.Series{Title: selected.Title,
		Year: selected.Year, TVmazeID: selected.TVmazeID, IMDBID: selected.IMDBID,
		Aliases: selected.Aliases, IsAnime: req.IsAnime, MonitorMode: req.MonitorMode,
		Status: model.SeriesFollowing, ProfileID: req.ProfileID, RootFolder: req.RootFolder,
		RuntimeMinutes: selected.RuntimeMinutes})
	if err != nil {
		return Conflict("could not add series").WithCause(err)
	}
	if s.engine != nil {
		_ = s.engine.MetadataOnce(r.Context())
	}
	return reply(w, r, http.StatusCreated, created)
}

func (s *Server) handleSeriesGet(w http.ResponseWriter, r *http.Request, id int64) error {
	series, err := s.store.Series().Get(r.Context(), id)
	if err != nil {
		return NotFound("series %d not found", id)
	}
	episodes, err := s.store.Episodes().ListBySeries(r.Context(), id)
	if err != nil {
		return err
	}
	return reply(w, r, http.StatusOK, map[string]any{"series": series, "episodes": episodes})
}

type seriesPatchRequest struct {
	MonitorMode *model.MonitorMode  `json:"monitor_mode"`
	Status      *model.SeriesStatus `json:"status"`
	ProfileID   *int64              `json:"profile_id"`
	IsAnime     *bool               `json:"is_anime"`
}

func (s *Server) handleSeriesPatch(w http.ResponseWriter, r *http.Request, id int64) error {
	item, err := s.store.Series().Get(r.Context(), id)
	if err != nil {
		return NotFound("series %d not found", id)
	}
	req, err := decodeJSON[seriesPatchRequest](r)
	if err != nil {
		return err
	}
	if req.MonitorMode != nil {
		item.MonitorMode = *req.MonitorMode
	}
	if req.Status != nil {
		item.Status = *req.Status
	}
	if req.ProfileID != nil {
		item.ProfileID = *req.ProfileID
	}
	if req.IsAnime != nil {
		item.IsAnime = *req.IsAnime
	}
	item, err = s.store.Series().Update(r.Context(), item)
	if err != nil {
		return BadRequest("invalid series update").WithCause(err)
	}
	return reply(w, r, http.StatusOK, item)
}

// isRetryable reports whether a search can be forced for an episode in this state.
func isRetryable(state model.ItemState) bool {
	return state == model.StateWanted || state == model.StateFailed || state == model.StateImportFailed
}

func (s *Server) handleSeriesSearch(w http.ResponseWriter, r *http.Request, id int64) error {
	episodes, err := s.store.Episodes().ListBySeries(r.Context(), id)
	if err != nil {
		return err
	}
	count := 0
	for _, episode := range episodes {
		if !isRetryable(episode.State) {
			continue
		}
		if err := s.engine.ForceSearch(r.Context(), model.SubjectEpisode, episode.ID); err == nil {
			count++
		}
	}
	return reply(w, r, http.StatusAccepted, map[string]any{"searched": count})
}
