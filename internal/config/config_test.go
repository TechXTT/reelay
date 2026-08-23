package config

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// writeConfig materialises the shipped example with the two library roots
// pointed at a temp dir, since Validate insists roots exist.
func writeConfig(t *testing.T, mutate func(s string) string) string {
	t.Helper()

	raw, err := os.ReadFile(filepath.Join("..", "..", "config.example.yaml"))
	if err != nil {
		t.Fatalf("read config.example.yaml: %v", err)
	}

	dir := t.TempDir()
	tv := filepath.Join(dir, "tv")
	movies := filepath.Join(dir, "movies")
	for _, d := range []string{tv, movies} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}

	s := string(raw)
	s = strings.Replace(s, `tv_root: "//nas/Series"`, `tv_root: `+quote(tv), 1)
	s = strings.Replace(s, `movie_root: "//nas/Movies"`, `movie_root: `+quote(movies), 1)
	s = strings.Replace(s, `save_path_tv: "//nas/Series/.reelay-downloads"`,
		`save_path_tv: `+quote(filepath.Join(tv, ".reelay-downloads")), 1)
	s = strings.Replace(s, `save_path_movies: "//nas/Movies/.reelay-downloads"`,
		`save_path_movies: `+quote(filepath.Join(movies, ".reelay-downloads")), 1)
	if mutate != nil {
		s = mutate(s)
	}

	path := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(path, []byte(s), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func quote(s string) string { return `"` + filepath.ToSlash(s) + `"` }

// The shipped example must load cleanly. If it does not, every quickstart in
// the README is broken.
func TestExampleConfigIsValid(t *testing.T) {
	path := writeConfig(t, nil)

	cfg, warnings, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Server.Port != 7878 {
		t.Errorf("port = %d, want 7878", cfg.Server.Port)
	}
	if cfg.Schedules.SearchInterval.String() != "15m0s" {
		t.Errorf("search_interval = %s, want 15m0s", cfg.Schedules.SearchInterval)
	}
	if len(cfg.Indexers) != 1 || cfg.Indexers[0].Name != "tpb" {
		t.Fatalf("indexers = %+v", cfg.Indexers)
	}
	// Deliberately slow: this indexer signals throttling by returning its
	// no-results row, so a too-fast default degrades searches silently rather
	// than erroring. See the comment in config.example.yaml.
	if got := cfg.Indexers[0].RateLimitPerSecond; got != 0.1 {
		t.Errorf("rate_limit_per_second = %v, want 0.1", got)
	}
	if p := cfg.DefaultProfile(); p == nil || p.Name != "TV 1080p" {
		t.Errorf("DefaultProfile() = %+v, want TV 1080p", p)
	}

	// The example ships with no auth token on loopback and no TMDB key: both
	// must warn, neither may fail.
	if !hasWarning(warnings, "auth_token") {
		t.Errorf("expected an auth_token warning, got %v", warnings)
	}
	if !hasWarning(warnings, "tmdb_api_key") {
		t.Errorf("expected a tmdb_api_key warning, got %v", warnings)
	}
}

func TestUnknownKeyIsRejected(t *testing.T) {
	path := writeConfig(t, func(s string) string {
		return strings.Replace(s, "  port: 7878", "  port: 7878\n  prot: 7879", 1)
	})
	_, _, err := Load(path)
	if err == nil {
		t.Fatal("expected a typo'd key to fail the load")
	}
	if !strings.Contains(err.Error(), "prot") {
		t.Errorf("error should name the offending key, got: %v", err)
	}
}

func TestEnvOverrides(t *testing.T) {
	path := writeConfig(t, nil)

	t.Setenv("REELAY_SERVER_PORT", "9999")
	t.Setenv("REELAY_SERVER_AUTH_TOKEN", "0123456789abcdef0123")
	t.Setenv("REELAY_LOGGING_LEVEL", "debug")
	t.Setenv("REELAY_SCHEDULES_SEARCH_INTERVAL", "45m")
	t.Setenv("REELAY_LIBRARY_HARDLINK", "false")
	t.Setenv("REELAY_SERVER_CORS_ORIGINS", "http://a.local, http://b.local")
	t.Setenv("REELAY_SCORING_RESOLUTION_WEIGHT", "1500")

	cfg, _, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Server.Port != 9999 {
		t.Errorf("port = %d, want 9999 from env", cfg.Server.Port)
	}
	if cfg.Server.AuthToken != "0123456789abcdef0123" {
		t.Errorf("auth_token not overridden: %q", cfg.Server.AuthToken)
	}
	if cfg.Logging.Level != "debug" {
		t.Errorf("level = %q, want debug", cfg.Logging.Level)
	}
	if cfg.Schedules.SearchInterval.String() != "45m0s" {
		t.Errorf("search_interval = %s, want 45m0s", cfg.Schedules.SearchInterval)
	}
	if cfg.Library.Hardlink {
		t.Error("hardlink should be false from env")
	}
	if want := []string{"http://a.local", "http://b.local"}; len(cfg.Server.CORSOrigins) != 2 ||
		cfg.Server.CORSOrigins[0] != want[0] || cfg.Server.CORSOrigins[1] != want[1] {
		t.Errorf("cors_origins = %q, want %q", cfg.Server.CORSOrigins, want)
	}
	if cfg.Scoring.ResolutionWeight != 1500 {
		t.Errorf("resolution_weight = %d, want 1500", cfg.Scoring.ResolutionWeight)
	}
}

func TestEnvOverrideRejectsGarbage(t *testing.T) {
	path := writeConfig(t, nil)
	t.Setenv("REELAY_SERVER_PORT", "not-a-number")

	_, _, err := Load(path)
	if err == nil {
		t.Fatal("expected a non-numeric port override to fail")
	}
	if !strings.Contains(err.Error(), "REELAY_SERVER_PORT") {
		t.Errorf("error should name the env var, got: %v", err)
	}
}

// The security rule from the spec: no token plus a routable bind address is a
// refusal to start, not a warning.
func TestUnauthenticatedNonLoopbackIsFatal(t *testing.T) {
	path := writeConfig(t, func(s string) string {
		return strings.Replace(s, `bind: "127.0.0.1"`, `bind: "0.0.0.0"`, 1)
	})
	_, _, err := Load(path)
	if err == nil {
		t.Fatal("expected an empty auth_token on 0.0.0.0 to be fatal")
	}
	if !strings.Contains(err.Error(), "server.auth_token") {
		t.Errorf("error should name server.auth_token, got: %v", err)
	}
}

func TestValidationReportsEveryProblemAtOnce(t *testing.T) {
	path := writeConfig(t, func(s string) string {
		s = strings.Replace(s, "  port: 7878", "  port: 99999", 1)
		s = strings.Replace(s, "  level: info", "  level: shouty", 1)
		s = strings.Replace(s, `  type: qbittorrent`, `  type: deluge`, 1)
		return s
	})

	_, _, err := Load(path)
	if err == nil {
		t.Fatal("expected validation to fail")
	}
	var probs *Problems
	if !asProblems(err, &probs) {
		t.Fatalf("expected *Problems, got %T", err)
	}
	if len(probs.Items) < 3 {
		t.Errorf("expected at least 3 problems reported together, got %d: %v", len(probs.Items), probs.Items)
	}
	for _, want := range []string{"server.port", "logging.level", "downloader.type"} {
		if !hasWarning(probs.Items, want) {
			t.Errorf("problems should mention %s: %v", want, probs.Items)
		}
	}
}

func TestRootFolderRules(t *testing.T) {
	cases := []struct {
		name string
		root string
		want string
	}{
		{"empty", `""`, "must not be empty"},
		{"tilde", `"~/media/tv"`, "tilde is not expanded"},
		{"posix root", `"/"`, "filesystem root"},
		{"missing", `"./definitely-not-here-8f3a"`, "does not exist"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			path := writeConfig(t, func(s string) string {
				// Replace whatever writeConfig substituted in for tv_root.
				i := strings.Index(s, "  tv_root: ")
				j := strings.Index(s[i:], "\n") + i
				return s[:i] + "  tv_root: " + tc.root + s[j:]
			})
			_, _, err := Load(path)
			if err == nil {
				t.Fatalf("expected tv_root %s to be rejected", tc.root)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error %q should contain %q", err.Error(), tc.want)
			}
		})
	}
}

