package api

import (
	"net/http"
	"strings"

	"github.com/TechXTT/reelay/internal/model"
)

func (s *Server) handleRequests(w http.ResponseWriter, r *http.Request) error {
	serverID := strings.TrimSpace(r.URL.Query().Get("server_id"))
	userID := strings.TrimSpace(r.URL.Query().Get("user_id"))
	if serverID == "" || userID == "" {
		return BadRequest("server_id and user_id are required")
	}
	values, err := s.store.Requests().List(r.Context(), serverID, userID)
	if err != nil {
		return err
	}
	writeJSON(w, s.logFor(r), http.StatusOK, map[string]any{"items": values, "limit": model.MediaRequestListLimit})
	return nil
}
