package engine

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"
)

const (
	backupNameLayout = "20060102-150405"
	backupPrefix     = "reelay-"
	backupSuffix     = "Z.db"
)

// backupName matches only files Reelay itself wrote; anything else in the
// backup directory is never read, renamed, or deleted.
var backupName = regexp.MustCompile(`^reelay-\d{8}-\d{6}Z\.db$`)

// BackupStatus summarizes scheduled backups for the setup checks.
type BackupStatus struct {
	Enabled     bool
	Dir         string
	Keep        int
	Newest      time.Time // zero when no completed backup exists
	Count       int
	LastError   string
	LastAttempt time.Time // zero until the first attempt this process
}

// BackupOnce writes a snapshot when the newest one is older than
// backup_interval, then prunes to backup_keep. Called hourly by the loop.
func (e *Engine) BackupOnce(ctx context.Context) error {
	var now = e.clock.Now().UTC()
	var dir = e.cfg.Database.BackupDir
	var names []string
	var newest time.Time
	var final, partial string
	var err error

	names, err = backupFiles(dir, true)
	if err != nil {
		return e.recordBackup(now, err)
	}
	if len(names) > 0 {
		newest = backupTime(names[0])
	}
	if !newest.IsZero() && now.Sub(newest) < e.cfg.Database.BackupInterval.Duration {
		return nil
	}
	final = filepath.Join(dir, backupPrefix+now.Format(backupNameLayout)+backupSuffix)
	partial = final + ".partial"
	if err = e.store.Backup(ctx, partial); err == nil {
		err = os.Rename(partial, final)
	}
	if err != nil {
		_ = os.Remove(partial)
		return e.recordBackup(now, fmt.Errorf("scheduled backup: %w", err))
	}
	e.log.Info("scheduled backup written", "path", final)
	names, err = backupFiles(dir, false)
	if err == nil {
		for i := e.cfg.Database.BackupKeep; i < len(names); i++ {
			err = errors.Join(err, os.Remove(filepath.Join(dir, names[i])))
		}
	}
	return e.recordBackup(now, err)
}

// recordBackup stores the attempt outcome and returns err unchanged.
func (e *Engine) recordBackup(at time.Time, err error) error {
	e.backupMu.Lock()
	defer e.backupMu.Unlock()
	e.backupAttempt = at
	e.backupErr = ""
	if err != nil {
		e.backupErr = err.Error()
	}
	return err
}

// BackupStatus reads the directory at call time so it is correct after restarts.
func (e *Engine) BackupStatus() BackupStatus {
	var status = BackupStatus{Enabled: e.cfg.Database.BackupInterval.Duration > 0,
		Dir: e.cfg.Database.BackupDir, Keep: e.cfg.Database.BackupKeep}
	var names []string

	e.backupMu.Lock()
	status.LastError, status.LastAttempt = e.backupErr, e.backupAttempt
	e.backupMu.Unlock()
	if !status.Enabled {
		return status
	}
	names, _ = backupFiles(status.Dir, false)
	status.Count = len(names)
	if len(names) > 0 {
		status.Newest = backupTime(names[0])
	}
	return status
}

// backupFiles returns completed backup names, newest first. A missing directory
// is empty: the first backup creates it. Only the backup loop sets
// removePartials; a status read must not delete a snapshot still being written.
func backupFiles(dir string, removePartials bool) ([]string, error) {
	var entries, err = os.ReadDir(dir)
	var names []string

	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	for _, entry := range entries {
		var name = entry.Name()

		if entry.IsDir() {
			continue
		}
		if removePartials && strings.HasSuffix(name, ".db.partial") && backupName.MatchString(strings.TrimSuffix(name, ".partial")) {
			if err = os.Remove(filepath.Join(dir, name)); err != nil {
				return nil, err
			}
			continue
		}
		if backupName.MatchString(name) {
			names = append(names, name)
		}
	}
	sort.Sort(sort.Reverse(sort.StringSlice(names)))
	return names, nil
}

// backupTime parses the UTC timestamp from a name already matched by backupName.
func backupTime(name string) time.Time {
	var stamp = strings.TrimSuffix(strings.TrimPrefix(name, backupPrefix), backupSuffix)
	var t, err = time.ParseInLocation(backupNameLayout, stamp, time.UTC)

	if err != nil {
		return time.Time{}
	}
	return t
}
