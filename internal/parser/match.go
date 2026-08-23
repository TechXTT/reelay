package parser

import (
	"fmt"

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

// Matches reports whether a parsed release satisfies what we want.
//
// Title matching runs three comparisons in decreasing confidence:
//  1. normalised equality against the title or any alias
//  2. article-insensitive equality ("The Expanse" vs "Expanse")
//  3. bounded edit distance, with the budget scaled by title length
//
// Numbering is then checked exactly. There is no fuzziness on season or
// episode numbers: grabbing the wrong episode is worse than grabbing nothing.
func Matches(p Parsed, want model.Wanted) MatchResult {
	if p.Title == "" {
		return matchNo("release name could not be parsed into a title")
	}
	if res := matchTitle(p, want); !res.OK {
		return res
	}

	switch want.Kind {
	case model.SubjectMovie:
		return matchMovie(p, want)
	case model.SubjectEpisode:
		return matchEpisode(p, want)
	default:
		return matchNo("unsupported wanted kind %q", want.Kind)
	}
}

func matchTitle(p Parsed, want model.Wanted) MatchResult {
	candidates := append([]string{want.Title}, want.Aliases...)

	for _, c := range candidates {
		if c == "" {
			continue
		}
		if p.Title == c {
			return matchOK()
		}
	}
	pm := NormalizeForMatch(p.Title)
	for _, c := range candidates {
		if c == "" {
			continue
		}
		if pm == NormalizeForMatch(c) {
			return matchOK()
		}
	}
	for _, c := range candidates {
		if c == "" {
			continue
		}
		cm := NormalizeForMatch(c)
		budget := fuzzyBudget(len(cm))
		if budget == 0 {
			continue
		}
		if Levenshtein(pm, cm, budget) <= budget {
			return matchOK()
		}
	}
	return matchNo("title %q does not match %q", p.Title, want.Title)
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
	out := ""
	for _, e := range eps {
		out += fmt.Sprintf("E%02d", e)
	}
	return out
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
