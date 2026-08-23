// Package buildinfo carries version strings stamped in at link time.
package buildinfo

import (
	"fmt"
	"runtime"
)

// Overridden via -ldflags "-X github.com/TechXTT/reelay/internal/buildinfo.Version=..."
var (
	Version = "dev"
	Commit  = "none"
	Date    = "unknown"
)

type Info struct {
	Version   string `json:"version"`
	Commit    string `json:"commit"`
	Date      string `json:"date"`
	GoVersion string `json:"go_version"`
	Platform  string `json:"platform"`
}

func Get() Info {
	return Info{
		Version:   Version,
		Commit:    Commit,
		Date:      Date,
		GoVersion: runtime.Version(),
		Platform:  runtime.GOOS + "/" + runtime.GOARCH,
	}
}

func (i Info) String() string {
	return fmt.Sprintf("reelay %s (commit %s, built %s, %s, %s)",
		i.Version, i.Commit, i.Date, i.GoVersion, i.Platform)
}
