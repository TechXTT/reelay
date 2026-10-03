package model

import "time"

const MediaRequestListLimit = 100

type MediaRequest struct {
	ID               int64      `json:"id"`
	ServerID         string     `json:"server_id"`
	UserID           string     `json:"user_id"`
	MediaType        string     `json:"media_type"`
	TMDBID           int        `json:"tmdb_id"`
	Title            string     `json:"title"`
	Year             int        `json:"year"`
	RequestedAt      time.Time  `json:"requested_at"`
	MonitorMode      string     `json:"monitor_mode"`
	SubjectType      string     `json:"subject_type"`
	SubjectID        int64      `json:"subject_id"`
	State            string     `json:"state"`
	Progress         float64    `json:"progress"`
	LastError        string     `json:"last_error"`
	NextSearchAt     *time.Time `json:"next_search_at"`
	Available        bool       `json:"available"`
	ImportedEpisodes int        `json:"imported_episodes"`
	TotalEpisodes    int        `json:"total_episodes"`
}
