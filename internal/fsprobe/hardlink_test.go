package fsprobe

import (
	"os"
	"path/filepath"
	"testing"
)

// On any normal local filesystem the probe must come back Supported. If this
// fails on the dev machine, the probe itself is broken — not the filesystem.
func TestHardlinkSupportedOnSameFilesystem(t *testing.T) {
	dir := t.TempDir()
	from := filepath.Join(dir, "downloads")
	to := filepath.Join(dir, "library")
	mkdirs(t, from, to)

	res := Hardlink(from, to)
	if res.Support != Supported {
		t.Fatalf("Support = %s (%s); expected hardlinks to work inside one temp dir", res.Support, res.Detail)
	}
	if res.Status != "supported" {
		t.Errorf("Status = %q", res.Status)
	}

	// The probe must not leave anything behind.
	assertEmpty(t, from)
	assertEmpty(t, to)
}

func TestHardlinkSkippedWhenPathMissing(t *testing.T) {
	dir := t.TempDir()
	from := filepath.Join(dir, "downloads")
	mkdirs(t, from)

	res := Hardlink(from, filepath.Join(dir, "not-created"))
	if res.Support != Unknown {
		t.Errorf("Support = %s, want unknown for a missing library path", res.Support)
	}
	if res.Detail == "" {
		t.Error("a skipped probe must explain itself")
	}
}

func TestHardlinkSkippedWhenPathIsAFile(t *testing.T) {
	dir := t.TempDir()
	from := filepath.Join(dir, "downloads")
	mkdirs(t, from)

	notADir := filepath.Join(dir, "file.txt")
	if err := os.WriteFile(notADir, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}

	res := Hardlink(from, notADir)
	if res.Support != Unknown {
		t.Errorf("Support = %s, want unknown when the target is a file", res.Support)
	}
}

// Probing twice in a row must give the same answer: a leftover destination
// from a previous run would otherwise fail with EEXIST and look unsupported.
func TestHardlinkProbeIsRepeatable(t *testing.T) {
	dir := t.TempDir()
	from := filepath.Join(dir, "downloads")
	to := filepath.Join(dir, "library")
	mkdirs(t, from, to)

	first := Hardlink(from, to)
	second := Hardlink(from, to)
	if first.Support != second.Support {
		t.Errorf("probe not repeatable: %s then %s (%s)", first.Support, second.Support, second.Detail)
	}
}

func TestSupportString(t *testing.T) {
	for s, want := range map[Support]string{
		Supported:   "supported",
		Unsupported: "unsupported",
		Unknown:     "unknown",
	} {
		if got := s.String(); got != want {
			t.Errorf("Support(%d).String() = %q, want %q", s, got, want)
		}
	}
}

func mkdirs(t *testing.T, dirs ...string) {
	t.Helper()
	for _, d := range dirs {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
}

func assertEmpty(t *testing.T, dir string) {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		names := make([]string, 0, len(entries))
		for _, e := range entries {
			names = append(names, e.Name())
		}
		t.Errorf("probe left files behind in %s: %v", dir, names)
	}
}
