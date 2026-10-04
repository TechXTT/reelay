package parser

import (
	"fmt"
	"strings"

	"github.com/TechXTT/reelay/internal/model"
)

// MatchResult explains a matching decision. The reason is recorded against the
// candidate so the UI can say "11 of 12 rejected, and here is why" rather than
// silently discarding everything.
type MatchResult struct {
	OK     bool
	Reason string
}

func matchOK() MatchResult { return MatchResult{OK: true} }
func matchNo(f string, a ...any) MatchResult {
	return MatchResult{Reason: fmt.Sprintf(f, a...)}
}

// Matcher checks parsed releases against one wanted item. The wanted title and
// aliases are normalised once at construction, so matching a whole result set
// does not re-normalise them per release.
type Matcher struct {
	want   model.Wanted
	titles []wantedTitle
}

type wantedTitle struct {
	raw        string
	normalized string
	budget     int
}

// NewMatcher prepares want for repeated matching.
func NewMatcher(want model.Wanted) *Matcher {
	m := &Matcher{want: want, titles: make([]wantedTitle, 0, 1+len(want.Aliases))}
	for _, raw := range append([]string{want.Title}, want.Aliases...) {
		if raw == "" {
			continue
		}
		normalized := NormalizeForMatch(raw)
		m.titles = append(m.titles, wantedTitle{raw: raw, normalized: normalized, budget: fuzzyBudget(len(normalized))})
	}
	return m
}

// Matches reports whether a parsed release satisfies what we want.
func Matches(p Parsed, want model.Wanted) MatchResult {
	return NewMatcher(want).Match(p)
}

// Match reports whether a parsed release satisfies the wanted item.
//
// Title matching accepts any of these comparisons against the title or aliases:
//  1. normalised equality against the title or any alias
//  2. article-insensitive equality ("The Expanse" vs "Expanse")
//  3. bounded edit distance, with the budget scaled by title length
//
// Numbering is then checked exactly. There is no fuzziness on season or
// episode numbers: grabbing the wrong episode is worse than grabbing nothing.
func (m *Matcher) Match(p Parsed) MatchResult {
	if p.Title == "" {
		return matchNo("release name could not be parsed into a title")
	}
	if res := m.matchTitle(p); !res.OK {
		return res
	}

	switch m.want.Kind {
	case model.SubjectMovie:
		return matchMovie(p, m.want)
	case model.SubjectEpisode:
		return matchEpisode(p, m.want)
	default:
		return matchNo("unsupported wanted kind %q", m.want.Kind)
	}
}

func (m *Matcher) matchTitle(p Parsed) MatchResult {
	pm := NormalizeForMatch(p.Title)
	for _, t := range m.titles {
		if p.Title == t.raw || pm == t.normalized {
			return matchOK()
		}
		if t.budget > 0 && Levenshtein(pm, t.normalized, t.budget) <= t.budget {
			return matchOK()
		}
	}
	return matchNo("title %q does not match %q", p.Title, m.want.Title)
}

func matchMovie(p Parsed, want model.Wanted) MatchResult {
	if p.HasEpisodeInfo() {
		return matchNo("release carries season/episode numbering but a movie was wanted")
	}
	// A missing year is tolerated — plenty of correct releases omit it — but a
	// year that disagrees is a different film. One year of slack covers the
	// festival-versus-general-release discrepancy that TMDB and scene groups
	// routinely disagree on.
	if want.Year > 0 && p.Year > 0 {
		if diff := p.Year - want.Year; diff > 1 || diff < -1 {
			return matchNo("year %d does not match wanted %d", p.Year, want.Year)
		}
	}
	return matchOK()
}

func matchEpisode(p Parsed, want model.Wanted) MatchResult {
	// Anime absolute numbering: a release may identify the episode only by its
	// continuous number, with no season at all.
	if want.IsAnime && want.AbsoluteEp > 0 && p.AbsoluteEp > 0 {
		if p.AbsoluteEp == want.AbsoluteEp {
			return matchOK()
		}
		// An absolute number that disagrees is only decisive when there is no
		// season/episode pair to fall back on.
		if p.Season == 0 && len(p.Episodes) == 0 {
			return matchNo("absolute episode %d does not match wanted %d", p.AbsoluteEp, want.AbsoluteEp)
		}
	}

	if p.AirDate != "" {
		// Daily show: the caller supplies the air date through the alias list
		// only if it wants date matching, which the engine does not use yet.
		return matchNo("release is date-based (%s); date matching is not wired up", p.AirDate)
	}

	if p.Season == 0 {
		return matchNo("release has no season number")
	}
	if p.IsSeasonPack && len(p.Episodes) == 0 {
		if p.SeasonEnd > 0 {
			if want.Season < p.Season || want.Season > p.SeasonEnd {
				return matchNo("season pack covers S%02d-S%02d, wanted S%02d", p.Season, p.SeasonEnd, want.Season)
			}
			return matchOK()
		}
		if p.Season != want.Season {
			return matchNo("season pack is S%02d, wanted S%02d", p.Season, want.Season)
		}
		return matchOK()
	}

	if !p.CoversEpisode(want.Season, want.Episode) {
		return matchNo("release covers S%02d%s, wanted S%02dE%02d",
			p.Season, formatEpisodes(p.Episodes), want.Season, want.Episode)
	}
	return matchOK()
}

func formatEpisodes(eps []int) string {
	if len(eps) == 0 {
		return " (season pack)"
	}
	var b strings.Builder
	for _, e := range eps {
		fmt.Fprintf(&b, "E%02d", e)
	}
	return b.String()
}

// WantedEpisodesCovered counts how many of the caller's still-missing episodes
// this release would supply. Season-pack scoring needs the count, not a bool.
func WantedEpisodesCovered(p Parsed, want model.Wanted) int {
	if len(want.WantedEpisodes) == 0 {
		return 0
	}
	n := 0
	for _, e := range want.WantedEpisodes {
		if p.CoversEpisode(want.Season, e) {
			n++
		}
	}
	return n
}
