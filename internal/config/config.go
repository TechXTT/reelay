// Package config loads, env-overrides and validates Reelay's configuration.
//
// Config is read exactly once at startup. Nothing in this package is safe to
// mutate afterwards, and nothing else in Reelay re-reads the file.
package config

import (
	"bytes"
	"fmt"
	"os"
	"time"

	"gopkg.in/yaml.v3"
)

// Duration wraps time.Duration so YAML can carry "15m" instead of nanoseconds.
type Duration struct {
	time.Duration
}

func (d *Duration) UnmarshalYAML(n *yaml.Node) error {
	var s string
	if err := n.Decode(&s); err != nil {
		return fmt.Errorf("expected a duration string like \"15m\": %w", err)
	}
	return d.Set(s)
}

// Set parses a Go duration string. Shared with the env-override path.
func (d *Duration) Set(s string) error {
	v, err := time.ParseDuration(s)
	if err != nil {
		return fmt.Errorf("invalid duration %q: %w", s, err)
	}
	d.Duration = v
	return nil
}

func (d Duration) MarshalYAML() (any, error) { return d.String(), nil }

func Dur(d time.Duration) Duration { return Duration{d} }

type Config struct {
	Server     Server     `yaml:"server"`
	Database   Database   `yaml:"database"`
	Logging    Logging    `yaml:"logging"`
	Runtime    Runtime    `yaml:"runtime"`
	Indexers   []Indexer  `yaml:"indexers"`
	Downloader Downloader `yaml:"downloader"`
	Metadata   Metadata   `yaml:"metadata"`
	Library    Library    `yaml:"library"`
	Schedules  Schedules  `yaml:"schedules"`
	Profiles   []Profile  `yaml:"profiles"`
	Scoring    Scoring    `yaml:"scoring"`
}

type Server struct {
	Bind            string   `yaml:"bind"`
	Port            int      `yaml:"port"`
	AuthToken       string   `yaml:"auth_token"`
	CORSOrigins     []string `yaml:"cors_origins"`
	ReadTimeout     Duration `yaml:"read_timeout"`
	WriteTimeout    Duration `yaml:"write_timeout"`
	ShutdownTimeout Duration `yaml:"shutdown_timeout"`
}

type Database struct {
	Path string `yaml:"path"`
}

type Logging struct {
	Level  string `yaml:"level"`
	Format string `yaml:"format"`
}

type Runtime struct {
	SQLiteCacheKB     int      `yaml:"sqlite_cache_kb"`
	SearchConcurrency int      `yaml:"search_concurrency"`
	MaxSSEClients     int      `yaml:"max_sse_clients"`
	AuditRetention    Duration `yaml:"audit_retention"`
}

type Indexer struct {
	Name               string   `yaml:"name"`
	Type               string   `yaml:"type"`
	Enabled            bool     `yaml:"enabled"`
	BaseURL            string   `yaml:"base_url"`
	UserAgent          string   `yaml:"user_agent"`
	RateLimitPerSecond float64  `yaml:"rate_limit_per_second"`
	RateLimitBurst     int      `yaml:"rate_limit_burst"`
	RequestTimeout     Duration `yaml:"request_timeout"`
	MaxRetries         int      `yaml:"max_retries"`
	FailureThreshold   int      `yaml:"failure_threshold"`
	BreakerCooldown    Duration `yaml:"breaker_cooldown"`
	Trackers           []string `yaml:"trackers"`
}

type PathMapping struct {
	DownloaderPrefix string `yaml:"downloader_prefix"`
	LocalPrefix      string `yaml:"local_prefix"`
}

type Downloader struct {
	Type           string        `yaml:"type"`
	URL            string        `yaml:"url"`
	Username       string        `yaml:"username"`
	Password       string        `yaml:"password"`
	CategoryTV     string        `yaml:"category_tv"`
	CategoryMovies string        `yaml:"category_movies"`
	SavePathTV     string        `yaml:"save_path_tv"`
	SavePathMovies string        `yaml:"save_path_movies"`
	AddPaused      bool          `yaml:"add_paused"`
	StallTimeout   Duration      `yaml:"stall_timeout"`
	PathMappings   []PathMapping `yaml:"path_mappings"`
}

type Metadata struct {
	TMDBAPIKey     string   `yaml:"tmdb_api_key"`
	TMDBBaseURL    string   `yaml:"tmdb_base_url"`
	TVmazeBaseURL  string   `yaml:"tvmaze_base_url"`
	CacheTTL       Duration `yaml:"cache_ttl"`
	RequestTimeout Duration `yaml:"request_timeout"`
}

