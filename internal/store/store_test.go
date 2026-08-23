package store

import (
	"context"
	"database/sql"
	"errors"
	"io"
	"log/slog"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func testLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func openTestStore(t *testing.T) *Store {
	t.Helper()
	s, err := Open(context.Background(), Options{
		Path:      filepath.Join(t.TempDir(), "nested", "reelay.db"),
		CacheKB:   512,
		ReadConns: 2,
	})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() {
		if err := s.Close(); err != nil {
			t.Errorf("Close: %v", err)
		}
	})
	return s
}

func TestOpenCreatesDirectoryAndMigrates(t *testing.T) {
	ctx := context.Background()
	s := openTestStore(t)

	if err := s.Ping(ctx); err != nil {
		t.Fatalf("Ping: %v", err)
	}
	if err := Migrate(ctx, s, testLogger()); err != nil {
		t.Fatalf("Migrate: %v", err)
	}

	v, err := SchemaVersion(ctx, s)
	if err != nil {
		t.Fatalf("SchemaVersion: %v", err)
	}
	if v != 3 {
		t.Errorf("schema version = %d, want 3", v)
	}

	// Every table 0001 promises must exist.
	for _, table := range []string{
		"kv", "metadata_cache", "indexer_health", "schema_migrations",
		"quality_profiles", "series", "episodes", "movies", "releases",
		"candidate_evaluations", "grabs", "release_blacklist", "imports",
		"state_transitions", "item_locks",
	} {
		var name string
		err := s.Reader().QueryRowContext(ctx,
			"SELECT name FROM sqlite_master WHERE type='table' AND name=?", table).Scan(&name)
		if err != nil {
			t.Errorf("table %s missing after migration: %v", table, err)
		}
	}
}

func TestMigrateIsIdempotent(t *testing.T) {
	ctx := context.Background()
	s := openTestStore(t)

	for i := 0; i < 3; i++ {
		if err := Migrate(ctx, s, testLogger()); err != nil {
			t.Fatalf("Migrate run %d: %v", i+1, err)
		}
	}

	var n int
	if err := s.Reader().QueryRowContext(ctx,
		"SELECT COUNT(*) FROM schema_migrations").Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 3 {
		t.Errorf("schema_migrations has %d rows after 3 runs, want 3", n)
	}
}

// A checksum mismatch means someone edited an applied migration. Detecting it
// is the difference between one loud startup failure and two installations
// with quietly different schemas.
func TestMigrateDetectsEditedMigration(t *testing.T) {
	ctx := context.Background()
	s := openTestStore(t)
	if err := Migrate(ctx, s, testLogger()); err != nil {
		t.Fatal(err)
	}

	_, err := s.Writer().ExecContext(ctx,
		"UPDATE schema_migrations SET checksum = 'deadbeefdeadbeef' WHERE version = 1")
	if err != nil {
		t.Fatal(err)
	}

	err = Migrate(ctx, s, testLogger())
	if err == nil {
		t.Fatal("expected a checksum mismatch to be fatal")
	}
	if !strings.Contains(err.Error(), "modified after it was applied") {
		t.Errorf("unexpected error: %v", err)
	}
}

// A database carrying a migration this binary has never heard of means a
// downgrade. Refuse rather than corrupt.
func TestMigrateRefusesUnknownFutureMigration(t *testing.T) {
	ctx := context.Background()
	s := openTestStore(t)
	if err := Migrate(ctx, s, testLogger()); err != nil {
		t.Fatal(err)
	}

	_, err := s.Writer().ExecContext(ctx,
		"INSERT INTO schema_migrations (version, name, checksum, applied_at) VALUES (99, 'from_the_future', 'x', ?)",
		FormatTime(time.Now()))
	if err != nil {
		t.Fatal(err)
	}

	err = Migrate(ctx, s, testLogger())
	if err == nil {
		t.Fatal("expected an unknown applied migration to be fatal")
	}
	if !strings.Contains(err.Error(), "older Reelay") {
		t.Errorf("unexpected error: %v", err)
	}
}

func TestKVRoundTrip(t *testing.T) {
	ctx := context.Background()
	s := openTestStore(t)
	if err := Migrate(ctx, s, testLogger()); err != nil {
		t.Fatal(err)
	}
	kv := s.KV()

	if _, err := kv.Get(ctx, "nope"); !errors.Is(err, ErrNotFound) {
		t.Errorf("Get on a missing key = %v, want ErrNotFound", err)
	}

	if err := kv.Set(ctx, "loop.search.last_run", "value-1"); err != nil {
		t.Fatal(err)
	}
	if err := kv.Set(ctx, "loop.search.last_run", "value-2"); err != nil {
		t.Fatalf("upsert: %v", err)
	}
	got, err := kv.Get(ctx, "loop.search.last_run")
	if err != nil {
		t.Fatal(err)
	}
	if got != "value-2" {
		t.Errorf("Get = %q, want value-2", got)
	}

	// Time round-trips through the canonical format, to the second.
	want := time.Date(2026, 8, 23, 11, 30, 15, 999, time.UTC)
	if err := kv.SetTime(ctx, "t", want); err != nil {
		t.Fatal(err)
	}
	gotTime, err := kv.GetTime(ctx, "t")
	if err != nil {
		t.Fatal(err)
	}
	if !gotTime.Equal(want.Truncate(time.Second)) {
		t.Errorf("GetTime = %s, want %s", gotTime, want.Truncate(time.Second))
	}

	if err := kv.Delete(ctx, "t"); err != nil {
		t.Fatal(err)
	}
	if _, err := kv.Get(ctx, "t"); !errors.Is(err, ErrNotFound) {
		t.Errorf("after Delete, Get = %v, want ErrNotFound", err)
	}
}

// STRICT tables and CHECK constraints are load-bearing: they are what turns a
// typo'd state into a write error instead of a stuck item.
func TestSchemaRejectsBadEnumValue(t *testing.T) {
	ctx := context.Background()
	s := openTestStore(t)
	if err := Migrate(ctx, s, testLogger()); err != nil {
		t.Fatal(err)
	}

	_, err := s.Writer().ExecContext(ctx,
		`INSERT INTO indexer_health (name, healthy, consecutive_failures, checked_at)
		 VALUES ('tpb', 7, 0, ?)`, FormatTime(time.Now()))
	if err == nil {
		t.Fatal("expected the healthy CHECK constraint to reject 7")
	}
}

func TestInTxRollsBack(t *testing.T) {
	ctx := context.Background()
	s := openTestStore(t)
	if err := Migrate(ctx, s, testLogger()); err != nil {
		t.Fatal(err)
	}

	sentinel := errors.New("nope")
	err := s.InTx(ctx, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx,
			"INSERT INTO kv (key, value, updated_at) VALUES ('a','b',?)", FormatTime(time.Now())); err != nil {
			return err
		}
		return sentinel
	})
	if !errors.Is(err, sentinel) {
		t.Fatalf("InTx error = %v, want sentinel", err)
	}

	if _, err := s.KV().Get(ctx, "a"); !errors.Is(err, ErrNotFound) {
		t.Errorf("row survived a rolled-back transaction")
	}
}

func TestParseTimeRejectsGarbage(t *testing.T) {
	if _, err := ParseTime("2026-08-23 11:30:15"); err == nil {
		t.Error("expected a non-RFC3339 timestamp to fail")
	}
	round, err := ParseTime(FormatTime(time.Now()))
	if err != nil {
		t.Fatal(err)
	}
	if round.Location() != time.UTC {
		t.Errorf("parsed time is not UTC: %s", round)
	}
}
