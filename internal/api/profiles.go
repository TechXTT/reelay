package api

import (
	"net/http"

	"github.com/TechXTT/reelay/internal/model"
)

func (s *Server) handleProfileCreate(w http.ResponseWriter, r *http.Request) error {
	profile, err := decodeJSON[model.QualityProfile](r)
	if err != nil {
		return err
	}
	profile.ID = 0
	created, err := s.store.Profiles().Create(r.Context(), profile)
	if err != nil {
		return BadRequest("invalid quality profile").WithCause(err)
	}
	return reply(w, r, http.StatusCreated, created)
}

func (s *Server) handleProfilePatch(w http.ResponseWriter, r *http.Request, id int64) error {
	profile, err := decodeJSON[model.QualityProfile](r)
	if err != nil {
		return err
	}
	profile.ID = id
	updated, err := s.store.Profiles().Update(r.Context(), profile)
	if err != nil {
		return BadRequest("invalid quality profile").WithCause(err)
	}
	return reply(w, r, http.StatusOK, updated)
}

func (s *Server) handleProfileDelete(w http.ResponseWriter, r *http.Request, id int64) error {
	if err := s.store.Profiles().Delete(r.Context(), id); err != nil {
		return Conflict("profile is in use, default, or missing").WithCause(err)
	}
	w.WriteHeader(http.StatusNoContent)
	return nil
}
