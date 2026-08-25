// Package model holds Reelay's domain types.
//
// It imports nothing from the rest of the project and performs no I/O. Every
// other package may depend on this one; this one depends on none of them.
package model

import (
	"fmt"
	"time"
)

// ItemState is the lifecycle of one wanted thing — an episode or a movie.
//
//	unmonitored ─┐
//	             ├→ wanted → searching → grabbed → downloading → importing → imported
//	             │              │                       │            │
//	             │              └→ (no match) ──────────┘            └→ import_failed
//	             │                     ↓                                     │
//	             └──────────────── wanted (retry after backoff) ←─────────────┘
//	                                                                       failed
type ItemState string

const (
	StateUnmonitored  ItemState = "unmonitored"
	StateWanted       ItemState = "wanted"
	StateSearching    ItemState = "searching"
	StateGrabbed      ItemState = "grabbed"
	StateDownloading  ItemState = "downloading"
	StateImporting    ItemState = "importing"
	StateImported     ItemState = "imported"
	StateImportFailed ItemState = "import_failed"
	StateFailed       ItemState = "failed"
)

// AllItemStates is the authoritative list, used to validate input and to keep
// the SQL CHECK constraints and this enum in step.
var AllItemStates = []ItemState{
	StateUnmonitored, StateWanted, StateSearching, StateGrabbed,
	StateDownloading, StateImporting, StateImported, StateImportFailed,
	StateFailed,
}

func (s ItemState) Valid() bool {
	for _, v := range AllItemStates {
		if s == v {
			return true
		}
	}
	return false
}

// Active reports whether the item is mid-flight, so the status loop should be
// watching it.
func (s ItemState) Active() bool {
	switch s {
	case StateGrabbed, StateDownloading, StateImporting:
		return true
	default:
		return false
	}
}

// Terminal reports whether Reelay is done with the item unless a human acts.
func (s ItemState) Terminal() bool {
	switch s {
	case StateImported, StateFailed, StateUnmonitored:
		return true
	default:
		return false
	}
}

// CanTransitionTo encodes the state machine's legal edges. Enforcing this in
// one place is what stops an item from ending up somewhere no loop handles.
func (s ItemState) CanTransitionTo(next ItemState) bool {
	if !next.Valid() {
		return false
	}
	if s == next {
		return true // idempotent re-assertion is allowed
	}
	switch s {
	case StateUnmonitored:
		return next == StateWanted
	case StateWanted:
		return next == StateSearching || next == StateUnmonitored || next == StateFailed
	case StateSearching:
		// Back to wanted when nothing acceptable was found.
		return next == StateGrabbed || next == StateWanted ||
			next == StateFailed || next == StateUnmonitored
	case StateGrabbed:
		return next == StateDownloading || next == StateWanted || next == StateUnmonitored
	case StateDownloading:
		return next == StateImporting || next == StateWanted || next == StateUnmonitored
	case StateImporting:
		return next == StateImported || next == StateImportFailed
	case StateImportFailed:
		return next == StateWanted || next == StateFailed || next == StateUnmonitored
	case StateImported:
		// Only an upgrade search or an explicit re-monitor moves this.
		return next == StateWanted || next == StateUnmonitored
	case StateFailed:
		return next == StateWanted || next == StateUnmonitored
	default:
		return false
	}
}

// GrabState tracks one handoff to the download client.
type GrabState string

const (
	GrabPending     GrabState = "pending"
	GrabDownloading GrabState = "downloading"
	GrabCompleted   GrabState = "completed"
	GrabImporting   GrabState = "importing"
	GrabImported    GrabState = "imported"
	GrabStalled     GrabState = "stalled"
	GrabRemoved     GrabState = "removed"
	GrabFailed      GrabState = "failed"
)

func (g GrabState) Valid() bool {
	switch g {
	case GrabPending, GrabDownloading, GrabCompleted, GrabImporting,
		GrabImported, GrabStalled, GrabRemoved, GrabFailed:
		return true
	default:
		return false
	}
}

func (g GrabState) Active() bool {
	switch g {
	case GrabPending, GrabDownloading, GrabCompleted, GrabImporting:
		return true
	default:
		return false
	}
}

// MonitorMode decides which episodes of a series Reelay wants.
type MonitorMode string

