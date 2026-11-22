package api

import (
	"net/http"
	"strings"

	"github.com/TechXTT/reelay/internal/store"
)

// pluginCapabilitiesHeader lists optional plugin features. It is a header, not
// a body field, because the sync body is decoded strictly and older Reelay
// servers must keep accepting newer plugins.
const pluginCapabilitiesHeader = "X-Reelay-Plugin-Capabilities"

func pluginSupports(r *http.Request, capability string) bool {
	for _, value := range strings.Split(r.Header.Get(pluginCapabilitiesHeader), ",") {
		if strings.EqualFold(strings.TrimSpace(value), capability) {
			return true
		}
	}
	return false
}

type episodeConfirmationsRequest struct {
	ServerID string                      `json:"server_id"`
	UserID   string                      `json:"user_id"`
	Episodes []store.EpisodeConfirmation `json:"episodes"`
}

func (s *Server) handleEpisodeConfirmationCandidates(w http.ResponseWriter, r *http.Request) error {
	serverID, userID, err := userScope(r)
	if err != nil {
		return err
	}
	items, err := s.store.Recommendations().PendingEpisodeConfirmations(r.Context(), serverID, userID)
	if err != nil {
		return err
	}
	return reply(w, r, http.StatusOK, map[string]any{"items": items})
}

func (s *Server) handleEpisodeConfirmations(w http.ResponseWriter, r *http.Request) error {
	body, err := decodeJSON[episodeConfirmationsRequest](r)
	if err != nil {
		return err
	}
	if strings.TrimSpace(body.ServerID) == "" || strings.TrimSpace(body.UserID) == "" || len(body.Episodes) > store.EpisodeConfirmationLimit {
		return BadRequest("episode confirmations need server_id, user_id, and at most %d episodes", store.EpisodeConfirmationLimit)
	}
	for i, episode := range body.Episodes {
		if episode.RequestID <= 0 || episode.Season < 0 || episode.Number <= 0 || strings.TrimSpace(episode.ItemID) == "" {
			return BadRequest("episode at index %d needs request_id, season, number, and item_id", i)
		}
	}
	confirmed, err := s.store.Recommendations().ConfirmEpisodes(r.Context(), body.ServerID, body.UserID, body.Episodes)
	if err != nil {
		return err
	}
	if confirmed > 0 {
		s.publishRequestsUpdated(body.ServerID, body.UserID)
	}
	return reply(w, r, http.StatusOK, map[string]any{"confirmed": confirmed})
}