func TestTemplateValidation(t *testing.T) {
	cases := []struct {
		name    string
		replace string
		with    string
		want    string
	}{
		{
			name:    "unknown placeholder",
			replace: `  movie_folder_template: "{Title} ({Year})"`,
			with:    `  movie_folder_template: "{Title} ({Yeer})"`,
			want:    "unknown placeholder {Yeer}",
		},
		{
			name:    "separator in file template",
			replace: `  movie_file_template: "{Title} ({Year}) [{Resolution} {Source} {VideoCodec}]-{Group}"`,
			with:    `  movie_file_template: "{Title}/{Year}"`,
			want:    "must not contain a path separator",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			path := writeConfig(t, func(s string) string {
				out := strings.Replace(s, tc.replace, tc.with, 1)
				if out == s {
					t.Fatalf("test fixture did not match: %q", tc.replace)
				}
				return out
			})
			_, _, err := Load(path)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Errorf("want error containing %q, got %v", tc.want, err)
			}
		})
	}
}

// An uncapped age penalty eventually outweighs resolution, which is the bug
// the spec's default weights would have shipped with.
func TestUncappedAgePenaltyIsRejected(t *testing.T) {
	path := writeConfig(t, func(s string) string {
		return strings.Replace(s, "  age_penalty_max: 60", "  age_penalty_max: 0", 1)
	})
	_, _, err := Load(path)
	if err == nil || !strings.Contains(err.Error(), "scoring.age_penalty_max") {
		t.Errorf("want an age_penalty_max problem, got %v", err)
	}
}

