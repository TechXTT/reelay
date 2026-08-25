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
	excluded, err := s.store.Recommendations().ExcludedTMDBIDs(ctx, serverID, userID, mediaType)
	if err != nil {
		return err
	}
	ratings, err := s.store.Recommendations().Ratings(ctx, serverID, userID, mediaType)
	if err != nil {
		return err
	}
	type aggregate struct {
		item     metadata.DiscoveryItem
		provider float64
		matches  int
	}
	pool := map[int]*aggregate{}
	signals := make([]tasteSignal, 0, len(seeds)+len(ratings))
	searchSeeds := make([]model.JellyfinItem, 0, len(seeds)+len(ratings))
	for _, seed := range seeds {
		if detail, detailErr := s.provider.DiscoveryDetails(ctx, mediaType, seed.TMDBID); detailErr == nil {
			seed = tasteItem(detail)
		}
		signals = append(signals, tasteSignal{item: seed, weight: 1})
		searchSeeds = append(searchSeeds, seed)
	}
	rated := map[int]bool{}
	for _, rating := range ratings {
		if rated[rating.TMDBID] {
			continue
		}
		rated[rating.TMDBID] = true
		detail, detailErr := s.provider.DiscoveryDetails(ctx, mediaType, rating.TMDBID)
		if detailErr != nil {
			continue
		}
		weight := float64(rating.Rating-3) / 2
		if weight != 0 {
			signals = append(signals, tasteSignal{item: tasteItem(detail), weight: weight})
		}
		if rating.Rating >= 4 {
			searchSeeds = append(searchSeeds, tasteItem(detail))
		}
	}
	add := func(values []metadata.DiscoveryItem) {
		for rank, item := range values {
			if item.TMDBID <= 0 || excluded[item.TMDBID] {
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
	if len(searchSeeds) == 0 {
		values, err := s.provider.Discover(ctx, mediaType)
		if err != nil {
			return err
		}
		add(values)
	} else {
		queried := map[int]bool{}
		for _, seed := range searchSeeds {
			if queried[seed.TMDBID] {
				continue
			}
			queried[seed.TMDBID] = true
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
	profile := tasteProfile(signals)
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
	s.log.Info("recommendations generated", "server", serverID, "user", userID, "type", mediaType, "seeds", len(searchSeeds), "ratings", len(ratings), "candidates", len(candidates), "results", len(values))
	return nil
}

type tasteSignal struct {
	item   model.JellyfinItem
	weight float64
}

func tasteProfile(signals []tasteSignal) Profile {
	p := Profile{Genres: map[string]float64{}, Keywords: map[string]float64{}, People: map[string]float64{}, Languages: map[string]float64{}, Countries: map[string]float64{}}
	for _, signal := range signals {
		seed, weight := signal.item, signal.weight
		for _, v := range seed.Genres {
			p.Genres[strings.ToLower(v)] += 3 * weight
		}
		for _, v := range seed.Keywords {
			p.Keywords[strings.ToLower(v)] += 3 * weight
		}
		for _, v := range seed.People {
			p.People[strings.ToLower(v)] += 3 * weight
		}
		if seed.Language != "" {
			p.Languages[strings.ToLower(seed.Language)] += weight
		}
		if seed.Country != "" {
			p.Countries[strings.ToLower(seed.Country)] += weight
		}
		if weight > 0 && seed.Year > 0 {
			p.Years = append(p.Years, seed.Year)
		}
		if weight > 0 && seed.RuntimeMinutes > 0 {
			p.Runtimes = append(p.Runtimes, seed.RuntimeMinutes)
		}
	}
	return p
}

func tasteItem(item metadata.DiscoveryItem) model.JellyfinItem {
	return model.JellyfinItem{MediaType: item.MediaType, TMDBID: item.TMDBID, Title: item.Title, Year: item.Year, Genres: item.Genres, Keywords: item.Keywords, People: item.People, Language: item.Language, Country: item.Country, RuntimeMinutes: item.RuntimeMinutes, Present: true}
}

func toModel(item metadata.DiscoveryItem) model.Recommendation {
	return model.Recommendation{TMDBID: item.TMDBID, Title: item.Title, Year: item.Year, Overview: item.Overview, PosterURL: item.PosterURL, Genres: item.Genres, Keywords: item.Keywords, People: item.People, Language: item.Language, Country: item.Country, RuntimeMinutes: item.RuntimeMinutes}
}
