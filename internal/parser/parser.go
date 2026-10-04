// Package parser turns a raw release name into structured metadata.
//
// There is no Go equivalent of Python's guessit, and wrapping a Python process
// to get one is not on the table, so this is an ordered regex cascade — the
// same approach Sonarr takes. Scene and P2P naming is a narrow, well-documented
// grammar; the value is entirely in the ordering and in the corpus that locks
// the behaviour down (see testdata/releases.json).
//
// The cascade, in order, and why each step comes where it does:
//
//  1. Strip the container extension, then site tags. Both live at the very end
//     and would otherwise be mistaken for a release group.
//  2. Extract the release group. It is the most position-dependent token in the
//     name; leaving it in place makes it fodder for the codec and language
//     regexes further down.
//  3. Find the episode/season/date/absolute marker and record where it starts.
//     Everything to its left is the title. This is the load-bearing step: get
//     the boundary wrong and every later extraction is polluted.
//  4. Only if no episode marker matched, look for a movie year — which is
//     ambiguous with titles that *are* years (2012, 1917, Blade Runner 2049).
//  5. Pull quality tokens out of the remainder, where order no longer matters
//     because each token type has a disjoint vocabulary.
//  6. Normalise the title for matching, keeping the raw form for display.
package parser

import (
	"regexp"
	"slices"
	"strconv"
	"strings"
)

// Parsed is the structured form of a release name. A zero value for a field
// means "not present in the name", never "default" — callers must treat
// Resolution == "" as unknown rather than as any particular quality.
type Parsed struct {
	// TitleRaw preserves capitalisation and punctuation for display.
	TitleRaw string `json:"title_raw"`
	// Title is normalised: lowercase, punctuation stripped, & expanded.
	Title string `json:"title"`

	Year int `json:"year,omitempty"`

	Season   int   `json:"season,omitempty"`
	Episodes []int `json:"episodes,omitempty"`
	// SeasonEnd is set for multi-season packs (S01-S03 -> Season 1, SeasonEnd 3).
	SeasonEnd    int  `json:"season_end,omitempty"`
	IsSeasonPack bool `json:"is_season_pack,omitempty"`

	// AbsoluteEp is anime-style continuous numbering across seasons.
	AbsoluteEp int `json:"absolute_ep,omitempty"`

	// AirDate is set for daily shows, as YYYY-MM-DD.
	AirDate string `json:"air_date,omitempty"`

	Resolution string   `json:"resolution,omitempty"`
	Source     string   `json:"source,omitempty"`
	VideoCodec string   `json:"video_codec,omitempty"`
	AudioCodec string   `json:"audio_codec,omitempty"`
	HDR        []string `json:"hdr,omitempty"`
	Language   []string `json:"language,omitempty"`

	ReleaseGroup string `json:"release_group,omitempty"`
	Proper       bool   `json:"proper,omitempty"`
	Repack       bool   `json:"repack,omitempty"`
	Container    string `json:"container,omitempty"`

	// Truncated flags a name that hit The Pirate Bay's 80-character cut. Those
	// names routinely lose the release group and the trailing quality tokens,
	// so scoring must not read an absent group as "unknown group, no bonus"
	// when it is really "we cannot see the group".
	Truncated bool `json:"truncated,omitempty"`
}

// HasEpisodeInfo reports whether this looks like TV rather than a movie.
func (p Parsed) HasEpisodeInfo() bool {
	return p.Season > 0 || len(p.Episodes) > 0 || p.AbsoluteEp > 0 || p.AirDate != ""
}

// FirstEpisode is a convenience for the common single-episode case.
func (p Parsed) FirstEpisode() int {
	if len(p.Episodes) == 0 {
		return 0
	}
	return p.Episodes[0]
}

// CoversEpisode reports whether this release contains the given episode of the
// given season, accounting for season packs and multi-episode files.
func (p Parsed) CoversEpisode(season, episode int) bool {
	// A multi-season pack covers a range.
	if p.Season != season && !(p.SeasonEnd > 0 && season >= p.Season && season <= p.SeasonEnd) {
		return false
	}
	if p.IsSeasonPack && len(p.Episodes) == 0 {
		return true
	}
	return slices.Contains(p.Episodes, episode)
}

// tpbNameLimit is the hard cut apibay applies to the `name` field. Measured,
// not documented: across a 100-row live response the longest name was exactly
// 80 characters, with mid-word truncation.
const tpbNameLimit = 80

