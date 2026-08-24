package metadata

import (
	"context"
	"time"
)

type Movie struct {
	TMDBID         int    `json:"tmdb_id"`
	Title          string `json:"title"`
	OriginalTitle  string `json:"original_title,omitempty"`
	Year           int    `json:"year"`
	RuntimeMinutes int    `json:"runtime_minutes,omitempty"`
	IMDBID         string `json:"imdb_id,omitempty"`
	Overview       string `json:"overview,omitempty"`
	PosterURL      string `json:"poster_url,omitempty"`
}

type Series struct {
	TVmazeID       int      `json:"tvmaze_id"`
	Title          string   `json:"title"`
	Year           int      `json:"year"`
	Status         string   `json:"status,omitempty"`
	RuntimeMinutes int      `json:"runtime_minutes,omitempty"`
	IMDBID         string   `json:"imdb_id,omitempty"`
	Aliases        []string `json:"aliases,omitempty"`
	Overview       string   `json:"overview,omitempty"`
	PosterURL      string   `json:"poster_url,omitempty"`
}

type Episode struct {
	ProviderID     int        `json:"provider_id"`
	Season         int        `json:"season"`
	Number         int        `json:"number"`
	AbsoluteNumber int        `json:"absolute_number,omitempty"`
	Title          string     `json:"title,omitempty"`
	AirDate        *time.Time `json:"air_date,omitempty"`
	RuntimeMinutes int        `json:"runtime_minutes,omitempty"`
}

type MovieProvider interface {
	SearchMovies(ctx context.Context, title string, year int) ([]Movie, error)
	MovieDetails(ctx context.Context, id int) (Movie, error)
}

type SeriesProvider interface {
	SearchSeries(ctx context.Context, title string) ([]Series, error)
	SeriesEpisodes(ctx context.Context, id int) ([]Episode, error)
}

type DiscoveryItem struct {
	MediaType      string
	TMDBID         int
	Title          string
	Year           int
	Overview       string
	PosterURL      string
	Genres         []string
	Keywords       []string
	People         []string
	Language       string
	Country        string
	RuntimeMinutes int
	VoteAverage    float64
	VoteCount      int
	IMDBID         string
	TVDBID         int
}

type RecommendationProvider interface {
	Recommendations(ctx context.Context, mediaType string, tmdbID int) ([]DiscoveryItem, error)
	Similar(ctx context.Context, mediaType string, tmdbID int) ([]DiscoveryItem, error)
	Discover(ctx context.Context, mediaType string) ([]DiscoveryItem, error)
	DiscoveryDetails(ctx context.Context, mediaType string, tmdbID int) (DiscoveryItem, error)
}

type ExternalSeriesProvider interface {
	LookupSeries(ctx context.Context, tvdbID int, imdbID string) (Series, error)
}

type Cache interface {
	Get(ctx context.Context, provider, key string, now time.Time) ([]byte, bool, error)
	Put(ctx context.Context, provider, key string, payload []byte, fetchedAt time.Time, ttl time.Duration) error
}
