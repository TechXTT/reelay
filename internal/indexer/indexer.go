// Package indexer defines the contract every torrent indexer implements, plus
// the shared circuit breaker that keeps a sick indexer from being hammered.
//
// The engine depends on this package's interfaces and never on a concrete
// implementation; concretes are constructed in main.go and injected.
package indexer

import (
	"context"
	"errors"
	"fmt"
	"time"
)

// Sentinel errors callers are expected to branch on.
var (
	// ErrNoResults means the indexer answered, but with its own "nothing
	// found" marker rather than real rows.
	//
	// This is NOT the same as an empty slice, and the distinction matters more
	// than it looks. The Pirate Bay's JSON API returns the identical
	// "No results returned" row both when a search genuinely has no matches
	// and when it is rate-limiting the caller. Treating that as authoritative
	// emptiness makes the engine conclude a release does not exist, walk its
	// backoff out to six hours, and eventually give up after thirty days on a
	// title that was available the whole time — a silent failure that would be
	// very hard to diagnose from the outside.
	//
	// The engine therefore treats this as "inconclusive": advance the backoff,
	// but do not count it toward the give-up deadline.
	ErrNoResults = errors.New("indexer returned its no-results marker")

	// ErrUnhealthy means the circuit breaker is open and no request was made.
	ErrUnhealthy = errors.New("indexer is unhealthy; request not attempted")

	// ErrUnsupported means the indexer cannot service this kind of query, e.g.
	// a recent-items listing on an indexer that has no such endpoint.
	ErrUnsupported = errors.New("indexer does not support this query")
)

// Query describes one search.
type Query struct {
	// Term is the search string. Empty together with Recent means "newest
	// items, no filter".
	Term string

	// Categories filters results client-side. Empty means no filtering.
	//
	// Deliberately a post-filter rather than a request parameter. The Pirate
	// Bay's cat= accepts one value per request, so honouring the spec's anime
	// rule (search 205, 208 and 299) through the API would cost three requests
	// against a very tight budget. Omitting cat= searches everything at once,
	// and filtering afterwards cannot lose a correctly-named release that was
	// filed under the wrong category — which on this indexer is common.
	Categories []int

	// MinSeeders drops dead releases before they reach the scorer.
	MinSeeders int

	// Recent asks for the indexer's newest listing instead of a term search.
	// This is how a release is caught within minutes rather than at the next
	// full search.
	Recent bool
}

func (q Query) String() string {
	if q.Recent {
		return "<recent>"
	}
	return q.Term
}

// Release is one candidate, exactly as the indexer reported it. The raw title
// is left unparsed; internal/parser owns interpretation.
type Release struct {
	Title       string    `json:"title"`
	InfoHash    string    `json:"info_hash"`
	Magnet      string    `json:"magnet"`
	SizeBytes   int64     `json:"size_bytes"`
	Seeders     int       `json:"seeders"`
	Leechers    int       `json:"leechers"`
	PublishedAt time.Time `json:"published_at"`
	Indexer     string    `json:"indexer"`
	Category    int       `json:"category"`

	// Beyond the minimum contract, and all three are actually useful:
	// Files distinguishes a season pack from a single episode before parsing,
	// IMDBID gives the matcher an identifier that survives title mangling, and
	// Uploader is what the UI shows when a release looks suspect.
	Files    int    `json:"files,omitempty"`
	IMDBID   string `json:"imdb_id,omitempty"`
	Uploader string `json:"uploader,omitempty"`
}

func (r Release) String() string {
	return fmt.Sprintf("%s [%s, %s, %d seeders]",
		r.Title, r.Indexer, HumanSize(r.SizeBytes), r.Seeders)
}

// SizeMB is the size in whole megabytes, which is the unit quality profiles use.
func (r Release) SizeMB() int { return int(r.SizeBytes / (1024 * 1024)) }

// Indexer is a source of candidate releases.
type Indexer interface {
	Name() string
	Search(ctx context.Context, q Query) ([]Release, error)
	// Healthy reports whether the indexer is currently usable. It must not
	// make a network request: the health endpoint is polled by browsers and
	// container probes, and turning that into indexer traffic is how you get
	// rate-limited by your own dashboard.
	Healthy(ctx context.Context) error
}

// Category ranges. Verified against live responses rather than taken from
// documentation.
const (
	CatVideo         = 200
	CatMovies        = 201
	CatMoviesDVDR    = 202
	CatMusicVideos   = 203
	CatMovieClips    = 204
	CatTVShows       = 205
	CatHandheld      = 206
	CatMoviesHD      = 207
	CatTVShowsHD     = 208
	Cat3D            = 209
	CatVideoOther    = 299
	CatVideoRangeEnd = 299
)

// IsVideoCategory covers the whole 2xx block rather than an explicit list.
//
// Live responses contain categories the published table does not mention —
// 212 shows up routinely in TV searches — so an allowlist silently discards
// valid candidates. A range is robust against categories being added, and the
// parser is the real filter anyway.
func IsVideoCategory(cat int) bool {
	return cat >= CatVideo && cat <= CatVideoRangeEnd
}

// VideoCategories is the filter to hand a recent-items query, which has no
// search term to constrain it and otherwise returns music, games and porn.
func VideoCategories() []int {
	out := make([]int, 0, CatVideoRangeEnd-CatVideo+1)
	for c := CatVideo; c <= CatVideoRangeEnd; c++ {
		out = append(out, c)
	}
	return out
}

// HumanSize renders bytes for logs and the UI.
func HumanSize(b int64) string {
	const unit = 1024
	if b < unit {
		return fmt.Sprintf("%d B", b)
	}
	units := []string{"KiB", "MiB", "GiB", "TiB"}
	size := float64(b)
	i := -1
	for size >= unit && i < len(units)-1 {
		size /= unit
		i++
	}
	return fmt.Sprintf("%.1f %s", size, units[i])
}
