package scoring

import (
	"fmt"
	"strings"

	"github.com/TechXTT/reelay/internal/model"
	"github.com/TechXTT/reelay/internal/parser"
)

// reject runs the stage-1 hard filters in order and returns the first failure
// as (category, reason), or ("", "") if the candidate is acceptable.
//
// Order is chosen so the reason a human sees is the most useful one. Cheap,
// decisive checks come first — a blacklisted hash or the wrong episode
// entirely — because reporting "wrong resolution" for a release of a different
// show is technically true and completely unhelpful.
func reject(c *Candidate, in Input) (category, reason string) {
	for _, f := range filters {
		if cat, why := f(c, in); cat != "" {
			return cat, why
		}
	}
	return "", ""
}

type filterFunc func(c *Candidate, in Input) (category, reason string)

var filters = []filterFunc{
	filterUnparseable,
	filterBlacklisted,
	filterItemMatch,
	filterBannedTerms,
	filterRequiredTerms,
	filterResolution,
	filterSource,
	filterSeeders,
	filterSize,
	filterUpgrade,
}

func filterUnparseable(c *Candidate, _ Input) (string, string) {
	if c.Parsed.Title == "" {
		return RejectUnparseable, "release name could not be parsed into a title"
	}
	return "", ""
}

// filterBlacklisted drops releases already tried and failed for this item.
//
// Per-item rather than global: a torrent that stalled for one episode may be
// the only source for another, and a global blacklist would quietly remove it
// from consideration everywhere.
func filterBlacklisted(c *Candidate, in Input) (string, string) {
	if in.Blacklist[c.Release.InfoHash] {
		return RejectBlacklisted, "info hash was blacklisted after a previous failed grab for this item"
	}
	return "", ""
}

func filterItemMatch(c *Candidate, in Input) (string, string) {
	if in.Want == nil {
		return "", ""
	}
	if m := parser.Matches(c.Parsed, *in.Want); !m.OK {
		return RejectWrongItem, m.Reason
	}
	return "", ""
}

// filterBannedTerms matches against the RAW release name, not the parsed form.
//
// Deliberate: the parser normalises and discards, and a banned term is often
// exactly the thing it threw away. Matching the raw name means a marker cannot
// be laundered by successful parsing.
func filterBannedTerms(c *Candidate, in Input) (string, string) {
	haystack := " " + strings.ToLower(c.Release.Title) + " "
	for _, term := range in.Profile.BannedTerms {
		term = strings.ToLower(strings.TrimSpace(term))
		if term == "" {
			continue
		}
		if containsTerm(haystack, term) {
			return RejectBannedTerm, fmt.Sprintf("release name contains the banned term %q", term)
		}
	}
	// A cam-family source is banned whether or not the profile spelled out
	// every marker, because the parser recognises far more spellings of it
	// than any hand-written term list will.
	if c.Parsed.Source == "cam" && !allowsSource(in.Profile, "cam") {
		return RejectBannedTerm, "release is a cam/telesync/screener rip"
	}
	return "", ""
}

func filterRequiredTerms(c *Candidate, in Input) (string, string) {
	haystack := " " + strings.ToLower(c.Release.Title) + " "
	for _, term := range in.Profile.RequiredTerms {
		term = strings.ToLower(strings.TrimSpace(term))
		if term == "" {
			continue
		}
		if !containsTerm(haystack, term) {
			return RejectRequiredTerm, fmt.Sprintf("release name is missing the required term %q", term)
		}
	}
	return "", ""
}

func filterResolution(c *Candidate, in Input) (string, string) {
	if len(in.Profile.AllowedResolutions) == 0 {
		return "", ""
	}
	if c.Parsed.Resolution == "" {
		// An unstated resolution is not an implicit pass. Scene naming always
		// states it, so a missing one usually means the name was truncated or
		// the release is unusual — either way, grabbing it is a gamble the
		// profile did not authorise.
		return RejectResolution, "release does not state a resolution"
	}
	if in.Profile.ResolutionRank(c.Parsed.Resolution) < 0 {
		return RejectResolution, fmt.Sprintf("resolution %s is not in the profile's allowed list (%s)",
			c.Parsed.Resolution, strings.Join(in.Profile.AllowedResolutions, ", "))
	}
	return "", ""
}

