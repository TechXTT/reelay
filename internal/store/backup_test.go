package store

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

func TestBackupRestoreConsistentSnapshotAndRefuseOverwrite(t *testing.T) {
	var ctx = context.Background()
	var st = migratedDomainStore(t)
	var backup = filepath.Join(t.TempDir(), "backup #100%.db")
	var destination = filepath.Join(t.TempDir(), "restore.db")

	if err := st.KV().Set(ctx, "before", "persisted"); err != nil {
		t.Fatal(err)
	}
	if err := st.Backup(ctx, backup); err != nil {
		t.Fatal(err)
	}
	if err := st.KV().Set(ctx, "after", "not in snapshot"); err != nil {
		t.Fatal(err)
	}
	if err := st.Backup(ctx, backup); err == nil {
		t.Fatal("backup overwrote an existing file")
	}
	if err := Restore(ctx, backup, destination); err != nil {
		t.Fatal(err)
	}
	if err := Restore(ctx, backup, destination); err == nil {
		t.Fatal("restore overwrote existing database")
	}
	restored, err := Open(ctx, Options{Path: destination})
	if err != nil {
		t.Fatal(err)
	}
	defer restored.Close()
	if err := Migrate(ctx, restored, testLogger()); err != nil {
		t.Fatal(err)
	}
	value, err := restored.KV().Get(ctx, "before")
	if err != nil || value != "persisted" {
		t.Fatalf("snapshot lost data: %q %v", value, err)
	}
	if _, err := restored.KV().Get(ctx, "after"); err == nil {
		t.Fatal("snapshot includes later writes")
	}
	invalid := filepath.Join(t.TempDir(), "invalid.db")
	if err := os.WriteFile(invalid, []byte("not SQLite"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := Restore(ctx, invalid, filepath.Join(t.TempDir(), "target.db")); err == nil {
		t.Fatal("restored corrupt file")
	}
}
