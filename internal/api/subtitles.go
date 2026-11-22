package api

import (
	"errors"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"

	"github.com/TechXTT/reelay/internal/store"
)

const (
	subtitleListDefaultLimit = 100
	subtitleListMaxLimit     = 500
)

// Jellyfin's subtitle providers take ISO 639-2 three-letter codes, the same
// codes a library's "download languages" setting stores.
var subtitleLanguagePattern = regexp.MustCompile(`^[a-z]{3}$`)

type subtitleCoverageRequest struct {
	ServerID string `json:"server_id"`
	// AuditLibrary names the one library a per-library pass covers; empty is a full pass.
	AuditLibrary string `json:"audit_library"`
	// AuditError, sent with complete, reports a pass that could not finish.
	AuditError string                  `json:"audit_error"`
	SyncToken  string                  `json:"sync_token"`
	Complete   bool                    `json:"complete"`
	Items      []store.SubtitleSource  `json:"items"`
	Providers  []string                `json:"providers"`
	Libraries  []store.SubtitleLibrary `json:"libraries"`
}

type subtitleLeaseRequest struct {
	ServerID string `json:"server_id"`
}

type subtitleResultsRequest struct {
	ServerID     string                 `json:"server_id"`
	Results      []store.SubtitleResult `json:"results"`
	PauseSeconds int                    `json:"pause_seconds"`
}

type subtitleAuditRequest struct {
	ServerID string `json:"server_id"`
	// Library limits the pass to one Jellyfin library; empty audits every library.
	Library string `json:"library"`
}

type subtitleFollowRequest struct {
	ServerID string `json:"server_id"`
	Language string `json:"language"`
}

type subtitleBatchRequest struct {
	ServerID       string   `json:"server_id"`
	Library        string   `json:"library"`
	MediaType      string   `json:"media_type"`
	Language       string   `json:"language"`
	MediaSourceIDs []string `json:"media_source_ids"`
	Limit          int      `json:"limit"`
}

type subtitleJobActionRequest struct {
	Action     string `json:"action"`
	SubtitleID string `json:"subtitle_id"`
}

func validSubtitleSource(source store.SubtitleSource) bool {
	if strings.TrimSpace(source.MediaSourceID) == "" || strings.TrimSpace(source.ItemID) == "" || strings.TrimSpace(source.Fingerprint) == "" {
		return false
	}
	if source.MediaType != "movie" && source.MediaType != "episode" {
		return false
	}
	return source.Status == store.SubtitlePresent || source.Status == store.SubtitleMissing || source.Status == store.SubtitleUnknown
}

func requireServerID(serverID string) error {
	if strings.TrimSpace(serverID) == "" {
		return BadRequest("server_id is required")
	}
	return nil
}

// handleSubtitleCoverage receives one batch of the plugin's coverage pass.
// Completing the pass removes sources it did not see and stores the audit.
func (s *Server) handleSubtitleCoverage(w http.ResponseWriter, r *http.Request) error {
	var removed int

	req, err := decodeJSON[subtitleCoverageRequest](r)
	if err != nil {
		return err
	}
	if strings.TrimSpace(req.ServerID) == "" || strings.TrimSpace(req.SyncToken) == "" {
		return BadRequest("server_id and sync_token are required")
	}
	if len(req.Items) > store.SubtitleCoverageBatchLimit {
		return BadRequest("coverage batch exceeds %d items", store.SubtitleCoverageBatchLimit)
	}
	for i, item := range req.Items {
		if !validSubtitleSource(item) {
			return BadRequest("item at index %d needs media_source_id, item_id, fingerprint, a movie or episode media_type, and a present, missing or unknown status", i)
		}
	}
	staged, queued, err := s.store.Subtitles().StageCoverage(r.Context(), req.ServerID, req.SyncToken, req.Items)
	if err != nil {
		return err
	}
	if req.Complete {
		audit := store.SubtitleAudit{Library: req.AuditLibrary, Providers: req.Providers, Libraries: req.Libraries}
		removed, err = s.store.Subtitles().CompleteCoverage(r.Context(), req.ServerID, req.SyncToken, audit, truncateText(req.AuditError, 2000))
		if err != nil {
			return err
		}
	}
	return reply(w, r, http.StatusOK, map[string]any{"staged": staged, "queued": queued, "removed": removed, "completed": req.Complete})
}

