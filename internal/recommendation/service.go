package recommendation

import (
	"context"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"time"

	"github.com/TechXTT/reelay/internal/config"
	"github.com/TechXTT/reelay/internal/metadata"
	"github.com/TechXTT/reelay/internal/model"
	"github.com/TechXTT/reelay/internal/store"
)

type Service struct {
	store    *store.Store
	provider metadata.RecommendationProvider
	cfg      config.Recommendations
	now      func() time.Time
	log      *slog.Logger
}

func NewService(st *store.Store, provider metadata.RecommendationProvider, cfg config.Recommendations, now func() time.Time, log *slog.Logger) *Service {
	if now == nil {
		now = time.Now
	}
	if log == nil {
		log = slog.Default()
	}
	return &Service{store: st, provider: provider, cfg: cfg, now: now, log: log}
}

func (s *Service) GenerateAll(ctx context.Context) error {
	if !s.cfg.Enabled {
		return nil
	}
	users, err := s.store.Recommendations().Users(ctx)
	if err != nil {
		return err
	}
	var first error
	for _, user := range users {
		if !user.Enabled {
			continue
		}
		for _, mediaType := range []string{"movie", "series"} {
			if err := s.Generate(ctx, user.ServerID, user.UserID, mediaType); err != nil {
				s.log.Error("recommendation generation failed", "server", user.ServerID, "user", user.UserID, "type", mediaType, "error", err)
				if first == nil {
					first = err
				}
			}
		}
	}
	_ = s.store.Recommendations().Prune(ctx, s.now())
	return first
}

func (s *Service) Generate(ctx context.Context, serverID, userID, mediaType string) error {
	seeds, err := s.store.Recommendations().PositiveSeeds(ctx, serverID, userID, mediaType, s.cfg.SeedLimit)
	if err != nil {
		return err
	}
	owned, err := s.store.Recommendations().OwnedTMDBIDs(ctx, serverID, mediaType)
	if err != nil {
		return err
	}
	type aggregate struct {
		item     metadata.DiscoveryItem
		provider float64
		matches  int
	}
	pool := map[int]*aggregate{}
	tasteSeeds := append([]model.JellyfinItem(nil), seeds...)
	add := func(values []metadata.DiscoveryItem) {
		for rank, item := range values {
			if item.TMDBID <= 0 || owned[item.TMDBID] {
				continue
			}
			score := 1 - float64(rank)/float64(max(1, len(values)))
			a := pool[item.TMDBID]
			if a == nil {
				a = &aggregate{item: item}
				pool[item.TMDBID] = a
			}
			if score > a.provider {
				a.provider = score
			}
			a.matches++
		}
	}
	if len(seeds) == 0 {
		values, err := s.provider.Discover(ctx, mediaType)
		if err != nil {
			return err
		}
		add(values)
	} else {
		for i, seed := range seeds {
			if detail, detailErr := s.provider.DiscoveryDetails(ctx, mediaType, seed.TMDBID); detailErr == nil {
				tasteSeeds[i].Genres = detail.Genres
				tasteSeeds[i].Keywords = detail.Keywords
				tasteSeeds[i].People = detail.People
				tasteSeeds[i].Language = detail.Language
				tasteSeeds[i].Country = detail.Country
				tasteSeeds[i].RuntimeMinutes = detail.RuntimeMinutes
			}
			values, err := s.provider.Recommendations(ctx, mediaType, seed.TMDBID)
			if err != nil {
				return fmt.Errorf("recommendations for %s: %w", seed.Title, err)
			}
			add(values)
			if similar, err := s.provider.Similar(ctx, mediaType, seed.TMDBID); err == nil {
				add(similar)
			}
			if len(pool) >= s.cfg.CandidateLimit {
				break
			}
		}
	}
	profile := tasteProfile(tasteSeeds)
	candidates := make([]Candidate, 0, min(len(pool), s.cfg.CandidateLimit))
	for _, a := range pool {
		candidates = append(candidates, Candidate{Item: toModel(a.item), ProviderScore: a.provider, SeedMatches: a.matches, VoteAverage: a.item.VoteAverage, VoteCount: a.item.VoteCount})
	}
	sort.Slice(candidates, func(i, j int) bool { return candidates[i].ProviderScore > candidates[j].ProviderScore })
	if len(candidates) > s.cfg.CandidateLimit {
		candidates = candidates[:s.cfg.CandidateLimit]
	}
	enrichLimit := min(len(candidates), max(s.cfg.ResultLimit*2, 40))
	for i := 0; i < enrichLimit; i++ {
		detail, err := s.provider.DiscoveryDetails(ctx, mediaType, candidates[i].Item.TMDBID)
		if err != nil {
			continue
		}
		candidates[i].Item, candidates[i].VoteAverage, candidates[i].VoteCount = toModel(detail), detail.VoteAverage, detail.VoteCount
	}
	weights := Weights{Provider: float64(s.cfg.ProviderWeight), Affinity: float64(s.cfg.AffinityWeight), People: float64(s.cfg.PeopleWeight), MultiSeed: float64(s.cfg.MultiSeedWeight), Rating: float64(s.cfg.RatingWeight), Preference: float64(s.cfg.PreferenceWeight), Novelty: float64(s.cfg.NoveltyWeight)}
	values := Rank(candidates, profile, weights, s.cfg.ResultLimit)
	now := s.now().UTC()
	for i := range values {
		values[i].ServerID = serverID
		values[i].UserID = userID
		values[i].MediaType = mediaType
		values[i].Status = "active"
		values[i].GeneratedAt = now
		values[i].ExpiresAt = now.Add(s.cfg.Expiry.Duration)
	}
	if err := s.store.Recommendations().Replace(ctx, serverID, userID, mediaType, values); err != nil {
		return err
	}
	s.log.Info("recommendations generated", "server", serverID, "user", userID, "type", mediaType, "seeds", len(seeds), "candidates", len(candidates), "results", len(values))
	return nil
}

func tasteProfile(seeds []model.JellyfinItem) Profile {
	p := Profile{Genres: map[string]float64{}, Keywords: map[string]float64{}, People: map[string]float64{}, Languages: map[string]float64{}, Countries: map[string]float64{}}
	for _, seed := range seeds {
		for _, v := range seed.Genres {
			p.Genres[strings.ToLower(v)] += 3
		}
		for _, v := range seed.Keywords {
			p.Keywords[strings.ToLower(v)] += 3
		}
		for _, v := range seed.People {
			p.People[strings.ToLower(v)] += 3
		}
		if seed.Language != "" {
			p.Languages[strings.ToLower(seed.Language)]++
		}
		if seed.Country != "" {
			p.Countries[strings.ToLower(seed.Country)]++
		}
		if seed.Year > 0 {
			p.Years = append(p.Years, seed.Year)
		}
		if seed.RuntimeMinutes > 0 {
			p.Runtimes = append(p.Runtimes, seed.RuntimeMinutes)
		}
	}
	return p
}

func toModel(item metadata.DiscoveryItem) model.Recommendation {
	return model.Recommendation{TMDBID: item.TMDBID, Title: item.Title, Year: item.Year, Overview: item.Overview, PosterURL: item.PosterURL, Genres: item.Genres, Keywords: item.Keywords, People: item.People, Language: item.Language, Country: item.Country, RuntimeMinutes: item.RuntimeMinutes}
}