func filterSource(c *Candidate, in Input) (string, string) {
	if len(in.Profile.AllowedSources) == 0 {
		return "", ""
	}
	if c.Parsed.Source == "" {
		// Unlike resolution, a missing source is tolerated: apibay truncates
		// names at 80 characters and the source token is often what falls off
		// the end. Rejecting on it would discard a large share of otherwise
		// good candidates from this indexer.
		return "", ""
	}
	if in.Profile.SourceRank(c.Parsed.Source) < 0 {
		return RejectSource, fmt.Sprintf("source %s is not in the profile's allowed list (%s)",
			c.Parsed.Source, strings.Join(in.Profile.AllowedSources, ", "))
	}
	return "", ""
}

func filterSeeders(c *Candidate, in Input) (string, string) {
	if c.Release.Seeders < in.Profile.MinSeeders {
		return RejectSeeders, fmt.Sprintf("%d seeders is below the profile floor of %d",
			c.Release.Seeders, in.Profile.MinSeeders)
	}
	return "", ""
}

// filterSize applies the profile's size window, scaled by how much content the
// release actually contains.
//
// A flat window cannot work for both a single episode and a ten-episode season
// pack: a limit that admits the pack admits an absurdly oversized single
// episode, and one that bounds the episode rejects every pack.
func filterSize(c *Candidate, in Input) (string, string) {
	if in.Profile.MaxSizeMB <= 0 && in.Profile.MinSizeMB <= 0 {
		return "", ""
	}
	units := contentUnits(c.Parsed, c.Release.Files)
	scale := 1.0
	if in.Want != nil && in.Want.Kind == model.SubjectEpisode {
		scale = runtimeScale(in.RuntimeMinutes)
	}

	minMB := int(float64(in.Profile.MinSizeMB) * float64(units) * scale)
	maxMB := int(float64(in.Profile.MaxSizeMB) * float64(units) * scale)
	sizeMB := c.Release.SizeMB()

	// A zero or missing size cannot be judged; let it through rather than
	// discard a candidate over a field the indexer failed to populate.
	if c.Release.SizeBytes <= 0 {
		return "", ""
	}
	if in.Profile.MinSizeMB > 0 && sizeMB < minMB {
		return RejectSize, fmt.Sprintf("%d MB is below the %d MB floor (%d %s x %d MB%s)",
			sizeMB, minMB, units, unitWord(units), in.Profile.MinSizeMB, scaleNote(scale))
	}
	if in.Profile.MaxSizeMB > 0 && sizeMB > maxMB {
		return RejectSize, fmt.Sprintf("%d MB exceeds the %d MB ceiling (%d %s x %d MB%s)",
			sizeMB, maxMB, units, unitWord(units), in.Profile.MaxSizeMB, scaleNote(scale))
	}
	return "", ""
}

// contentUnits estimates how many episodes' worth of video a release holds.
func contentUnits(p parser.Parsed, files int) int {
	// Multi-season packs are sized by their span: a six-season set legitimately
	// runs to a hundred episodes, and clamping it to one season's worth would
	// reject a complete-series grab the user actually wants.
	seasons := 1
	if p.SeasonEnd > p.Season {
		seasons = p.SeasonEnd - p.Season + 1
	}

	switch {
	case p.IsSeasonPack && len(p.Episodes) == 0:
		// The torrent's file count is the best available proxy for a pack's
		// episode count, and it comes free from the indexer. Clamped because a
		// pack that ships subtitles and artwork inflates the count.
		if files > 1 {
			return clamp(files, 1, 40*seasons)
		}
		// No file count: assume a typical season rather than a single episode,
		// or every pack fails the ceiling.
		return 10 * seasons
	case len(p.Episodes) > 1:
		return len(p.Episodes)
	default:
		return 1
	}
}

