package downloader

import "strings"

// Mapping translates one path prefix from the download client's view to ours.
type Mapping struct {
	DownloaderPrefix string
	LocalPrefix      string
}

// PathMapper rewrites paths the download client reports into paths this process
// can actually open.
//
// Needed whenever Reelay and the client do not share a filesystem view: the
// client in a container reports /downloads/tv, Reelay on the host sees
// D:\downloads\tv. Built in phase 5 rather than when it is first strictly
// needed, because retrofitting it means touching the importer — every path that
// crosses the boundary has to go through here, and finding them all later is
// the expensive way to do it.
type PathMapper struct {
	mappings []Mapping
}

func NewPathMapper(mappings []Mapping) *PathMapper {
	// Copy so a later config mutation cannot change behaviour under a running
	// engine.
	out := make([]Mapping, len(mappings))
	copy(out, mappings)
	return &PathMapper{mappings: out}
}

// Local converts a client-reported path into a local one. Longest matching
// prefix wins, so a specific mapping beats a general one regardless of the
// order they appear in config.
func (m *PathMapper) Local(clientPath string) string {
	if m == nil || len(m.mappings) == 0 || clientPath == "" {
		return clientPath
	}

	best := -1
	out := clientPath
	for _, mp := range m.mappings {
		if mp.DownloaderPrefix == "" {
			continue
		}
		if len(mp.DownloaderPrefix) <= best {
			continue
		}
		if !hasPathPrefix(clientPath, mp.DownloaderPrefix) {
			continue
		}
		best = len(mp.DownloaderPrefix)
		rest := clientPath[len(mp.DownloaderPrefix):]
		out = joinMapped(mp.LocalPrefix, rest)
	}
	return out
}

// joinMapped splices the remainder onto the new prefix and normalises
// separators to match the STYLE OF THE TARGET, not the style of the host.
//
// Keying this on runtime.GOOS was wrong and the tests caught it: a Reelay
// running on Windows but mapping onto a POSIX path (topology B, where the
// download client and the library live on the NAS) produced "\mnt\series\..."
// — a path that exists nowhere. The local prefix already says which kind of
// path it is, so ask it.
func joinMapped(localPrefix, rest string) string {
	joined := strings.TrimRight(localPrefix, `/\`) + rest
	if isWindowsStyle(localPrefix) {
		return strings.ReplaceAll(joined, "/", `\`)
	}
	return strings.ReplaceAll(joined, `\`, "/")
}

// isWindowsStyle reports whether a path is a Windows path: a UNC share, a drive
// letter, or something containing a backslash.
func isWindowsStyle(p string) bool {
	if strings.HasPrefix(p, `\\`) {
		return true
	}
	if len(p) >= 2 && p[1] == ':' {
		return true
	}
	return strings.Contains(p, `\`)
}

// hasPathPrefix compares path prefixes tolerantly: / and \ are equivalent, and
// case is ignored for Windows-style paths.
//
// Case sensitivity follows the PATH, not the host, for the same reason as
// joinMapped. Windows treats D:\Downloads and d:\downloads as one directory;
// Linux does not, and a containerised client reporting /Downloads really is
// somewhere else from /downloads even when Reelay happens to run on Windows.
func hasPathPrefix(p, prefix string) bool {
	fold := isWindowsStyle(prefix) || isWindowsStyle(p)
	np, npre := normalizePath(p, fold), normalizePath(prefix, fold)
	npre = strings.TrimRight(npre, "/")
	if npre == "" {
		return false
	}
	if !strings.HasPrefix(np, npre) {
		return false
	}
	// Require a boundary so /downloads does not match /downloads-old.
	rest := np[len(npre):]
	return rest == "" || rest[0] == '/'
}

func normalizePath(p string, fold bool) string {
	p = strings.ReplaceAll(p, `\`, "/")
	if fold {
		p = strings.ToLower(p)
	}
	return p
}
