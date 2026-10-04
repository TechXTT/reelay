package api

import (
	"net/http"
	"strings"

	"github.com/TechXTT/reelay/internal/store"
)

func (s *Server) handleRecommendationPreferences(w http.ResponseWriter, r *http.Request) error {
	var serverID = strings.TrimSpace(r.URL.Query().Get("server_id"))
	var userID = strings.TrimSpace(r.URL.Query().Get("user_id"))
	var preferences store.RecommendationPreferences

	if serverID == "" || userID == "" {
		return BadRequest("server_id and user_id are required")
	}
	users, err := s.store.Recommendations().Users(r.Context())
	if err != nil {
		return err
	}
	found := false
	for _, user := range users {
		if user.ServerID == serverID && user.UserID == userID {
			found = true
		}
	}
	if !found {
		return NotFound("Jellyfin user not found")
	}
	if r.Method == http.MethodPut {
		if err := decodeBody(r, &preferences); err != nil {
			return err
		}
		if err := s.store.Recommendations().SavePreferences(r.Context(), serverID, userID, preferences); err != nil {
			return BadRequest("invalid preferences").WithCause(err)
		}
	}
	preferences, err = s.store.Recommendations().Preferences(r.Context(), serverID, userID)
	if err != nil {
		return err
	}
	writeJSON(w, s.logFor(r), http.StatusOK, preferences)
	return nil
}

func (s *Server) handleRecommendationHistory(w http.ResponseWriter, r *http.Request) error {
	var serverID = strings.TrimSpace(r.URL.Query().Get("server_id"))
	var userID = strings.TrimSpace(r.URL.Query().Get("user_id"))
	var mediaType = r.URL.Query().Get("media_type")

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
	writeJSON(w, s.logFor(r), http.StatusOK, map[string]any{"items": values, "ratings": ratings})
	return nil
}
