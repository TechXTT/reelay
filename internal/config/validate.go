package config

import (
	"errors"
	"fmt"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/TechXTT/reelay/internal/model"
)

// Problems is the aggregate validation failure. We report every bad key at
// once rather than making the operator fix them one restart at a time.
type Problems struct {
	Items []string
}

func (p *Problems) Error() string {
	return fmt.Sprintf("invalid configuration (%d problem(s)):\n  - %s",
		len(p.Items), strings.Join(p.Items, "\n  - "))
}

type checker struct {
	problems []string
	warnings []string
}

// bad records a fatal problem against a specific dotted config key.
func (c *checker) bad(key, format string, args ...any) {
	c.problems = append(c.problems, fmt.Sprintf("%s: %s", key, fmt.Sprintf(format, args...)))
}

func (c *checker) warn(format string, args ...any) {
	c.warnings = append(c.warnings, fmt.Sprintf(format, args...))
}

func (c *checker) notEmpty(key, value string) {
	if value == "" {
		c.bad(key, "must not be empty")
	}
}

func (c *checker) positive(key string, d Duration) {
	if d.Duration <= 0 {
		c.bad(key, "must be greater than zero")
	}
}

func (c *checker) notNegative(key string, value int) {
	if value < 0 {
		c.bad(key, "must not be negative")
	}
}

func (c *checker) atLeast(key string, value, minimum int) {
	if value < minimum {
		c.bad(key, "must be at least %d", minimum)
	}
}

// oneOf reports value when it is not in allowed; the message lists the choices.
func (c *checker) oneOf(key, value string, allowed []string) {
	if !slices.Contains(allowed, value) {
		c.bad(key, "%q is not one of %s", value, strings.Join(allowed, ", "))
	}
}

func index(key string, i int) string {
	return fmt.Sprintf("%s[%d]", key, i)
}

var (
	validLogLevels  = []string{"debug", "info", "warn", "error"}
	validLogFormats = []string{"json", "text"}
	validResolution = []string{"2160p", "1080p", "720p", "480p"}
	validSources    = []string{"remux", "bluray", "webdl", "webrip", "hdtv", "dvd", "cam"}
	validHDR        = []string{"hdr10", "hdr10plus", "dv", "hlg"}
	// Named placeholders permitted in the naming templates.
	templatePlaceholders = map[string]bool{
		"Title": true, "Year": true, "Season": true, "Episode": true,
		"EpisodeTitle": true, "Absolute": true, "Resolution": true,
		"Source": true, "VideoCodec": true, "AudioCodec": true,
		"HDR": true, "Group": true, "Ext": true,
	}
	placeholderRe = regexp.MustCompile(`\{([A-Za-z]+)\}`)
)

// Validate checks every key and returns warnings plus, on failure, *Problems.
func (c *Config) Validate() ([]string, error) {
	ck := &checker{}

	c.validateServer(ck)
	c.validateDatabase(ck)
	c.validateLogging(ck)
	c.validateRuntime(ck)
	c.validateIndexers(ck)
	c.validateDownloader(ck)
	c.validateMetadata(ck)
	c.validateLibrary(ck)
	c.validateSchedules(ck)
	c.validateProfiles(ck)
	c.validateScoring(ck)
	c.validateRecommendations(ck)
	c.validateAvailability(ck)

	sort.Strings(ck.problems)
	if len(ck.problems) > 0 {
		return ck.warnings, &Problems{Items: ck.problems}
	}
	return ck.warnings, nil
}

func (c *Config) validateAvailability(ck *checker) {
	for name, rawURL := range c.Availability.JellyfinServers {
		key := "availability.jellyfin_servers." + name
		validateHTTPURL(ck, key, rawURL)
		if parsed, err := url.Parse(rawURL); err == nil && parsed.RawQuery != "" {
			ck.bad(key, "must be a server base URL without a query")
		}
	}
	if c.Availability.WebhookURL != "" {
		validateHTTPURL(ck, "availability.webhook_url", c.Availability.WebhookURL)
	}
	if format := c.Availability.WebhookFormat; format != "" && format != "json" && format != "ntfy" {
		ck.bad("availability.webhook_format", "must be json or ntfy")
	}
}

