package config

import (
	"errors"
	"fmt"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"
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

var (
	validLogLevels  = []string{"debug", "info", "warn", "error"}
	validLogFormats = []string{"json", "text"}
	validResolution = []string{"2160p", "1080p", "720p", "480p"}
	validSources    = []string{"remux", "bluray", "webdl", "webrip", "hdtv", "dvd", "cam"}
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

	sort.Strings(ck.problems)
	if len(ck.problems) > 0 {
		return ck.warnings, &Problems{Items: ck.problems}
	}
	return ck.warnings, nil
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
			ck.bad(fmt.Sprintf("server.cors_origins[%d]", i), "%q is not an absolute URL", o)
		}
	}
	if c.Server.ReadTimeout.Duration <= 0 {
		ck.bad("server.read_timeout", "must be greater than zero")
	}
	if c.Server.WriteTimeout.Duration < 0 {
		ck.bad("server.write_timeout", "must not be negative (0 disables the deadline, required for SSE)")
	}
	if c.Server.ShutdownTimeout.Duration <= 0 {
		ck.bad("server.shutdown_timeout", "must be greater than zero")
	}
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
}

func (c *Config) validateLogging(ck *checker) {
	if !contains(validLogLevels, c.Logging.Level) {
		ck.bad("logging.level", "%q is not one of %s", c.Logging.Level, strings.Join(validLogLevels, ", "))
	}
	if !contains(validLogFormats, c.Logging.Format) {
		ck.bad("logging.format", "%q is not one of %s", c.Logging.Format, strings.Join(validLogFormats, ", "))
	}
}

func (c *Config) validateRuntime(ck *checker) {
	if c.Runtime.SQLiteCacheKB < 64 {
		ck.bad("runtime.sqlite_cache_kb", "%d is too small; use at least 64", c.Runtime.SQLiteCacheKB)
	}
	if c.Runtime.SearchConcurrency < 1 {
		ck.bad("runtime.search_concurrency", "must be at least 1")
	}
	if c.Runtime.MaxSSEClients < 1 {
		ck.bad("runtime.max_sse_clients", "must be at least 1")
	}
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

		if ix.Type != "piratebay" {
			ck.bad(k("type"), "%q is not a supported indexer type (have: piratebay)", ix.Type)
		}
		if u, err := url.Parse(ix.BaseURL); err != nil || u.Scheme == "" || u.Host == "" {
			ck.bad(k("base_url"), "%q is not an absolute http(s) URL", ix.BaseURL)
		}
		if ix.UserAgent == "" {
			ck.bad(k("user_agent"), "must not be empty")
		}
		if ix.RateLimitPerSecond <= 0 {
			ck.bad(k("rate_limit_per_second"), "must be greater than zero")
		}
		if ix.RateLimitBurst < 1 {
			ck.bad(k("rate_limit_burst"), "must be at least 1")
		}
		if ix.RequestTimeout.Duration <= 0 {
			ck.bad(k("request_timeout"), "must be greater than zero")
		}
		if ix.MaxRetries < 0 {
			ck.bad(k("max_retries"), "must not be negative")
		}
		if ix.FailureThreshold < 1 {
			ck.bad(k("failure_threshold"), "must be at least 1")
		}
		if ix.BreakerCooldown.Duration <= 0 {
			ck.bad(k("breaker_cooldown"), "must be greater than zero")
		}
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
	if u, err := url.Parse(d.URL); err != nil || u.Scheme == "" || u.Host == "" {
		ck.bad("downloader.url", "%q is not an absolute http(s) URL", d.URL)
	}
	// The category is the safety boundary: Reelay only ever touches torrents
	// it labelled itself. An empty category would make every torrent in the
	// client fair game.
	if strings.TrimSpace(d.CategoryTV) == "" {
		ck.bad("downloader.category_tv", "must not be empty; it is how Reelay avoids touching your other torrents")
	}
	if strings.TrimSpace(d.CategoryMovies) == "" {
		ck.bad("downloader.category_movies", "must not be empty; it is how Reelay avoids touching your other torrents")
	}
	if d.CategoryTV == d.CategoryMovies && d.CategoryTV != "" {
		ck.warn("downloader.category_tv and category_movies are identical (%q); imports will still work but the client view is harder to read.", d.CategoryTV)
	}
	if d.SavePathTV == "" {
		ck.bad("downloader.save_path_tv", "must not be empty")
	}
	if d.SavePathMovies == "" {
		ck.bad("downloader.save_path_movies", "must not be empty")
	}
	if d.StallTimeout.Duration <= 0 {
		ck.bad("downloader.stall_timeout", "must be greater than zero")
	}
	for i, m := range d.PathMappings {
		if m.DownloaderPrefix == "" {
			ck.bad(fmt.Sprintf("downloader.path_mappings[%d].downloader_prefix", i), "must not be empty")
		}
		if m.LocalPrefix == "" {
			ck.bad(fmt.Sprintf("downloader.path_mappings[%d].local_prefix", i), "must not be empty")
		}
	}
}

