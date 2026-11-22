package recommendation

import (
	"math"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/TechXTT/reelay/internal/model"
)

// Diversity bounds how much of each block of Window ranked titles one seed
// title (SeedCap) or one genre (GenreCap) may fill. A zero Window turns the
// bounds off.
type Diversity struct {
	Window, SeedCap, GenreCap int
}

// ranked is a scored title waiting for its place in the list. Score holds its
// relevance until arrange places it.
type ranked struct {
	model.Recommendation
	novelty  float64
	genres   map[string]bool
	interest string
}

func newRanked(value model.Recommendation) ranked {
	return ranked{Recommendation: value, novelty: 1, genres: genreSet(value.Genres), interest: interest(value)}
}

// interest names the seed title that most strongly produced a title, or ""
// when it came from a catalog listing or predates provenance. Catalog titles
// are not capped: without seeds there is no single interest to dominate.
func interest(value model.Recommendation) string {
	if len(value.Sources) == 0 || value.Sources[0].Kind != "seed" {
		return ""
	}
	return strconv.Itoa(value.Sources[0].TMDBID)
}

// arrange orders up to limit titles greedily. Each step takes the title with
// the highest relevance plus novelty bonus, where the bonus rewards genres
// unlike those already placed. While a block of d.Window positions fills, a
// title that would exceed d.SeedCap for its seed or d.GenreCap for one of its
// genres waits unless no other title remains. Lists are read back ordered by
// score, so a title placed after one with a lower value gets a score just
// below it, and the difference is recorded as a negative "diversity"
// component.
func (d Diversity) arrange(scored []ranked, novelty float64, limit int) []model.Recommendation {
	var count = min(limit, len(scored))
	var selected = make([]model.Recommendation, 0, count)
	var taken = make([]bool, len(scored))
	var seeds = map[string]int{}
	var genres = map[string]int{}

	sort.SliceStable(scored, func(i, j int) bool {
		if scored[i].Score != scored[j].Score {
			return scored[i].Score > scored[j].Score
		}
		return scored[i].TMDBID < scored[j].TMDBID
	})
	// Picked entries are flagged instead of removed so ties keep resolving to
	// the earliest (highest relevance, lowest TMDB id) candidate.
	for len(selected) < count {
		var best, bestValue, bestFits = -1, math.Inf(-1), false

		if d.Window > 0 && len(selected)%d.Window == 0 {
			clear(seeds)
			clear(genres)
		}
		for i := range scored {
			if taken[i] {
				continue
			}
			value := scored[i].Score + scored[i].novelty*novelty
			fits := d.fits(scored[i], seeds, genres)
			if best < 0 || (fits && !bestFits) || (fits == bestFits && value > bestValue) {
				best, bestValue, bestFits = i, value, fits
			}
		}
		pick := &scored[best]
		value := min(100, max(0, math.Round(bestValue*10)/10))
		score := value
		if len(selected) > 0 && score > selected[len(selected)-1].Score {
			score = max(0, math.Round((selected[len(selected)-1].Score-.1)*10)/10)
			pick.Components["diversity"] = math.Round((score-value)*10) / 10
		}
		pick.Components["novelty"] = math.Round((bestValue-pick.Score)*10) / 10
		pick.Score = score
		selected = append(selected, pick.Recommendation)
		taken[best] = true
		if len(selected) == count {
			break
		}
		if pick.interest != "" {
			seeds[pick.interest]++
		}
		for genre := range pick.genres {
			genres[genre]++
		}
		for i := range scored {
			if !taken[i] {
				scored[i].novelty = math.Min(scored[i].novelty, 1-jaccard(scored[i].genres, pick.genres))
			}
		}
	}
	return selected
}

// fits reports whether placing candidate keeps the current block within the
// seed and genre caps.
func (d Diversity) fits(candidate ranked, seeds, genres map[string]int) bool {
	if d.Window == 0 {
		return true
	}
	if candidate.interest != "" && seeds[candidate.interest] >= d.SeedCap {
		return false
	}
	for genre := range candidate.genres {
		if genres[genre] >= d.GenreCap {
			return false
		}
	}
	return true
}

// Concentration measures the top ten of an ordered, non-empty list: the
// share held by its most common genre and by its most common source, and how
// many distinct sources (interests) and genres it draws on.
func Concentration(mediaType string, generatedAt time.Time, values []model.Recommendation) model.RecommendationConcentration {
	var top = values[:min(10, len(values))]
	var genreCounts = map[string]int{}
	var genreLabels = map[string]string{}
	var sourceCounts = map[string]int{}
	var sourceLabels = map[string]string{}
	var result = model.RecommendationConcentration{MediaType: mediaType, GeneratedAt: generatedAt, SampleSize: len(top)}

	for _, value := range top {
		seen := map[string]bool{}
		for _, genre := range value.Genres {
			key := strings.ToLower(genre)
			if seen[key] {
				continue
			}
			seen[key] = true
			genreCounts[key]++
			genreLabels[key] = genre
		}
		key, label := "unknown", "Unknown source"
		if len(value.Sources) > 0 {
			source := value.Sources[0]
			key, label = source.Kind+":"+source.Listing, "TMDB "+strings.ReplaceAll(source.Listing, "_", " ")
			if source.Kind == "seed" {
				key, label = "seed:"+strconv.Itoa(source.TMDBID), source.Title
			}
		}
		sourceCounts[key]++
		sourceLabels[key] = label
	}
	genre, genreCount := dominant(genreCounts, genreLabels)
	seed, seedCount := dominant(sourceCounts, sourceLabels)
	result.DominantGenre, result.DominantGenreShare = genre, float64(genreCount)/float64(len(top))
	result.DominantSeed, result.DominantSeedShare = seed, float64(seedCount)/float64(len(top))
	result.DistinctInterests, result.DistinctGenres = len(sourceCounts), len(genreCounts)
	return result
}

// dominant returns the label and count of the most common key, breaking ties
// by label so the result is deterministic.
func dominant(counts map[string]int, labels map[string]string) (string, int) {
	var label string
	var count int

	for key, n := range counts {
		if n > count || (n == count && labels[key] < label) {
			label, count = labels[key], n
		}
	}
	return label, count
}
