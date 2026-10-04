package api

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"github.com/TechXTT/reelay/internal/engine"
	"github.com/TechXTT/reelay/internal/fsprobe"
)

func (s *Server) handleSetup(w http.ResponseWriter, r *http.Request) error {
	var ctx, cancel = context.WithTimeout(r.Context(), 10*time.Second)
	var checks = make([]map[string]any, 0)
	var add = func(name string, err error, action string) {
		status, detail := "ok", "Connection succeeded"
		if err != nil {
			status, detail = "down", err.Error()
		}
		checks = append(checks, map[string]any{"name": name, "status": status, "detail": detail, "action": action})
	}

	defer cancel()
	add("Database", s.store.Ping(ctx), "Keep SQLite on local storage and check service-account permissions.")
	if s.downloader != nil {
		add("Download client", s.downloader.Healthy(ctx), "Check the URL, credentials, and Reelay categories in the download client.")
	}
	for _, indexer := range s.indexers {
		add("Indexer "+indexer.Name(), indexer.Healthy(ctx), "Check the endpoint and credentials; wait for the circuit-breaker retry time.")
		if checks[len(checks)-1]["status"] == "ok" {
			checks[len(checks)-1]["detail"] = "Circuit breaker permits searches; connectivity is checked during a search."
		}
	}
	for name, root := range map[string]string{"TV library": s.cfg.Library.TVRoot, "Movie library": s.cfg.Library.MovieRoot} {
		info, err := os.Stat(root)
		if err == nil && !info.IsDir() {
			err = os.ErrInvalid
		}
		add(name, err, "Mount the library at the configured path and grant the service account access.")
		available, spaceErr := fsprobe.FreeSpace(root)
		detail := "Available disk space"
		reserve := uint64(s.cfg.Downloader.MinFreeSpaceMB) << 20
		belowReserve := spaceErr == nil && reserve > 0 && available < reserve
		if spaceErr != nil {
			detail = spaceErr.Error()
		} else if belowReserve {
			detail = fmt.Sprintf("Available disk space is below the configured reserve of %d MB (downloader.min_free_space_mb)", s.cfg.Downloader.MinFreeSpaceMB)
		}
		checks = append(checks, map[string]any{"name": name + " free space", "status": map[bool]string{true: "ok", false: "down"}[spaceErr == nil && !belowReserve], "detail": detail, "available_bytes": available, "action": "Keep enough space for downloaded media and for a copy when hardlinks are unavailable."})
	}
	for _, mapping := range s.cfg.Downloader.PathMappings {
		_, err := os.Stat(mapping.LocalPrefix)
		add("Download path "+mapping.DownloaderPrefix, err, "Map the download client's path to the same files mounted at local_prefix on the Reelay host.")
	}
	users, err := s.store.Recommendations().Users(ctx)
	if err != nil {
		return err
	}
	checks = append(checks, map[string]any{"name": "Jellyfin Discover", "status": map[bool]string{true: "ok", false: "skipped"}[len(users) > 0], "detail": "Synchronize users using the Reelay Jellyfin plugin; enable Discover for each intended user.", "action": "In the plugin, configure Reelay URL and token, synchronize, then enable the user's Discover library."})
	if s.engine != nil {
		checks = append(checks, backupCheck(s.engine.BackupStatus(), s.cfg.Database.BackupInterval.Duration, s.clock.Now().UTC()))
	}
	return reply(w, r, http.StatusOK, map[string]any{"checks": checks, "users": users, "database": filepath.Base(s.store.Path()), "webhook_enabled": s.cfg.Availability.WebhookURL != ""})
}

func (s *Server) handleDatabaseBackup(w http.ResponseWriter, r *http.Request) error {
	var directory, err = os.MkdirTemp("", "reelay-backup-")

	if err != nil {
		return Internal(err)
	}
	defer os.RemoveAll(directory)
	backup := filepath.Join(directory, "reelay.db")
	if err := s.store.Backup(r.Context(), backup); err != nil {
		return Conflict("database backup failed").WithCause(err)
	}
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Content-Disposition", `attachment; filename="reelay-backup.db"`)
	w.Header().Set("Cache-Control", "no-store")
	http.ServeFile(w, r, backup)
	return nil
}

// backupCheck reports scheduled-backup health. Down means the last attempt
// failed, or backups are over twice the interval old after at least one attempt.
func backupCheck(status engine.BackupStatus, interval time.Duration, now time.Time) map[string]any {
	var state, detail = "ok", "No backup written yet"

	if !status.Enabled {
		return map[string]any{"name": "Scheduled backups", "status": "skipped", "detail": "Disabled; set database.backup_interval, backup_dir, and backup_keep to enable.", "action": backupAction}
	}
	if !status.Newest.IsZero() {
		detail = "Newest backup " + status.Newest.UTC().Format(time.RFC3339)
	}
	detail += fmt.Sprintf("; %d kept (keep %d)", status.Count, status.Keep)
	if status.LastError != "" {
		state = "down"
		detail += "; last error: " + status.LastError
	} else if !status.LastAttempt.IsZero() && (status.Newest.IsZero() || now.Sub(status.Newest) > 2*interval) {
		state = "down"
		detail += "; newest backup is older than twice the interval"
	}
	return map[string]any{"name": "Scheduled backups", "status": state, "detail": detail, "action": backupAction}
}

const backupAction = "Check that backup_dir exists on local or NAS storage with free space and that the service account can write to it."
