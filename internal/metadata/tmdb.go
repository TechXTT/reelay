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
	} `json:"external_ids"`
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
