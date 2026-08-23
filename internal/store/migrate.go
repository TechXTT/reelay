package store

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"fmt"
	"io/fs"
	"log/slog"
	"regexp"
	"sort"
	"strconv"
	"time"

	"github.com/TechXTT/reelay/migrations"
)

var migrationName = regexp.MustCompile(`^(\d{4})_([a-z0-9_]+)\.sql$`)

type migration struct {
	Version  int
	Name     string
	SQL      string
	Checksum string
}

// Migrate applies every pending migration, each in its own transaction.
//
// Already-applied migrations are checksummed against the embedded file. A
// mismatch is fatal: editing a shipped migration means two installations have
// silently divergent schemas, and that is far cheaper to catch here than to
// diagnose from a constraint violation three weeks later.
func Migrate(ctx context.Context, s *Store, log *slog.Logger) error {
	ms, err := loadMigrations()
	if err != nil {
		return err
	}

	if err := ensureMigrationTable(ctx, s); err != nil {
		return err
	}

	applied, err := appliedMigrations(ctx, s)
	if err != nil {
		return err
	}

	pending := 0
	for _, m := range ms {
		if prev, ok := applied[m.Version]; ok {
			if prev.checksum != m.Checksum {
				return fmt.Errorf(
					"store: migration %04d_%s.sql was modified after it was applied "+
						"(recorded %s, embedded %s); add a new migration instead of editing this one",
					m.Version, prev.name, short(prev.checksum), short(m.Checksum))
			}
			continue
		}
		if err := applyMigration(ctx, s, m); err != nil {
			return err
		}
		log.Info("applied migration", "version", m.Version, "name", m.Name)
		pending++
	}

	// A database ahead of the binary means someone downgraded. Refuse rather
	// than operate against a schema we do not understand.
	for version, rec := range applied {
		if !hasVersion(ms, version) {
			return fmt.Errorf(
				"store: database contains migration %04d_%s applied at %s that this binary does not know about; "+
					"you are running an older Reelay than the one that created this database",
				version, rec.name, rec.appliedAt)
		}
	}

	if pending == 0 {
		log.Debug("schema up to date", "version", maxVersion(ms))
	} else {
		log.Info("schema migrated", "applied", pending, "version", maxVersion(ms))
	}
	return nil
}

// SchemaVersion is the highest applied migration, for the health endpoint.
func SchemaVersion(ctx context.Context, s *Store) (int, error) {
	var v int
	err := s.ro.QueryRowContext(ctx,
		"SELECT COALESCE(MAX(version), 0) FROM schema_migrations").Scan(&v)
	if err != nil {
		return 0, fmt.Errorf("store: read schema version: %w", err)
	}
	return v, nil
}

func loadMigrations() ([]migration, error) {
	entries, err := fs.ReadDir(migrations.FS, ".")
	if err != nil {
		return nil, fmt.Errorf("store: read embedded migrations: %w", err)
	}

	out := make([]migration, 0, len(entries))
	seen := map[int]string{}
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		m := migrationName.FindStringSubmatch(e.Name())
		if m == nil {
			return nil, fmt.Errorf(
				"store: migration file %q does not match NNNN_lower_snake.sql", e.Name())
		}
		version, err := strconv.Atoi(m[1])
		if err != nil {
			return nil, fmt.Errorf("store: migration %q has an unparseable version: %w", e.Name(), err)
		}
		if other, dup := seen[version]; dup {
			return nil, fmt.Errorf("store: migrations %q and %q share version %d", other, e.Name(), version)
		}
		seen[version] = e.Name()

		body, err := migrations.FS.ReadFile(e.Name())
		if err != nil {
			return nil, fmt.Errorf("store: read migration %q: %w", e.Name(), err)
		}
		sum := sha256.Sum256(body)
		out = append(out, migration{
			Version:  version,
			Name:     m[2],
			SQL:      string(body),
			Checksum: hex.EncodeToString(sum[:]),
		})
	}

	sort.Slice(out, func(i, j int) bool { return out[i].Version < out[j].Version })
	if len(out) == 0 {
		return nil, fmt.Errorf("store: no migrations embedded; the binary is misbuilt")
	}
	return out, nil
}

func ensureMigrationTable(ctx context.Context, s *Store) error {
	// Not STRICT: this table bootstraps the migration system itself and must
	// be creatable before any migration has run.
	const ddl = `
CREATE TABLE IF NOT EXISTS schema_migrations (
    version    INTEGER PRIMARY KEY,
    name       TEXT NOT NULL,
    checksum   TEXT NOT NULL,
    applied_at TEXT NOT NULL
)`
	if _, err := s.rw.ExecContext(ctx, ddl); err != nil {
		return fmt.Errorf("store: create schema_migrations: %w", err)
	}
	return nil
}

type appliedRecord struct {
	name      string
	checksum  string
	appliedAt string
}

func appliedMigrations(ctx context.Context, s *Store) (map[int]appliedRecord, error) {
	rows, err := s.rw.QueryContext(ctx,
		"SELECT version, name, checksum, applied_at FROM schema_migrations")
	if err != nil {
		return nil, fmt.Errorf("store: read schema_migrations: %w", err)
	}
	defer rows.Close()

	out := map[int]appliedRecord{}
	for rows.Next() {
		var v int
		var rec appliedRecord
		if err := rows.Scan(&v, &rec.name, &rec.checksum, &rec.appliedAt); err != nil {
			return nil, fmt.Errorf("store: scan schema_migrations: %w", err)
		}
		out[v] = rec
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: iterate schema_migrations: %w", err)
	}
	return out, nil
}

func applyMigration(ctx context.Context, s *Store, m migration) error {
	return s.InTx(ctx, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, m.SQL); err != nil {
			return fmt.Errorf("store: migration %04d_%s failed: %w", m.Version, m.Name, err)
		}
		_, err := tx.ExecContext(ctx,
			"INSERT INTO schema_migrations (version, name, checksum, applied_at) VALUES (?, ?, ?, ?)",
			m.Version, m.Name, m.Checksum, FormatTime(time.Now()))
		if err != nil {
			return fmt.Errorf("store: record migration %04d_%s: %w", m.Version, m.Name, err)
		}
		return nil
	})
}

func hasVersion(ms []migration, v int) bool {
	for _, m := range ms {
		if m.Version == v {
			return true
		}
	}
	return false
}

func maxVersion(ms []migration) int {
	if len(ms) == 0 {
		return 0
	}
	return ms[len(ms)-1].Version
}

func short(sum string) string {
	if len(sum) <= 12 {
		return sum
	}
	return sum[:12]
}
