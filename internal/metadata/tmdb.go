package metadata

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

type TMDBOptions struct {
	BaseURL    string
	APIKey     string
	HTTPClient *http.Client
	Timeout    time.Duration
	Cache      Cache
	CacheTTL   time.Duration
	Now        func() time.Time
}

type TMDB struct {
	http  httpJSONClient
	key   string
	cache Cache
	ttl   time.Duration
	now   func() time.Time
}

func NewTMDB(opt TMDBOptions) (*TMDB, error) {
	h, err := newHTTPJSONClient(opt.BaseURL, opt.HTTPClient, opt.Timeout)
	if err != nil {
		return nil, err
	}
	if opt.CacheTTL <= 0 {
		opt.CacheTTL = 24 * time.Hour
	}
	if opt.Now == nil {
		opt.Now = time.Now
	}
	return &TMDB{http: h, key: opt.APIKey, cache: opt.Cache, ttl: opt.CacheTTL, now: opt.Now}, nil
}

type tmdbSearchResponse struct {
	Results []tmdbMovie `json:"results"`
}

type tmdbMovie struct {
	ID            int    `json:"id"`
	Title         string `json:"title"`
	OriginalTitle string `json:"original_title"`
	ReleaseDate   string `json:"release_date"`
	Overview      string `json:"overview"`
	PosterPath    string `json:"poster_path"`
	Runtime       int    `json:"runtime"`
	IMDBID        string `json:"imdb_id"`
	ExternalIDs   struct {
		IMDBID string `json:"imdb_id"`
		TVDBID int    `json:"tvdb_id"`
	} `json:"external_ids"`
	Name             string  `json:"name"`
	OriginalName     string  `json:"original_name"`
	FirstAirDate     string  `json:"first_air_date"`
	OriginalLanguage string  `json:"original_language"`
	VoteAverage      float64 `json:"vote_average"`
	VoteCount        int     `json:"vote_count"`
	GenreIDs         []int   `json:"genre_ids"`
	Genres           []struct {
		ID   int    `json:"id"`
		Name string `json:"name"`
	} `json:"genres"`
	OriginCountry       []string `json:"origin_country"`
	ProductionCountries []struct {
		ISO string `json:"iso_3166_1"`
	} `json:"production_countries"`
	EpisodeRunTime []int `json:"episode_run_time"`
	Credits        struct {
		Cast []struct {
			Name string `json:"name"`
		} `json:"cast"`
		Crew []struct {
			Name string `json:"name"`
			Job  string `json:"job"`
		} `json:"crew"`
	} `json:"credits"`
	Keywords struct {
		Keywords []struct {
			Name string `json:"name"`
		} `json:"keywords"`
		Results []struct {
			Name string `json:"name"`
		} `json:"results"`
	} `json:"keywords"`
}

func (t *TMDB) SearchMovies(ctx context.Context, title string, year int) ([]Movie, error) {
	title = strings.TrimSpace(title)
	if title == "" {
		return nil, errors.New("tmdb: empty movie title")
	}
	if strings.TrimSpace(t.key) == "" {
		return []Movie{{Title: title, OriginalTitle: title, Year: year}}, nil
	}
	cacheKey := fmt.Sprintf("search:%s:%d", strings.ToLower(title), year)
	var payload tmdbSearchResponse
	_, hit, err := cacheLoad(ctx, t.cache, "tmdb", cacheKey, t.now(), &payload)
	if err != nil {
		return nil, err
	}
	if !hit {
		q := url.Values{"api_key": {t.key}, "query": {title}, "include_adult": {"false"}}
		if year > 0 {
			q.Set("primary_release_year", strconv.Itoa(year))
		}
		raw, err := t.http.get(ctx, "/search/movie", q, &payload)
		if err != nil {
			return nil, err
		}
		if err := cacheStore(ctx, t.cache, "tmdb", cacheKey, raw, t.now(), t.ttl); err != nil {
			return nil, err
		}
	}
	out := make([]Movie, 0, len(payload.Results))
	for _, m := range payload.Results {
		out = append(out, convertTMDBMovie(m))
	}
	return out, nil
}

