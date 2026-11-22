package api

import (
	"net/http"
	"strings"
)

func (s *Server) handleChangeDirection(w http.ResponseWriter, r *http.Request) error {
	serverID, userID, err := userScope(r)
	if err != nil {
		return err
	}
	req, err := decodeJSON[struct {
		MediaType string `json:"media_type"`
		Genre     string `json:"away_from_genre"`
		Seen      []int  `json:"seen"`
	}](r)
	if err != nil {
		return err
	}
	req.Genre = strings.TrimSpace(req.Genre)
	if (req.MediaType != "movie" && req.MediaType != "series") || len(req.Genre) > 100 || len(req.Seen) > 200 {
		return BadRequest("invalid media type, genre, or number of previously shown titles")
	}
	if s.recommendations == nil || !s.cfg.Recommendations.Enabled {
		return Unavailable("recommendations are disabled")
	}
	picks, err := s.recommendations.ChangeDirection(r.Context(), serverID, userID, req.MediaType, req.Genre, req.Seen)
	if err != nil {
		return Unavailable("could not load a different set of picks").WithCause(err)
	}
	return reply(w, r, http.StatusOK, map[string]any{"items": picks})
}
