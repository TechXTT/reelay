package api

import (
	"net/http"

	"github.com/TechXTT/reelay/internal/model"
)

type movieCreateRequest struct {
	Query      string `json:"query"`
	TMDBID     int    `json:"tmdb_id"`
	Year       int    `json:"year"`
	ProfileID  int64  `json:"profile_id"`
	RootFolder string `json:"root_folder"`
}

func (s *Server) handleMovieCreate(w http.ResponseWriter, r *http.Request) error {
	req, err := decodeJSON[movieCreateRequest](r)
	if err != nil {
		return err
	}
	if s.movies == nil {
		return Unavailable("movie metadata provider is unavailable")
	}
	values, err := s.movies.SearchMovies(r.Context(), req.Query, req.Year)
	if err != nil || len(values) == 0 {
		return BadRequest("movie not found").WithCause(err)
	}
	selected := values[0]
	if req.TMDBID > 0 {
		if details, detailsErr := s.movies.MovieDetails(r.Context(), req.TMDBID); detailsErr == nil {
			selected = details
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
		req.RootFolder = s.cfg.Library.MovieRoot
	}
	created, err := s.store.Movies().Create(r.Context(), model.Movie{Title: selected.Title,
		Year: selected.Year, TMDBID: selected.TMDBID, IMDBID: selected.IMDBID,
		RuntimeMinutes: selected.RuntimeMinutes, ProfileID: req.ProfileID,
		RootFolder: req.RootFolder, State: model.StateWanted}, "movie added through API")
	if err != nil {
		return Conflict("could not add movie").WithCause(err)
	}
	return reply(w, r, http.StatusCreated, created)
}

func (s *Server) handleMovieGet(w http.ResponseWriter, r *http.Request, id int64) error {
	item, err := s.store.Movies().Get(r.Context(), id)
	if err != nil {
		return NotFound("movie %d not found", id)
	}
	return reply(w, r, http.StatusOK, item)
}

type moviePatchRequest struct {
	ProfileID *int64 `json:"profile_id"`
}

func (s *Server) handleMoviePatch(w http.ResponseWriter, r *http.Request, id int64) error {
	item, err := s.store.Movies().Get(r.Context(), id)
	if err != nil {
		return NotFound("movie %d not found", id)
	}
	req, err := decodeJSON[moviePatchRequest](r)
	if err != nil {
		return err
	}
	if req.ProfileID != nil {
		item.ProfileID = *req.ProfileID
	}
	item, err = s.store.Movies().Update(r.Context(), item)
	if err != nil {
		return BadRequest("invalid movie update").WithCause(err)
	}
	return reply(w, r, http.StatusOK, item)
}