func (t *TMDB) MovieDetails(ctx context.Context, id int) (Movie, error) {
	if id <= 0 || strings.TrimSpace(t.key) == "" {
		return Movie{}, errors.New("tmdb: movie details require a positive id and api key")
	}
	cacheKey := "movie:" + strconv.Itoa(id)
	var payload tmdbMovie
	_, hit, err := cacheLoad(ctx, t.cache, "tmdb", cacheKey, t.now(), &payload)
	if err != nil {
		return Movie{}, err
	}
	if !hit {
		q := url.Values{"api_key": {t.key}, "append_to_response": {"external_ids"}}
		raw, err := t.http.get(ctx, "/movie/"+strconv.Itoa(id), q, &payload)
		if err != nil {
			return Movie{}, err
		}
		if err := cacheStore(ctx, t.cache, "tmdb", cacheKey, raw, t.now(), t.ttl); err != nil {
			return Movie{}, err
		}
	}
	return convertTMDBMovie(payload), nil
}

func convertTMDBMovie(m tmdbMovie) Movie {
	imdb := m.IMDBID
	if imdb == "" {
		imdb = m.ExternalIDs.IMDBID
	}
	return Movie{
		TMDBID: m.ID, Title: m.Title, OriginalTitle: m.OriginalTitle,
		Year: yearFromDate(m.ReleaseDate), RuntimeMinutes: m.Runtime,
		IMDBID: imdb, Overview: m.Overview, PosterURL: tmdbPoster(m.PosterPath),
	}
}

func tmdbPoster(path string) string {
	if path == "" {
		return ""
	}
	return "https://image.tmdb.org/t/p/w342" + path
}

func yearFromDate(s string) int {
	if len(s) < 4 {
		return 0
	}
	y, _ := strconv.Atoi(s[:4])
	return y
}

func cacheLoad(ctx context.Context, cache Cache, provider, key string, now time.Time, dst any) ([]byte, bool, error) {
	if cache == nil {
		return nil, false, nil
	}
	raw, hit, err := cache.Get(ctx, provider, key, now)
	if err != nil || !hit {
		return raw, hit, err
	}
	if err := json.Unmarshal(raw, dst); err != nil {
		return nil, false, fmt.Errorf("metadata: decode cached %s/%s: %w", provider, key, err)
	}
	return raw, true, nil
}

func cacheStore(ctx context.Context, cache Cache, provider, key string, raw []byte, now time.Time, ttl time.Duration) error {
	if cache == nil {
		return nil
	}
	return cache.Put(ctx, provider, key, raw, now, ttl)
}

var _ MovieProvider = (*TMDB)(nil)
var _ RecommendationProvider = (*TMDB)(nil)

func (t *TMDB) Recommendations(ctx context.Context, mediaType string, id int) ([]DiscoveryItem, error) {
	return t.discoveryList(ctx, mediaType, id, "recommendations")
}

func (t *TMDB) Similar(ctx context.Context, mediaType string, id int) ([]DiscoveryItem, error) {
	return t.discoveryList(ctx, mediaType, id, "similar")
}

func (t *TMDB) Discover(ctx context.Context, mediaType string) ([]DiscoveryItem, error) {
	kind, err := tmdbKind(mediaType)
	if err != nil {
		return nil, err
	}
	var payload tmdbSearchResponse
	q := url.Values{"api_key": {t.key}, "include_adult": {"false"}, "sort_by": {"vote_average.desc"}, "vote_count.gte": {"250"}}
	if _, err := t.cachedGET(ctx, "discover:"+kind, "/discover/"+kind, q, &payload); err != nil {
		return nil, err
	}
	return t.convertDiscovery(ctx, mediaType, payload.Results)
}

func (t *TMDB) DiscoveryDetails(ctx context.Context, mediaType string, id int) (DiscoveryItem, error) {
	kind, err := tmdbKind(mediaType)
	if err != nil {
		return DiscoveryItem{}, err
	}
	if id <= 0 || strings.TrimSpace(t.key) == "" {
		return DiscoveryItem{}, errors.New("tmdb: discovery details require a positive id and api key")
	}
	var payload tmdbMovie
	q := url.Values{"api_key": {t.key}, "append_to_response": {"external_ids,credits,keywords"}}
	if _, err := t.cachedGET(ctx, fmt.Sprintf("details:%s:%d", kind, id), "/"+kind+"/"+strconv.Itoa(id), q, &payload); err != nil {
		return DiscoveryItem{}, err
	}
	return convertDiscoveryItem(mediaType, payload, nil), nil
}

