package model

import (
	"slices"
	"time"
)

const MediaRequestListLimit = 100

// Request wait kinds name what a request is waiting for in viewer-safe terms.
// The import and search retry kinds wait for the viewer; the rest wait for
// the service and clear on their own.
const (
	WaitSearchService  = "search_service"
	WaitDownloadClient = "download_client"
	WaitDownloadFiles  = "download_files"
	WaitStorage        = "storage"
	WaitMetadata       = "metadata"
	WaitImportRetry    = "import_retry"
	WaitSearchRetry    = "search_retry"
)

type MediaRequest struct {
	ID               int64        `json:"id"`
	ServerID         string       `json:"server_id"`
	UserID           string       `json:"user_id"`
	MediaType        string       `json:"media_type"`
	TMDBID           int          `json:"tmdb_id"`
	Title            string       `json:"title"`
	Year             int          `json:"year"`
	RequestedAt      time.Time    `json:"requested_at"`
	MonitorMode      string       `json:"monitor_mode"`
	SubjectType      string       `json:"subject_type"`
	SubjectID        int64        `json:"subject_id"`
	State            string       `json:"state"`
	Progress         float64      `json:"progress"`
	LastError        string       `json:"last_error"`
	DownloadWait     string       `json:"download_wait,omitempty"`
	CanRetryImport   bool         `json:"can_retry_import"`
	WaitKind         string       `json:"wait_kind,omitempty"`
	WaitingFor       string       `json:"waiting_for,omitempty"`
	NextCheckAt      *time.Time   `json:"next_check_at,omitempty"`
	NeedsAction      bool         `json:"needs_action"`
	ActionHint       string       `json:"action_hint,omitempty"`
	NextSearchAt     *time.Time   `json:"next_search_at"`
	Available        bool         `json:"available"`
	ImportedEpisodes int          `json:"imported_episodes"`
	TotalEpisodes    int          `json:"total_episodes"`
	CancelledAt      *time.Time   `json:"cancelled_at"`
	Seasons          []int        `json:"seasons"`
	ReadySeason      int          `json:"ready_season,omitempty"`
	ReadyEpisode     int          `json:"ready_episode,omitempty"`
	JellyfinItemID   string       `json:"jellyfin_item_id"`
	SubtitleCoverage string       `json:"subtitle_coverage,omitempty"`
	JellyfinURL      string       `json:"jellyfin_url"`
	Trial            *SeriesTrial `json:"trial,omitempty"`

	// WaitObservedAt is when the download client wait was last observed.
	WaitObservedAt *time.Time `json:"-"`
}

type TrialEpisode struct {
	ID      int64 `json:"id"`
	Season  int   `json:"season"`
	Number  int   `json:"number"`
	Watched bool  `json:"watched"`
}

type SeriesTrial struct {
	RequestID       int64          `json:"request_id"`
	ServerID        string         `json:"server_id"`
	UserID          string         `json:"user_id"`
	TMDBID          int            `json:"tmdb_id"`
	Title           string         `json:"title"`
	EpisodeLimit    int            `json:"episode_limit"`
	Scope           string         `json:"scope"`
	WatchedEpisodes int            `json:"watched_episodes"`
	State           string         `json:"state"`
	Decision        string         `json:"decision"`
	Rating          int            `json:"rating"`
	Episodes        []TrialEpisode `json:"episodes"`
}

func (r MediaRequest) IncludesEpisode(episode Episode) bool {
	if r.Trial != nil && r.Trial.Decision != "continue" {
		return slices.ContainsFunc(r.Trial.Episodes, func(selected TrialEpisode) bool { return selected.ID == episode.ID })
	}
	return r.MonitorMode != "" || len(r.Seasons) == 0 || slices.Contains(r.Seasons, episode.Season)
}
