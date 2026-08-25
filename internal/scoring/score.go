package scoring

import (
	"fmt"
	"math"
	"strings"

	"github.com/TechXTT/reelay/internal/parser"
)

// componentsFor computes the stage-2 breakdown for an accepted candidate.
//
// Every component is returned even when it scores zero, because "why did this
// lose" is answered by the zeroes as often as by the points.
func componentsFor(c Candidate, in Input) []Component {
	w := in.Weights
	out := make([]Component, 0, 9)

	out = append(out, resolutionComponent(c, in))
	out = append(out, sourceComponent(c, in))
	out = append(out, groupComponent(c, in))
	out = append(out, languageComponent(c, in))
	out = append(out, properComponent(c, w.ProperRepackWeight))
	out = append(out, seederComponent(c, w.SeederWeightMax))
	out = append(out, hdrComponent(c, in))
	if comp, ok := seasonPackComponent(c, in); ok {
		out = append(out, comp)
	}
	out = append(out, ageComponent(c, in))

	return out
}

// resolutionComponent scores position in the profile's preference order rather
// than raw pixel count, so a profile that prefers 1080p over 2160p (smaller
// files, adequate for the display) gets what it asked for.
func resolutionComponent(c Candidate, in Input) Component {
	ranks := len(in.Profile.AllowedResolutions)
	rank := in.Profile.ResolutionRank(c.Parsed.Resolution)
	if ranks == 0 || rank < 0 {
		return Component{Name: "resolution", Points: 0, Detail: "no ranked resolution"}
	}
	points := in.Weights.ResolutionWeight * rank / ranks
	return Component{
		Name:   "resolution",
		Points: points,
		Detail: fmt.Sprintf("%s (preference %d of %d)", c.Parsed.Resolution, ranks-rank+1, ranks),
	}
}

func sourceComponent(c Candidate, in Input) Component {
	ranks := len(in.Profile.AllowedSources)
	rank := in.Profile.SourceRank(c.Parsed.Source)
	if ranks == 0 || rank < 0 {
		detail := "no ranked source"
		if c.Parsed.Source == "" && c.Parsed.Truncated {
			detail = "source unknown (name truncated by the indexer)"
		} else if c.Parsed.Source == "" {
			detail = "source not stated"
		}
		return Component{Name: "source", Points: 0, Detail: detail}
	}
	points := in.Weights.SourceWeight * rank / ranks
	return Component{
		Name:   "source",
		Points: points,
		Detail: fmt.Sprintf("%s (preference %d of %d)", c.Parsed.Source, ranks-rank+1, ranks),
	}
}

// groupWeightBaseline is the group_weight value at which a profile's per-group
// scores are taken at face value. Keeping the baseline explicit means a user
// can dial the whole dimension up or down without rewriting every group score.
const groupWeightBaseline = 300

// groupComponent applies the profile's per-group score, which may be negative
// for groups the user actively dislikes.
//
// The important case is the absent group. On this indexer roughly two in five
// releases have no recoverable group — names are truncated at 80 characters and
// the uploader's handle sits where the group would be. Scoring that as "unknown
// group, no bonus" is right; scoring it as a penalty would systematically
// prefer the minority of releases whose names happened to fit.
func groupComponent(c Candidate, in Input) Component {
	if c.Parsed.ReleaseGroup == "" {
		detail := "no release group in the name"
		if c.Parsed.Truncated {
			detail = "group unknown: the indexer truncated the name at 80 characters"
		}
		return Component{Name: "group", Points: 0, Detail: detail}
	}
	if len(in.Profile.PreferredGroups) == 0 {
		return Component{Name: "group", Points: 0,
			Detail: fmt.Sprintf("%s (profile lists no group preferences)", c.Parsed.ReleaseGroup)}
	}

	// Group names are compared case-insensitively: indexers are inconsistent
	// about capitalisation and "NTb" versus "ntb" is the same team.
	want := strings.ToLower(c.Parsed.ReleaseGroup)
	for name, score := range in.Profile.PreferredGroups {
		if strings.ToLower(name) != want {
			continue
		}
		points := score * in.Weights.GroupWeight / groupWeightBaseline
		verb := "preferred"
		if score < 0 {
			verb = "disliked"
		}
		return Component{
			Name:   "group",
			Points: points,
			Detail: fmt.Sprintf("%s (%s, profile score %d)", c.Parsed.ReleaseGroup, verb, score),
		}
	}
	return Component{Name: "group", Points: 0,
		Detail: fmt.Sprintf("%s (not in the profile's group list)", c.Parsed.ReleaseGroup)}
}

