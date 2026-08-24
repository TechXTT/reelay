package metadata

import (
	"context"
	"fmt"
	"html"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"

	"golang.org/x/time/rate"
)

type TVmazeOptions struct {
	BaseURL    string
	HTTPClient *http.Client
	Timeout    time.Duration
	Cache      Cache
	CacheTTL   time.Duration
	Now        func() time.Time
}

type TVmaze struct {
	http    httpJSONClient
	cache   Cache
	ttl     time.Duration
	now     func() time.Time
	limiter *rate.Limiter
}

func NewTVmaze(opt TVmazeOptions) (*TVmaze, error) {
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
	return &TVmaze{
		http: h, cache: opt.Cache, ttl: opt.CacheTTL, now: opt.Now,
		// TVmaze guarantees at least 20 requests per 10 seconds. A burst of
		// two keeps search responsive while staying inside that floor.
		limiter: rate.NewLimiter(rate.Limit(2), 2),
	}, nil
}

type tvmazeSearchResult struct {
	Score float64    `json:"score"`
	Show  tvmazeShow `json:"show"`
}

type tvmazeShow struct {
	ID             int    `json:"id"`
	Name           string `json:"name"`
	Status         string `json:"status"`
	Premiered      string `json:"premiered"`
	Runtime        int    `json:"runtime"`
	AverageRuntime int    `json:"averageRuntime"`
	Summary        string `json:"summary"`
	Externals      struct {
		IMDB string `json:"imdb"`
		TVDB int    `json:"thetvdb"`
	} `json:"externals"`
	Image *struct {
		Medium   string `json:"medium"`
		Original string `json:"original"`
	} `json:"image"`
}

type tvmazeEpisode struct {
	ID       int    `json:"id"`
	Name     string `json:"name"`
	Season   int    `json:"season"`
	Number   *int   `json:"number"`
	AirDate  string `json:"airdate"`
	AirStamp string `json:"airstamp"`
	Runtime  int    `json:"runtime"`
}

func (t *TVmaze) SearchSeries(ctx context.Context, title string) ([]Series, error) {
	title = strings.TrimSpace(title)
	if title == "" {
		return nil, fmt.Errorf("tvmaze: empty series title")
	}
	cacheKey := "search:" + strings.ToLower(title)
	var payload []tvmazeSearchResult
	_, hit, err := cacheLoad(ctx, t.cache, "tvmaze", cacheKey, t.now(), &payload)
	if err != nil {
		return nil, err
	}
	if !hit {
		if err := t.limiter.Wait(ctx); err != nil {
			return nil, fmt.Errorf("tvmaze: rate limit wait: %w", err)
		}
		raw, err := t.http.get(ctx, "/search/shows", url.Values{"q": {title}}, &payload)
		if err != nil {
			return nil, err
		}
		if err := cacheStore(ctx, t.cache, "tvmaze", cacheKey, raw, t.now(), t.ttl); err != nil {
			return nil, err
		}
	}
	out := make([]Series, 0, len(payload))
	for _, result := range payload {
		out = append(out, convertTVmazeShow(result.Show))
	}
	return out, nil
}

func (t *TVmaze) SeriesEpisodes(ctx context.Context, id int) ([]Episode, error) {
	if id <= 0 {
		return nil, fmt.Errorf("tvmaze: invalid show id %d", id)
	}
	cacheKey := "episodes:" + strconv.Itoa(id)
	var payload []tvmazeEpisode
	_, hit, err := cacheLoad(ctx, t.cache, "tvmaze", cacheKey, t.now(), &payload)
	if err != nil {
		return nil, err
	}
	if !hit {
		if err := t.limiter.Wait(ctx); err != nil {
			return nil, fmt.Errorf("tvmaze: rate limit wait: %w", err)
		}
		raw, err := t.http.get(ctx, "/shows/"+strconv.Itoa(id)+"/episodes", nil, &payload)
		if err != nil {
			return nil, err
		}
		if err := cacheStore(ctx, t.cache, "tvmaze", cacheKey, raw, t.now(), t.ttl); err != nil {
			return nil, err
		}
	}
	out := make([]Episode, 0, len(payload))
	for _, ep := range payload {
		number := 0
		if ep.Number != nil {
			number = *ep.Number
		}
		out = append(out, Episode{
			ProviderID: ep.ID, Season: ep.Season, Number: number,
			Title: ep.Name, AirDate: tvmazeAirDate(ep), RuntimeMinutes: ep.Runtime,
		})
	}
	return out, nil
}

func convertTVmazeShow(s tvmazeShow) Series {
	runtime := s.Runtime
	if runtime == 0 {
		runtime = s.AverageRuntime
	}
	poster := ""
	if s.Image != nil {
		poster = s.Image.Medium
		if poster == "" {
			poster = s.Image.Original
		}
	}
	return Series{
		TVmazeID: s.ID, Title: s.Name, Year: yearFromDate(s.Premiered),
		Status: s.Status, RuntimeMinutes: runtime, IMDBID: s.Externals.IMDB,
		Overview: stripHTML(s.Summary), PosterURL: poster,
	}
}

func tvmazeAirDate(ep tvmazeEpisode) *time.Time {
	if ep.AirStamp != "" {
		if t, err := time.Parse(time.RFC3339, ep.AirStamp); err == nil {
			u := t.UTC()
			return &u
		}
	}
	if ep.AirDate != "" {
		if t, err := time.Parse("2006-01-02", ep.AirDate); err == nil {
			return &t
		}
	}
	return nil
}

var htmlTag = regexp.MustCompile(`<[^>]+>`)

func stripHTML(s string) string {
	return strings.TrimSpace(html.UnescapeString(htmlTag.ReplaceAllString(s, "")))
}

var _ SeriesProvider = (*TVmaze)(nil)
var _ ExternalSeriesProvider = (*TVmaze)(nil)

func (t *TVmaze) LookupSeries(ctx context.Context, tvdbID int, imdbID string) (Series, error) {
	q := url.Values{}
	key := ""
	if tvdbID > 0 {
		q.Set("thetvdb", strconv.Itoa(tvdbID))
		key = "tvdb:" + strconv.Itoa(tvdbID)
	} else if imdbID != "" {
		q.Set("imdb", imdbID)
		key = "imdb:" + imdbID
	} else {
		return Series{}, fmt.Errorf("tvmaze: lookup requires a TVDB or IMDb id")
	}
	var payload tvmazeShow
	_, hit, err := cacheLoad(ctx, t.cache, "tvmaze", "lookup:"+key, t.now(), &payload)
	if err != nil {
		return Series{}, err
	}
	if !hit {
		if err := t.limiter.Wait(ctx); err != nil {
			return Series{}, fmt.Errorf("tvmaze: rate limit wait: %w", err)
		}
		raw, err := t.http.get(ctx, "/lookup/shows", q, &payload)
		if err != nil {
			return Series{}, err
		}
		if err := cacheStore(ctx, t.cache, "tvmaze", "lookup:"+key, raw, t.now(), t.ttl); err != nil {
			return Series{}, err
		}
	}
	return convertTVmazeShow(payload), nil
}
