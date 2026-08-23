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
	if p.Season != season {
		// A multi-season pack covers a range.
		if !(p.SeasonEnd > 0 && season >= p.Season && season <= p.SeasonEnd) {
			return false
		}
	}
	if p.IsSeasonPack && len(p.Episodes) == 0 {
		return true
	}
	for _, e := range p.Episodes {
		if e == episode {
			return true
		}
	}
	return false
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
	if len(raw) >= tpbNameLimit {
		p.Truncated = true
	}

	s, p.Container = stripContainer(s)
	s = stripTrackerPrefix(s)
	s, bracketGroup := stripBracketTags(s)
	s = normalizeSeparators(s)

	// Keep the pre-group form for the whole-name scans below: stripping
	// "-PROPER" as a group would otherwise also delete the PROPER flag.
	full := s

	var dashGroup string
	s, dashGroup = stripTrailingGroup(s)
	switch {
	case dashGroup != "":
		p.ReleaseGroup = dashGroup
	case bracketGroup != "":
		p.ReleaseGroup = bracketGroup
	}

	// Step 3: where does the title stop?
	boundary := len(s)
	if m := matchEpisodeMarker(s, &p); m >= 0 {
		boundary = m
	} else if m := matchMovieYear(s, &p); m >= 0 {
		boundary = m
	} else if m := matchQualityBoundary(s); m > 0 {
		// Neither numbering nor a year. Plenty of real names have neither —
		// "Inception.1080p.BluRay.x264-GROUP", or a fansub whole-series pack —
		// and without this fallback the entire name became the title and every
		// quality token went unread.
		boundary = m
	}

	titlePart := s[:boundary]
	rest := s[boundary:]

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

	extractTokens(rest, &p)
	// Language and PROPER/REPACK markers turn up anywhere in the name —
	// "FRENCH.Show.S01E01" puts the language first, and PROPER sometimes sits
	// in the group position — so both scan the full pre-group string.
	extractLanguages(full, &p)
	extractFlags(full, &p)

	p.TitleRaw = cleanTitleRaw(titlePart)
	p.Title = NormalizeTitle(p.TitleRaw)
	return p
}

// ---------------------------------------------------------------------------
// Step 1: container and site tags
// ---------------------------------------------------------------------------

var containers = map[string]bool{
	"mkv": true, "mp4": true, "avi": true, "m4v": true, "ts": true,
	"mov": true, "wmv": true, "flv": true, "webm": true, "m2ts": true,
	"iso": true, "img": true,
}

func stripContainer(s string) (string, string) {
	i := strings.LastIndexByte(s, '.')
	if i < 0 || i == len(s)-1 {
		return s, ""
	}
	ext := strings.ToLower(s[i+1:])
	if containers[ext] {
		return s[:i], "." + ext
	}
	return s, ""
}

// siteTags are tracker and aggregator stamps. They sit exactly where a release
// group sits, so they have to go before group extraction runs.
//
// Codec and bit-depth tokens deliberately do NOT belong here, even though they
// also turn up in trailing brackets. Listing "hevc" and "x265" here silently
// deleted the video codec from every YTS and fansub release: "[x265]" was
// erased before the token pass could read it. Trailing quality tokens are
// rejected as group names by looksLikeQualityToken instead, which does not
// destroy the information.
var siteTags = map[string]bool{
	"eztv": true, "eztvx.to": true, "eztvx": true, "ettv": true,
	"tgx": true, "tgxgoodies": true, "rartv": true, "rarbgx": true,
	"glodls": true, "1337x": true, "1337xx": true, "uindex.org": true,
	"torrentgalaxy": true, "prof": true, "noname": true, "silence": true,
	"mkvcage": true, "psa": true, "qxr": true, "megusta": true,
}

// bracketGroups are fansub and P2P groups that publish their name in brackets
// at the end rather than after a dash.
var bracketGroups = map[string]string{
	"yts.mx": "YTS.MX", "yts.am": "YTS.AM", "yts.lt": "YTS.LT", "yts.ag": "YTS.AG",
	"yify": "YIFY", "rarbg": "RARBG", "judas": "Judas",
	"subsplease": "SubsPlease", "erai-raws": "Erai-raws",
	"horriblesubs": "HorribleSubs", "anime time": "Anime Time",
	"ember": "EMBER", "cuervo": "Cuervo", "tenzin": "t3nzin",
}