// runtimeScale adjusts the window for shows that are not ~45 minutes.
// A 22-minute sitcom and a 90-minute drama do not belong in the same envelope.
func runtimeScale(runtimeMinutes int) float64 {
	if runtimeMinutes <= 0 {
		return 1
	}
	scale := float64(runtimeMinutes) / 45.0
	// Clamped so a bad metadata value cannot make the window meaningless.
	if scale < 0.4 {
		return 0.4
	}
	if scale > 4 {
		return 4
	}
	return scale
}

// filterUpgrade rejects a candidate that would not improve on what we have.
//
// Runs last: it is the only filter whose answer depends on our own library
// rather than on the release, and it produces the most confusing message, so
// every simpler reason gets to speak first.
func filterUpgrade(c *Candidate, in Input) (string, string) {
	if in.Imported == nil {
		return "", ""
	}

	haveRes := in.Profile.ResolutionRank(in.Imported.Resolution)
	wantRes := in.Profile.ResolutionRank(c.Parsed.Resolution)
	haveSrc := in.Profile.SourceRank(in.Imported.Source)
	wantSrc := in.Profile.SourceRank(c.Parsed.Source)

	switch {
	case wantRes > haveRes:
		return "", "" // better resolution is always an upgrade
	case wantRes < haveRes:
		return RejectNotAnUpgrade, fmt.Sprintf(
			"already imported at %s; %s is lower", in.Imported.Resolution, c.Parsed.Resolution)
	}

	// Same resolution: a better source still counts.
	switch {
	case wantSrc > haveSrc:
		return "", ""
	case wantSrc < haveSrc:
		return RejectNotAnUpgrade, fmt.Sprintf(
			"already imported at %s %s; %s is a lower-quality source",
			in.Imported.Resolution, in.Imported.Source, c.Parsed.Source)
	}

	// Identical quality. A PROPER or REPACK of the same quality is a fix for a
	// broken release, which is the one case worth re-downloading for.
	isFix := c.Parsed.Proper || c.Parsed.Repack
	hadFix := in.Imported.Proper || in.Imported.Repack
	if isFix && !hadFix {
		return "", ""
	}

	// At the profile's cutoff there is nothing left to chase.
	if in.Profile.UpgradeUntil != "" &&
		in.Profile.ResolutionRank(in.Imported.Resolution) >= in.Profile.ResolutionRank(in.Profile.UpgradeUntil) {
		return RejectNotAnUpgrade, fmt.Sprintf(
			"already imported at the profile's upgrade cutoff (%s)", in.Profile.UpgradeUntil)
	}
	return RejectNotAnUpgrade, fmt.Sprintf(
		"already imported at the same quality (%s %s)", in.Imported.Resolution, in.Imported.Source)
}

// containsTerm does a whole-word-ish match.
//
// Substring matching is not good enough: "ts" is a banned telesync marker and
// also appears in "Ghosts", "Nights" and half the titles on any indexer.
// Both haystack and needle are space-padded by the caller.
func containsTerm(paddedHaystack, term string) bool {
	if term == "" {
		return false
	}
	// Multi-word terms are matched as a phrase.
	if strings.ContainsAny(term, " .-") {
		return strings.Contains(paddedHaystack, term)
	}
	for i := 0; i+len(term) <= len(paddedHaystack); i++ {
		if paddedHaystack[i:i+len(term)] != term {
			continue
		}
		if isWordBoundary(paddedHaystack, i-1) && isWordBoundary(paddedHaystack, i+len(term)) {
			return true
		}
	}
	return false
}

func isWordBoundary(s string, i int) bool {
	if i < 0 || i >= len(s) {
		return true
	}
	c := s[i]
	return !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9')
}

func allowsSource(p model.QualityProfile, src string) bool {
	return p.SourceRank(src) >= 0
}

func clamp(v, lo, hi int) int {
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}

func unitWord(n int) string {
	if n == 1 {
		return "episode"
	}
	return "episodes"
}

func scaleNote(scale float64) string {
	if scale == 1 {
		return ""
	}
	return fmt.Sprintf(", runtime scale %.2f", scale)
}
