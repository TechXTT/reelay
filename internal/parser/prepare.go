package parser

import (
	"regexp"
	"strconv"
	"strings"
)

// Steps 1 and 2 of the cascade: strip container, site tags and the release
// group so what remains is title, numbering and quality tokens.

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
	if !strings.Contains(s, "[") {
		return s, ""
	}
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

var numericQualityRe = regexp.MustCompile(`^\d+([.\-]\d+)?(bit|p|i|ch)?$`)

func looksLikeQualityToken(lower string) bool {
	if resolutionAlias[lower] != "" || sourceAlias[lower] != "" ||
		videoAlias[lower] != "" || audioAlias[lower] != "" || hdrAlias[lower] != "" {
		return true
	}
	// "5.1", "7.1", "10bit", "1080p"
	return numericQualityRe.MatchString(lower)
}

var separatorReplacer = strings.NewReplacer(".", " ", "_", " ", "(", " ( ", ")", " ) ")

// normalizeSeparators turns dots and underscores into spaces, pads
// parentheses so they tokenise on their own, and collapses runs of whitespace.
//
// apibay has already done half of this: it replaces dots with spaces in the
// `name` field, so "H.264" arrives as "H 264" and "DDP5.1" as "DDP5 1". The
// token regexes below have to tolerate both forms, which is why they all allow
// an optional separator where scene naming has a dot.
func normalizeSeparators(s string) string {
	return strings.Join(strings.Fields(separatorReplacer.Replace(s)), " ")
}

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
	if !strings.Contains(s, ".") {
		return s
	}
	return trackerPrefixRe.ReplaceAllString(s, "")
}
