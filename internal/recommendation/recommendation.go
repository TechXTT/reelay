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
	scored := make([]model.Recommendation, 0, len(candidates))
	for _, c := range candidates {
		components := map[string]float64{
			"provider":   clamp(c.ProviderScore) * weights.Provider,
			"affinity":   ((overlap(c.Item.Genres, profile.Genres) + overlap(c.Item.Keywords, profile.Keywords)) / 2) * weights.Affinity,
			"people":     overlap(c.Item.People, profile.People) * weights.People,
			"multi_seed": clamp(float64(c.SeedMatches-1)/3) * weights.MultiSeed,
			"rating":     bayesianRating(c.VoteAverage, c.VoteCount) * weights.Rating,
			"preference": preference(c.Item, profile) * weights.Preference,
		}
		base := 0.0
		for _, value := range components {
			base += value
		}
		c.Item.Score = math.Round(base*10) / 10
		c.Item.Components = components
		c.Item.Reasons = reasons(c.Item, components, c.SeedMatches)
		scored = append(scored, c.Item)
	}
	sort.SliceStable(scored, func(i, j int) bool { return scored[i].Score > scored[j].Score })

	// Greedy diversity bonus: reward a candidate that is unlike items already
	// selected without allowing novelty to overwhelm relevance.
	selected := make([]model.Recommendation, 0, min(limit, len(scored)))
	for len(scored) > 0 && len(selected) < limit {
		best, bestValue := 0, -1.0
		for i := range scored {
			novelty := 1.0
			for _, previous := range selected {
				novelty = math.Min(novelty, 1-jaccard(scored[i].Genres, previous.Genres))
			}
			value := scored[i].Score + novelty*weights.Novelty
			if value > bestValue {
				best, bestValue = i, value
			}
		}
		scored[best].Components["novelty"] = math.Round((bestValue-scored[best].Score)*10) / 10
		scored[best].Score = math.Min(100, math.Round(bestValue*10)/10)
		selected = append(selected, scored[best])
		scored = append(scored[:best], scored[best+1:]...)
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

func reasons(item model.Recommendation, parts map[string]float64, seedMatches int) []string {
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

func jaccard(a, b []string) float64 {
	set := map[string]bool{}
	for _, s := range a {
		set[strings.ToLower(s)] = true
	}
	intersection, union := 0, len(set)
	for _, s := range b {
		k := strings.ToLower(s)
		if set[k] {
			intersection++
		} else {
			set[k] = true
			union++
		}
	}
	if union == 0 {
		return 0
	}
	return float64(intersection) / float64(union)
}

func clamp(v float64) float64 {
	if v < 0 {
		return 0
	}
	if v > 1 {
		return 1
	}
	return v
}
func abs(v int) int {
	if v < 0 {
		return -v
	}
	return v
}
