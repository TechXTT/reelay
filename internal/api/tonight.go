package api

import (
	"errors"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/TechXTT/reelay/internal/store"
)

const (
	tonightDefaultLimit = 60
	tonightMaxLimit     = 100
)

type jellyfinUserItemsRequest struct {
	ServerID  string   `json:"server_id"`
	UserID    string   `json:"user_id"`
	SyncToken string   `json:"sync_token"`
	Complete  bool     `json:"complete"`
	ItemIDs   []string `json:"item_ids"`
}

// handleJellyfinUserItems receives one batch of the catalog items a Jellyfin
// user can access. Completing the pass replaces the user's visible set at once.
// An unknown user is 409, not 404: the plugin reads 404 as "this Reelay does
// not support per-user libraries".
func (s *Server) handleJellyfinUserItems(w http.ResponseWriter, r *http.Request) error {
	var visible int

	req, err := decodeJSON[jellyfinUserItemsRequest](r)
	if err != nil {
		return err
	}
	if len(req.ItemIDs) > 500 {
		return BadRequest("user item batch exceeds 500 items")
	}
	if strings.TrimSpace(req.ServerID) == "" || strings.TrimSpace(req.UserID) == "" || strings.TrimSpace(req.SyncToken) == "" {
		return BadRequest("server_id, user_id and sync_token are required")
	}
	staged, err := s.store.Tonight().StageUserItems(r.Context(), req.ServerID, req.UserID, req.SyncToken, req.ItemIDs)
	if errors.Is(err, store.ErrNotFound) {
		return Conflict("Jellyfin user %s has not been synchronized", req.UserID)
	}
	if err != nil {
		return err
	}
	if req.Complete {
		visible, err = s.store.Tonight().CompleteUserItems(r.Context(), req.ServerID, req.UserID, req.SyncToken)
		if errors.Is(err, store.ErrNotFound) {
			return Conflict("Jellyfin user %s has not been synchronized", req.UserID)
		}
		if err != nil {
			return err
		}
	}
	return reply(w, r, http.StatusOK, map[string]any{"staged": staged, "completed": req.Complete, "visible": visible})
}

// linkTonightItems adds Jellyfin play and poster links to playable items when
// the item's server has a configured base URL.
func (s *Server) linkTonightItems(items []store.TonightItem) {
	for i := range items {
		base := strings.TrimRight(s.cfg.Availability.JellyfinServers[items[i].ServerID], "/")
		if base == "" || !items[i].Playable {
			continue
		}
		items[i].JellyfinURL = base + "/web/index.html#!/details?id=" + url.QueryEscape(items[i].ItemID)
		items[i].PosterURL = base + "/Items/" + url.PathEscape(items[i].ItemID) + "/Images/Primary?fillHeight=360&quality=90"
	}
}

type tonightResponse struct {
	Supported       bool                `json:"supported"`
	LibrarySyncedAt *time.Time          `json:"library_synced_at,omitempty"`
	Items           []store.TonightItem `json:"items"`
	Limit           int                 `json:"limit"`
	Offset          int                 `json:"offset"`
	HasMore         bool                `json:"has_more"`
}