const (
	// MonitorAll wants every aired episode, including the back catalogue.
	MonitorAll MonitorMode = "all"
	// MonitorFutureOnly wants episodes that air from now on. This is the
	// "I'm currently watching this" mode and the sensible default.
	MonitorFutureOnly   MonitorMode = "future_only"
	MonitorLatestSeason MonitorMode = "latest_season"
	MonitorNone         MonitorMode = "none"
)

func (s SubjectType) ValidItem() bool {
	return s == SubjectEpisode || s == SubjectMovie
}

func (m MonitorMode) Valid() bool {
	switch m {
	case MonitorAll, MonitorFutureOnly, MonitorLatestSeason, MonitorNone:
		return true
	}
	return false
}

type SeriesStatus string

const (
	SeriesFollowing SeriesStatus = "following"
	SeriesPaused    SeriesStatus = "paused"
	SeriesEnded     SeriesStatus = "ended"
)

func (s SeriesStatus) Valid() bool {
	switch s {
	case SeriesFollowing, SeriesPaused, SeriesEnded:
		return true
	}
	return false
}

// SubjectType names what a grab, transition or blacklist entry is about.
type SubjectType string

const (
	SubjectEpisode SubjectType = "episode"
	SubjectMovie   SubjectType = "movie"
	SubjectGrab    SubjectType = "grab"
)

type Series struct {
	ID                  int64        `json:"id"`
	Title               string       `json:"title"`
	SortTitle           string       `json:"sort_title"`
	Year                int          `json:"year,omitempty"`
	TVmazeID            int          `json:"tvmaze_id,omitempty"`
	TMDBID              int          `json:"tmdb_id,omitempty"`
	IMDBID              string       `json:"imdb_id,omitempty"`
	Aliases             []string     `json:"aliases,omitempty"`
	IsAnime             bool         `json:"is_anime"`
	AbsoluteOffset      int          `json:"absolute_offset"`
	MonitorMode         MonitorMode  `json:"monitor_mode"`
	Status              SeriesStatus `json:"status"`
	ProfileID           int64        `json:"quality_profile_id"`
	RootFolder          string       `json:"root_folder"`
	RuntimeMinutes      int          `json:"runtime_minutes,omitempty"`
	AddedAt             time.Time    `json:"added_at"`
	EpisodesRefreshedAt time.Time    `json:"episodes_refreshed_at,omitempty"`
}

type Recommendation struct {
	ID             int64              `json:"id"`
	ServerID       string             `json:"server_id"`
	UserID         string             `json:"user_id"`
	MediaType      string             `json:"media_type"`
	TMDBID         int                `json:"tmdb_id"`
	Title          string             `json:"title"`
	Year           int                `json:"year,omitempty"`
	Overview       string             `json:"overview,omitempty"`
	PosterURL      string             `json:"poster_url,omitempty"`
	Score          float64            `json:"score"`
	Reasons        []string           `json:"reasons"`
	Components     map[string]float64 `json:"components,omitempty"`
	Genres         []string           `json:"genres,omitempty"`
	Keywords       []string           `json:"keywords,omitempty"`
	People         []string           `json:"people,omitempty"`
	Language       string             `json:"language,omitempty"`
	Country        string             `json:"country,omitempty"`
	RuntimeMinutes int                `json:"runtime_minutes,omitempty"`
	Status         string             `json:"status"`
	GeneratedAt    time.Time          `json:"generated_at"`
	ExpiresAt      time.Time          `json:"expires_at"`
}

type JellyfinUser struct {
	ServerID    string    `json:"server_id"`
	UserID      string    `json:"user_id"`
	DisplayName string    `json:"display_name"`
	Enabled     bool      `json:"enabled"`
	LastSynced  time.Time `json:"last_synced_at,omitempty"`
}

type JellyfinItem struct {
	ServerID       string   `json:"server_id"`
	ItemID         string   `json:"item_id"`
	MediaType      string   `json:"media_type"`
	TMDBID         int      `json:"tmdb_id,omitempty"`
	TVDBID         int      `json:"tvdb_id,omitempty"`
	IMDBID         string   `json:"imdb_id,omitempty"`
	Title          string   `json:"title"`
	Year           int      `json:"year,omitempty"`
	Genres         []string `json:"genres,omitempty"`
	Keywords       []string `json:"keywords,omitempty"`
	People         []string `json:"people,omitempty"`
	Language       string   `json:"language,omitempty"`
	Country        string   `json:"country,omitempty"`
	RuntimeMinutes int      `json:"runtime_minutes,omitempty"`
	Present        bool     `json:"present"`
}