func validateHTTPURL(ck *checker, key, rawURL string) {
	parsed, err := url.Parse(rawURL)
	if err != nil || parsed.Host == "" || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.User != nil || parsed.Fragment != "" {
		ck.bad(key, "must be an absolute HTTP(S) URL without user credentials or a fragment")
	}
}

func checkAbsoluteURL(ck *checker, key, rawURL string) {
	if u, err := url.Parse(rawURL); err != nil || u.Scheme == "" || u.Host == "" {
		ck.bad(key, "%q is not an absolute http(s) URL", rawURL)
	}
}

func (c *Config) validateRecommendations(ck *checker) {
	r := c.Recommendations
	if !r.Enabled {
		return
	}
	if c.Metadata.TMDBAPIKey == "" {
		ck.bad("recommendations.enabled", "requires metadata.tmdb_api_key")
	}
	ck.positive("recommendations.refresh_interval", r.RefreshInterval)
	if r.Expiry.Duration < r.RefreshInterval.Duration {
		ck.bad("recommendations.expiry", "must be at least refresh_interval")
	}
	if r.SeedLimit < 1 || r.SeedLimit > 50 {
		ck.bad("recommendations.seed_limit", "must be between 1 and 50")
	}
	if r.CandidateLimit < 10 || r.CandidateLimit > 1000 {
		ck.bad("recommendations.candidate_limit", "must be between 10 and 1000")
	}
	if r.ResultLimit < 1 || r.ResultLimit > r.CandidateLimit {
		ck.bad("recommendations.result_limit", "must be positive and no greater than candidate_limit")
	}
	total := r.ProviderWeight + r.AffinityWeight + r.PeopleWeight + r.MultiSeedWeight + r.RatingWeight + r.PreferenceWeight + r.NoveltyWeight
	if total != 100 {
		ck.bad("recommendations", "weights must sum to 100 (got %d)", total)
	}
}

func (c *Config) validateServer(ck *checker) {
	if c.Server.Bind == "" {
		ck.bad("server.bind", "must not be empty (use 127.0.0.1 to stay local)")
	} else if net.ParseIP(c.Server.Bind) == nil {
		ck.bad("server.bind", "%q is not a valid IP address", c.Server.Bind)
	}
	if c.Server.Port < 1 || c.Server.Port > 65535 {
		ck.bad("server.port", "%d is out of range 1-65535", c.Server.Port)
	}

	// Reelay stores download-client credentials. Exposing it unauthenticated on
	// a routable interface is not a configuration we are willing to start in.
	if c.Server.AuthToken == "" {
		if isLoopback(c.Server.Bind) {
			ck.warn("server.auth_token is empty; the API is UNAUTHENTICATED and bound to loopback only (%s). Set a token before changing server.bind.", c.Server.Bind)
		} else {
			ck.bad("server.auth_token", "must be set when server.bind (%s) is not a loopback address; generate one with `openssl rand -hex 32` or set %s",
				c.Server.Bind, EnvKey("server.auth_token"))
		}
	} else if len(c.Server.AuthToken) < 16 {
		ck.bad("server.auth_token", "is %d characters; use at least 16", len(c.Server.AuthToken))
	}

	for i, o := range c.Server.CORSOrigins {
		if _, err := url.ParseRequestURI(o); err != nil {
			ck.bad(index("server.cors_origins", i), "%q is not an absolute URL", o)
		}
	}
	ck.positive("server.read_timeout", c.Server.ReadTimeout)
	if c.Server.WriteTimeout.Duration < 0 {
		ck.bad("server.write_timeout", "must not be negative (0 disables the deadline, required for SSE)")
	}
	ck.positive("server.shutdown_timeout", c.Server.ShutdownTimeout)
}