// languageComponent rewards the earliest matching language preference, scaled
// by its position so a first choice beats a second.
func languageComponent(c Candidate, in Input) Component {
	prefs := in.Profile.LanguagePrefs
	if len(prefs) == 0 {
		return Component{Name: "language", Points: 0, Detail: "profile has no language preference"}
	}
	if len(c.Parsed.Language) == 0 {
		// No language markers usually means the original audio, which for the
		// overwhelming majority of libraries is the wanted one. Neutral, not
		// penalised.
		return Component{Name: "language", Points: 0, Detail: "no language markers (assumed original audio)"}
	}
	for i, pref := range prefs {
		for _, got := range c.Parsed.Language {
			if got != pref {
				continue
			}
			points := in.Weights.LanguageWeight * (len(prefs) - i) / len(prefs)
			return Component{
				Name:   "language",
				Points: points,
				Detail: fmt.Sprintf("%s (preference %d of %d)", pref, i+1, len(prefs)),
			}
		}
	}
	return Component{Name: "language", Points: 0,
		Detail: fmt.Sprintf("languages %s match none of %s",
			strings.Join(c.Parsed.Language, "/"), strings.Join(prefs, "/"))}
}

func properComponent(c Candidate, weight int) Component {
	switch {
	case c.Parsed.Proper && c.Parsed.Repack:
		return Component{Name: "proper_repack", Points: weight, Detail: "PROPER and REPACK"}
	case c.Parsed.Proper:
		return Component{Name: "proper_repack", Points: weight, Detail: "PROPER"}
	case c.Parsed.Repack:
		return Component{Name: "proper_repack", Points: weight, Detail: "REPACK"}
	default:
		return Component{Name: "proper_repack", Points: 0}
	}
}

// seederComponent has diminishing returns by design: swarm health matters, but
// a 5000-seeder 720p rip must never beat a 40-seeder 2160p remux.
//
// min(max, 30*log2(1+seeders)) saturates at 31 seeders, so above that the
// component is effectively flat — which is the intent. Anything with a healthy
// swarm is treated as equally healthy.
func seederComponent(c Candidate, maxPoints int) Component {
	s := c.Release.Seeders
	if s <= 0 {
		return Component{Name: "seeders", Points: 0, Detail: "no seeders"}
	}
	points := int(30 * math.Log2(1+float64(s)))
	if points > maxPoints {
		points = maxPoints
	}
	detail := fmt.Sprintf("%d seeders", s)
	if points == maxPoints {
		detail += " (at the cap)"
	}
	return Component{Name: "seeders", Points: points, Detail: detail}
}

func hdrComponent(c Candidate, in Input) Component {
	prefs := in.Profile.HDRPrefs
	if len(prefs) == 0 {
		return Component{Name: "hdr", Points: 0, Detail: "profile has no HDR preference"}
	}
	if len(c.Parsed.HDR) == 0 {
		return Component{Name: "hdr", Points: 0, Detail: "SDR"}
	}
	for i, pref := range prefs {
		for _, got := range c.Parsed.HDR {
			if got != pref {
				continue
			}
			points := in.Weights.HDRWeight * (len(prefs) - i) / len(prefs)
			return Component{
				Name:   "hdr",
				Points: points,
				Detail: fmt.Sprintf("%s (preference %d of %d)", pref, i+1, len(prefs)),
			}
		}
	}
	return Component{Name: "hdr", Points: 0,
		Detail: fmt.Sprintf("%s matches none of %s",
			strings.Join(c.Parsed.HDR, "/"), strings.Join(prefs, "/"))}
}

