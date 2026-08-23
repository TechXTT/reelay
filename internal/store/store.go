// Package store owns the SQLite database: connection setup, migrations and
// (from phase 6) the repositories.
package store

import (
	"context"
	"database/sql"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"time"

	// Pure-Go SQLite. Deliberately not mattn/go-sqlite3: cgo would break the
	// linux/arm and linux/arm64 cross-compiles this project depends on.
	_ "modernc.org/sqlite"
)

// TimeFormat is the single canonical on-disk timestamp format. Fixed-width UTC
// so lexicographic comparison equals chronological comparison.
const TimeFormat = "2006-01-02T15:04:05Z"

// FormatTime renders t for storage. Callers must not hand-roll this.
func FormatTime(t time.Time) string { return t.UTC().Format(TimeFormat) }

// ParseTime reads a stored timestamp.
func ParseTime(s string) (time.Time, error) {
	t, err := time.Parse(TimeFormat, s)
	if err != nil {
		return time.Time{}, fmt.Errorf("parse stored timestamp %q: %w", s, err)
	}
	return t, nil
}

// Store holds two handles over the same database file.
//
// SQLite permits one writer at a time. Rather than discover that as sporadic
// SQLITE_BUSY under load, we funnel every write through a single-connection
// pool (serialised in-process, no lock contention) and let reads run
// concurrently against WAL snapshots. Splitting them now avoids reworking
// every repository later.
type Store struct {
	rw   *sql.DB // MaxOpenConns(1); all INSERT/UPDATE/DELETE/DDL
	ro   *sql.DB // read-only pool
	path string
	now  func() time.Time
}

type Options struct {
	Path    string
	CacheKB int
	// ReadConns bounds the read pool. Keep it at 2 on the NAS.
	ReadConns int
	// Now is injected by lifecycle tests. Production leaves it nil.
	Now func() time.Time
}

// Open creates the parent directory if needed, opens both pools and verifies
// connectivity. It does not run migrations; call Migrate for that.
func Open(ctx context.Context, opt Options) (*Store, error) {
	if opt.Path == "" {
		return nil, fmt.Errorf("store: empty database path")
	}
	if opt.CacheKB <= 0 {
		opt.CacheKB = 2048
	}
	if opt.ReadConns <= 0 {
		opt.ReadConns = 2
	}
	if opt.Now == nil {
		opt.Now = time.Now
	}

	if dir := filepath.Dir(opt.Path); dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o750); err != nil {
			return nil, fmt.Errorf("store: create database directory %s: %w", dir, err)
		}
	}

	rw, err := openPool(ctx, opt, true)
	if err != nil {
		return nil, err
	}
	ro, err := openPool(ctx, opt, false)
	if err != nil {
		_ = rw.Close()
		return nil, err
	}

	return &Store{rw: rw, ro: ro, path: opt.Path, now: opt.Now}, nil
}

func (s *Store) nowUTC() time.Time { return s.now().UTC().Truncate(time.Second) }

func openPool(ctx context.Context, opt Options, write bool) (*sql.DB, error) {
	// modernc's driver takes PRAGMAs as _pragma query parameters, applied to
	// every new connection in the pool.
	q := url.Values{}
	q.Add("_pragma", "journal_mode(WAL)")
	q.Add("_pragma", "busy_timeout(5000)")
	q.Add("_pragma", "foreign_keys(ON)")
	// NORMAL is the right trade for WAL: a power cut can lose the last
	// transaction but cannot corrupt the file, and it removes an fsync per
	// commit — which matters a great deal on NAS-grade storage.
	q.Add("_pragma", "synchronous(NORMAL)")
	// Negative cache_size means KiB rather than pages.
	q.Add("_pragma", fmt.Sprintf("cache_size(-%d)", opt.CacheKB))

	if write {
		// BEGIN IMMEDIATE for every transaction. Without it SQLite starts
		// write transactions deferred and can fail on upgrade with
		// SQLITE_BUSY_SNAPSHOT, which no busy_timeout will save you from.
		q.Set("_txlock", "immediate")
	}

	dsn := "file:" + filepath.ToSlash(opt.Path) + "?" + q.Encode()
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("store: open sqlite %s: %w", opt.Path, err)
	}

	if write {
		db.SetMaxOpenConns(1)
		db.SetMaxIdleConns(1)
	} else {
		db.SetMaxOpenConns(opt.ReadConns)
		db.SetMaxIdleConns(opt.ReadConns)
	}
	db.SetConnMaxLifetime(0) // long-lived process; recycling buys nothing

	pingCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	if err := db.PingContext(pingCtx); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("store: connect to %s: %w", opt.Path, err)
	}
	return db, nil
}

// Writer returns the serialised write handle.
func (s *Store) Writer() *sql.DB { return s.rw }

// Reader returns the concurrent read handle.
func (s *Store) Reader() *sql.DB { return s.ro }

// Path is the database file location, for logs and the health endpoint.
func (s *Store) Path() string { return s.path }

// Ping is the health check. It touches the read pool, which is what the API
// actually serves from.
func (s *Store) Ping(ctx context.Context) error {
	var one int
	if err := s.ro.QueryRowContext(ctx, "SELECT 1").Scan(&one); err != nil {
		return fmt.Errorf("store: ping: %w", err)
	}
	return nil
}

// Close flushes the WAL and closes both pools.
func (s *Store) Close() error {
	// A truncating checkpoint leaves a single .db file behind rather than a
	// -wal the next process has to recover, which keeps backups simple.
	if _, err := s.rw.Exec("PRAGMA wal_checkpoint(TRUNCATE)"); err != nil {
		// Not fatal: we are shutting down either way, but the operator should
		// know the WAL was left dirty.
		err = fmt.Errorf("store: wal checkpoint on close: %w", err)
		_ = s.ro.Close()
		_ = s.rw.Close()
		return err
	}
	var errs []error
	if err := s.ro.Close(); err != nil {
		errs = append(errs, fmt.Errorf("store: close read pool: %w", err))
	}
	if err := s.rw.Close(); err != nil {
		errs = append(errs, fmt.Errorf("store: close write pool: %w", err))
	}
	if len(errs) > 0 {
		return errs[0]
	}
	return nil
}

// InTx runs fn inside a write transaction, rolling back on error or panic.
func (s *Store) InTx(ctx context.Context, fn func(*sql.Tx) error) error {
	tx, err := s.rw.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("store: begin transaction: %w", err)
	}
	defer func() {
		if p := recover(); p != nil {
			_ = tx.Rollback()
			panic(p)
		}
	}()
	if err := fn(tx); err != nil {
		if rbErr := tx.Rollback(); rbErr != nil {
			return fmt.Errorf("%w (rollback also failed: %v)", err, rbErr)
		}
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("store: commit: %w", err)
	}
	return nil
}