func (c *Config) validateDatabase(ck *checker) {
	if c.Database.Path == "" {
		ck.bad("database.path", "must not be empty")
		return
	}
	// SQLite WAL relies on shared-memory locking that network filesystems do
	// not provide. Silent corruption is the failure mode, so shout early.
	if looksNetworked(c.Database.Path) {
		ck.warn("database.path (%s) looks like a network location; SQLite WAL is not safe over SMB/NFS. Keep the database on local disk.", c.Database.Path)
	}
	dir := filepath.Dir(c.Database.Path)
	if dir != "" && dir != "." {
		if st, err := os.Stat(dir); err == nil && !st.IsDir() {
			ck.bad("database.path", "parent %q exists but is not a directory", dir)
		}
	}
	c.validateBackup(ck, dir)
}

// validateBackup checks the scheduled-backup keys. They are ignored while
// backup_interval is 0, so existing configs stay valid.
func (c *Config) validateBackup(ck *checker, databaseDir string) {
	var d = c.Database
	var backupAbs, databaseAbs string
	var err error

	if d.BackupInterval.Duration < 0 {
		ck.bad("database.backup_interval", "must not be negative (0 disables scheduled backups)")
		return
	}
	if d.BackupInterval.Duration == 0 {
		return
	}
	if d.BackupInterval.Duration < time.Hour {
		ck.bad("database.backup_interval", "%s is too frequent; use at least 1h", d.BackupInterval.Duration)
	}
	ck.atLeast("database.backup_keep", d.BackupKeep, 1)
	if strings.TrimSpace(d.BackupDir) == "" {
		ck.bad("database.backup_dir", "must be set when backup_interval is enabled")
		return
	}
	if !filepath.IsAbs(d.BackupDir) {
		ck.warn("database.backup_dir (%s) is relative; it resolves against the working directory. Use an absolute path.", d.BackupDir)
	}
	backupAbs, err = filepath.Abs(d.BackupDir)
	if err != nil {
		ck.bad("database.backup_dir", "cannot resolve %q: %v", d.BackupDir, err)
		return
	}
	databaseAbs, err = filepath.Abs(databaseDir)
	if err != nil {
		return
	}
	backupAbs, databaseAbs = filepath.Clean(backupAbs), filepath.Clean(databaseAbs)
	if backupAbs == databaseAbs || (runtime.GOOS == "windows" && strings.EqualFold(backupAbs, databaseAbs)) {
		ck.bad("database.backup_dir", "%q is the database directory; use a separate folder so a disk or folder loss cannot take the backups with it", d.BackupDir)
	}
}

func (c *Config) validateLogging(ck *checker) {
	ck.oneOf("logging.level", c.Logging.Level, validLogLevels)
	ck.oneOf("logging.format", c.Logging.Format, validLogFormats)
}

func (c *Config) validateRuntime(ck *checker) {
	if c.Runtime.SQLiteCacheKB < 64 {
		ck.bad("runtime.sqlite_cache_kb", "%d is too small; use at least 64", c.Runtime.SQLiteCacheKB)
	}
	ck.atLeast("runtime.search_concurrency", c.Runtime.SearchConcurrency, 1)
	ck.atLeast("runtime.max_sse_clients", c.Runtime.MaxSSEClients, 1)
	if c.Runtime.AuditRetention.Duration < 0 {
		ck.bad("runtime.audit_retention", "must not be negative (0 disables pruning)")
	}
}

