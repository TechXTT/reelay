package config

import "testing"

func TestUserNtfyBaseURLValidation(t *testing.T) {
	var bad = []string{"ntfy.sh", "ftp://ntfy.sh", "https://user:pw@ntfy.sh", "https://ntfy.sh/#frag", "https://ntfy.sh/?auth=1", "https://"}
	var good = []string{"", "https://ntfy.sh", "http://ntfy.local:8080/prefix"}

	for _, value := range bad {
		var items = backupConfigProblems(t, func(c *Config) { c.Availability.UserNtfyBaseURL = value })

		if !hasWarning(items, "availability.user_ntfy_base_url") {
			t.Errorf("%q: want an availability.user_ntfy_base_url problem, got %v", value, items)
		}
	}
	for _, value := range good {
		if items := backupConfigProblems(t, func(c *Config) { c.Availability.UserNtfyBaseURL = value }); len(items) != 0 {
			t.Errorf("%q rejected: %v", value, items)
		}
	}
}