func TestBackoffMustAscend(t *testing.T) {
	path := writeConfig(t, func(s string) string {
		return strings.Replace(s, "  search_backoff: [15m, 30m, 1h, 2h, 4h, 6h]",
			"  search_backoff: [15m, 5m, 1h]", 1)
	})
	_, _, err := Load(path)
	if err == nil || !strings.Contains(err.Error(), "ascending") {
		t.Errorf("want an ascending-backoff problem, got %v", err)
	}
}

// recycle_dir has to be a bare name so the recycle folder always lands on the
// same filesystem as the file it is replacing.
func TestRecycleDirRejectsAPath(t *testing.T) {
	path := writeConfig(t, func(s string) string {
		return strings.Replace(s, `  recycle_dir: ".reelay-recycle"`,
			`  recycle_dir: "//nas/Series/.recycle"`, 1)
	})
	_, _, err := Load(path)
	if err == nil || !strings.Contains(err.Error(), "library.recycle_dir") {
		t.Errorf("want a recycle_dir problem, got %v", err)
	}
}

// A UNC share root is a legitimate library root — a NAS share is exactly how
// this gets deployed — but it must warn, because Reelay then owns the share.
func TestUNCShareRootWarnsButIsAllowed(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("UNC paths are a Windows concept")
	}
	// Only the classification matters here; Stat will fail on a fake host, so
	// assert on isUNCShareRoot/isDriveRoot directly.
	if !isUNCShareRoot(`\\Azeroth\Series`) {
		t.Error(`\\Azeroth\Series should be recognised as a UNC share root`)
	}
	if isDriveRoot(`\\Azeroth\Series`) {
		t.Error(`\\Azeroth\Series must not be treated as a drive root, or no NAS share can ever be a library`)
	}
	if isUNCShareRoot(`\\Azeroth\Series\Shows`) {
		t.Error(`a subdirectory of a share is not a share root`)
	}
	for _, p := range []string{`C:\`, `C:/`, "/", `\`} {
		if !isDriveRoot(p) {
			t.Errorf("%q should be rejected as a drive root", p)
		}
	}
	// A bare "C:" is drive-*relative* ("current directory on C:"), not a root:
	// filepath.Clean turns it into "C:.". It is caught by the not-absolute
	// warning instead.
	if filepath.IsAbs(`C:`) {
		t.Error(`expected "C:" to be a relative path on Windows`)
	}
}

func TestEmptyDownloaderCategoryIsFatal(t *testing.T) {
	path := writeConfig(t, func(s string) string {
		return strings.Replace(s, `  category_tv: "reelay-tv"`, `  category_tv: ""`, 1)
	})
	_, _, err := Load(path)
	if err == nil || !strings.Contains(err.Error(), "downloader.category_tv") {
		t.Errorf("want a category_tv problem, got %v", err)
	}
}

func TestEnvKey(t *testing.T) {
	cases := map[string]string{
		"server.auth_token":       "REELAY_SERVER_AUTH_TOKEN",
		"downloader.password":     "REELAY_DOWNLOADER_PASSWORD",
		"runtime.sqlite_cache_kb": "REELAY_RUNTIME_SQLITE_CACHE_KB",
	}
	for in, want := range cases {
		if got := EnvKey(in); got != want {
			t.Errorf("EnvKey(%q) = %q, want %q", in, got, want)
		}
	}
}

func hasWarning(list []string, substr string) bool {
	for _, w := range list {
		if strings.Contains(w, substr) {
			return true
		}
	}
	return false
}

func asProblems(err error, target **Problems) bool {
	p, ok := err.(*Problems)
	if ok {
		*target = p
	}
	return ok
}
