package recommendation

import (
	"context"
	"fmt"
	"log/slog"
	"slices"
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
	repo := s.store.Recommendations()
	preferences, err := repo.Preferences(ctx, serverID, userID)
	if err != nil {
		return err
	}
	seeds, err := repo.PositiveSeeds(ctx, serverID, userID, mediaType, s.cfg.SeedLimit)
	if err != nil {
		return err
	}
	excluded, err := repo.ExcludedTMDBIDs(ctx, serverID, userID, mediaType)
	if err != nil {
		return err
	}
	ratings, err := repo.Ratings(ctx, serverID, userID, mediaType)
	if err != nil {
		return err
	}
	run := &generation{
		service:   s,
		ctx:       ctx,
		mediaType: mediaType,
		excluded:  excluded,
		filter:    newPreferenceFilter(preferences),
		details:   map[int]detailResult{},
	}
	signals, searchSeeds := run.gatherSignals(seeds, ratings)
	pool, err := run.fetchCandidates(searchSeeds)
	if err != nil {
		return err
	}
	candidates := run.enrich(pool)
	values := Rank(candidates, tasteProfile(signals), s.weights(preferences), s.cfg.ResultLimit)
	now := s.now().UTC()
	for i := range values {
		values[i].ServerID = serverID
		values[i].UserID = userID
		values[i].MediaType = mediaType
		values[i].Status = "active"
		values[i].GeneratedAt = now
		values[i].ExpiresAt = now.Add(s.cfg.Expiry.Duration)
	}
	if err := repo.Replace(ctx, serverID, userID, mediaType, values); err != nil {
		return err
	}
	s.log.Info("recommendations generated", "server", serverID, "user", userID, "type", mediaType, "seeds", len(searchSeeds), "ratings", len(ratings), "candidates", len(candidates), "results", len(values))
	return nil
}

func (s *Service) weights(preferences store.RecommendationPreferences) Weights {
	weights := Weights{Provider: float64(s.cfg.ProviderWeight), Affinity: float64(s.cfg.AffinityWeight), People: float64(s.cfg.PeopleWeight), MultiSeed: float64(s.cfg.MultiSeedWeight), Rating: float64(s.cfg.RatingWeight), Preference: float64(s.cfg.PreferenceWeight), Novelty: float64(s.cfg.NoveltyWeight)}
	weights.Novelty *= float64(preferences.Diversity) / 100
	switch preferences.Familiarity {
	case "familiar":
		weights.Affinity += weights.Novelty
		weights.Novelty = 0
	case "explore":
		weights.Novelty += weights.Affinity / 2
		weights.Affinity /= 2
	}
	return weights
}

// preferenceFilter holds a user's language and excluded-genre preferences as
// sets so each candidate is checked in constant time.
type preferenceFilter struct {
	languages map[string]bool
	genres    map[string]bool
}

func newPreferenceFilter(preferences store.RecommendationPreferences) preferenceFilter {
	filter := preferenceFilter{languages: make(map[string]bool, len(preferences.Languages)), genres: make(map[string]bool, len(preferences.ExcludedGenres))}
	for _, language := range preferences.Languages {
		filter.languages[language] = true
	}
	for _, genre := range preferences.ExcludedGenres {
		filter.genres[strings.ToLower(genre)] = true
	}
	return filter
}

// allows reports whether a title passes the preferences. Provider listings may
// omit the language, so an empty language is only rejected once details are
// known (languageKnown).
func (f preferenceFilter) allows(language string, genres []string, languageKnown bool) bool {
	if len(f.languages) > 0 && (languageKnown || language != "") && !f.languages[language] {
		return false
	}
	return !f.excludesGenre(genres)
}

func (f preferenceFilter) excludesGenre(genres []string) bool {
	if len(f.genres) == 0 {
		return false
	}
	return slices.ContainsFunc(genres, func(genre string) bool { return f.genres[strings.ToLower(genre)] })
}

type detailResult struct {
	item metadata.DiscoveryItem
	err  error
}

type aggregate struct {
	item     metadata.DiscoveryItem
	provider float64
	matches  int
}

// generation is the state of one Generate call. Provider details are memoized
// so an id shared by seeds, ratings and candidates is fetched once.
type generation struct {
	service   *Service
	ctx       context.Context
	mediaType string
	excluded  map[int]bool
	filter    preferenceFilter
	details   map[int]detailResult
}

func (g *generation) detail(tmdbID int) (metadata.DiscoveryItem, error) {
	if cached, ok := g.details[tmdbID]; ok {
		return cached.item, cached.err
	}
	item, err := g.service.provider.DiscoveryDetails(g.ctx, g.mediaType, tmdbID)
	g.details[tmdbID] = detailResult{item: item, err: err}
	return item, err
}

