package api

import (
	"net/http"
	"strings"

	"github.com/TechXTT/reelay/internal/store"
)

func (s *Server) handleRecommendationPreferences(w http.ResponseWriter, r *http.Request) error {
	serverID, userID, err := userScope(r)
	if err != nil {
		return err
	}
	known, err := s.store.Recommendations().UserExists(r.Context(), serverID, userID)
	if err != nil {
		return err
	}
	if !known {
		return NotFound("Jellyfin user not found")
	}
	if r.Method == http.MethodPut {
		preferences, err := decodeJSON[store.RecommendationPreferences](r)
		if err != nil {
			return err
		}
		if err := s.store.Recommendations().SavePreferences(r.Context(), serverID, userID, preferences); err != nil {
			return BadRequest("invalid preferences").WithCause(err)
		}
	}
	preferences, err := s.store.Recommendations().Preferences(r.Context(), serverID, userID)
	if err != nil {
		return err
	}
	return reply(w, r, http.StatusOK, preferences)
}

func (s *Server) handleRecommendationHistory(w http.ResponseWriter, r *http.Request) error {
	serverID := strings.TrimSpace(r.URL.Query().Get("server_id"))
	userID := strings.TrimSpace(r.URL.Query().Get("user_id"))
	mediaType := r.URL.Query().Get("media_type")
	if serverID == "" || userID == "" || (mediaType != "movie" && mediaType != "series") {
		return BadRequest("server_id, user_id and media_type are required")
	}
	values, err := s.store.Recommendations().List(r.Context(), serverID, userID, mediaType, "dismissed", 100, 0)
	if err != nil {
		return err
	}
	ratings, err := s.store.Recommendations().HistoryRatings(r.Context(), serverID, userID, mediaType)
	if err != nil {
		return err
	}
	return reply(w, r, http.StatusOK, map[string]any{"items": values, "ratings": ratings})
}
