package model

import (
	"slices"
	"time"
)

const MediaRequestListLimit = 100

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
	NextSearchAt     *time.Time   `json:"next_search_at"`
	Available        bool         `json:"available"`
	ImportedEpisodes int          `json:"imported_episodes"`
	TotalEpisodes    int          `json:"total_episodes"`
	CancelledAt      *time.Time   `json:"cancelled_at"`
	Seasons          []int        `json:"seasons"`
	JellyfinItemID   string       `json:"jellyfin_item_id"`
	JellyfinURL      string       `json:"jellyfin_url"`
	Trial            *SeriesTrial `json:"trial,omitempty"`
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