var bracketRe = regexp.MustCompile(`\[([^\[\]]*)\]`)

// stripBracketTags removes every [...] group, returning the release group if
// one of them named a known publisher.
//
// Quality tokens also appear in brackets (YTS writes "[1080p] [WEBRip]"), so
// the contents are pushed back into the string as plain text rather than
// discarded — only site tags and recognised group names are consumed.
func stripBracketTags(s string) (string, string) {
	group := ""
	out := bracketRe.ReplaceAllStringFunc(s, func(m string) string {
		inner := strings.TrimSpace(m[1 : len(m)-1])
		key := strings.ToLower(inner)
		if g, ok := bracketGroups[key]; ok {
			if group == "" {
				group = g
			}
			return " "
		}
		if siteTags[key] || inner == "" {
			return " "
		}
		// Anime fansub prefix: "[GroupName] Show - 01". A leading bracket that
		// is not a quality token is the group.
		if strings.HasPrefix(s, m) && !looksLikeQualityToken(key) {
			if group == "" {
				group = inner
			}
			return " "
		}
		// Otherwise keep the contents: it is probably "[1080p]" or "[Dual Audio]".
		return " " + inner + " "
	})
	return out, group
}

func looksLikeQualityToken(lower string) bool {
	if resolutionAlias[lower] != "" || sourceAlias[lower] != "" ||
		videoAlias[lower] != "" || audioAlias[lower] != "" || hdrAlias[lower] != "" {
		return true
	}
	// "5.1", "7.1", "10bit", "1080p"
	return regexp.MustCompile(`^\d+([.\-]\d+)?(bit|p|i|ch)?$`).MatchString(lower)
}

var separatorRe = regexp.MustCompile(`[._]+|\s{2,}`)

// normalizeSeparators collapses dots, underscores and runs of spaces.
//
// apibay has already done half of this: it replaces dots with spaces in the
// `name` field, so "H.264" arrives as "H 264" and "DDP5.1" as "DDP5 1". The
// token regexes below have to tolerate both forms, which is why they all allow
// an optional separator where scene naming has a dot.
func normalizeSeparators(s string) string {
	s = separatorRe.ReplaceAllString(s, " ")
	s = strings.ReplaceAll(s, "(", " ( ")
	s = strings.ReplaceAll(s, ")", " ) ")
	return strings.Join(strings.Fields(s), " ")
}

// ---------------------------------------------------------------------------
// Step 2: release group
// ---------------------------------------------------------------------------

// trailingGroupRe matches a scene-style "-GROUP" suffix.
//
// The dash must NOT be preceded by whitespace. apibay appends the uploader's
// username as " - Lulloz", and treating that as a release group would poison
// every preferred_groups comparison. Scene convention never puts a space
// before the group dash, so this distinction is free.
var trailingGroupRe = regexp.MustCompile(`\S-([A-Za-z0-9]{2,}(?:\.[A-Za-z]{2,})?)\s*$`)

func stripTrailingGroup(s string) (string, string) {
	m := trailingGroupRe.FindStringSubmatchIndex(s)
	if m == nil {
		return s, ""
	}
	candidate := s[m[2]:m[3]]
	lower := strings.ToLower(candidate)
	// "...1080p-x264" or "...-1080p" is not a group.
	if siteTags[lower] || looksLikeQualityToken(lower) {
		return s, ""
	}
	// A pure number after a dash is an episode range ("S01E01-02"), not a group.
	if _, err := strconv.Atoi(candidate); err == nil {
		return s, ""
	}
	// Cut at the dash, keeping the character before it.
	return strings.TrimSpace(s[:m[2]-1]), candidate
}

// ---------------------------------------------------------------------------
// Step 3: episode, season, absolute and date markers
// ---------------------------------------------------------------------------

