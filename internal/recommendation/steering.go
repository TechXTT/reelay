package recommendation

import (
	"context"
	"math"
	"slices"

	"github.com/TechXTT/reelay/internal/config"
	"github.com/TechXTT/reelay/internal/model"
)

// Steering nudges a viewer's regular list for one Discover response. It lives
// in the browser session, is sent with each list request and is never stored;
// saved preferences remain the only lasting channel.
type Steering struct {
	MoreLike    []int // TMDB ids of listed titles to see more like
	LessLike    []int // TMDB ids of listed titles to see less like; they are hidden
	BoostGenres []string
	MuteGenres  []string
	BoostSeeds  []int // TMDB ids of seed titles
	MuteSeeds   []int
}

// Empty reports whether the steering changes nothing.
func (s Steering) Empty() bool {
	return len(s.MoreLike)+len(s.LessLike)+len(s.BoostGenres)+len(s.MuteGenres)+len(s.BoostSeeds)+len(s.MuteSeeds) == 0
}

// Steer re-ranks a user's stored active recommendations with steering
// applied. Each title's relevance components are kept, a "steering" component
// is added, and the list is arranged again with the configured novelty and
// diversity, so unsteered titles keep their regular relative order. Titles
// marked "less like this" are left out. Nothing is written.
func (s *Service) Steer(ctx context.Context, serverID, userID string, values []model.Recommendation, steering Steering) ([]model.Recommendation, error) {
	var weight = float64(s.cfg.SteeringWeight)
	var references = make(map[int]ranked, len(values))
	var scored = make([]ranked, 0, len(values))
	var boost = genreSet(steering.BoostGenres)
	var mute = genreSet(steering.MuteGenres)

	preferences, err := s.store.Recommendations().Preferences(ctx, serverID, userID)
	if err != nil {
		return nil, err
	}
	if weight == 0 {
		weight = config.DefaultSteeringWeight
	}
	for _, value := range values {
		references[value.TMDBID] = newRanked(value)
	}
	for _, value := range values {
		if slices.Contains(steering.LessLike, value.TMDBID) {
			continue
		}
		candidate := references[value.TMDBID]
		components := make(map[string]float64, len(relevanceComponents)+1)
		relevance := 0.0
		for _, name := range relevanceComponents {
			components[name] = value.Components[name]
			relevance += value.Components[name]
		}
		adjustment := weight * steering.adjustment(candidate, references, boost, mute)
		if adjustment != 0 {
			components["steering"] = math.Round(adjustment*10) / 10
		}
		candidate.Components = components
		candidate.Score = math.Round((relevance+adjustment)*10) / 10
		scored = append(scored, candidate)
	}
	return s.diversity().arrange(scored, s.weights(preferences).Novelty, len(scored)), nil
}

// adjustment is the steering effect on one title in steering-weight units: +1
// for a boosted and -1 for a muted genre or seed title, plus its similarity to
// the closest "more like this" title minus that to the closest "less like
// this" title.
func (s Steering) adjustment(candidate ranked, references map[int]ranked, boost, mute map[string]bool) float64 {
	var total = closest(candidate, s.MoreLike, references) - closest(candidate, s.LessLike, references)

	if overlaps(candidate.genres, boost) {
		total++
	}
	if overlaps(candidate.genres, mute) {
		total--
	}
	if fromSeed(candidate, s.BoostSeeds) {
		total++
	}
	if fromSeed(candidate, s.MuteSeeds) {
		total--
	}
	return total
}

// closest returns the highest similarity between candidate and the listed
// reference titles; ids no longer in the list are ignored.
func closest(candidate ranked, ids []int, references map[int]ranked) float64 {
	var best float64

	for _, id := range ids {
		if reference, ok := references[id]; ok {
			best = max(best, similarity(candidate, reference))
		}
	}
	return best
}

// similarity is the mean of genre overlap (Jaccard) and sharing the same
// strongest seed title.
func similarity(a, b ranked) float64 {
	var sameSeed float64

	if a.interest != "" && a.interest == b.interest {
		sameSeed = 1
	}
	return (jaccard(a.genres, b.genres) + sameSeed) / 2
}

func overlaps(genres, set map[string]bool) bool {
	for genre := range genres {
		if set[genre] {
			return true
		}
	}
	return false
}

func fromSeed(candidate ranked, seeds []int) bool {
	return slices.ContainsFunc(candidate.Sources, func(source model.RecommendationSource) bool {
		return source.Kind == "seed" && slices.Contains(seeds, source.TMDBID)
	})
}