// seasonPackComponent rewards a pack that supplies several things we are
// missing, and PENALISES one that does not.
//
// The spec only defines the bonus. Withholding it is not enough: with a bonus
// of zero, a 139 GB six-season pack ties with a 4 GB single-season pack for the
// right to deliver one 40-minute episode, and the tie falls to whichever has
// more seeders. That is not a hypothetical — it is what the first run of this
// code did against a real candidate set.
//
// Over-fetching is not quality-neutral. It costs hours of transfer, gigabytes
// on the NAS, and on a DS214se over SMB it is the difference between an import
// finishing tonight and finishing tomorrow. So a pack that covers fewer than
// two wanted episodes takes a penalty of the same magnitude as the bonus it
// failed to earn, doubled when it spans multiple seasons, because a
// complete-series grab for one episode is the worst case of all.
func seasonPackComponent(c Candidate, in Input) (Component, bool) {
	if in.Want == nil || len(in.Want.WantedEpisodes) == 0 {
		return Component{}, false
	}

	covered := parser.WantedEpisodesCovered(c.Parsed, *in.Want)
	isPack := c.Parsed.IsSeasonPack || len(c.Parsed.Episodes) > 1

	if covered >= 2 {
		points := in.Weights.SeasonPackWeight
		detail := fmt.Sprintf("covers %d wanted episodes", covered)
		if span := c.Parsed.SeasonEnd - c.Parsed.Season; span > 0 {
			// Prefer one bounded season at a time. A complete-series pack can be
			// useful as a fallback, but it must not beat a season pack merely
			// because both satisfy the same episodes in the season being scored.
			points -= span * in.Weights.SeasonPackWeight
			detail = fmt.Sprintf("covers %d wanted episodes but spans %d extra seasons", covered, span)
		}
		return Component{
			Name:   "season_pack",
			Points: points,
			Detail: detail,
		}, true
	}
	if !isPack {
		// A single episode file is exactly what was asked for; neither bonus
		// nor penalty applies.
		return Component{}, false
	}

	penalty := in.Weights.SeasonPackWeight
	detail := fmt.Sprintf("pack covers only %d of the wanted episodes", covered)
	if c.Parsed.SeasonEnd > c.Parsed.Season {
		penalty *= 2
		detail = fmt.Sprintf("S%02d-S%02d pack covers only %d of the wanted episodes",
			c.Parsed.Season, c.Parsed.SeasonEnd, covered)
	}
	return Component{
		Name:   "season_pack",
		Points: -penalty,
		Detail: detail + "; oversized for this request",
	}, true
}

// ageComponent is a small tiebreak favouring fresher releases, and it is
// CAPPED.
//
// Uncapped, at one point per day, a three-year-old 2160p remux carries a −1095
// penalty and loses to a fresh 720p rip — the age term silently becomes the
// dominant signal. Config validation refuses to start with an uncapped penalty
// for exactly this reason.
func ageComponent(c Candidate, in Input) Component {
	if c.Release.PublishedAt.IsZero() {
		return Component{Name: "age", Points: 0, Detail: "publication date unknown"}
	}
	days := int(in.Now.Sub(c.Release.PublishedAt).Hours() / 24)
	if days <= 0 {
		return Component{Name: "age", Points: 0, Detail: "published today"}
	}
	penalty := days * in.Weights.AgePenaltyPerDay
	capped := false
	if in.Weights.AgePenaltyMax > 0 && penalty > in.Weights.AgePenaltyMax {
		penalty = in.Weights.AgePenaltyMax
		capped = true
	}
	detail := fmt.Sprintf("%d days old", days)
	if capped {
		detail += fmt.Sprintf(" (penalty capped at %d)", in.Weights.AgePenaltyMax)
	}
	return Component{Name: "age", Points: -penalty, Detail: detail}
}