// handleSubtitleLease hands the plugin its due subtitle jobs.
func (s *Server) handleSubtitleLease(w http.ResponseWriter, r *http.Request) error {
	req, err := decodeJSON[subtitleLeaseRequest](r)
	if err != nil {
		return err
	}
	if err = requireServerID(req.ServerID); err != nil {
		return err
	}
	lease, err := s.store.Subtitles().Lease(r.Context(), req.ServerID, store.SubtitleLeaseLimit)
	if err != nil {
		return err
	}
	return reply(w, r, http.StatusOK, lease)
}

// handleSubtitleResults records the plugin's outcomes for leased jobs.
func (s *Server) handleSubtitleResults(w http.ResponseWriter, r *http.Request) error {
	var outcomes = map[string]bool{
		store.SubtitleDownloaded: true, store.SubtitleConfirmed: true, store.SubtitleCovered: true, store.SubtitleStale: true,
		store.SubtitleNoMatch: true, store.SubtitleWeakMatch: true, store.SubtitleNoProvider: true, store.SubtitleRateLimited: true,
		store.SubtitleFailed: true, store.SubtitleNotVisible: true,
	}

	req, err := decodeJSON[subtitleResultsRequest](r)
	if err != nil {
		return err
	}
	if err = requireServerID(req.ServerID); err != nil {
		return err
	}
	if len(req.Results) > store.SubtitleLeaseLimit*4 || req.PauseSeconds < 0 {
		return BadRequest("results need at most %d entries and a non-negative pause_seconds", store.SubtitleLeaseLimit*4)
	}
	for i, result := range req.Results {
		if result.JobID <= 0 || strings.TrimSpace(result.LeaseToken) == "" || !outcomes[result.Outcome] {
			return BadRequest("result at index %d needs job_id, lease_token and a known outcome", i)
		}
		if result.Coverage != nil && !validSubtitleSource(*result.Coverage) {
			return BadRequest("result at index %d has invalid coverage", i)
		}
	}
	applied, err := s.store.Subtitles().Report(r.Context(), req.ServerID, req.Results, time.Duration(req.PauseSeconds)*time.Second)
	if err != nil {
		return err
	}
	return reply(w, r, http.StatusOK, map[string]any{"applied": applied})
}

// handleSubtitles summarizes subtitle coverage and download work for a server.
func (s *Server) handleSubtitles(w http.ResponseWriter, r *http.Request) error {
	overview, err := s.store.Subtitles().Overview(r.Context(), r.URL.Query().Get("server_id"))
	if err != nil {
		return err
	}
	return reply(w, r, http.StatusOK, overview)
}

// handleSubtitleCoverageList pages through a server's coverage rows.
func (s *Server) handleSubtitleCoverageList(w http.ResponseWriter, r *http.Request) error {
	var query = r.URL.Query()
	var filter = store.SubtitleCoverageFilter{ServerID: query.Get("server_id"), Status: query.Get("status"), MediaType: query.Get("media_type"), Library: query.Get("library")}
	var err error

	if err = requireServerID(filter.ServerID); err != nil {
		return err
	}
	if filter.Status != "" && filter.Status != store.SubtitlePresent && filter.Status != store.SubtitleMissing && filter.Status != store.SubtitleUnknown {
		return BadRequest("status must be present, missing or unknown")
	}
	if filter.MediaType != "" && filter.MediaType != "movie" && filter.MediaType != "episode" {
		return BadRequest("media_type must be movie or episode")
	}
	if filter.Limit, err = pageParam(r, "limit", subtitleListDefaultLimit, subtitleListMaxLimit); err != nil {
		return err
	}
	if filter.Offset, err = pageParam(r, "offset", 0, 1000000); err != nil {
		return err
	}
	items, total, err := s.store.Subtitles().ListCoverage(r.Context(), filter)
	if err != nil {
		return err
	}
	// Lets the operator check a sample against the player's subtitle selector.
	if base := strings.TrimRight(s.cfg.Availability.JellyfinServers[filter.ServerID], "/"); base != "" {
		for i := range items {
			items[i].JellyfinURL = base + "/web/index.html#!/details?id=" + url.QueryEscape(items[i].MediaSourceID)
		}
	}
	return reply(w, r, http.StatusOK, map[string]any{"items": items, "total": total, "limit": filter.Limit, "offset": filter.Offset,
		"has_more": filter.Offset+len(items) < total})
}

