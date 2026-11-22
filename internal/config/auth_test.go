package config

import (
	"testing"
	"time"
)

func TestAuthIsOffInTheExample(t *testing.T) {
	cfg, _, err := Load(writeConfig(t, nil))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Auth.LoginsEnabled || cfg.Auth.MemberRequestQuota != 0 {
		t.Errorf("example auth = %+v, want logins off and no quota", cfg.Auth)
	}
}

func TestAuthEnvOverrides(t *testing.T) {
	t.Setenv("REELAY_AUTH_LOGINS_ENABLED", "true")
	t.Setenv("REELAY_AUTH_SESSION_TTL", "72h")
	t.Setenv("REELAY_AUTH_MEMBER_REQUEST_QUOTA", "5")
	t.Setenv("REELAY_AUTH_MEMBER_QUOTA_WINDOW", "24h")
	cfg := &Config{}
	if err := applyEnv(cfg); err != nil {
		t.Fatal(err)
	}
	if !cfg.Auth.LoginsEnabled || cfg.Auth.SessionTTL.Duration != 72*time.Hour ||
		cfg.Auth.MemberRequestQuota != 5 || cfg.Auth.MemberQuotaWindow.Duration != 24*time.Hour {
		t.Errorf("env auth = %+v", cfg.Auth)
	}
}

func TestAuthValidation(t *testing.T) {
	enabled := func(c *Config) {
		c.Server.AuthToken = "0123456789abcdef-token"
		c.Availability.JellyfinServers = map[string]string{"home": "http://jellyfin.local:8096"}
		c.Auth = Auth{LoginsEnabled: true, SessionTTL: Dur(168 * time.Hour)}
	}
	cases := []struct {
		name   string
		mutate func(c *Config)
		want   string
	}{
		{"ttl below one hour", func(c *Config) { enabled(c); c.Auth.SessionTTL = Dur(30 * time.Minute) }, "auth.session_ttl"},
		{"ttl unset", func(c *Config) { enabled(c); c.Auth.SessionTTL = Dur(0) }, "auth.session_ttl"},
		{"ttl above one year", func(c *Config) { enabled(c); c.Auth.SessionTTL = Dur(8761 * time.Hour) }, "auth.session_ttl"},
		{"no static token", func(c *Config) { enabled(c); c.Server.AuthToken = "" }, "requires server.auth_token"},
		{"no jellyfin server", func(c *Config) { enabled(c); c.Availability.JellyfinServers = nil }, "availability.jellyfin_servers"},
		{"negative quota", func(c *Config) { c.Auth.MemberRequestQuota = -1 }, "auth.member_request_quota"},
		{"quota without window", func(c *Config) { c.Auth.MemberRequestQuota, c.Auth.MemberQuotaWindow = 3, Dur(0) }, "auth.member_quota_window"},
		{"quota with short window", func(c *Config) { c.Auth.MemberRequestQuota, c.Auth.MemberQuotaWindow = 3, Dur(time.Minute) }, "auth.member_quota_window"},
	}
	for _, tc := range cases {
		if items := backupConfigProblems(t, tc.mutate); !hasWarning(items, tc.want) {
			t.Errorf("%s: want a %s problem, got %v", tc.name, tc.want, items)
		}
	}

	valid := map[string]func(c *Config){
		"enabled":               enabled,
		"one hour ttl":          func(c *Config) { enabled(c); c.Auth.SessionTTL = Dur(time.Hour) },
		"one year ttl":          func(c *Config) { enabled(c); c.Auth.SessionTTL = Dur(8760 * time.Hour) },
		"quota with window":     func(c *Config) { enabled(c); c.Auth.MemberRequestQuota, c.Auth.MemberQuotaWindow = 3, Dur(time.Hour) },
		"window ignored at 0":   func(c *Config) { c.Auth.MemberQuotaWindow = Dur(time.Second) },
		"disabled needs no ttl": func(c *Config) { c.Auth.LoginsEnabled = false },
	}
	for name, mutate := range valid {
		if items := backupConfigProblems(t, mutate); len(items) != 0 {
			t.Errorf("%s: valid auth config rejected: %v", name, items)
		}
	}
}