func (t *TMDB) discoveryList(ctx context.Context, mediaType string, id int, endpoint string) ([]DiscoveryItem, error) {
	kind, err := tmdbKind(mediaType)
	if err != nil {
		return nil, err
	}
	if id <= 0 || strings.TrimSpace(t.key) == "" {
		return nil, errors.New("tmdb: recommendations require a positive id and api key")
	}
	var payload tmdbSearchResponse
	path := "/" + kind + "/" + strconv.Itoa(id) + "/" + endpoint
	q := url.Values{"api_key": {t.key}, "page": {"1"}}
	if _, err := t.cachedGET(ctx, fmt.Sprintf("%s:%s:%d", endpoint, kind, id), path, q, &payload); err != nil {
		return nil, err
	}
	return t.convertDiscovery(ctx, mediaType, payload.Results)
}

func (t *TMDB) cachedGET(ctx context.Context, cacheKey, path string, q url.Values, dst any) ([]byte, error) {
	if _, hit, err := cacheLoad(ctx, t.cache, "tmdb", cacheKey, t.now(), dst); err != nil {
		return nil, err
	} else if hit {
		return nil, nil
	}
	raw, err := t.http.get(ctx, path, q, dst)
	if err != nil {
		return nil, err
	}
	if err := cacheStore(ctx, t.cache, "tmdb", cacheKey, raw, t.now(), t.ttl); err != nil {
		return nil, err
	}
	return raw, nil
}

func (t *TMDB) convertDiscovery(ctx context.Context, mediaType string, values []tmdbMovie) ([]DiscoveryItem, error) {
	genres, err := t.genreMap(ctx, mediaType)
	if err != nil {
		return nil, err
	}
	out := make([]DiscoveryItem, 0, len(values))
	for _, value := range values {
		out = append(out, convertDiscoveryItem(mediaType, value, genres))
	}
	return out, nil
}

func (t *TMDB) genreMap(ctx context.Context, mediaType string) (map[int]string, error) {
	kind, err := tmdbKind(mediaType)
	if err != nil {
		return nil, err
	}
	var payload struct {
		Genres []struct {
			ID   int    `json:"id"`
			Name string `json:"name"`
		} `json:"genres"`
	}
	if _, err := t.cachedGET(ctx, "genres:"+kind, "/genre/"+kind+"/list", url.Values{"api_key": {t.key}}, &payload); err != nil {
		return nil, err
	}
	out := make(map[int]string, len(payload.Genres))
	for _, genre := range payload.Genres {
		out[genre.ID] = genre.Name
	}
	return out, nil
}

func tmdbKind(mediaType string) (string, error) {
	switch mediaType {
	case "movie":
		return "movie", nil
	case "series":
		return "tv", nil
	default:
		return "", fmt.Errorf("tmdb: unsupported media type %q", mediaType)
	}
}

func convertDiscoveryItem(mediaType string, value tmdbMovie, genreNames map[int]string) DiscoveryItem {
	title, date := value.Title, value.ReleaseDate
	if mediaType == "series" {
		title, date = value.Name, value.FirstAirDate
	}
	genres := make([]string, 0, len(value.GenreIDs)+len(value.Genres))
	for _, id := range value.GenreIDs {
		if name := genreNames[id]; name != "" {
			genres = append(genres, name)
		}
	}
	for _, genre := range value.Genres {
		genres = append(genres, genre.Name)
	}
	keywords := make([]string, 0, len(value.Keywords.Keywords)+len(value.Keywords.Results))
	for _, keyword := range value.Keywords.Keywords {
		keywords = append(keywords, keyword.Name)
	}
	for _, keyword := range value.Keywords.Results {
		keywords = append(keywords, keyword.Name)
	}
	people := make([]string, 0, 12)
	for i, person := range value.Credits.Cast {
		if i >= 8 {
			break
		}
		people = append(people, person.Name)
	}
	for _, person := range value.Credits.Crew {
		if person.Job == "Director" || person.Job == "Creator" {
			people = append(people, person.Name)
		}
	}
	runtime := value.Runtime
	if runtime == 0 && len(value.EpisodeRunTime) > 0 {
		runtime = value.EpisodeRunTime[0]
	}
	country := ""
	if len(value.OriginCountry) > 0 {
		country = value.OriginCountry[0]
	} else if len(value.ProductionCountries) > 0 {
		country = value.ProductionCountries[0].ISO
	}
	return DiscoveryItem{MediaType: mediaType, TMDBID: value.ID, Title: title, Year: yearFromDate(date), Overview: value.Overview,
		PosterURL: tmdbPoster(value.PosterPath), Genres: genres, Keywords: keywords, People: people, Language: value.OriginalLanguage,
		Country: country, RuntimeMinutes: runtime, VoteAverage: value.VoteAverage, VoteCount: value.VoteCount,
		IMDBID: value.ExternalIDs.IMDBID, TVDBID: value.ExternalIDs.TVDBID}
}
