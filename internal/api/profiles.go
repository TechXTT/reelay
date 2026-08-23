package api

import (
	"net/http"

	"github.com/TechXTT/reelay/internal/model"
)

func (s *Server) handleProfilesList(w http.ResponseWriter, r *http.Request) error {
	values, err := s.store.Profiles().List(r.Context())
	if err != nil {
		return err
	}
	writeJSON(w, s.logFor(r), http.StatusOK, map[string]any{"items": values})
	return nil
}

func (s *Server) handleProfileCreate(w http.ResponseWriter, r *http.Request) error {
	var profile model.QualityProfile
	if err := decodeBody(r, &profile); err != nil {
		return err
	}
	profile.ID = 0
	created, err := s.store.Profiles().Create(r.Context(), profile)
	if err != nil {
		return BadRequest("invalid quality profile").WithCause(err)
	}
	writeJSON(w, s.logFor(r), http.StatusCreated, created)
	return nil
}

func (s *Server) handleProfilePatch(w http.ResponseWriter, r *http.Request) error {
	id, err := pathID(r)
	if err != nil {
		return err
	}
	var profile model.QualityProfile
	if err := decodeBody(r, &profile); err != nil {
		return err
	}
	profile.ID = id
	updated, err := s.store.Profiles().Update(r.Context(), profile)
	if err != nil {
		return BadRequest("invalid quality profile").WithCause(err)
	}
	writeJSON(w, s.logFor(r), http.StatusOK, updated)
	return nil
}

func (s *Server) handleProfileDelete(w http.ResponseWriter, r *http.Request) error {
	id, err := pathID(r)
	if err != nil {
		return err
	}
	if err := s.store.Profiles().Delete(r.Context(), id); err != nil {
		return Conflict("profile is in use, default, or missing").WithCause(err)
	}
	w.WriteHeader(http.StatusNoContent)
	return nil
}
