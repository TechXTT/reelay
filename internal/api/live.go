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
	if _, err := fmt.Fprint(w, ": connected\n\n"); err != nil {
		return nil
	}
	_ = controller.Flush()
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
			if _, err := fmt.Fprintf(w, "event: %s\ndata: %s\n\n", event.Type, payload); err != nil {
				return nil
			}
			_ = controller.Flush()
		case <-heartbeat.C:
			if _, err := fmt.Fprint(w, ": heartbeat\n\n"); err != nil {
				return nil
			}
			_ = controller.Flush()
		}
	}
}

func (s *Server) handleMetadataSearch(w http.ResponseWriter, r *http.Request) error {
	query := strings.TrimSpace(r.URL.Query().Get("q"))
	if query == "" {
		return BadRequest("q is required")
	}
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
		writeJSON(w, s.logFor(r), http.StatusOK, map[string]any{"items": values})
	case "series":
		if s.series == nil {
			return Unavailable("series metadata provider unavailable")
		}
		values, err := s.series.SearchSeries(r.Context(), query)
		if err != nil {
			return Unavailable("series metadata search failed").WithCause(err)
		}
		writeJSON(w, s.logFor(r), http.StatusOK, map[string]any{"items": values})
	default:
		return BadRequest("type must be movie or series")
	}
	return nil
}
