package api

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"time"

	"github.com/TechXTT/reelay/internal/engine"
	"github.com/TechXTT/reelay/internal/model"
	"github.com/TechXTT/reelay/internal/store"
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

const (
	diagnosticSubjectLimit   = 50
	diagnosticCandidateLimit = 100
)

func (s *Server) handleRequestDiagnostics(w http.ResponseWriter, r *http.Request, id int64) error {
	request, err := s.store.Requests().Get(r.Context(), id)
	if err != nil {
		return NotFound("request not found")
	}
	subjects, err := s.diagnosticSubjects(r.Context(), request)
	if err != nil {
		return err
	}
	for i := range subjects {
		if err := s.fillDiagnostics(r.Context(), &subjects[i]); err != nil {
			return err
		}
	}
	return reply(w, r, http.StatusOK, map[string]any{"request": request, "subjects": subjects,
		"subject_limit": diagnosticSubjectLimit, "candidate_limit": diagnosticCandidateLimit})
}

// diagnosticSubjects lists the movie, or the unfinished episodes covered by the request.
func (s *Server) diagnosticSubjects(ctx context.Context, request model.MediaRequest) ([]diagnosticSubject, error) {
	subjects := make([]diagnosticSubject, 0)
	if request.MediaType == "movie" {
		movie, err := s.store.Movies().Get(ctx, request.SubjectID)
		if err != nil || movie.TMDBID != request.TMDBID {
			return nil, Conflict("requested subject is missing")
		}
		return append(subjects, diagnosticSubject{Type: model.SubjectMovie, ID: movie.ID, Title: movie.Title, State: movie.State, Error: movie.LastError, SearchAttempts: movie.SearchAttempts, NextSearchAt: movie.NextSearchAt}), nil
	}

	series, err := s.store.Series().Get(ctx, request.SubjectID)
	if err != nil || series.TMDBID != request.TMDBID {
		return nil, Conflict("requested subject is missing")
	}
	episodes, err := s.store.Episodes().ListBySeries(ctx, series.ID)
	if err != nil {
		return nil, err
	}
	for _, episode := range episodes {
		if !request.IncludesEpisode(episode) || episode.State == model.StateUnmonitored || episode.State == model.StateImported {
			continue
		}
		subjects = append(subjects, diagnosticSubject{Type: model.SubjectEpisode, ID: episode.ID, Title: episode.Title, State: episode.State, Error: episode.LastError, SearchAttempts: episode.SearchAttempts, NextSearchAt: episode.NextSearchAt})
		if len(subjects) >= diagnosticSubjectLimit {
			break
		}
	}
	return subjects, nil
}

// fillDiagnostics adds recent transitions, the last search time and scored candidates.
func (s *Server) fillDiagnostics(ctx context.Context, subject *diagnosticSubject) error {
	history, err := s.store.Transitions().History(ctx, subject.Type, subject.ID, 10)
	if err != nil {
		return err
	}
	subject.History = history
	for _, transition := range history {
		if transition.To == model.StateSearching || transition.From == model.StateSearching {
			at := transition.At
			subject.LastSearchAt = &at
			break
		}
	}

	candidates, err := s.store.Decisions().Candidates(ctx, subject.Type, subject.ID)
	if err != nil {
		return err
	}
	candidates = candidates[:min(diagnosticCandidateLimit, len(candidates))]
	releaseIDs := make([]int64, len(candidates))
	for i, candidate := range candidates {
		releaseIDs[i] = candidate.ReleaseID
	}
	releases, err := s.store.Releases().GetMany(ctx, releaseIDs)
	if err != nil {
		return err
	}
	subject.Candidates = make([]diagnosticCandidate, 0, len(candidates))
	for _, candidate := range candidates {
		release, found := releases[candidate.ReleaseID]
		if !found {
			return fmt.Errorf("release %d: %w", candidate.ReleaseID, store.ErrNotFound)
		}
		subject.Candidates = append(subject.Candidates, diagnosticCandidate{candidate, release})
	}
	return nil
}

type requestGrabRequest struct {
	SubjectType model.SubjectType `json:"subject_type"`
	SubjectID   int64             `json:"subject_id"`
	ReleaseID   int64             `json:"release_id"`
}

func (s *Server) handleRequestGrab(w http.ResponseWriter, r *http.Request, id int64) error {
	body, err := decodeJSON[requestGrabRequest](r)
	if err != nil {
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
	if err := s.checkGrabTarget(r.Context(), request, body); err != nil {
		return err
	}
	candidates, err := s.store.Decisions().Candidates(r.Context(), body.SubjectType, body.SubjectID)
	if err != nil {
		return err
	}
	if !slices.ContainsFunc(candidates, func(candidate model.CandidateEvaluation) bool {
		return candidate.ReleaseID == body.ReleaseID && candidate.Accepted
	}) {
		return Conflict("only an accepted candidate from the last search can be selected")
	}
	grab, err := s.engine.ManualGrab(r.Context(), body.SubjectType, body.SubjectID, body.ReleaseID)
	var lowDisk *engine.LowDiskError
	if errors.As(err, &lowDisk) {
		return InsufficientStorage("%s", lowDisk.Detail).WithCause(err)
	}
	if err != nil {
		return Conflict("release could not be grabbed").WithCause(err)
	}
	return reply(w, r, http.StatusCreated, grab)
}

// checkGrabTarget verifies the chosen subject belongs to the request and still awaits a release.
func (s *Server) checkGrabTarget(ctx context.Context, request model.MediaRequest, body requestGrabRequest) error {
	if request.MediaType == "movie" {
		movie, err := s.store.Movies().Get(ctx, request.SubjectID)
		if err != nil || movie.TMDBID != request.TMDBID || body.SubjectType != model.SubjectMovie || body.SubjectID != movie.ID {
			return BadRequest("release target does not belong to this request")
		}
		if movie.State == model.StateImported {
			return Conflict("requested movie is already imported")
		}
		return nil
	}

	series, err := s.store.Series().Get(ctx, request.SubjectID)
	if err != nil || series.TMDBID != request.TMDBID {
		return Conflict("requested subject is missing")
	}
	episode, err := s.store.Episodes().Get(ctx, body.SubjectID)
	if err != nil || body.SubjectType != model.SubjectEpisode || episode.SeriesID != series.ID || !request.IncludesEpisode(episode) {
		return BadRequest("release target does not belong to this request")
	}
	if episode.State == model.StateImported || episode.State == model.StateUnmonitored {
		return Conflict("episode is not awaiting a release")
	}
	return nil
}