// Parse never fails. A name it cannot make sense of yields a Parsed with an
// empty Title, which the matcher then rejects — a parse error and an
// unmatchable release want the same handling, so there is no error to return.
func Parse(raw string) Parsed {
	p := Parsed{}
	s := strings.TrimSpace(raw)
	if s == "" {
		return p
	}
	p.Truncated = len(raw) >= tpbNameLimit

	// Steps 1 and 2: reduce the name to "full" (everything but container and
	// tags) and "body" (full without the release group).
	s, p.Container = stripContainer(s)
	s = stripTrackerPrefix(s)
	s, bracketGroup := stripBracketTags(s)
	full := normalizeSeparators(s)

	// "full" is kept for the whole-name scans below: stripping "-PROPER" as a
	// group would otherwise also delete the PROPER flag.
	body, dashGroup := stripTrailingGroup(full)
	switch {
	case dashGroup != "":
		p.ReleaseGroup = dashGroup
	case bracketGroup != "":
		p.ReleaseGroup = bracketGroup
	}
	lowerFull := strings.ToLower(full)

	// Steps 3 and 4: where does the title stop?
	boundary := findTitleBoundary(body, lowerFull, &p)
	titlePart, rest := body[:boundary], body[boundary:]

	// "Show (2015) S01E01" puts the year *before* the episode marker, so it
	// lands inside the title slice and would otherwise become part of the
	// title. Pull it back out.
	//
	// Guarded on Year == 0: when the year is already known the title's trailing
	// number belongs to the title. Without the guard "Blade Runner 2049 2017"
	// correctly identified 2017 as the year and then deleted 2049 from the
	// title, leaving "Blade Runner".
	if p.Year == 0 {
		head, trailingYear := splitTrailingYear(titlePart)
		if trailingYear > 0 {
			titlePart = head
			p.Year = trailingYear
		}
	}

	// Anime often carries both a season marker and absolute numbering:
	// "Vinland Saga S2 - 07 [1080p]". The season marker wins the boundary
	// because it comes first, leaving the absolute number in the remainder.
	if p.AbsoluteEp == 0 && p.Season > 0 {
		if abs := absoluteAfterMarker(rest); abs > 0 {
			p.AbsoluteEp = abs
			// A bare "S2" looked like a season pack until the absolute number
			// showed up. One episode, not a whole season.
			if len(p.Episodes) == 0 {
				p.IsSeasonPack = false
			}
		}
	}

	// A season pack marker leaves the year in the remainder rather than before
	// the boundary, so look for it there too: "The Expanse S01 (2015) 1080p".
	// Skipped for daily shows, where the "year" is the date's year.
	if p.Year == 0 && p.AirDate == "" {
		if y := yearInRemainder(rest); y > 0 {
			p.Year = y
		}
	}

	// Step 5: quality tokens. Language and PROPER/REPACK markers turn up
	// anywhere in the name — "FRENCH.Show.S01E01" puts the language first, and
	// PROPER sometimes sits in the group position — so both scan the full
	// pre-group string.
	extractTokens(rest, &p)
	extractLanguages(lowerFull, &p)
	extractFlags(full, lowerFull, &p)

	// Step 6: title.
	p.TitleRaw = cleanTitleRaw(titlePart)
	p.Title = NormalizeTitle(p.TitleRaw)
	return p
}

// findTitleBoundary returns the index in body where the title stops, recording
// any numbering or year found on the way into p.
func findTitleBoundary(body, lowerFull string, p *Parsed) int {
	if m := matchEpisodeMarker(body, lowerFull, p); m >= 0 {
		return m
	}
	if m := matchMovieYear(body, p); m >= 0 {
		return m
	}
	// Neither numbering nor a year. Plenty of real names have neither —
	// "Inception.1080p.BluRay.x264-GROUP", or a fansub whole-series pack —
	// and without this fallback the entire name became the title and every
	// quality token went unread.
	if m := matchQualityBoundary(body); m > 0 {
		return m
	}
	return len(body)
}

var (
	leadingJunkRe = regexp.MustCompile(`(?i)^\s*(?:www[\s.]\S+[\s.](?:org|com|net|to|io|me)|www\.\S+)\s*-*\s*`)
	trailingPunct = " -–—_.([{)]}"
)

// cleanTitleRaw removes tracker prefixes and dangling punctuation from the
// title slice. apibay in particular prepends "www.UIndex.org    -    " to
// names uploaded through certain sites.
func cleanTitleRaw(s string) string {
	if t := strings.TrimLeft(s, "\t\n\f\r "); len(t) >= 3 && strings.EqualFold(t[:3], "www") {
		s = leadingJunkRe.ReplaceAllString(s, "")
	}
	s = strings.Trim(s, trailingPunct)
	return strings.Join(strings.Fields(s), " ")
}

func atoi(s string) int {
	var n, err = strconv.Atoi(s)

	if err != nil {
		return 0
	}
	return n
}
