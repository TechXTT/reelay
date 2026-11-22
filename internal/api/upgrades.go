package api

import (
	"errors"
	"net/http"
	"strconv"

	"github.com/TechXTT/reelay/internal/engine"
	"github.com/TechXTT/reelay/internal/model"
	"github.com/TechXTT/reelay/internal/store"
)

const (
	upgradeListDefaultLimit = 100
	upgradeListMaxLimit     = 500
)

// pageParam reads an optional non-negative integer query parameter.
func pageParam(r *http.Request, name string, fallback, maximum int) (int, error) {
	raw := r.URL.Query().Get(name)
	if raw == "" {
		return fallback, nil
	}
	value, err := strconv.Atoi(raw)
	if err != nil || value < 0 || value > maximum {
		return 0, BadRequest("%s must be an integer between 0 and %d", name, maximum)
	}
	return value, nil
}

// handleUpgrades lists imported items below their profile's upgrade cutoff,
// including ones whose upgrade is already running.
func (s *Server) handleUpgrades(w http.ResponseWriter, r *http.Request) error {
	if s.engine == nil {
		return Unavailable("engine is unavailable")
	}
	limit, err := pageParam(r, "limit", upgradeListDefaultLimit, upgradeListMaxLimit)
	if err != nil {
		return err
	}
	if limit == 0 {
		return BadRequest("limit must be at least 1")
	}
	offset, err := pageParam(r, "offset", 0, 1000000)
	if err != nil {
		return err
	}
	status, err := s.engine.Upgrades(r.Context())
	if err != nil {
		return err
	}
	total := len(status.Items)
	items := status.Items[min(offset, total):min(offset+limit, total)]
	if items == nil {
		items = []engine.UpgradeItem{}
	}
	return reply(w, r, http.StatusOK, map[string]any{"items": items, "limit": limit, "offset": offset,
		"total": total, "has_more": offset+len(items) < total, "truncated": status.Truncated,
		"enabled": status.Enabled, "max_upgrade_grabs": status.MaxGrabs, "inflight": status.Inflight})
}

// handleUpgradeSearch starts an upgrade search for one imported item now.
func (s *Server) handleUpgradeSearch(w http.ResponseWriter, r *http.Request) error {
	if s.engine == nil {
		return Unavailable("engine is unavailable")
	}
	subject := model.SubjectType(r.PathValue("subject"))
	if !subject.ValidItem() {
		return BadRequest("subject must be movie or episode")
	}
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil || id <= 0 {
		return BadRequest("invalid id %q", r.PathValue("id"))
	}
	err = s.engine.StartUpgrade(r.Context(), subject, id)
	var notUpgradable *engine.NotUpgradableError
	switch {
	case err == nil:
		return reply(w, r, http.StatusAccepted, map[string]any{"queued": true})
	case errors.Is(err, store.ErrNotFound):
		return NotFound("%s %d not found", subject, id)
	case errors.Is(err, engine.ErrUpgradesDisabled), errors.Is(err, engine.ErrUpgradeLimit):
		return Conflict("%s", err.Error())
	case errors.As(err, &notUpgradable):
		return Conflict("cannot upgrade: %s", notUpgradable.Reason)
	default:
		return err
	}
}
