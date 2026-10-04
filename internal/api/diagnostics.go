package api

import (
	"net/http"
	"slices"
	"time"

	"github.com/TechXTT/reelay/internal/model"
)

type diagnosticCandidate struct {
	Evaluation model.CandidateEvaluation `json:"evaluation"`
	Release    model.StoredRelease       `json:"release"`
}

type diagnosticSubject struct {
	Type           model.SubjectType       `json:"type"`
	ID             int64                   `json:"id"`
	Title          string                  `json:"title"`
	State          model.ItemState         `json:"state"`
	Error          string                  `json:"error"`
	SearchAttempts int                     `json:"search_attempts"`
	NextSearchAt   *time.Time              `json:"next_search_at,omitempty"`
	LastSearchAt   *time.Time              `json:"last_search_at,omitempty"`
	History        []model.StateTransition `json:"history"`
	Candidates     []diagnosticCandidate   `json:"candidates"`
}

func (s *Server) handleRequestDiagnostics(w http.ResponseWriter, r *http.Request) error {
	var id, err = pathID(r)
	var subjects = make([]diagnosticSubject, 0)

	if err != nil {
		return err
	}
	request, err := s.store.Requests().Get(r.Context(), id)
	if err != nil {
		return NotFound("request not found")
	}
	if request.MediaType == "movie" {
		movie, err := s.store.Movies().Get(r.Context(), request.SubjectID)
		if err != nil || movie.TMDBID != request.TMDBID {
			return Conflict("requested subject is missing")
		}
		subjects = append(subjects, diagnosticSubject{Type: model.SubjectMovie, ID: movie.ID, Title: movie.Title, State: movie.State, Error: movie.LastError, SearchAttempts: movie.SearchAttempts, NextSearchAt: movie.NextSearchAt})
	} else {
		series, err := s.store.Series().Get(r.Context(), request.SubjectID)
		if err != nil || series.TMDBID != request.TMDBID {
			return Conflict("requested subject is missing")
		}
		episodes, err := s.store.Episodes().ListBySeries(r.Context(), series.ID)
		if err != nil {
			return err
		}
		for _, episode := range episodes {
			if request.MonitorMode == "" && len(request.Seasons) > 0 && !slices.Contains(request.Seasons, episode.Season) {
				continue
			}
			if episode.State == model.StateUnmonitored || episode.State == model.StateImported {
				continue
			}
			subjects = append(subjects, diagnosticSubject{Type: model.SubjectEpisode, ID: episode.ID, Title: episode.Title, State: episode.State, Error: episode.LastError, SearchAttempts: episode.SearchAttempts, NextSearchAt: episode.NextSearchAt})
			if len(subjects) >= 50 {
				break
			}
		}
	}
	for i := range subjects {
		subjects[i].History, err = s.store.Transitions().History(r.Context(), subjects[i].Type, subjects[i].ID, 10)
		if err != nil {
			return err
		}
		for _, transition := range subjects[i].History {
			if transition.To == model.StateSearching || transition.From == model.StateSearching {
				at := transition.At
				subjects[i].LastSearchAt = &at
				break
			}
		}
		candidates, err := s.store.Decisions().Candidates(r.Context(), subjects[i].Type, subjects[i].ID)
		if err != nil {
			return err
		}
		subjects[i].Candidates = make([]diagnosticCandidate, 0)
		for _, candidate := range candidates[:min(100, len(candidates))] {
			release, err := s.store.Releases().Get(r.Context(), candidate.ReleaseID)
			if err != nil {
				return err
			}
			subjects[i].Candidates = append(subjects[i].Candidates, diagnosticCandidate{candidate, release})
		}
	}
	writeJSON(w, s.logFor(r), http.StatusOK, map[string]any{"request": request, "subjects": subjects, "subject_limit": 50, "candidate_limit": 100})
	return nil
}

func (s *Server) handleRequestGrab(w http.ResponseWriter, r *http.Request) error {
	var id, err = pathID(r)
	var body struct {
		SubjectType model.SubjectType `json:"subject_type"`
		SubjectID   int64             `json:"subject_id"`
		ReleaseID   int64             `json:"release_id"`
	}

	if err != nil {
		return err
	}
	if err := decodeBody(r, &body); err != nil {
		return err
	}
	if s.engine == nil {
		return Unavailable("engine is unavailable")
	}
	request, err := s.store.Requests().Get(r.Context(), id)
	if err != nil {
		return NotFound("request not found")
	}
	if request.CancelledAt != nil {
		return Conflict("request has been withdrawn")
	}
	if request.MediaType == "movie" {
		movie, err := s.store.Movies().Get(r.Context(), request.SubjectID)
		if err != nil || movie.TMDBID != request.TMDBID || body.SubjectType != model.SubjectMovie || body.SubjectID != movie.ID {
			return BadRequest("release target does not belong to this request")
		}
		if movie.State == model.StateImported {
			return Conflict("requested movie is already imported")
		}
	} else {
		series, err := s.store.Series().Get(r.Context(), request.SubjectID)
		if err != nil || series.TMDBID != request.TMDBID {
			return Conflict("requested subject is missing")
		}
		episode, err := s.store.Episodes().Get(r.Context(), body.SubjectID)
		if err != nil || body.SubjectType != model.SubjectEpisode || episode.SeriesID != series.ID || (request.MonitorMode == "" && len(request.Seasons) > 0 && !slices.Contains(request.Seasons, episode.Season)) {
			return BadRequest("release target does not belong to this request")
		}
		if episode.State == model.StateImported || episode.State == model.StateUnmonitored {
			return Conflict("episode is not awaiting a release")
		}
	}
	candidates, err := s.store.Decisions().Candidates(r.Context(), body.SubjectType, body.SubjectID)
	if err != nil {
		return err
	}
	allowed := false
	for _, candidate := range candidates {
		if candidate.ReleaseID == body.ReleaseID && candidate.Accepted {
			allowed = true
		}
	}
	if !allowed {
		return Conflict("only an accepted candidate from the last search can be selected")
	}
	grab, err := s.engine.ManualGrab(r.Context(), body.SubjectType, body.SubjectID, body.ReleaseID)
	if err != nil {
		return Conflict("release could not be grabbed").WithCause(err)
	}
	writeJSON(w, s.logFor(r), http.StatusCreated, grab)
	return nil
}