func (c *Config) validateIndexers(ck *checker) {
	if len(c.Indexers) == 0 {
		ck.bad("indexers", "at least one indexer must be configured")
		return
	}
	seen := map[string]bool{}
	enabled := 0
	for i, ix := range c.Indexers {
		k := func(f string) string { return fmt.Sprintf("indexers[%d].%s", i, f) }
		if ix.Name == "" {
			ck.bad(k("name"), "must not be empty")
		} else if seen[ix.Name] {
			ck.bad(k("name"), "duplicate indexer name %q", ix.Name)
		}
		seen[ix.Name] = true

		if ix.Type != "piratebay" && ix.Type != "torznab" {
			ck.bad(k("type"), "%q is not a supported indexer type (have: piratebay, torznab)", ix.Type)
		}
		if ix.Type == "torznab" {
			validateHTTPURL(ck, k("base_url"), ix.BaseURL)
			if parsed, err := url.Parse(ix.BaseURL); err == nil && parsed.RawQuery != "" {
				ck.bad(k("base_url"), "Torznab endpoint must not contain a query; use api_key or api_key_env")
			}
			if ix.APIKey != "" && ix.APIKeyEnv != "" {
				ck.bad(k("api_key"), "choose api_key or api_key_env, not both")
			}
		}
		checkAbsoluteURL(ck, k("base_url"), ix.BaseURL)
		ck.notEmpty(k("user_agent"), ix.UserAgent)
		if ix.RateLimitPerSecond <= 0 {
			ck.bad(k("rate_limit_per_second"), "must be greater than zero")
		}
		ck.atLeast(k("rate_limit_burst"), ix.RateLimitBurst, 1)
		ck.positive(k("request_timeout"), ix.RequestTimeout)
		ck.notNegative(k("max_retries"), ix.MaxRetries)
		ck.atLeast(k("failure_threshold"), ix.FailureThreshold, 1)
		ck.positive(k("breaker_cooldown"), ix.BreakerCooldown)
		if len(ix.Trackers) == 0 {
			ck.warn("indexers[%d] (%s) has no trackers; magnets built from an info_hash alone may never find peers.", i, ix.Name)
		}
		if ix.Enabled {
			enabled++
		}
	}
	if enabled == 0 {
		ck.warn("every indexer is disabled; nothing will ever be searched.")
	}
}

func (c *Config) validateDownloader(ck *checker) {
	d := c.Downloader
	if d.Type != "qbittorrent" {
		ck.bad("downloader.type", "%q is not a supported download client (have: qbittorrent)", d.Type)
	}
	checkAbsoluteURL(ck, "downloader.url", d.URL)
	// The category is the safety boundary: Reelay only ever touches torrents
	// it labelled itself. An empty category would make every torrent in the
	// client fair game.
	for key, category := range map[string]string{"downloader.category_tv": d.CategoryTV, "downloader.category_movies": d.CategoryMovies} {
		if strings.TrimSpace(category) == "" {
			ck.bad(key, "must not be empty; it is how Reelay avoids touching your other torrents")
		}
	}
	if d.CategoryTV == d.CategoryMovies && d.CategoryTV != "" {
		ck.warn("downloader.category_tv and category_movies are identical (%q); imports will still work but the client view is harder to read.", d.CategoryTV)
	}
	ck.notEmpty("downloader.save_path_tv", d.SavePathTV)
	ck.notEmpty("downloader.save_path_movies", d.SavePathMovies)
	ck.positive("downloader.stall_timeout", d.StallTimeout)
	for i, m := range d.PathMappings {
		ck.notEmpty(index("downloader.path_mappings", i)+".downloader_prefix", m.DownloaderPrefix)
		ck.notEmpty(index("downloader.path_mappings", i)+".local_prefix", m.LocalPrefix)
	}
}

func (c *Config) validateMetadata(ck *checker) {
	checkAbsoluteURL(ck, "metadata.tvmaze_base_url", c.Metadata.TVmazeBaseURL)
	checkAbsoluteURL(ck, "metadata.tmdb_base_url", c.Metadata.TMDBBaseURL)
	if c.Metadata.TMDBAPIKey == "" {
		ck.warn("metadata.tmdb_api_key is empty; movie lookups fall back to the title and year you type.")
	}
	ck.positive("metadata.cache_ttl", c.Metadata.CacheTTL)
	ck.positive("metadata.request_timeout", c.Metadata.RequestTimeout)
}

