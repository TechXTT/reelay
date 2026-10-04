package parser

import (
	"regexp"
	"strings"
)

// Steps 3 and 4 of the cascade: find where the title stops. The boundary is an
// episode, season, date or absolute-number marker, else a movie year, else the
// first unambiguous quality token.

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
//
// lower is the lowercased name, used only to skip patterns whose literal
// keyword cannot occur; it may be longer than s (s has the group cut off).
func matchEpisodeMarker(s, lower string, p *Parsed) int {
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
	if m := findIfContains(reWordy, s, lower, "eason"); m != nil {
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

	if m := findIfContains(reNxNN, s, lower, "x"); m != nil {
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

	if m := findIfContains(reCompleteSeries, s, lower, "complete"); m != nil {
		consider(m[0], func() { p.IsSeasonPack = true })
	}

	// Anime absolute numbering is last and weakest: " - 034" also matches
	// hyphenated titles and part numbers, so it only wins when it is the
	// earliest marker found and nothing stronger explained the name.
	if m := reAbsoluteDash.FindStringSubmatchIndex(s); m != nil {
		n := atoi(s[m[2]:m[3]])
		// Reject 4-digit values that are really years, and 0.
		if plausibleAbsolute(n) {
			consider(m[0], func() { p.AbsoluteEp = n })
		}
	}

	// "Show E034" / "Show EP034". Safe to try last: when a real SxxExx exists
	// it matches earlier in the string and wins the boundary, and the two-digit
	// minimum keeps it away from stray single digits.
	if m := reAbsoluteE.FindStringSubmatchIndex(s); m != nil {
		n := atoi(s[m[2]:m[3]])
		if plausibleAbsolute(n) {
			consider(m[0], func() { p.AbsoluteEp = n })
		}
	}

	if best == nil {
		return -1
	}
	best.apply()
	return best.start
}

// findIfContains runs re against s unless the lowercased name lacks the
// literal keyword every match of re must contain. The keywords are chosen to
// have no non-ASCII case folds, so the shortcut never hides a match.
func findIfContains(re *regexp.Regexp, s, lower, keyword string) []int {
	if !strings.Contains(lower, keyword) {
		return nil
	}
	return re.FindStringSubmatchIndex(s)
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
		if n := atoi(m[1]); plausibleAbsolute(n) {
			return n
		}
	}
	return 0
}

// plausibleAbsolute rejects 0 and values that are really years.
func plausibleAbsolute(n int) bool {
	return n > 0 && !(n >= 1900 && n <= 2099)
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