// handleSubtitleJobs pages through a server's subtitle jobs.
func (s *Server) handleSubtitleJobs(w http.ResponseWriter, r *http.Request) error {
	var serverID = r.URL.Query().Get("server_id")
	var state = r.URL.Query().Get("state")

	if err := requireServerID(serverID); err != nil {
		return err
	}
	limit, err := pageParam(r, "limit", subtitleListDefaultLimit, subtitleListMaxLimit)
	if err != nil {
		return err
	}
	offset, err := pageParam(r, "offset", 0, 1000000)
	if err != nil {
		return err
	}
	items, total, err := s.store.Subtitles().ListJobs(r.Context(), serverID, state, limit, offset)
	if err != nil {
		return err
	}
	return reply(w, r, http.StatusOK, map[string]any{"items": items, "total": total, "limit": limit, "offset": offset,
		"has_more": offset+len(items) < total})
}

// handleSubtitleAudit asks the plugin to run a coverage pass on its next check.
func (s *Server) handleSubtitleAudit(w http.ResponseWriter, r *http.Request) error {
	req, err := decodeJSON[subtitleAuditRequest](r)
	if err != nil {
		return err
	}
	if err = requireServerID(req.ServerID); err != nil {
		return err
	}
	if err = s.store.Subtitles().RequestAudit(r.Context(), req.ServerID, strings.TrimSpace(req.Library)); err != nil {
		return err
	}
	return reply(w, r, http.StatusAccepted, map[string]any{"requested": true})
}

// handleSubtitleFollow sets or clears the language queued for new media that
// Jellyfin probes without any subtitles.
func (s *Server) handleSubtitleFollow(w http.ResponseWriter, r *http.Request) error {
	req, err := decodeJSON[subtitleFollowRequest](r)
	if err != nil {
		return err
	}
	if err = requireServerID(req.ServerID); err != nil {
		return err
	}
	if req.Language != "" && !subtitleLanguagePattern.MatchString(req.Language) {
		return BadRequest("language must be a lowercase ISO 639-2 code such as eng, or empty to stop following")
	}
	if err = s.store.Subtitles().SetFollow(r.Context(), req.ServerID, req.Language); err != nil {
		return err
	}
	return reply(w, r, http.StatusOK, map[string]any{"follow_language": req.Language})
}

// handleSubtitleBatch queues a bounded batch of downloads for sources that
// currently have no subtitles.
func (s *Server) handleSubtitleBatch(w http.ResponseWriter, r *http.Request) error {
	req, err := decodeJSON[subtitleBatchRequest](r)
	if err != nil {
		return err
	}
	if err = requireServerID(req.ServerID); err != nil {
		return err
	}
	if !subtitleLanguagePattern.MatchString(req.Language) {
		return BadRequest("language must be a lowercase ISO 639-2 code such as eng")
	}
	if req.Limit < 1 || req.Limit > store.SubtitleBatchLimit || len(req.MediaSourceIDs) > store.SubtitleBatchLimit {
		return BadRequest("a batch takes a limit and at most %d media sources", store.SubtitleBatchLimit)
	}
	if req.MediaType != "" && req.MediaType != "movie" && req.MediaType != "episode" {
		return BadRequest("media_type must be movie or episode")
	}
	filter := store.SubtitleCoverageFilter{ServerID: req.ServerID, Library: req.Library, MediaType: req.MediaType, Limit: req.Limit}
	queued, err := s.store.Subtitles().QueueBatch(r.Context(), filter, req.Language, req.MediaSourceIDs)
	if err != nil {
		return err
	}
	return reply(w, r, http.StatusOK, map[string]any{"queued": queued})
}

// handleSubtitleJobAction retries, cancels, or picks a candidate for one job.
func (s *Server) handleSubtitleJobAction(w http.ResponseWriter, r *http.Request, id int64) error {
	req, err := decodeJSON[subtitleJobActionRequest](r)
	if err != nil {
		return err
	}
	if req.Action != "retry" && req.Action != "cancel" && req.Action != "choose" {
		return BadRequest("action must be retry, cancel or choose")
	}
	if req.Action == "choose" && strings.TrimSpace(req.SubtitleID) == "" {
		return BadRequest("choose needs subtitle_id")
	}
	err = s.store.Subtitles().JobAction(r.Context(), id, req.Action, req.SubtitleID)
	if errors.Is(err, store.ErrNotFound) {
		return NotFound("subtitle job %d not found", id)
	}
	if errors.Is(err, store.ErrConflict) {
		return Conflict("subtitle job %d cannot %s in its current state", id, req.Action)
	}
	if err != nil {
		return err
	}
	return reply(w, r, http.StatusOK, map[string]any{"id": id, "action": req.Action})
}

func truncateText(value string, limit int) string {
	if len(value) <= limit {
		return value
	}
	return value[:limit]
}