// gatherSignals turns positive seeds and signed ratings into taste signals and
// the list of titles to query the provider with.
func (g *generation) gatherSignals(seeds []model.JellyfinItem, ratings []model.RecommendationRating) ([]tasteSignal, []model.JellyfinItem) {
	signals := make([]tasteSignal, 0, len(seeds)+len(ratings))
	searchSeeds := make([]model.JellyfinItem, 0, len(seeds)+len(ratings))
	for _, seed := range seeds {
		if detail, err := g.detail(seed.TMDBID); err == nil {
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
		detail, err := g.detail(rating.TMDBID)
		if err != nil {
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
	return signals, searchSeeds
}

// fetchCandidates queries the provider once per distinct seed (or falls back to
// discovery without seeds) and aggregates the listings into a candidate pool.
func (g *generation) fetchCandidates(searchSeeds []model.JellyfinItem) (map[int]*aggregate, error) {
	provider := g.service.provider
	pool := map[int]*aggregate{}
	if len(searchSeeds) == 0 {
		values, err := provider.Discover(g.ctx, g.mediaType)
		if err != nil {
			return nil, err
		}
		g.addListing(pool, values, map[int]bool{})
		return pool, nil
	}
	queried := map[int]bool{}
	for _, seed := range searchSeeds {
		if queried[seed.TMDBID] {
			continue
		}
		queried[seed.TMDBID] = true
		values, err := provider.Recommendations(g.ctx, g.mediaType, seed.TMDBID)
		if err != nil {
			return nil, fmt.Errorf("recommendations for %s: %w", seed.Title, err)
		}
		matched := map[int]bool{}
		g.addListing(pool, values, matched)
		if similar, err := provider.Similar(g.ctx, g.mediaType, seed.TMDBID); err == nil {
			g.addListing(pool, similar, matched)
		}
		if len(pool) >= g.service.cfg.CandidateLimit {
			break
		}
	}
	return pool, nil
}

// addListing scores one provider listing by position and merges the allowed
// items into the pool. matched tracks which ids this seed already counted.
func (g *generation) addListing(pool map[int]*aggregate, values []metadata.DiscoveryItem, matched map[int]bool) {
	for rank, item := range values {
		if item.TMDBID <= 0 || g.excluded[item.TMDBID] || !g.filter.allows(item.Language, item.Genres, false) {
			continue
		}
		score := 1 - float64(rank)/float64(max(1, len(values)))
		entry := pool[item.TMDBID]
		if entry == nil {
			entry = &aggregate{item: item}
			pool[item.TMDBID] = entry
		}
		if score > entry.provider {
			entry.provider = score
		}
		if !matched[item.TMDBID] {
			entry.matches++
			matched[item.TMDBID] = true
		}
	}
}

// enrich orders the pool by provider score, keeps CandidateLimit entries,
// fetches details for the best of them and re-applies the preferences with the
// now-known language and genres.
func (g *generation) enrich(pool map[int]*aggregate) []Candidate {
	cfg := g.service.cfg
	candidates := make([]Candidate, 0, len(pool))
	for _, entry := range pool {
		candidates = append(candidates, Candidate{Item: toModel(entry.item), ProviderScore: entry.provider, SeedMatches: entry.matches, VoteAverage: entry.item.VoteAverage, VoteCount: entry.item.VoteCount})
	}
	sort.Slice(candidates, func(i, j int) bool {
		if candidates[i].ProviderScore != candidates[j].ProviderScore {
			return candidates[i].ProviderScore > candidates[j].ProviderScore
		}
		return candidates[i].Item.TMDBID < candidates[j].Item.TMDBID
	})
	candidates = candidates[:min(len(candidates), cfg.CandidateLimit)]
	enrichLimit := min(len(candidates), max(cfg.ResultLimit*2, 40))
	for i := range candidates[:enrichLimit] {
		detail, err := g.detail(candidates[i].Item.TMDBID)
		if err != nil {
			continue
		}
		candidates[i].Item, candidates[i].VoteAverage, candidates[i].VoteCount = toModel(detail), detail.VoteAverage, detail.VoteCount
	}
	return slices.DeleteFunc(candidates, func(candidate Candidate) bool {
		return !g.filter.allows(candidate.Item.Language, candidate.Item.Genres, true)
	})
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
	return model.Recommendation{TMDBID: item.TMDBID, Title: item.Title, Year: item.Year, Overview: item.Overview, PosterURL: item.PosterURL, Genres: item.Genres, Keywords: item.Keywords, People: item.People, Language: item.Language, Country: item.Country, RuntimeMinutes: item.RuntimeMinutes, VoteAverage: item.VoteAverage, VoteCount: item.VoteCount}
}
