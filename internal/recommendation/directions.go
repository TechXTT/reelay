package recommendation

import (
	"context"
	"errors"
	"strings"

	"github.com/TechXTT/reelay/internal/metadata"
	"github.com/TechXTT/reelay/internal/model"
)

type directionProvider interface {
	DiscoverOutsideGenre(context.Context, string, string, int) ([]metadata.DiscoveryItem, error)
}

// ChangeDirection looks beyond the usual seed titles for this browsing session.
func (s *Service) ChangeDirection(ctx context.Context, serverID, userID, mediaType, genre string, seen []int) ([]model.Recommendation, error) {
	const limit = 6
	provider, ok := s.provider.(directionProvider)
	if !ok {
		return nil, errors.New("the provider does not support discovery by genre")
	}
	repo := s.store.Recommendations()
	prefs, err := repo.Preferences(ctx, serverID, userID)
	if err != nil {
		return nil, err
	}
	excluded, err := repo.ExcludedTMDBIDs(ctx, serverID, userID, mediaType)
	if err != nil {
		return nil, err
	}
	for _, id := range seen {
		excluded[id] = true
	}
	filter := newPreferenceFilter(prefs)
	if genre != "" {
		filter.genres[strings.ToLower(genre)] = true
	}
	var candidates []Candidate
	for page := 1; page <= 3 && len(candidates) < limit; page++ {
		listing, err := provider.DiscoverOutsideGenre(ctx, mediaType, genre, page)
		if err != nil {
			return nil, err
		}
		if len(listing) == 0 {
			break
		}
		for rank, item := range listing {
			if item.TMDBID <= 0 || excluded[item.TMDBID] || len(item.Genres) == 0 || !filter.allows(item.Language, item.Genres, true) {
				continue
			}
			excluded[item.TMDBID] = true
			position := 1 - float64(rank)/float64(len(listing))
			candidates = append(candidates, Candidate{
				Item: toModel(item), VoteAverage: item.VoteAverage, VoteCount: item.VoteCount,
				ProviderScore: position / float64(page),
			})
		}
	}
	picks := Rank(candidates, Profile{}, DefaultWeights(), limit)
	now := s.now().UTC()
	for i := range picks {
		picks[i].ServerID, picks[i].UserID, picks[i].MediaType = serverID, userID, mediaType
		picks[i].Status, picks[i].GeneratedAt, picks[i].ExpiresAt = "active", now, now.Add(s.cfg.Expiry.Duration)
		picks[i].Reasons = []string{"From TMDB discovery, outside your usual recommendations"}
		picks[i].Sources = []model.RecommendationSource{{Kind: "catalog", Listing: "discover_outside_genre"}}
	}
	return repo.OfferDirection(ctx, serverID, userID, mediaType, picks)
}