func (c *Config) validateLibrary(ck *checker) {
	l := c.Library
	checkRoot(ck, "library.tv_root", l.TVRoot)
	checkRoot(ck, "library.movie_root", l.MovieRoot)

	ck.atLeast("library.min_video_size_mb", l.MinVideoSizeMB, 1)
	checkExts(ck, "library.video_extensions", l.VideoExtensions, true)
	checkExts(ck, "library.subtitle_extensions", l.SubtitleExtensions, false)

	switch {
	case l.RecycleDir == "":
		ck.warn("library.recycle_dir is empty; replaced files are deleted outright on upgrade.")
	case strings.ContainsAny(l.RecycleDir, `/\`):
		ck.bad("library.recycle_dir", "%q must be a bare folder name, not a path: it is created inside "+
			"each library root so that replacing a file is a rename rather than a copy across filesystems", l.RecycleDir)
	case l.RecycleDir == "." || l.RecycleDir == "..":
		ck.bad("library.recycle_dir", "%q is not a usable folder name", l.RecycleDir)
	}
	if l.AllowMove && l.Hardlink {
		ck.warn("library.allow_move and library.hardlink are both true; hardlink is attempted first and move only happens on fallback.")
	}
	if !l.Hardlink {
		ck.warn("library.hardlink is false; every import copies the file, doubling disk use and preventing the client from seeding the same bytes.")
	}
	if l.MaxPathLength < 64 {
		ck.bad("library.max_path_length", "%d is too small to hold a real filename", l.MaxPathLength)
	}

	for key, tpl := range map[string]string{
		"library.tv_folder_template":    l.TVFolderTemplate,
		"library.tv_file_template":      l.TVFileTemplate,
		"library.anime_file_template":   l.AnimeFileTemplate,
		"library.movie_folder_template": l.MovieFolderTemplate,
		"library.movie_file_template":   l.MovieFileTemplate,
	} {
		checkTemplate(ck, key, tpl)
	}

	if l.PostImportWebhook != "" {
		checkAbsoluteURL(ck, "library.post_import_webhook", l.PostImportWebhook)
	}
}

func (c *Config) validateSchedules(ck *checker) {
	s := c.Schedules
	for key, d := range map[string]Duration{
		"schedules.search_interval":   s.SearchInterval,
		"schedules.recent_interval":   s.RecentInterval,
		"schedules.status_interval":   s.StatusInterval,
		"schedules.metadata_interval": s.MetadataInterval,
	} {
		ck.positive(key, d)
	}
	if s.StatusInterval.Duration > 0 && s.StatusInterval.Duration < 5*time.Second {
		ck.warn("schedules.status_interval (%s) is very aggressive; the download client is polled that often forever.", s.StatusInterval)
	}
	if s.AirGrace.Duration < 0 {
		ck.bad("schedules.air_grace", "must not be negative")
	}
	if len(s.SearchBackoff) == 0 {
		ck.bad("schedules.search_backoff", "must contain at least one interval")
	}
	for i, d := range s.SearchBackoff {
		ck.positive(index("schedules.search_backoff", i), d)
		if i > 0 && d.Duration < s.SearchBackoff[i-1].Duration {
			ck.bad(index("schedules.search_backoff", i), "%s is shorter than the previous step %s; the list must be ascending",
				d, s.SearchBackoff[i-1])
		}
	}
	ck.positive("schedules.search_give_up_after", s.SearchGiveUpAfter)
}

func (c *Config) validateProfiles(ck *checker) {
	if len(c.Profiles) == 0 {
		ck.bad("profiles", "at least one seed quality profile is required")
		return
	}
	seen := map[string]bool{}
	defaults := 0
	for i, p := range c.Profiles {
		k := func(f string) string { return fmt.Sprintf("profiles[%d].%s", i, f) }
		if p.Name == "" {
			ck.bad(k("name"), "must not be empty")
		} else if seen[p.Name] {
			ck.bad(k("name"), "duplicate profile name %q", p.Name)
		}
		seen[p.Name] = true
		if p.Default {
			defaults++
		}

		if len(p.AllowedResolutions) == 0 {
			ck.bad(k("allowed_resolutions"), "must list at least one resolution, best first")
		}
		for j, r := range p.AllowedResolutions {
			ck.oneOf(index(k("allowed_resolutions"), j), r, validResolution)
		}
		if len(p.AllowedSources) == 0 {
			ck.bad(k("allowed_sources"), "must list at least one source, best first")
		}
		for j, src := range p.AllowedSources {
			ck.oneOf(index(k("allowed_sources"), j), src, validSources)
		}
		ck.notNegative(k("min_size_mb"), p.MinSizeMB)
		if p.MaxSizeMB <= p.MinSizeMB {
			ck.bad(k("max_size_mb"), "%d must be greater than min_size_mb (%d)", p.MaxSizeMB, p.MinSizeMB)
		}
		ck.notNegative(k("min_seeders"), p.MinSeeders)
		if p.MinSeeders == 0 {
			ck.warn("profiles[%d] (%s) has min_seeders 0; dead releases will be grabbed and stall.", i, p.Name)
		}
		if p.UpgradeUntil != "" && !slices.Contains(p.AllowedResolutions, p.UpgradeUntil) {
			ck.bad(k("upgrade_until"), "%q is not in this profile's allowed_resolutions", p.UpgradeUntil)
		}
		for j, h := range p.HDRPrefs {
			ck.oneOf(index(k("hdr_prefs"), j), h, validHDR)
		}
		for j, lang := range p.LanguagePrefs {
			if len(lang) != 2 {
				ck.bad(index(k("language_prefs"), j), "%q is not a two-letter ISO-639-1 code", lang)
			}
		}
		for _, names := range model.CaseDuplicateGroups(p.PreferredGroups) {
			quoted := make([]string, len(names))
			for j, name := range names {
				quoted[j] = fmt.Sprintf("%q", name)
			}
			ck.bad(k("preferred_groups"), "%s differ only by case; keep one", strings.Join(quoted, ", "))
		}
	}
	if defaults == 0 {
		ck.warn("no profile is flagged default; %q will be used.", c.Profiles[0].Name)
	} else if defaults > 1 {
		ck.bad("profiles", "%d profiles are flagged default; exactly one may be", defaults)
	}
}

func (c *Config) validateScoring(ck *checker) {
	s := c.Scoring
	for key, v := range map[string]int{
		"scoring.resolution_weight":    s.ResolutionWeight,
		"scoring.source_weight":        s.SourceWeight,
		"scoring.group_weight":         s.GroupWeight,
		"scoring.language_weight":      s.LanguageWeight,
		"scoring.proper_repack_weight": s.ProperRepackWeight,
		"scoring.seeder_weight_max":    s.SeederWeightMax,
		"scoring.hdr_weight":           s.HDRWeight,
		"scoring.season_pack_weight":   s.SeasonPackWeight,
		"scoring.age_penalty_per_day":  s.AgePenaltyPerDay,
		"scoring.age_penalty_max":      s.AgePenaltyMax,
	} {
		if v < 0 {
			ck.bad(key, "must not be negative (%d)", v)
		}
	}
	// An uncapped age penalty eventually dominates every quality signal: a
	// three-year-old 2160p remux would score -1095 and lose to fresh 720p.
	if s.AgePenaltyPerDay > 0 && s.AgePenaltyMax == 0 {
		ck.bad("scoring.age_penalty_max", "must be greater than zero when age_penalty_per_day is set, or old high-quality releases can never win")
	}
	if s.AgePenaltyMax >= s.ResolutionWeight {
		ck.bad("scoring.age_penalty_max", "%d is at least resolution_weight (%d); age would outrank quality", s.AgePenaltyMax, s.ResolutionWeight)
	}
}

// checkRoot enforces the hard rule from the spec: never write outside a real,
// existing, non-root directory.
func checkRoot(ck *checker, key, p string) {
	if strings.TrimSpace(p) == "" {
		ck.bad(key, "must not be empty")
		return
	}
	if strings.HasPrefix(p, "~") {
		ck.bad(key, "%q starts with ~; tilde is not expanded, give an absolute path", p)
		return
	}
	if isDriveRoot(p) {
		ck.bad(key, "%q is a filesystem root; Reelay refuses to manage one", p)
		return
	}
	// A dedicated network share is a legitimate library root — that is how a
	// NAS is normally laid out — but it means Reelay owns everything in it,
	// including whatever download folder has to live there for hardlinks.
	if isUNCShareRoot(p) {
		ck.warn("%s (%s) is the root of a network share, so Reelay manages the whole share. "+
			"The download folder has to live inside it for hardlinks to work; keep it dot-prefixed "+
			"and drop a .ignore file in it so the media server skips it.", key, p)
	}
	if !filepath.IsAbs(p) {
		ck.warn("%s (%s) is a relative path; it resolves against the working directory, which systemd may not set as you expect.", key, p)
	}
	st, err := os.Stat(p)
	switch {
	case errors.Is(err, os.ErrNotExist):
		ck.bad(key, "%q does not exist; create it (Reelay will not)", p)
	case err != nil:
		ck.bad(key, "%q is not readable: %v", p, err)
	case !st.IsDir():
		ck.bad(key, "%q is not a directory", p)
	}
}

// isDriveRoot reports whether p is "/", "\", "." or a Windows drive root such
// as "C:\". Cheap belt-and-braces against a catastrophic recursive delete.
//
// A UNC share root ("\\server\share") deliberately does NOT count: on Windows
// filepath.VolumeName returns the whole "\\server\share" prefix, so the naive
// "volume with nothing after it" test would reject every NAS share.
func isDriveRoot(p string) bool {
	clean := filepath.Clean(p)
	if clean == "/" || clean == "\\" || clean == "." {
		return true
	}
	vol := filepath.VolumeName(clean)
	if vol == "" || strings.HasPrefix(vol, `\\`) || strings.HasPrefix(vol, "//") {
		return false
	}
	return strings.Trim(strings.TrimPrefix(clean, vol), `\/`) == ""
}

// isUNCShareRoot reports whether p is exactly "\\server\share", with no
// subdirectory below it.
func isUNCShareRoot(p string) bool {
	clean := filepath.Clean(p)
	vol := filepath.VolumeName(clean)
	if !strings.HasPrefix(vol, `\\`) && !strings.HasPrefix(vol, "//") {
		return false
	}
	return strings.Trim(strings.TrimPrefix(clean, vol), `\/`) == ""
}

// looksNetworked spots UNC paths and drive-less network syntax. It is a
// heuristic: a mapped drive letter pointing at the NAS is indistinguishable
// from a local disk without syscalls we do not want at config-parse time.
func looksNetworked(p string) bool {
	return strings.HasPrefix(p, `\\`) || strings.HasPrefix(p, "//")
}

func checkExts(ck *checker, key string, exts []string, required bool) {
	if len(exts) == 0 {
		if required {
			ck.bad(key, "must list at least one extension")
		}
		return
	}
	for i, e := range exts {
		if !strings.HasPrefix(e, ".") {
			ck.bad(index(key, i), "%q must start with a dot", e)
		}
		if e != strings.ToLower(e) {
			ck.bad(index(key, i), "%q must be lowercase; matching is case-insensitive on the lowered form", e)
		}
	}
}

func checkTemplate(ck *checker, key, tpl string) {
	if strings.TrimSpace(tpl) == "" {
		ck.bad(key, "must not be empty")
		return
	}
	// A path separator in a *file* template would silently create directories.
	if strings.Contains(key, "file_template") && strings.ContainsAny(tpl, `/\`) {
		ck.bad(key, "must not contain a path separator; use the matching folder template for directory structure")
	}
	for _, m := range placeholderRe.FindAllStringSubmatch(tpl, -1) {
		if !templatePlaceholders[m[1]] {
			ck.bad(key, "unknown placeholder %s", m[0])
		}
	}
	if !strings.Contains(tpl, "{Title}") {
		ck.bad(key, "must contain {Title}")
	}
}

func isLoopback(bind string) bool {
	ip := net.ParseIP(bind)
	return ip != nil && ip.IsLoopback()
}
