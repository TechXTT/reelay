package downloader

import (
	"runtime"
	"testing"
)

func TestPathMapperNoMappings(t *testing.T) {
	m := NewPathMapper(nil)
	const p = "/downloads/tv/Show/file.mkv"
	if got := m.Local(p); got != p {
		t.Errorf("with no mappings the path must be unchanged, got %q", got)
	}
	// A nil mapper is usable: topology A configures none.
	var nilMapper *PathMapper
	if got := nilMapper.Local(p); got != p {
		t.Errorf("nil mapper changed the path to %q", got)
	}
}

// The container case: qBittorrent reports POSIX paths, Reelay on the host sees
// a Windows drive.
func TestPathMapperContainerToWindows(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("separator normalisation is OS-dependent")
	}
	m := NewPathMapper([]Mapping{
		{DownloaderPrefix: "/downloads", LocalPrefix: `D:\downloads`},
	})
	got := m.Local("/downloads/tv/Some Show/ep.mkv")
	want := `D:\downloads\tv\Some Show\ep.mkv`
	if got != want {
		t.Errorf("Local() = %q, want %q", got, want)
	}
}

// Longest prefix wins regardless of the order in config, or a general mapping
// listed first would shadow the specific one that was meant to apply.
func TestPathMapperLongestPrefixWins(t *testing.T) {
	m := NewPathMapper([]Mapping{
		{DownloaderPrefix: "/data", LocalPrefix: "/mnt/general"},
		{DownloaderPrefix: "/data/torrents/tv", LocalPrefix: "/mnt/series"},
		{DownloaderPrefix: "/data/torrents", LocalPrefix: "/mnt/downloads"},
	})
	cases := map[string]string{
		"/data/torrents/tv/Show/ep.mkv":    "/mnt/series/Show/ep.mkv",
		"/data/torrents/movies/Film/f.mkv": "/mnt/downloads/movies/Film/f.mkv",
		"/data/other/thing.mkv":            "/mnt/general/other/thing.mkv",
		"/elsewhere/thing.mkv":             "/elsewhere/thing.mkv",
	}
	for in, want := range cases {
		if got := m.Local(in); got != want {
			t.Errorf("Local(%q) = %q, want %q", in, got, want)
		}
	}
}

// /downloads must not match /downloads-old. Without a boundary check this
// silently rewrites an unrelated directory.
func TestPathMapperRequiresAPathBoundary(t *testing.T) {
	m := NewPathMapper([]Mapping{
		{DownloaderPrefix: "/downloads", LocalPrefix: "/mnt/dl"},
	})
	if got := m.Local("/downloads-old/thing.mkv"); got != "/downloads-old/thing.mkv" {
		t.Errorf("Local() = %q; /downloads must not match /downloads-old", got)
	}
	if got := m.Local("/downloads"); got != "/mnt/dl" {
		t.Errorf("an exact prefix match should still map, got %q", got)
	}
}

// A containerised client reporting POSIX paths has to be recognised by a
// Windows Reelay, and Windows itself is case-insensitive.
func TestPathMapperSeparatorAndCaseTolerance(t *testing.T) {
	m := NewPathMapper([]Mapping{
		{DownloaderPrefix: `D:\Downloads`, LocalPrefix: `E:\media`},
	})
	// Forward slashes and different case, same directory on Windows.
	got := m.Local("d:/downloads/tv/ep.mkv")
	if runtime.GOOS == "windows" {
		if got != `E:\media\tv\ep.mkv` {
			t.Errorf("Local() = %q, want E:\\media\\tv\\ep.mkv", got)
		}
	} else if got == "d:/downloads/tv/ep.mkv" {
		t.Log("case folding is correctly not applied off Windows")
	}
}

func TestPathMapperIgnoresEmptyPrefix(t *testing.T) {
	m := NewPathMapper([]Mapping{{DownloaderPrefix: "", LocalPrefix: "/mnt/x"}})
	if got := m.Local("/anything"); got != "/anything" {
		t.Errorf("an empty prefix must not match everything, got %q", got)
	}
}

func TestPathMapperEmptyInput(t *testing.T) {
	m := NewPathMapper([]Mapping{{DownloaderPrefix: "/d", LocalPrefix: "/l"}})
	if got := m.Local(""); got != "" {
		t.Errorf("Local(\"\") = %q, want empty", got)
	}
}

// Mutating the caller's slice afterwards must not change behaviour under a
// running engine.
func TestPathMapperCopiesItsMappings(t *testing.T) {
	mappings := []Mapping{{DownloaderPrefix: "/a", LocalPrefix: "/b"}}
	m := NewPathMapper(mappings)
	mappings[0].LocalPrefix = "/hijacked"
	if got := m.Local("/a/x"); got != "/b/x" {
		t.Errorf("Local() = %q; the mapper should hold its own copy", got)
	}
}

func TestTorrentStatusComplete(t *testing.T) {
	cases := []struct {
		state    string
		progress float64
		want     bool
	}{
		{StateSeeding, 1, true},
		{StateCompleted, 1, true},
		// Still verifying. Importing this yields a corrupt library entry.
		{StateSeeding, 0.98, false},
		{StateDownloading, 1, false},
		{StateStalled, 0.5, false},
		{StatePaused, 1, false},
		{StateError, 1, false},
		{StateUnknown, 1, false},
	}
	for _, tc := range cases {
		s := TorrentStatus{State: tc.state, Progress: tc.progress}
		if got := s.Complete(); got != tc.want {
			t.Errorf("state=%s progress=%v Complete() = %v, want %v",
				tc.state, tc.progress, got, tc.want)
		}
	}
}

func TestTorrentStatusFailed(t *testing.T) {
	if !(TorrentStatus{State: StateError}).Failed() {
		t.Error("error state should report failed")
	}
	for _, s := range []string{StateDownloading, StateStalled, StateSeeding, StatePaused} {
		if (TorrentStatus{State: s}).Failed() {
			t.Errorf("state %s should not report failed", s)
		}
	}
}
