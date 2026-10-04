package config

import (
	"strings"
	"testing"
)

func TestNegativeMinFreeSpaceIsRejected(t *testing.T) {
	path := writeConfig(t, func(s string) string {
		return strings.Replace(s, `min_free_space_mb: 2048`, `min_free_space_mb: -1`, 1)
	})
	_, _, err := Load(path)
	if err == nil || !strings.Contains(err.Error(), "downloader.min_free_space_mb") {
		t.Errorf("want a min_free_space_mb problem, got %v", err)
	}
}