var (
	// S01E02, S1E2, S01 E02, S01.E02, and multi-episode continuations
	// S01E02E03 / S01E02-E04 / S01 E01-13.
	reSxxExx = regexp.MustCompile(`(?i)\bs(\d{1,4})\s?e(\d{1,4})`)
	// Continuation tokens immediately after the first episode. Group 1 records
	// whether a dash was present, which is what distinguishes a range
	// ("E01-E03" = 1,2,3) from a list ("E01E03" = 1,3). Group 4 captures any
	// letters glued to a bare number so the caller can reject "1080p".
	//
	// RE2 has no negative lookahead, so the "not a resolution" test that would
	// naturally be (?![\dpi]) lives in moreEpisodes instead.
	// Two accepted continuation shapes, and the difference is load-bearing:
	//   "E03" / "-E03" / " E03"  — optional space, optional dash, explicit E
	//   "-03"                    — dash with NO surrounding spaces
	// A spaced " - 037" is anime absolute numbering, not an episode range;
	// allowing spaces here turned "Bleach S02E13 - 037" into episodes 13..37.
	reEpisodeMore = regexp.MustCompile(`(?i)^(?:\s?(-?)e(\d{1,4})|(-)(\d+)([a-z]*))`)

	// 1x02, 01x02, with 1x02x03 continuations.
	// No trailing \b: in "01x02x03" the character after the episode number is
	// another 'x', which is a word character, so requiring a boundary made the
	// whole multi-episode form unmatchable.
	reNxNN   = regexp.MustCompile(`(?i)\b(\d{1,2})x(\d{1,3})`)
	reNxMore = regexp.MustCompile(`(?i)^x(\d{1,3})\b`)

	// "Season 1 Episode 2"
	reWordy = regexp.MustCompile(`(?i)\bseasons?\s?(\d{1,3})\s+episodes?\s?(\d{1,4})\b`)

	// Multi-season packs: S01-S03, S01-03, Seasons 1-3, "Season 1 to 6".
	// The spelled-out "to" form is not scene convention but shows up on real
	// Pirate Bay uploads.
	reSeasonRange = regexp.MustCompile(`(?i)\bs(?:eason)?s?\s?(\d{1,3})\s?(?:-|to)\s?s?(\d{1,3})\b`)

	// Single season pack: S01, Season 1, "Season 01 Complete".
	reSeasonOnly = regexp.MustCompile(`(?i)\b(?:s|season\s?)(\d{1,3})\b`)

	// "Complete Series", "The Complete Collection"
	reCompleteSeries = regexp.MustCompile(`(?i)\bcomplete\s+(?:series|collection)\b`)

	// Daily shows: 2024.03.15 or 2024-03-15 (normalised to spaces by now).
	reDateYMD = regexp.MustCompile(`\b(19|20)(\d{2})[\s-](\d{2})[\s-](\d{2})\b`)
	// 03.15.2024 (US ordering), only accepted when the first group is a
	// plausible month.
	reDateMDY = regexp.MustCompile(`\b(\d{2})[\s-](\d{2})[\s-]((?:19|20)\d{2})\b`)

	// Anime absolute numbering: "Show - 034", "Show - 034v2", "Show E034".
	reAbsoluteDash = regexp.MustCompile(`\s-\s(\d{1,4})(?:v\d)?\b`)
	reAbsoluteE    = regexp.MustCompile(`(?i)\be(?:p)?(\d{2,4})\b`)
)

