package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"
)

func (s *Store) Backup(ctx context.Context, destination string) error {
	return snapshotDatabase(ctx, s.ro, destination)
}

// Restore creates a fresh destination. Operators stop the service and preserve
// the old database before running it; an existing destination is never replaced.
func Restore(ctx context.Context, source, destination string) error {
	var info, err = os.Stat(source)
	var database *sql.DB
	var result string

	if err != nil || !info.Mode().IsRegular() {
		return fmt.Errorf("restore source must be an existing regular database file")
	}
	absolute, err := filepath.Abs(source)
	if err != nil {
		return err
	}
	uri := "file:" + strings.NewReplacer("%", "%25", "?", "%3F", "#", "%23").Replace(filepath.ToSlash(absolute)) + "?" + url.Values{"mode": {"ro"}}.Encode()
	database, err = sql.Open("sqlite", uri)
	if err != nil {
		return err
	}
	defer database.Close()
	if err := database.QueryRowContext(ctx, "PRAGMA quick_check").Scan(&result); err != nil {
		return fmt.Errorf("backup integrity check failed: %w", err)
	}
	if result != "ok" {
		return errors.New("backup integrity check failed")
	}
	var schema int
	if err := database.QueryRowContext(ctx, "SELECT MAX(version) FROM schema_migrations").Scan(&schema); err != nil {
		return errors.New("source is not a Reelay database")
	}
	versions, err := loadMigrations()
	if err != nil {
		return err
	}
	if schema > maxVersion(versions) {
		return errors.New("backup is from a newer Reelay binary")
	}
	rows, err := database.QueryContext(ctx, "SELECT version,checksum FROM schema_migrations ORDER BY version")
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var version int
		var checksum string
		if err := rows.Scan(&version, &checksum); err != nil {
			return err
		}
		valid := false
		for _, migration := range versions {
			if migration.Version == version && migration.Checksum == checksum {
				valid = true
				break
			}
		}
		if !valid {
			return errors.New("backup schema does not match this Reelay binary")
		}
	}
	if err := rows.Err(); err != nil {
		return err
	}
	if err := rows.Close(); err != nil {
		return err
	}
	return snapshotDatabase(ctx, database, destination)
}

func snapshotDatabase(ctx context.Context, database *sql.DB, destination string) error {
	var absolute, err = filepath.Abs(destination)

	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(absolute), 0700); err != nil {
		return err
	}
	reserved, err := os.OpenFile(absolute, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return fmt.Errorf("backup destination must not exist: %w", err)
	}
	if err := reserved.Close(); err != nil {
		_ = os.Remove(absolute)
		return err
	}
	if _, err := database.ExecContext(ctx, "VACUUM INTO ?", absolute); err != nil {
		_ = os.Remove(absolute)
		return fmt.Errorf("database snapshot failed: %w", err)
	}
	return nil
}
