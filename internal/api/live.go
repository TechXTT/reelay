package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"
)

func (s *Server) handleEvents(w http.ResponseWriter, r *http.Request) error {
	if s.engine == nil {
		return Unavailable("engine is unavailable")
	}
	updates, cancel, ok := s.engine.Events().Subscribe()
	if !ok {
		return Unavailable("SSE client limit reached")
	}
	defer cancel()
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("X-Accel-Buffering", "no")
	controller := http.NewResponseController(w)
	_ = controller.SetWriteDeadline(time.Time{})

	// send writes one frame and reports whether the client is still there.
	send := func(format string, args ...any) bool {
		if _, err := fmt.Fprintf(w, format, args...); err != nil {
			return false
		}
		_ = controller.Flush()
		return true
	}

	if !send(": connected\n\n") {
		return nil
	}
	heartbeat := time.NewTicker(20 * time.Second)
	defer heartbeat.Stop()
	for {
		select {
		case <-r.Context().Done():
			return nil
		case event := <-updates:
			payload, err := json.Marshal(event)
			if err != nil {
				continue
			}
			if !send("event: %s\ndata: %s\n\n", event.Type, payload) {
				return nil
			}
		case <-heartbeat.C:
			if !send(": heartbeat\n\n") {
				return nil
			}
		}
	}
}

func (s *Server) handleMetadataSearch(w http.ResponseWriter, r *http.Request) error {
	query := strings.TrimSpace(r.URL.Query().Get("q"))
	if query == "" {
		return BadRequest("q is required")
	}
	var items any
	switch r.URL.Query().Get("type") {
	case "movie":
		if s.movies == nil {
			return Unavailable("movie metadata provider unavailable")
		}
		year, _ := strconv.Atoi(r.URL.Query().Get("year"))
		values, err := s.movies.SearchMovies(r.Context(), query, year)
		if err != nil {
			return Unavailable("movie metadata search failed").WithCause(err)
		}
		items = values
	case "series":
		if s.series == nil {
			return Unavailable("series metadata provider unavailable")
		}
		values, err := s.series.SearchSeries(r.Context(), query)
		if err != nil {
			return Unavailable("series metadata search failed").WithCause(err)
		}
		items = values
	default:
		return BadRequest("type must be movie or series")
	}
	return reply(w, r, http.StatusOK, map[string]any{"items": items})
}