// matchEpisodeMarker tries every TV pattern and returns the index where the
// earliest one starts, or -1. Patterns are tried most-specific first, but the
// *earliest* match wins regardless of which pattern produced it — a specific
// pattern matching late in the string must not steal the title boundary from a
// general pattern matching early.
func matchEpisodeMarker(s string, p *Parsed) int {
	type candidate struct {
		start int
		apply func()
	}
	var best *candidate

	consider := func(start int, apply func()) {
		if start < 0 {
			return
		}
		if best == nil || start < best.start {
			best = &candidate{start: start, apply: apply}
		}
	}

	// "Season 1 Episode 2" — checked first because reSeasonOnly would also
	// match its "Season 1" half and report a season pack.
	if m := reWordy.FindStringSubmatchIndex(s); m != nil {
		season := atoi(s[m[2]:m[3]])
		ep := atoi(s[m[4]:m[5]])
		consider(m[0], func() {
			p.Season = season
			p.Episodes = []int{ep}
		})
	}

	if m := reSxxExx.FindStringSubmatchIndex(s); m != nil {
		season := atoi(s[m[2]:m[3]])
		first := atoi(s[m[4]:m[5]])
		tail := s[m[1]:]
		consider(m[0], func() {
			p.Season = season
			p.Episodes = append([]int{first}, moreEpisodes(tail, first)...)
		})
	}

	if m := reNxNN.FindStringSubmatchIndex(s); m != nil {
		season := atoi(s[m[2]:m[3]])
		first := atoi(s[m[4]:m[5]])
		tail := s[m[1]:]
		consider(m[0], func() {
			p.Season = season
			eps := []int{first}
			for {
				mm := reNxMore.FindStringSubmatch(tail)
				if mm == nil {
					break
				}
				eps = append(eps, atoi(mm[1]))
				tail = tail[len(mm[0]):]
			}
			p.Episodes = eps
		})
	}

	if m := reDateYMD.FindStringSubmatchIndex(s); m != nil {
		date := s[m[2]:m[3]] + s[m[4]:m[5]] + "-" + s[m[6]:m[7]] + "-" + s[m[8]:m[9]]
		if plausibleDate(date) {
			consider(m[0], func() { p.AirDate = date })
		}
	}
	if m := reDateMDY.FindStringSubmatchIndex(s); m != nil {
		month, day, year := s[m[2]:m[3]], s[m[4]:m[5]], s[m[6]:m[7]]
		date := year + "-" + month + "-" + day
		if plausibleDate(date) {
			consider(m[0], func() { p.AirDate = date })
		}
	}

	if m := reSeasonRange.FindStringSubmatchIndex(s); m != nil {
		from, to := atoi(s[m[2]:m[3]]), atoi(s[m[4]:m[5]])
		if to > from {
			consider(m[0], func() {
				p.Season = from
				p.SeasonEnd = to
				p.IsSeasonPack = true
			})
		}
	}

	if m := reSeasonOnly.FindStringSubmatchIndex(s); m != nil {
		season := atoi(s[m[2]:m[3]])
		// A bare "S" plus digits inside a word ("Se7en", "Sense8") cannot
		// reach here: the regex requires the digits to follow s directly and
		// a word boundary before it.
		consider(m[0], func() {
			p.Season = season
			p.IsSeasonPack = true
		})
	}

	if m := reCompleteSeries.FindStringIndex(s); m != nil {
		consider(m[0], func() { p.IsSeasonPack = true })
	}

	// Anime absolute numbering is last and weakest: " - 034" also matches
	// hyphenated titles and part numbers, so it only wins when it is the
	// earliest marker found and nothing stronger explained the name.
	if m := reAbsoluteDash.FindStringSubmatchIndex(s); m != nil {
		n := atoi(s[m[2]:m[3]])
		// Reject 4-digit values that are really years, and 0.
		if n > 0 && !(n >= 1900 && n <= 2099) {
			consider(m[0], func() { p.AbsoluteEp = n })
		}
	}

	// "Show E034" / "Show EP034". Safe to try last: when a real SxxExx exists
	// it matches earlier in the string and wins the boundary, and the two-digit
	// minimum keeps it away from stray single digits.
	if m := reAbsoluteE.FindStringSubmatchIndex(s); m != nil {
		n := atoi(s[m[2]:m[3]])
		if n > 0 && !(n >= 1900 && n <= 2099) {
			consider(m[0], func() { p.AbsoluteEp = n })
		}
	}

	if best == nil {
		return -1
	}
	best.apply()
	return best.start
}

// moreEpisodes collects continuation tokens after the first episode number.
//
// A dash means a range and gets expanded, so "S01E01-E03" yields 1,2,3 rather
// than 1,3 — a viewer who has none of those three needs all of them, and the
// importer has to know the file contains 2 as well.
func moreEpisodes(tail string, first int) []int {
	var out []int
	prev := first
	for {
		m := reEpisodeMore.FindStringSubmatch(tail)
		if m == nil {
			return out
		}
		explicitE := m[2] != ""
		dash := m[1] != "" || m[3] != ""
		raw := m[2]
		if raw == "" {
			raw = m[4]
		}
		if raw == "" {
			return out
		}

		// The negative lookahead RE2 will not give us: a bare number that is
		// four digits long, or that carries a p/i suffix, is a resolution.
		// Without this, "S01E01-1080p" parsed as episodes 1 through 1080.
		if !explicitE {
			if len(raw) > 3 {
				return out
			}
			if suffix := strings.ToLower(m[5]); suffix != "" &&
				(strings.HasPrefix(suffix, "p") || strings.HasPrefix(suffix, "i")) {
				return out
			}
		}

		n := atoi(raw)

		switch {
		case dash:
			// A range that does not move forward is not a range; it is a
			// mis-parse, so stop rather than emit nonsense.
			if n <= prev || n-prev > 200 {
				return out
			}
			for e := prev + 1; e <= n; e++ {
				out = append(out, e)
			}
		case explicitE:
			// Explicit "E03" continuation.
			out = append(out, n)
		default:
			// A bare number with no dash and no E is not an episode.
			return out
		}
		prev = n
		tail = tail[len(m[0]):]
	}
}