type JellyfinActivity struct {
	EventID    string    `json:"event_id"`
	ServerID   string    `json:"server_id"`
	UserID     string    `json:"user_id"`
	ItemID     string    `json:"item_id"`
	EventType  string    `json:"event_type"`
	Progress   float64   `json:"progress"`
	OccurredAt time.Time `json:"occurred_at"`
}

type RecommendationRating struct {
	ServerID  string    `json:"server_id"`
	UserID    string    `json:"user_id"`
	MediaType string    `json:"media_type"`
	TMDBID    int       `json:"tmdb_id"`
	Rating    int       `json:"rating"`
	UpdatedAt time.Time `json:"updated_at"`
}

type Episode struct {
	ID              int64      `json:"id"`
	SeriesID        int64      `json:"series_id"`
	Season          int        `json:"season"`
	Number          int        `json:"number"`
	AbsoluteNumber  int        `json:"absolute_number,omitempty"`
	Title           string     `json:"title,omitempty"`
	AirDate         *time.Time `json:"air_date,omitempty"`
	State           ItemState  `json:"state"`
	ChosenReleaseID int64      `json:"chosen_release_id,omitempty"`
	ImportedPath    string     `json:"imported_path,omitempty"`
	ImportedQuality string     `json:"imported_quality,omitempty"`
	SearchAttempts  int        `json:"search_attempts"`
	NextSearchAt    *time.Time `json:"next_search_at,omitempty"`
	FirstWantedAt   *time.Time `json:"first_wanted_at,omitempty"`
	LastError       string     `json:"last_error,omitempty"`
}

// Aired reports whether the episode's air time plus grace has passed. An
// episode that has not aired is never searched for.
func (e Episode) Aired(now time.Time, grace time.Duration) bool {
	if e.AirDate == nil {
		// No known air date: treat as aired so a metadata gap does not
		// silently park the episode forever.
		return true
	}
	return now.After(e.AirDate.Add(grace))
}

func (e Episode) String() string {
	return fmt.Sprintf("S%02dE%02d", e.Season, e.Number)
}

type Movie struct {
	ID              int64      `json:"id"`
	Title           string     `json:"title"`
	SortTitle       string     `json:"sort_title"`
	Year            int        `json:"year"`
	TMDBID          int        `json:"tmdb_id,omitempty"`
	IMDBID          string     `json:"imdb_id,omitempty"`
	RuntimeMinutes  int        `json:"runtime_minutes,omitempty"`
	ProfileID       int64      `json:"quality_profile_id"`
	RootFolder      string     `json:"root_folder"`
	State           ItemState  `json:"state"`
	ChosenReleaseID int64      `json:"chosen_release_id,omitempty"`
	ImportedPath    string     `json:"imported_path,omitempty"`
	ImportedQuality string     `json:"imported_quality,omitempty"`
	SearchAttempts  int        `json:"search_attempts"`
	NextSearchAt    *time.Time `json:"next_search_at,omitempty"`
	FirstWantedAt   *time.Time `json:"first_wanted_at,omitempty"`
	LastError       string     `json:"last_error,omitempty"`
	AddedAt         time.Time  `json:"added_at"`
}

// StoredRelease is a release we persisted because we scored or grabbed it.
// The in-flight search result type lives in internal/indexer.
type StoredRelease struct {
	ID          int64     `json:"id"`
	Indexer     string    `json:"indexer"`
	RawTitle    string    `json:"raw_title"`
	InfoHash    string    `json:"info_hash"`
	Magnet      string    `json:"magnet"`
	SizeBytes   int64     `json:"size_bytes"`
	Seeders     int       `json:"seeders"`
	Leechers    int       `json:"leechers"`
	PublishedAt time.Time `json:"published_at,omitempty"`
	Category    int       `json:"category,omitempty"`
	ParsedJSON  string    `json:"-"`
	Score       int       `json:"score"`
	SeenAt      time.Time `json:"seen_at"`
}

type CandidateEvaluation struct {
	ID          int64       `json:"id"`
	SubjectType SubjectType `json:"subject_type"`
	SubjectID   int64       `json:"subject_id"`
	ReleaseID   int64       `json:"release_id"`
	Accepted    bool        `json:"accepted"`
	ReasonCode  string      `json:"reason_code,omitempty"`
	Reason      string      `json:"reason,omitempty"`
	Score       int         `json:"score"`
	EvaluatedAt time.Time   `json:"evaluated_at"`
}