func (c *Config) validateMetadata(ck *checker) {
	if u, err := url.Parse(c.Metadata.TVmazeBaseURL); err != nil || u.Scheme == "" || u.Host == "" {
		ck.bad("metadata.tvmaze_base_url", "%q is not an absolute http(s) URL", c.Metadata.TVmazeBaseURL)
	}
	if u, err := url.Parse(c.Metadata.TMDBBaseURL); err != nil || u.Scheme == "" || u.Host == "" {
		ck.bad("metadata.tmdb_base_url", "%q is not an absolute http(s) URL", c.Metadata.TMDBBaseURL)
	}
	if c.Metadata.TMDBAPIKey == "" {
		ck.warn("metadata.tmdb_api_key is empty; movie lookups fall back to the title and year you type.")
	}
	if c.Metadata.CacheTTL.Duration <= 0 {
		ck.bad("metadata.cache_ttl", "must be greater than zero")
	}
	if c.Metadata.RequestTimeout.Duration <= 0 {
		ck.bad("metadata.request_timeout", "must be greater than zero")
	}
}

func (c *Config) validateLibrary(ck *checker) {
	l := c.Library
	checkRoot(ck, "library.tv_root", l.TVRoot)
	checkRoot(ck, "library.movie_root", l.MovieRoot)

	if l.MinVideoSizeMB < 1 {
		ck.bad("library.min_video_size_mb", "must be at least 1")
	}
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
		if u, err := url.Parse(l.PostImportWebhook); err != nil || u.Scheme == "" || u.Host == "" {
			ck.bad("library.post_import_webhook", "%q is not an absolute http(s) URL", l.PostImportWebhook)
		}
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
		if d.Duration <= 0 {
			ck.bad(key, "must be greater than zero")
		}
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
		if d.Duration <= 0 {
			ck.bad(fmt.Sprintf("schedules.search_backoff[%d]", i), "must be greater than zero")
		}
		if i > 0 && d.Duration < s.SearchBackoff[i-1].Duration {
			ck.bad(fmt.Sprintf("schedules.search_backoff[%d]", i), "%s is shorter than the previous step %s; the list must be ascending",
				d, s.SearchBackoff[i-1])
		}
	}
	if s.SearchGiveUpAfter.Duration <= 0 {
		ck.bad("schedules.search_give_up_after", "must be greater than zero")
	}
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
			if !contains(validResolution, r) {
				ck.bad(fmt.Sprintf("%s[%d]", k("allowed_resolutions"), j),
					"%q is not one of %s", r, strings.Join(validResolution, ", "))
			}
		}
		if len(p.AllowedSources) == 0 {
			ck.bad(k("allowed_sources"), "must list at least one source, best first")
		}
		for j, s := range p.AllowedSources {
			if !contains(validSources, s) {
				ck.bad(fmt.Sprintf("%s[%d]", k("allowed_sources"), j),
					"%q is not one of %s", s, strings.Join(validSources, ", "))
			}
		}
		if p.MinSizeMB < 0 {
			ck.bad(k("min_size_mb"), "must not be negative")
		}
		if p.MaxSizeMB <= p.MinSizeMB {
			ck.bad(k("max_size_mb"), "%d must be greater than min_size_mb (%d)", p.MaxSizeMB, p.MinSizeMB)
		}
		if p.MinSeeders < 0 {
			ck.bad(k("min_seeders"), "must not be negative")
		}
		if p.MinSeeders == 0 {
			ck.warn("profiles[%d] (%s) has min_seeders 0; dead releases will be grabbed and stall.", i, p.Name)
		}
		if p.UpgradeUntil != "" && !contains(p.AllowedResolutions, p.UpgradeUntil) {
			ck.bad(k("upgrade_until"), "%q is not in this profile's allowed_resolutions", p.UpgradeUntil)
		}
		for j, h := range p.HDRPrefs {
			if !contains([]string{"hdr10", "hdr10plus", "dv", "hlg"}, h) {
				ck.bad(fmt.Sprintf("%s[%d]", k("hdr_prefs"), j),
					"%q is not one of hdr10, hdr10plus, dv, hlg", h)
			}
		}
		for j, lang := range p.LanguagePrefs {
			if len(lang) != 2 {
				ck.bad(fmt.Sprintf("%s[%d]", k("language_prefs"), j),
					"%q is not a two-letter ISO-639-1 code", lang)
			}
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
			ck.bad(fmt.Sprintf("%s[%d]", key, i), "%q must start with a dot", e)
		}
		if e != strings.ToLower(e) {
			ck.bad(fmt.Sprintf("%s[%d]", key, i), "%q must be lowercase; matching is case-insensitive on the lowered form", e)
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

func contains(haystack []string, needle string) bool {
	for _, h := range haystack {
		if h == needle {
			return true
		}
	}
	return false
}
