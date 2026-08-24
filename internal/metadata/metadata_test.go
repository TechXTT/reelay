package metadata

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type memoryCache struct {
	mu   sync.Mutex
	data map[string][]byte
}

func (m *memoryCache) Get(_ context.Context, provider, key string, _ time.Time) ([]byte, bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	v, ok := m.data[provider+"/"+key]
	return v, ok, nil
}

func (m *memoryCache) Put(_ context.Context, provider, key string, payload []byte, _ time.Time, _ time.Duration) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.data == nil {
		m.data = map[string][]byte{}
	}
	m.data[provider+"/"+key] = append([]byte(nil), payload...)
	return nil
}

func TestTMDBSearchDetailsCacheAndNoKeyFallback(t *testing.T) {
	var requests atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		if r.URL.Query().Get("api_key") != "secret" {
			t.Errorf("missing api key: %s", r.URL.RawQuery)
		}
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/search/movie":
			if r.URL.Query().Get("query") != "Arrival" || r.URL.Query().Get("primary_release_year") != "2016" {
				t.Errorf("unexpected query: %s", r.URL.RawQuery)
			}
			_, _ = w.Write([]byte(`{"results":[{"id":329865,"title":"Arrival","original_title":"Arrival","release_date":"2016-11-10","overview":"First contact","poster_path":"/poster.jpg"}]}`))
		case "/movie/329865":
			_, _ = w.Write([]byte(`{"id":329865,"title":"Arrival","release_date":"2016-11-10","runtime":116,"external_ids":{"imdb_id":"tt2543164"}}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	cache := &memoryCache{}
	c, err := NewTMDB(TMDBOptions{BaseURL: srv.URL, APIKey: "secret", Cache: cache})
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		got, err := c.SearchMovies(context.Background(), "Arrival", 2016)
		if err != nil || len(got) != 1 || got[0].TMDBID != 329865 || got[0].Year != 2016 {
			t.Fatalf("search = %+v, %v", got, err)
		}
	}
	detail, err := c.MovieDetails(context.Background(), 329865)
	if err != nil || detail.RuntimeMinutes != 116 || detail.IMDBID != "tt2543164" {
		t.Fatalf("details = %+v, %v", detail, err)
	}
	if got := requests.Load(); got != 2 {
		t.Fatalf("HTTP requests = %d, want 2 (cached second search)", got)
	}

	fallback, err := NewTMDB(TMDBOptions{BaseURL: srv.URL})
	if err != nil {
		t.Fatal(err)
	}
	got, err := fallback.SearchMovies(context.Background(), "Typed Title", 2025)
	if err != nil || len(got) != 1 || got[0].TMDBID != 0 || got[0].Title != "Typed Title" {
		t.Fatalf("no-key fallback = %+v, %v", got, err)
	}
}

func TestTVmazeSearchEpisodesAndCache(t *testing.T) {
	var requests atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/search/shows":
			_, _ = w.Write([]byte(`[{"score":1,"show":{"id":41428,"name":"The Expanse","status":"Ended","premiered":"2015-12-14","averageRuntime":45,"summary":"<p>Humanity in space.</p>","externals":{"imdb":"tt3230854"},"image":{"medium":"https://img/poster.jpg"}}}]`))
		case "/shows/41428/episodes":
			_, _ = w.Write([]byte(`[{"id":1,"name":"Dulcinea","season":1,"number":1,"airdate":"2015-12-14","airstamp":"2015-12-15T03:00:00+00:00","runtime":45}]`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	c, err := NewTVmaze(TVmazeOptions{BaseURL: srv.URL, Cache: &memoryCache{}})
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		shows, err := c.SearchSeries(context.Background(), "The Expanse")
		if err != nil || len(shows) != 1 || shows[0].TVmazeID != 41428 ||
			shows[0].Year != 2015 || shows[0].Overview != "Humanity in space." {
			t.Fatalf("shows = %+v, %v", shows, err)
		}
		episodes, err := c.SeriesEpisodes(context.Background(), 41428)
		if err != nil || len(episodes) != 1 || episodes[0].AirDate == nil ||
			episodes[0].AirDate.Location() != time.UTC {
			t.Fatalf("episodes = %+v, %v", episodes, err)
		}
	}
	if got := requests.Load(); got != 2 {
		t.Fatalf("HTTP requests = %d, want 2", got)
	}
}

func TestCachedPayloadMustRemainDecodable(t *testing.T) {
	cache := &memoryCache{data: map[string][]byte{"tmdb/search:bad:0": []byte("not-json")}}
	c, err := NewTMDB(TMDBOptions{BaseURL: "https://example.invalid", APIKey: "x", Cache: cache})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.SearchMovies(context.Background(), "bad", 0); err == nil {
		t.Fatal("expected corrupt cached JSON to fail")
	}
	if !json.Valid([]byte(`{"ok":true}`)) {
		t.Fatal("test invariant")
	}
}

func TestTMDBDiscoveryAndTVmazeExternalLookup(t *testing.T) {
	tmdbServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/movie/42/recommendations":
			_, _ = w.Write([]byte(`{"results":[{"id":99,"title":"Candidate","release_date":"2024-01-02","genre_ids":[878],"vote_average":8.2,"vote_count":1200}]}`))
		case "/genre/movie/list":
			_, _ = w.Write([]byte(`{"genres":[{"id":878,"name":"Science Fiction"}]}`))
		case "/movie/99":
			_, _ = w.Write([]byte(`{"id":99,"title":"Candidate","release_date":"2024-01-02","runtime":121,"genres":[{"id":878,"name":"Science Fiction"}],"credits":{"cast":[{"name":"Actor A"}],"crew":[{"name":"Director A","job":"Director"}]},"keywords":{"keywords":[{"name":"space"}]},"external_ids":{"imdb_id":"tt0000099"}}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer tmdbServer.Close()
	tmdb, err := NewTMDB(TMDBOptions{BaseURL: tmdbServer.URL, APIKey: "secret", Cache: &memoryCache{}})
	if err != nil {
		t.Fatal(err)
	}
	values, err := tmdb.Recommendations(t.Context(), "movie", 42)
	if err != nil || len(values) != 1 || values[0].Genres[0] != "Science Fiction" {
		t.Fatalf("recommendations=%+v err=%v", values, err)
	}
	detail, err := tmdb.DiscoveryDetails(t.Context(), "movie", 99)
	if err != nil || detail.IMDBID != "tt0000099" || detail.RuntimeMinutes != 121 || len(detail.People) != 2 || len(detail.Keywords) != 1 {
		t.Fatalf("detail=%+v err=%v", detail, err)
	}

	tvmazeServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/lookup/shows" || r.URL.Query().Get("thetvdb") != "81189" {
			http.NotFound(w, r)
			return
		}
		_, _ = w.Write([]byte(`{"id":1396,"name":"Breaking Bad","premiered":"2008-01-20","averageRuntime":47,"externals":{"imdb":"tt0903747","thetvdb":81189}}`))
	}))
	defer tvmazeServer.Close()
	tvmaze, err := NewTVmaze(TVmazeOptions{BaseURL: tvmazeServer.URL, Cache: &memoryCache{}})
	if err != nil {
		t.Fatal(err)
	}
	series, err := tvmaze.LookupSeries(t.Context(), 81189, "")
	if err != nil || series.TVmazeID != 1396 || series.Year != 2008 {
		t.Fatalf("series=%+v err=%v", series, err)
	}
}