type BlacklistEntry struct {
	ID          int64       `json:"id"`
	SubjectType SubjectType `json:"subject_type"`
	SubjectID   int64       `json:"subject_id"`
	InfoHash    string      `json:"info_hash"`
	Reason      string      `json:"reason"`
	CreatedAt   time.Time   `json:"created_at"`
}

type Grab struct {
	ID           int64       `json:"id"`
	SubjectType  SubjectType `json:"subject_type"`
	SubjectID    int64       `json:"subject_id"`
	ReleaseID    int64       `json:"release_id"`
	TorrentHash  string      `json:"torrent_hash"`
	Category     string      `json:"category"`
	State        GrabState   `json:"state"`
	Progress     float64     `json:"progress"`
	ContentPath  string      `json:"content_path,omitempty"`
	Attempts     int         `json:"attempts"`
	LastError    string      `json:"last_error,omitempty"`
	CreatedAt    time.Time   `json:"created_at"`
	UpdatedAt    time.Time   `json:"updated_at"`
	ProgressedAt time.Time   `json:"progressed_at,omitempty"`
}

type QualityProfile struct {
	ID                 int64          `json:"id"`
	Name               string         `json:"name"`
	IsDefault          bool           `json:"is_default"`
	AllowedResolutions []string       `json:"allowed_resolutions"`
	AllowedSources     []string       `json:"allowed_sources"`
	MinSizeMB          int            `json:"min_size_mb"`
	MaxSizeMB          int            `json:"max_size_mb"`
	MinSeeders         int            `json:"min_seeders"`
	RequiredTerms      []string       `json:"required_terms"`
	BannedTerms        []string       `json:"banned_terms"`
	PreferredGroups    map[string]int `json:"preferred_groups"`
	LanguagePrefs      []string       `json:"language_prefs"`
	HDRPrefs           []string       `json:"hdr_prefs"`
	UpgradeUntil       string         `json:"upgrade_until,omitempty"`
	CreatedAt          time.Time      `json:"created_at"`
	UpdatedAt          time.Time      `json:"updated_at"`
}

// ResolutionRank is the position of res in the profile's preference order,
// highest first. Returns -1 when the resolution is not allowed at all.
func (p QualityProfile) ResolutionRank(res string) int {
	for i, r := range p.AllowedResolutions {
		if r == res {
			return len(p.AllowedResolutions) - i
		}
	}
	return -1
}

// SourceRank mirrors ResolutionRank for sources.
func (p QualityProfile) SourceRank(src string) int {
	for i, s := range p.AllowedSources {
		if s == src {
			return len(p.AllowedSources) - i
		}
	}
	return -1
}

// StateTransition is one audit row. Written on every state change, with a
// reason, because "why didn't it grab X" is otherwise unanswerable.
type StateTransition struct {
	ID          int64       `json:"id"`
	SubjectType SubjectType `json:"subject_type"`
	SubjectID   int64       `json:"subject_id"`
	From        ItemState   `json:"from_state,omitempty"`
	To          ItemState   `json:"to_state"`
	Reason      string      `json:"reason"`
	Detail      string      `json:"detail,omitempty"`
	At          time.Time   `json:"at"`
}

type ImportRecord struct {
	ID           int64       `json:"id"`
	GrabID       int64       `json:"grab_id"`
	SubjectType  SubjectType `json:"subject_type"`
	SubjectID    int64       `json:"subject_id"`
	SourcePath   string      `json:"source_path"`
	DestPath     string      `json:"dest_path"`
	Method       string      `json:"method"` // hardlink | copy | move
	SizeBytes    int64       `json:"size_bytes"`
	ReplacedPath string      `json:"replaced_path,omitempty"`
	At           time.Time   `json:"at"`
}

// Wanted is the matching target handed to the parser: what we are looking for,
// stripped of everything the parser does not need.
type Wanted struct {
	Kind SubjectType

	// Title and Aliases are already normalised (lowercase, punctuation
	// stripped). The parser compares against these, never against raw titles.
	Title   string
	Aliases []string

	Year int // movies only; 0 for episodes

	Season     int
	Episode    int
	AbsoluteEp int

	IsAnime bool

	// WantedEpisodes lets a season pack be evaluated against everything still
	// missing from that season, which is what makes pack scoring meaningful.
	WantedEpisodes []int
}
