package api

import (
	"encoding/json"
	"io"
	"net/http"
	"strconv"

	"github.com/TechXTT/reelay/internal/model"
)

func decodeBody(r *http.Request, dst any) error {
	dec := json.NewDecoder(io.LimitReader(r.Body, 1<<20))
	dec.DisallowUnknownFields()
	if err := dec.Decode(dst); err != nil {
		return BadRequest("invalid JSON body: %v", err)
	}
	return nil
}

func pathID(r *http.Request) (int64, error) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil || id <= 0 {
		return 0, BadRequest("invalid id %q", r.PathValue("id"))
	}
	return id, nil
}

func (s *Server) handleSeriesList(w http.ResponseWriter, r *http.Request) error {
	values, err := s.store.Series().List(r.Context())
	if err != nil {
		return err
	}
	writeJSON(w, s.logFor(r), http.StatusOK, map[string]any{"items": values})
	return nil
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
	var req seriesCreateRequest
	if err := decodeBody(r, &req); err != nil {
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
	writeJSON(w, s.logFor(r), http.StatusCreated, created)
	return nil
}

func (s *Server) handleSeriesGet(w http.ResponseWriter, r *http.Request) error {
	id, err := pathID(r)
	if err != nil {
		return err
	}
	series, err := s.store.Series().Get(r.Context(), id)
	if err != nil {
		return NotFound("series %d not found", id)
	}
	episodes, err := s.store.Episodes().ListBySeries(r.Context(), id)
	if err != nil {
		return err
	}
	writeJSON(w, s.logFor(r), http.StatusOK, map[string]any{"series": series, "episodes": episodes})
	return nil
}

type seriesPatchRequest struct {
	MonitorMode *model.MonitorMode  `json:"monitor_mode"`
	Status      *model.SeriesStatus `json:"status"`
	ProfileID   *int64              `json:"profile_id"`
	IsAnime     *bool               `json:"is_anime"`
}

func (s *Server) handleSeriesPatch(w http.ResponseWriter, r *http.Request) error {
	id, err := pathID(r)
	if err != nil {
		return err
	}
	item, err := s.store.Series().Get(r.Context(), id)
	if err != nil {
		return NotFound("series %d not found", id)
	}
	var req seriesPatchRequest
	if err := decodeBody(r, &req); err != nil {
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
	writeJSON(w, s.logFor(r), http.StatusOK, item)
	return nil
}

func (s *Server) handleSeriesDelete(w http.ResponseWriter, r *http.Request) error {
	id, err := pathID(r)
	if err != nil {
		return err
	}
	deleteFiles, err := queryBool(r.URL.Query().Get("deleteFiles"), "deleteFiles", false)
	if err != nil {
		return err
	}
	deleteDownloads, err := queryBool(r.URL.Query().Get("deleteDownloads"), "deleteDownloads", false)
	if err != nil {
		return err
	}
	if err := s.deleteSeriesCollection(r.Context(), id, deleteFiles, deleteDownloads); err != nil {
		return err
	}
	w.WriteHeader(http.StatusNoContent)
	return nil
}

func (s *Server) handleSeriesSearch(w http.ResponseWriter, r *http.Request) error {
	id, err := pathID(r)
	if err != nil {
		return err
	}
	episodes, err := s.store.Episodes().ListBySeries(r.Context(), id)
	if err != nil {
		return err
	}
	count := 0
	for _, episode := range episodes {
		if episode.State == model.StateWanted || episode.State == model.StateFailed || episode.State == model.StateImportFailed {
			if err := s.engine.ForceSearch(r.Context(), model.SubjectEpisode, episode.ID); err == nil {
				count++
			}
		}
	}
	writeJSON(w, s.logFor(r), http.StatusAccepted, map[string]any{"searched": count})
	return nil
}