// handleTonight lists the titles one user can play now. Every filter comes from
// the query string of this request; nothing is stored. Without per-user data
// from the plugin it reports supported=false and no items, never the
// server-wide catalog.
func (s *Server) handleTonight(w http.ResponseWriter, r *http.Request) error {
	var filter = store.TonightFilter{Limit: tonightDefaultLimit}
	var query = r.URL.Query()

	serverID, userID, err := userScope(r)
	if err != nil {
		return err
	}
	filter.ServerID, filter.UserID = serverID, userID
	filter.MediaType = query.Get("media_type")
	if filter.MediaType != "" && filter.MediaType != "movie" && filter.MediaType != "series" {
		return BadRequest("media_type must be movie or series")
	}
	if raw := query.Get("max_runtime"); raw != "" {
		filter.MaxRuntime, err = strconv.Atoi(raw)
		if err != nil || filter.MaxRuntime < 1 || filter.MaxRuntime > 1440 {
			return BadRequest("max_runtime must be a whole number of minutes from 1 to 1440")
		}
	}
	if raw := query.Get("unfinished"); raw != "" && raw != "true" && raw != "false" {
		return BadRequest("unfinished must be true or false")
	}
	filter.Unfinished = query.Get("unfinished") == "true"
	if raw := query.Get("limit"); raw != "" {
		filter.Limit, err = strconv.Atoi(raw)
		if err != nil || filter.Limit < 1 || filter.Limit > tonightMaxLimit {
			return BadRequest("limit must be from 1 to %d", tonightMaxLimit)
		}
	}
	if raw := query.Get("offset"); raw != "" {
		filter.Offset, err = strconv.Atoi(raw)
		if err != nil || filter.Offset < 0 || filter.Offset > 1000000 {
			return BadRequest("offset must be between 0 and 1000000")
		}
	}
	syncedAt, err := s.store.Tonight().LibrarySyncedAt(r.Context(), serverID, userID)
	if err != nil {
		return err
	}
	response := tonightResponse{Supported: syncedAt != nil, LibrarySyncedAt: syncedAt, Items: []store.TonightItem{}, Limit: filter.Limit, Offset: filter.Offset}
	if syncedAt == nil {
		return reply(w, r, http.StatusOK, response)
	}
	response.Items, response.HasMore, err = s.store.Tonight().List(r.Context(), filter)
	if err != nil {
		return err
	}
	s.linkTonightItems(response.Items)
	return reply(w, r, http.StatusOK, response)
}

func (s *Server) handleSavedForLater(w http.ResponseWriter, r *http.Request) error {
	serverID, userID, err := userScope(r)
	if err != nil {
		return err
	}
	syncedAt, err := s.store.Tonight().LibrarySyncedAt(r.Context(), serverID, userID)
	if err != nil {
		return err
	}
	items, err := s.store.Tonight().Saved(r.Context(), serverID, userID)
	if err != nil {
		return err
	}
	s.linkTonightItems(items)
	return reply(w, r, http.StatusOK, map[string]any{"supported": syncedAt != nil, "items": items, "limit": store.SavedForLaterLimit})
}

type saveForLaterRequest struct {
	ServerID string `json:"server_id"`
	UserID   string `json:"user_id"`
	ItemID   string `json:"item_id"`
}

func (s *Server) handleSaveForLater(w http.ResponseWriter, r *http.Request) error {
	req, err := decodeJSON[saveForLaterRequest](r)
	if err != nil {
		return err
	}
	serverID, userID, err := scopeFor(r, req.ServerID, req.UserID)
	if err != nil {
		return err
	}
	itemID := strings.TrimSpace(req.ItemID)
	if itemID == "" || len(itemID) > 64 {
		return BadRequest("item_id is required and at most 64 characters")
	}
	created, err := s.store.Tonight().Save(r.Context(), serverID, userID, itemID)
	if errors.Is(err, store.ErrNotFound) {
		return NotFound("title is not in this user's Jellyfin libraries")
	}
	if errors.Is(err, store.ErrSavedForLaterFull) {
		return Conflict("Save for later holds at most %d titles; remove one first", store.SavedForLaterLimit)
	}
	if err != nil {
		return err
	}
	status := http.StatusOK
	if created {
		status = http.StatusCreated
	}
	return reply(w, r, status, map[string]any{"item_id": itemID, "created": created})
}

func (s *Server) handleUnsaveForLater(w http.ResponseWriter, r *http.Request) error {
	serverID, userID, err := userScope(r)
	if err != nil {
		return err
	}
	removed, err := s.store.Tonight().Unsave(r.Context(), serverID, userID, r.PathValue("item_id"))
	if err != nil {
		return err
	}
	return reply(w, r, http.StatusOK, map[string]any{"removed": removed})
}
