// Package recommendation ranks metadata candidates against a bounded user
// taste profile. It deliberately stays deterministic and dependency-free.
package recommendation

import (
	"math"
	"sort"
	"strings"

	"github.com/TechXTT/reelay/internal/model"
)

type Weights struct {
	Provider, Affinity, People, MultiSeed, Rating, Preference, Novelty float64
}

func DefaultWeights() Weights {
	return Weights{Provider: 25, Affinity: 20, People: 15, MultiSeed: 15, Rating: 10, Preference: 5, Novelty: 10}
}

type Profile struct {
	Genres, Keywords, People map[string]float64
	Languages, Countries     map[string]float64
	Years, Runtimes          []int
}

type Candidate struct {
	Item          model.Recommendation
	ProviderScore float64
	SeedMatches   int
	VoteAverage   float64
	VoteCount     int
}

func Rank(candidates []Candidate, profile Profile, weights Weights, limit int) []model.Recommendation {
	if limit <= 0 {
		limit = 40
	}
	if weights == (Weights{}) {
		weights = DefaultWeights()
	}
	type rankedCandidate struct {
		model.Recommendation
		novelty float64
		genres  map[string]bool
	}
	scored := make([]rankedCandidate, 0, len(candidates))

	for _, c := range candidates {
		components := map[string]float64{
			"provider":   clamp(c.ProviderScore) * weights.Provider,
			"affinity":   ((overlap(c.Item.Genres, profile.Genres) + overlap(c.Item.Keywords, profile.Keywords)) / 2) * weights.Affinity,
			"people":     overlap(c.Item.People, profile.People) * weights.People,
			"multi_seed": clamp(float64(c.SeedMatches-1)/3) * weights.MultiSeed,
			"rating":     bayesianRating(c.VoteAverage, c.VoteCount) * weights.Rating,
			"preference": preference(c.Item, profile) * weights.Preference,
		}
		base := components["provider"] + components["affinity"] + components["people"] + components["multi_seed"] + components["rating"] + components["preference"]
		c.Item.Score = math.Round(base*10) / 10
		c.Item.Components = components
		c.Item.Reasons = reasons(components, c.SeedMatches)
		scored = append(scored, rankedCandidate{Recommendation: c.Item, novelty: 1, genres: genreSet(c.Item.Genres)})
	}
	sort.SliceStable(scored, func(i, j int) bool {
		if scored[i].Score != scored[j].Score {
			return scored[i].Score > scored[j].Score
		}
		return scored[i].TMDBID < scored[j].TMDBID
	})

	// Greedy diversity bonus: reward a candidate that is unlike items already
	// selected without allowing novelty to overwhelm relevance. Picked entries
	// are flagged instead of removed so ties keep resolving to the earliest
	// (highest relevance, lowest TMDB id) candidate.
	selected := make([]model.Recommendation, 0, min(limit, len(scored)))
	taken := make([]bool, len(scored))
	for len(selected) < min(limit, len(scored)) {
		best, bestValue := -1, math.Inf(-1)
		for i := range scored {
			if taken[i] {
				continue
			}
			if best < 0 {
				best = i
			}
			value := scored[i].Score + scored[i].novelty*weights.Novelty
			if value > bestValue {
				best, bestValue = i, value
			}
		}
		pick := &scored[best]
		pick.Components["novelty"] = math.Round((bestValue-pick.Score)*10) / 10
		pick.Score = math.Min(100, math.Round(bestValue*10)/10)
		selected = append(selected, pick.Recommendation)
		taken[best] = true
		if len(selected) == limit {
			break
		}
		for i := range scored {
			if !taken[i] {
				scored[i].novelty = math.Min(scored[i].novelty, 1-jaccard(scored[i].genres, pick.genres))
			}
		}
	}
	return selected
}

func overlap(values []string, profile map[string]float64) float64 {
	if len(values) == 0 || len(profile) == 0 {
		return 0
	}
	total := 0.0
	for _, value := range values {
		total += profile[strings.ToLower(value)]
	}
	return clamp(total / float64(len(values)*3))
}

func preference(item model.Recommendation, profile Profile) float64 {
	v := 0.0
	if profile.Languages[strings.ToLower(item.Language)] > 0 {
		v += .4
	}
	if profile.Countries[strings.ToLower(item.Country)] > 0 {
		v += .2
	}
	if near(item.Year, profile.Years, 6) {
		v += .2
	}
	if near(item.RuntimeMinutes, profile.Runtimes, 25) {
		v += .2
	}
	return v
}

func near(value int, values []int, distance int) bool {
	if value == 0 {
		return false
	}
	for _, other := range values {
		if abs(value-other) <= distance {
			return true
		}
	}
	return false
}

func bayesianRating(average float64, votes int) float64 {
	if average <= 0 || votes <= 0 {
		return 0
	}
	confidence := float64(votes) / float64(votes+250)
	return clamp((average / 10) * confidence)
}

func reasons(parts map[string]float64, seedMatches int) []string {
	type part struct {
		name  string
		score float64
	}
	partsList := []part{{"Matches your genres and themes", parts["affinity"]}, {"Matches cast and creators you enjoy", parts["people"]}, {"Strong TMDB recommendation", parts["provider"]}}
	sort.Slice(partsList, func(i, j int) bool { return partsList[i].score > partsList[j].score })
	out := make([]string, 0, 3)
	if seedMatches > 1 {
		out = append(out, "Recommended by multiple titles in your history")
	}
	for _, p := range partsList {
		if p.score > 0 && len(out) < 3 {
			out = append(out, p.name)
		}
	}
	if len(out) == 0 {
		out = append(out, "Included to broaden your recommendations")
	}
	return out
}

func jaccard(left, right map[string]bool) float64 {
	intersection := 0
	for genre := range left {
		if right[genre] {
			intersection++
		}
	}
	union := len(left) + len(right) - intersection
	if union == 0 {
		return 0
	}
	return float64(intersection) / float64(union)
}

func genreSet(genres []string) map[string]bool {
	set := make(map[string]bool, len(genres))
	for _, genre := range genres {
		set[strings.ToLower(genre)] = true
	}
	return set
}

func clamp(v float64) float64 {
	return min(max(v, 0), 1)
}

func abs(v int) int {
	if v < 0 {
		return -v
	}
	return v
}
