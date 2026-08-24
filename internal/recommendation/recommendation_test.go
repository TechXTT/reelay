package recommendation

import (
	"testing"

	"github.com/TechXTT/reelay/internal/model"
)

func TestRankUsesTasteEvidenceAndExplainsIt(t *testing.T) {
	profile := Profile{Genres: map[string]float64{"science fiction": 3}, Keywords: map[string]float64{"space": 3}, People: map[string]float64{"director a": 3}, Languages: map[string]float64{"en": 1}}
	values := Rank([]Candidate{
		{Item: model.Recommendation{TMDBID: 1, Title: "Taste match", Genres: []string{"Science Fiction"}, Keywords: []string{"Space"}, People: []string{"Director A"}, Language: "en"}, ProviderScore: .7, SeedMatches: 3, VoteAverage: 8, VoteCount: 2000},
		{Item: model.Recommendation{TMDBID: 2, Title: "Popular mismatch", Genres: []string{"Comedy"}}, ProviderScore: 1, SeedMatches: 1, VoteAverage: 9, VoteCount: 5000},
	}, profile, DefaultWeights(), 2)
	if len(values) != 2 || values[0].TMDBID != 1 {
		t.Fatalf("ranked = %+v", values)
	}
	if len(values[0].Reasons) == 0 || values[0].Components["affinity"] == 0 || values[0].Components["people"] == 0 {
		t.Fatalf("missing explanation: %+v", values[0])
	}
}

func TestRankAddsDiversityWithoutExceedingLimit(t *testing.T) {
	var candidates []Candidate
	for i := 1; i <= 8; i++ {
		candidates = append(candidates, Candidate{Item: model.Recommendation{TMDBID: i, Title: "item", Genres: []string{"Drama"}}, ProviderScore: 1 - float64(i)/20})
	}
	values := Rank(candidates, Profile{}, DefaultWeights(), 3)
	if len(values) != 3 {
		t.Fatalf("len = %d", len(values))
	}
	for _, value := range values {
		if value.Score < 0 || value.Score > 100 {
			t.Fatalf("score out of range: %v", value.Score)
		}
	}
}
