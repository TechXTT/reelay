package engine

import (
	"context"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/TechXTT/reelay/internal/clock"
	"github.com/TechXTT/reelay/internal/config"
	"github.com/TechXTT/reelay/internal/store"
)

func TestBackupOnceSchedulesAndPrunes(t *testing.T) {
	ctx := context.Background()
	clk := clock.NewFake(time.Date(2026, 8, 23, 12, 0, 0, 0, time.UTC))
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	db, err := store.Open(ctx, store.Options{Path: filepath.Join(t.TempDir(), "engine.db"), CacheKB: 512, Now: clk.Now})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := store.Migrate(ctx, db, logger); err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(t.TempDir(), "backups")
	cfg := testConfig()
	cfg.Database.BackupInterval = config.Dur(24 * time.Hour)
	cfg.Database.BackupDir = dir
	cfg.Database.BackupKeep = 2
	eng, err := New(Options{Store: db, Config: cfg, Downloader: &fakeDownloader{}, Clock: clk, Logger: logger})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"notes.txt", "reelay-manual.db"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("keep"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	stale := filepath.Join(dir, "reelay-20200101-000000Z.db.partial")
	if err := os.WriteFile(stale, []byte("junk"), 0o600); err != nil {
		t.Fatal(err)
	}
	count := func() int {
		names, err := backupFiles(dir, false)
		if err != nil {
			t.Fatal(err)
		}
		return len(names)
	}

	if err := eng.BackupOnce(ctx); err != nil {
		t.Fatal(err)
	}
	if count() != 1 {
		t.Fatalf("want 1 backup, got %d", count())
	}
	if _, err := os.Stat(stale); !os.IsNotExist(err) {
		t.Errorf("stale partial should be removed: %v", err)
	}
	clk.Advance(time.Hour)
	if err := eng.BackupOnce(ctx); err != nil || count() != 1 {
		t.Fatalf("second call within interval must write nothing: err=%v count=%d", err, count())
	}
	clk.Advance(24 * time.Hour)
	if err := eng.BackupOnce(ctx); err != nil || count() != 2 {
		t.Fatalf("want 2 backups after interval: err=%v count=%d", err, count())
	}
	clk.Advance(24 * time.Hour)
	if err := eng.BackupOnce(ctx); err != nil || count() != 2 {
		t.Fatalf("retention must keep exactly backup_keep: err=%v count=%d", err, count())
	}
	status := eng.BackupStatus()
	if !status.Enabled || status.Count != 2 || status.LastError != "" || status.Newest.IsZero() {
		t.Errorf("unexpected status: %+v", status)
	}
	for _, name := range []string{"notes.txt", "reelay-manual.db"} {
		if _, err := os.Stat(filepath.Join(dir, name)); err != nil {
			t.Errorf("%s must be untouched: %v", name, err)
		}
	}
}
