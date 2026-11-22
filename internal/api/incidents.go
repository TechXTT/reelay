package api

import (
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/TechXTT/reelay/internal/engine"
	"github.com/TechXTT/reelay/internal/store"
)

// resolvedIncidentsShown bounds the recently resolved incidents listed.
const resolvedIncidentsShown = 20

type incidentText struct {
	cause    string
	nextStep string
	action   string
}

// incidentGuidance tells the operator what each kind of incident means, what
// Reelay does next and what the operator can do. cause is a format taking the
// cause key.
var incidentGuidance = map[string]incidentText{
	store.IncidentIndexer: {
		cause:    "Indexer %s is failing",
		nextStep: "Searches the affected items again on the next search pass.",
		action:   "Check that the indexer is reachable and its API key is valid. The incident resolves on the next successful query.",
	},
	store.IncidentDownloadClient: {
		cause:    "The download client (%s) is unavailable",
		nextStep: "Polls the download client again; held searches retry on the next search pass.",
		action:   "Check that the download client is running and that Reelay's credentials and URL are correct.",
	},
	store.IncidentStorage: {
		cause:    "Not enough free space on %s",
		nextStep: "Checks free space again before the next search; held items are not counted as failed searches.",
		action:   "Free space on the volume or lower downloader.min_free_space_mb.",
	},
	store.IncidentLibrary: {
		cause:    "Library folder %s is unavailable or not writable",
		nextStep: "Nothing automatic; failed imports wait for Retry import.",
		action:   "Restore access to the library folder, then retry the affected imports from Requests.",
	},
	store.IncidentMetadata: {
		cause:    "Metadata provider %s is failing",
		nextStep: "Refreshes series metadata again on the next metadata pass.",
		action:   "Usually temporary. If it persists, check outbound network access from the Reelay host.",
	},
	store.IncidentNotifications: {
		cause:    "Notification receiver %s is failing",
		nextStep: "Retries each undelivered message with backoff of up to 24 hours.",
		action:   "Check the receiver URL and that it accepts requests from Reelay.",
	},
}

// downloadFilesGuidance is for the storage incidents of completed downloads
// whose files are not accessible; cause takes the local download path.
var downloadFilesGuidance = incidentText{
	cause:    "Completed downloads in %s are not accessible",
	nextStep: "Checks the downloads again on the next status poll.",
	action:   "Check that the download folder is mounted on the Reelay host and that downloader.path_mappings map the client's paths to it.",
}

// tmdbGuidance is for TMDB, which only recommendation generation checks.
var tmdbGuidance = incidentText{
	cause:    "Metadata provider %s is failing",
	nextStep: "Generates recommendations again on the next recommendation refresh.",
	action:   "Usually temporary. If it persists, check outbound network access from the Reelay host and the TMDB API key.",
}

type incidentDTO struct {
	store.Incident
	Cause          string `json:"cause"`
	NextStep       string `json:"next_step"`
	OperatorAction string `json:"operator_action"`
	// ResolvedNote says why an incident closed other than being seen healthy.
	ResolvedNote string `json:"resolved_note,omitempty"`
}

// nextCheck moves a scheduled check that has already passed forward by whole
// intervals, matching the loop ticker that will run it.
func nextCheck(at *time.Time, interval time.Duration, now time.Time) *time.Time {
	if at == nil || interval <= 0 || at.After(now) {
		return at
	}
	next := at.Add((now.Sub(*at)/interval + 1) * interval)
	return &next
}

func (s *Server) describeIncidents(incidents []store.Incident, now time.Time) []incidentDTO {
	out := make([]incidentDTO, len(incidents))
	for i, incident := range incidents {
		guidance := incidentGuidance[incident.Kind]
		subject := incident.Key
		if path, ok := strings.CutPrefix(incident.Key, engine.DownloadFilesKeyPrefix); ok && incident.Kind == store.IncidentStorage {
			guidance, subject = downloadFilesGuidance, path
		} else if incident.Kind == store.IncidentMetadata && incident.Key == engine.TMDBIncidentKey {
			guidance = tmdbGuidance
		}
		if incident.ResolvedAt == nil {
			incident.NextCheckAt = nextCheck(incident.NextCheckAt, engine.IncidentCheckInterval(s.cfg, incident.Kind, incident.Key), now)
		}
		out[i] = incidentDTO{Incident: incident, Cause: fmt.Sprintf(guidance.cause, subject),
			NextStep: guidance.nextStep, OperatorAction: guidance.action}
		if incident.ResolvedAt != nil && strings.HasPrefix(incident.Detail, store.IncidentCauseRemoved) {
			out[i].ResolvedNote = store.IncidentCauseRemoved
		}
	}
	return out
}

func (s *Server) handleIncidents(w http.ResponseWriter, r *http.Request) error {
	open, err := s.store.Incidents().Open(r.Context())
	if err != nil {
		return err
	}
	resolved, err := s.store.Incidents().Resolved(r.Context(), resolvedIncidentsShown)
	if err != nil {
		return err
	}
	now := s.clock.Now().UTC()
	return reply(w, r, http.StatusOK, map[string]any{"open": s.describeIncidents(open, now),
		"resolved": s.describeIncidents(resolved, now)})
}
