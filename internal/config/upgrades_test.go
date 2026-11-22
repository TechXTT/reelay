package config

import (
	"strings"
	"testing"
	"time"
)

func TestUpgradesAreDisabledInTheExampleAndNeedNothingElse(t *testing.T) {
	cfg, _, err := Load(writeConfig(t, nil))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Schedules.UpgradeInterval.Duration != 0 {
		t.Errorf("example upgrade_interval = %s, want 0 (disabled)", cfg.Schedules.UpgradeInterval)
	}
	if cfg.Downloader.MaxUpgradeGrabs != 2 {
		t.Errorf("example max_upgrade_grabs = %d, want 2", cfg.Downloader.MaxUpgradeGrabs)
	}
	missing := writeConfig(t, func(s string) string {
		s = strings.Replace(s, "  upgrade_interval: 0\r\n", "", 1)
		s = strings.Replace(s, "  upgrade_interval: 0\n", "", 1)
		s = strings.Replace(s, "  max_upgrade_grabs: 2\r\n", "", 1)
		return strings.Replace(s, "  max_upgrade_grabs: 2\n", "", 1)
	})
	cfg, _, err = Load(missing)
	if err != nil {
		t.Fatalf("config without upgrade keys rejected: %v", err)
	}
	if cfg.Schedules.UpgradeInterval.Duration != 0 || cfg.Downloader.MaxUpgradeGrabs != 0 {
		t.Errorf("missing keys = %s / %d, want disabled defaults", cfg.Schedules.UpgradeInterval, cfg.Downloader.MaxUpgradeGrabs)
	}
}

func TestUpgradeValidation(t *testing.T) {
	enabled := func(c *Config) {
		c.Schedules.UpgradeInterval = Dur(168 * time.Hour)
		c.Downloader.MaxUpgradeGrabs = 2
	}
	cases := []struct {
		name   string
		mutate func(c *Config)
		want   string
	}{
		{"interval below one hour", func(c *Config) { enabled(c); c.Schedules.UpgradeInterval = Dur(30 * time.Minute) }, "schedules.upgrade_interval"},
		{"negative interval", func(c *Config) { enabled(c); c.Schedules.UpgradeInterval = Dur(-time.Hour) }, "schedules.upgrade_interval"},
		{"enabled without a grab cap", func(c *Config) { enabled(c); c.Downloader.MaxUpgradeGrabs = 0 }, "downloader.max_upgrade_grabs"},
		{"negative cap while enabled", func(c *Config) { enabled(c); c.Downloader.MaxUpgradeGrabs = -1 }, "downloader.max_upgrade_grabs"},
		{"negative cap while disabled", func(c *Config) { c.Downloader.MaxUpgradeGrabs = -1 }, "downloader.max_upgrade_grabs"},
	}
	for _, tc := range cases {
		if items := backupConfigProblems(t, tc.mutate); !hasWarning(items, tc.want) {
			t.Errorf("%s: want a %s problem, got %v", tc.name, tc.want, items)
		}
	}
	if items := backupConfigProblems(t, func(c *Config) { enabled(c); c.Downloader.MaxUpgradeGrabs = -1 }); len(items) != 1 {
		t.Errorf("negative cap while enabled should be reported once, got %v", items)
	}
	if items := backupConfigProblems(t, enabled); len(items) != 0 {
		t.Errorf("valid enabled upgrade config rejected: %v", items)
	}
	disabled := func(c *Config) { c.Schedules.UpgradeInterval, c.Downloader.MaxUpgradeGrabs = Dur(0), 0 }
	if items := backupConfigProblems(t, disabled); len(items) != 0 {
		t.Errorf("disabled upgrades must need nothing else: %v", items)
	}
	if items := backupConfigProblems(t, func(c *Config) { c.Schedules.UpgradeInterval = Dur(time.Hour); c.Downloader.MaxUpgradeGrabs = 1 }); len(items) != 0 {
		t.Errorf("the one hour minimum must be accepted: %v", items)
	}
}