type Library struct {
	TVRoot    string `yaml:"tv_root"`
	MovieRoot string `yaml:"movie_root"`
	Hardlink  bool   `yaml:"hardlink"`
	AllowMove bool   `yaml:"allow_move"`

	// RecycleDir is a folder NAME, not a path: it is created inside whichever
	// root the replaced file lives in. With two library roots on two separate
	// network shares, a single absolute recycle path would force a
	// cross-filesystem copy for one of them on every upgrade.
	RecycleDir string `yaml:"recycle_dir"`

	MinVideoSizeMB     int      `yaml:"min_video_size_mb"`
	VideoExtensions    []string `yaml:"video_extensions"`
	SubtitleExtensions []string `yaml:"subtitle_extensions"`
	IgnorePatterns     []string `yaml:"ignore_patterns"`

	TVFolderTemplate    string `yaml:"tv_folder_template"`
	TVFileTemplate      string `yaml:"tv_file_template"`
	AnimeFileTemplate   string `yaml:"anime_file_template"`
	MovieFolderTemplate string `yaml:"movie_folder_template"`
	MovieFileTemplate   string `yaml:"movie_file_template"`

	MaxPathLength int `yaml:"max_path_length"`

	PostImportWebhook string `yaml:"post_import_webhook"`
}

type Schedules struct {
	SearchInterval    Duration   `yaml:"search_interval"`
	RecentInterval    Duration   `yaml:"recent_interval"`
	StatusInterval    Duration   `yaml:"status_interval"`
	MetadataInterval  Duration   `yaml:"metadata_interval"`
	AirGrace          Duration   `yaml:"air_grace"`
	SearchBackoff     []Duration `yaml:"search_backoff"`
	SearchGiveUpAfter Duration   `yaml:"search_give_up_after"`
}

type Profile struct {
	Name               string         `yaml:"name"`
	Default            bool           `yaml:"default"`
	AllowedResolutions []string       `yaml:"allowed_resolutions"`
	AllowedSources     []string       `yaml:"allowed_sources"`
	MinSizeMB          int            `yaml:"min_size_mb"`
	MaxSizeMB          int            `yaml:"max_size_mb"`
	MinSeeders         int            `yaml:"min_seeders"`
	RequiredTerms      []string       `yaml:"required_terms"`
	BannedTerms        []string       `yaml:"banned_terms"`
	PreferredGroups    map[string]int `yaml:"preferred_groups"`
	LanguagePrefs      []string       `yaml:"language_prefs"`
	HDRPrefs           []string       `yaml:"hdr_prefs"`
	UpgradeUntil       string         `yaml:"upgrade_until"`
}

type Scoring struct {
	ResolutionWeight   int `yaml:"resolution_weight"`
	SourceWeight       int `yaml:"source_weight"`
	GroupWeight        int `yaml:"group_weight"`
	LanguageWeight     int `yaml:"language_weight"`
	ProperRepackWeight int `yaml:"proper_repack_weight"`
	SeederWeightMax    int `yaml:"seeder_weight_max"`
	HDRWeight          int `yaml:"hdr_weight"`
	SeasonPackWeight   int `yaml:"season_pack_weight"`
	AgePenaltyPerDay   int `yaml:"age_penalty_per_day"`
	AgePenaltyMax      int `yaml:"age_penalty_max"`
}

// Load reads path, applies REELAY_* environment overrides, then validates.
//
// Warnings are conditions we deliberately allow but the operator should see
// (empty auth token on loopback, a database path that looks networked).
func Load(path string) (cfg *Config, warnings []string, err error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, nil, fmt.Errorf("read config %s: %w", path, err)
	}

	c := &Config{}
	dec := yaml.NewDecoder(bytes.NewReader(raw))
	// Reject unknown keys: a typo'd setting that silently does nothing is the
	// single most expensive class of config bug to debug.
	dec.KnownFields(true)
	if err := dec.Decode(c); err != nil {
		return nil, nil, fmt.Errorf("parse config %s: %w", path, err)
	}

	if err := applyEnv(c); err != nil {
		return nil, nil, err
	}

	warnings, err = c.Validate()
	if err != nil {
		return nil, warnings, err
	}
	return c, warnings, nil
}

// Addr is the listen address for the HTTP server.
func (c *Config) Addr() string { return fmt.Sprintf("%s:%d", c.Server.Bind, c.Server.Port) }

// DefaultProfile returns the seed profile flagged default, else the first.
func (c *Config) DefaultProfile() *Profile {
	for i := range c.Profiles {
		if c.Profiles[i].Default {
			return &c.Profiles[i]
		}
	}
	if len(c.Profiles) > 0 {
		return &c.Profiles[0]
	}
	return nil
}

// EnabledIndexers is the subset the engine should actually poll.
func (c *Config) EnabledIndexers() []Indexer {
	out := make([]Indexer, 0, len(c.Indexers))
	for _, ix := range c.Indexers {
		if ix.Enabled {
			out = append(out, ix)
		}
	}
	return out
}