var trailingYearRe = regexp.MustCompile(`\(?\s*\b((?:19|20)\d{2})\b\s*\)?\s*$`)

// splitTrailingYear removes a year sitting at the end of the title slice, as in
// "The Expanse (2015) S01E01".
//
// It refuses to strip when doing so would empty the title, which is what keeps
// "1917" and "2012" as titles rather than as years.
func splitTrailingYear(title string) (string, int) {
	m := trailingYearRe.FindStringSubmatchIndex(title)
	if m == nil {
		return title, 0
	}
	head := cleanTitleRaw(title[:m[0]])
	if strings.TrimSpace(head) == "" {
		return title, 0
	}
	return head, atoi(title[m[2]:m[3]])
}

// absoluteAfterMarker finds anime absolute numbering that follows a season
// marker: "Show S02 - 27 [1080p]".
func absoluteAfterMarker(rest string) int {
	if m := reAbsoluteDash.FindStringSubmatch(rest); m != nil {
		n := atoi(m[1])
		if n > 0 && !(n >= 1900 && n <= 2099) {
			return n
		}
	}
	return 0
}

func plausibleDate(iso string) bool {
	if len(iso) != 10 {
		return false
	}
	year := atoi(iso[0:4])
	month := atoi(iso[5:7])
	day := atoi(iso[8:10])
	return year >= 1950 && year <= 2099 && month >= 1 && month <= 12 && day >= 1 && day <= 31
}

// ---------------------------------------------------------------------------
// Step 4: movie year
// ---------------------------------------------------------------------------

var yearRe = regexp.MustCompile(`\(?\b((?:19|20)\d{2})\b\)?`)

// matchMovieYear picks the year that delimits the title, and returns where the
// title stops.
//
// The hard cases are titles that are themselves years or end in one: 2012,
// 1917, Blade Runner 2049. Three rules, in order:
//
//  1. A year in parentheses is an explicit marker and wins.
//  2. Two year-like tokens separated by nothing but space means the first
//     belongs to the title and the second is the release year — "Blade Runner
//     2049 2017", "2012 2009".
//  3. Otherwise take the first year that leaves a non-empty title, so "1917
//     1080p BluRay" keeps 1917 as the title rather than as the year.
func matchMovieYear(s string, p *Parsed) int {
	all := yearRe.FindAllStringSubmatchIndex(s, -1)
	if len(all) == 0 {
		return -1
	}

	// Rule 1: parenthesised.
	for _, m := range all {
		if hasParens(s, m) && strings.TrimSpace(cleanTitleRaw(s[:m[0]])) != "" {
			p.Year = atoi(s[m[2]:m[3]])
			return m[0]
		}
	}

	// Rule 2: adjacent pair.
	for i := 0; i+1 < len(all); i++ {
		gap := strings.TrimSpace(s[all[i][1]:all[i+1][0]])
		if gap == "" && strings.TrimSpace(cleanTitleRaw(s[:all[i][0]])) != "" {
			p.Year = atoi(s[all[i+1][2]:all[i+1][3]])
			return all[i+1][0]
		}
	}

	// Rule 3: first year leaving a non-empty title.
	for _, m := range all {
		if strings.TrimSpace(cleanTitleRaw(s[:m[0]])) != "" {
			p.Year = atoi(s[m[2]:m[3]])
			return m[0]
		}
	}
	return -1
}

// hasParens reports whether the year at m is wrapped in parentheses.
//
// It inspects the surrounding characters rather than folding the brackets into
// yearRe, because normalizeSeparators pads parentheses with spaces — "(2017)"
// has already become "( 2017 )" by this point, so a regex expecting the
// bracket adjacent to the digits never matches.
func hasParens(s string, m []int) bool {
	before := strings.TrimRight(s[:m[0]], " ")
	after := strings.TrimLeft(s[m[1]:], " ")
	return strings.HasSuffix(before, "(") && strings.HasPrefix(after, ")")
}

// ---------------------------------------------------------------------------
// Step 4b: quality-token boundary, the last resort
// ---------------------------------------------------------------------------

// boundaryTokens are the only tokens trusted to end a title.
//
// A deliberately narrow list. The full alias tables contain weak, ambiguous
// entries — "hd", "sd", "bd", "ts", "web", "dolby" — that occur inside real
// titles: "Ultra HD Movie 2160p" would otherwise become the title "Ultra" with
// a 720p resolution. Every token here is one that essentially never appears in
// a title.
var boundaryTokens = map[string]bool{
	"2160p": true, "1080p": true, "720p": true, "480p": true, "576p": true,
	"1080i": true, "720i": true, "480i": true, "4k": true, "uhd": true,
	"bluray": true, "bdrip": true, "brrip": true, "webrip": true,
	"webdl": true, "hdtv": true, "dvdrip": true, "remux": true, "bdremux": true,
	"x264": true, "x265": true, "h264": true, "h265": true, "hevc": true,
	"xvid": true, "divx": true, "av1": true,
	"hdcam": true, "camrip": true, "telesync": true, "telecine": true,
	"dvdscr": true,
}

var wordRe = regexp.MustCompile(`[A-Za-z0-9+]+`)

// matchQualityBoundary returns the index of the earliest unambiguous quality
// token, or -1. It refuses to return a boundary that would leave an empty
// title, so a name that is nothing but "1080p" keeps that as its title and
// stays (correctly) unmatchable.
func matchQualityBoundary(s string) int {
	for _, loc := range wordRe.FindAllStringIndex(s, -1) {
		if !boundaryTokens[strings.ToLower(s[loc[0]:loc[1]])] {
			continue
		}
		if strings.TrimSpace(cleanTitleRaw(s[:loc[0]])) == "" {
			return -1
		}
		return loc[0]
	}
	return -1
}

// yearInRemainder finds a year after the title boundary, for season packs like
// "The Expanse S01 (2015) 1080p" where the year trails the season marker.
func yearInRemainder(rest string) int {
	for _, m := range yearRe.FindAllStringSubmatch(rest, -1) {
		if y := atoi(m[1]); y >= 1900 && y <= 2099 {
			return y
		}
	}
	return 0
}

// ---------------------------------------------------------------------------
// Step 6: title cleanup
// ---------------------------------------------------------------------------

// trackerPrefixRe matches an aggregator stamp prepended to the whole name, as
// in "www.UIndex.org    -    From.S04E01...".
//
// Applied BEFORE separator normalisation, while the dots are still dots: once
// "www.UIndex.org" has become "www UIndex org" there is no domain left to
// recognise. The required trailing dash is what keeps this away from real
// titles — a film would have to be called something like "Amazon.com - ..." to
// be caught.
var trackerPrefixRe = regexp.MustCompile(
	`(?i)^\s*(?:https?://)?(?:www\.)?[a-z0-9][a-z0-9.\-]*\.(?:org|com|net|to|io|me|se|cc|tv|info)\s*-+\s*`)

func stripTrackerPrefix(s string) string {
	return trackerPrefixRe.ReplaceAllString(s, "")
}

var (
	leadingJunkRe = regexp.MustCompile(`(?i)^\s*(?:www[\s.]\S+[\s.](?:org|com|net|to|io|me)|www\.\S+)\s*-*\s*`)
	trailingPunct = " -–—_.([{)]}"
)

// cleanTitleRaw removes tracker prefixes and dangling punctuation from the
// title slice. apibay in particular prepends "www.UIndex.org    -    " to
// names uploaded through certain sites.
func cleanTitleRaw(s string) string {
	s = leadingJunkRe.ReplaceAllString(s, "")
	s = strings.Trim(s, trailingPunct)
	return strings.Join(strings.Fields(s), " ")
}

func atoi(s string) int {
	n, err := strconv.Atoi(strings.TrimLeft(s, "0"))
	if err != nil {
		// All-zero strings trim to empty.
		if strings.Trim(s, "0") == "" {
			return 0
		}
		return 0
	}
	return n
}
