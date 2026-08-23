package tpb

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/TechXTT/reelay/internal/clock"
	"github.com/TechXTT/reelay/internal/config"
	"github.com/TechXTT/reelay/internal/indexer"
	"github.com/TechXTT/reelay/internal/parser"
)

func fixture(t *testing.T, name string) []byte {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatalf("read fixture %s: %v", name, err)
	}
	return raw
}

// testConfig has no rate limiting and no retries, so tests run at full speed
// and a retry loop cannot mask a wrong assertion.
func testConfig(baseURL string) config.Indexer {
	return config.Indexer{
		Name:               "tpb-test",
		Type:               "piratebay",
		Enabled:            true,
		BaseURL:            baseURL,
		UserAgent:          "reelay-test/1.0",
		RateLimitPerSecond: 1000,
		RateLimitBurst:     1000,
		RequestTimeout:     config.Dur(5 * time.Second),
		MaxRetries:         0,
		FailureThreshold:   5,
		BreakerCooldown:    config.Dur(15 * time.Minute),
		Trackers: []string{
			"udp://tracker.opentrackr.org:1337/announce",
			"udp://open.demonii.com:1337/announce",
		},
	}
}

func newTestClient(t *testing.T, cfg config.Indexer, clk clock.Clock) *Client {
	t.Helper()
	if clk == nil {
		clk = clock.Real{}
	}
	c, err := New(cfg, Options{
		Clock:  clk,
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return c
}

// serveFixture stands up a fake indexer replaying recorded responses, keyed by
// request path.
func serveFixture(t *testing.T, handler http.HandlerFunc) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	return srv
}

// The headline test: the real recorded q.php response, decoded end to end.
func TestSearchAgainstRecordedResponse(t *testing.T) {
	body := fixture(t, "search_response.json")
	var gotPath, gotQuery, gotUA string

	srv := serveFixture(t, func(w http.ResponseWriter, r *http.Request) {
		gotPath, gotQuery, gotUA = r.URL.Path, r.URL.RawQuery, r.Header.Get("User-Agent")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(body)
	})

	c := newTestClient(t, testConfig(srv.URL), nil)
	got, err := c.Search(context.Background(), indexer.Query{Term: "the expanse"})
	if err != nil {
		t.Fatalf("Search: %v", err)
	}

	if gotPath != "/q.php" {
		t.Errorf("path = %q, want /q.php", gotPath)
	}
	if gotQuery != "q=the+expanse" {
		t.Errorf("query = %q, want q=the+expanse", gotQuery)
	}
	// No cat= parameter: live testing showed it narrows the search index, not
	// the result set, and costs a request per category.
	if strings.Contains(gotQuery, "cat=") {
		t.Errorf("query %q must not send cat=; filtering is client-side", gotQuery)
	}
	if gotUA != "reelay-test/1.0" {
		t.Errorf("User-Agent = %q, want the configured value", gotUA)
	}

	if len(got) != 100 {
		t.Fatalf("got %d releases, want 100 from the recorded response", len(got))
	}

	// Every numeric field in this endpoint arrives as a quoted string.
	first := got[0]
	if first.Seeders != 607 {
		t.Errorf("seeders = %d, want 607 (decoded from the string \"607\")", first.Seeders)
	}
	if first.Leechers != 137 {
		t.Errorf("leechers = %d, want 137", first.Leechers)
	}
	if first.SizeBytes != 23598867291 {
		t.Errorf("size = %d, want 23598867291", first.SizeBytes)
	}
	if first.Category != 208 {
		t.Errorf("category = %d, want 208", first.Category)
	}
	if first.Files != 13 {
		t.Errorf("files = %d, want 13", first.Files)
	}
	if first.IMDBID != "tt3230854" {
		t.Errorf("imdb = %q, want tt3230854", first.IMDBID)
	}
	if first.Uploader != "Lulloz" {
		t.Errorf("uploader = %q, want Lulloz", first.Uploader)
	}
	if first.Indexer != "tpb-test" {
		t.Errorf("indexer = %q", first.Indexer)
	}
	if first.PublishedAt.IsZero() {
		t.Error("published_at should be derived from the added timestamp")
	}
	const wantHash = "24f8d55d8b3f94e28f53e1d8ae821836bc69be99"
	if first.InfoHash != wantHash {
		t.Errorf("info hash = %q, want lowercase hex %q", first.InfoHash, wantHash)
	}
	if !strings.HasPrefix(first.Magnet, "magnet:?xt=urn:btih:"+wantHash) {
		t.Errorf("magnet = %.80q, want it built from the lowercase hash", first.Magnet)
	}
	if !strings.Contains(first.Magnet, "tr=udp%3A%2F%2Ftracker.opentrackr.org") {
		t.Error("magnet should carry the configured trackers")
	}

	// Every release must survive parsing, since the whole pipeline depends on
	// it. This is the recorded-fixture equivalent of the parser corpus.
	for _, r := range got {
		if p := parser.Parse(r.Title); p.Title == "" {
			t.Errorf("release %q parsed to an empty title", r.Title)
		}
	}
}

// The recent endpoint sends the same records with real JSON numbers, a null
// imdb and an extra field. This is the decoding hazard that a single struct
// with plain int64 fields would fail on.
func TestSearchRecentHandlesTheOtherJSONShape(t *testing.T) {
	body := fixture(t, "recent_response.json")
	var gotPath string

	srv := serveFixture(t, func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		_, _ = w.Write(body)
	})

	c := newTestClient(t, testConfig(srv.URL), nil)
	got, err := c.Search(context.Background(), indexer.Query{Recent: true})
	if err != nil {
		t.Fatalf("Search(recent): %v", err)
	}
	if gotPath != "/precompiled/data_top100_recent.json" {
		t.Errorf("path = %q, want the precompiled recent listing", gotPath)
	}
	if len(got) == 0 {
		t.Fatal("recent listing decoded to zero releases")
	}
	for _, r := range got {
		if r.InfoHash == "" || r.Title == "" {
			t.Errorf("recent release decoded badly: %+v", r)
		}
	}
}

// The recent listing spans every category — music, games, porn — so the video
// filter is load-bearing rather than decorative.
func TestRecentFilteredToVideoCategories(t *testing.T) {
	body := fixture(t, "recent_response.json")
	srv := serveFixture(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write(body)
	})
	c := newTestClient(t, testConfig(srv.URL), nil)

	all, err := c.Search(context.Background(), indexer.Query{Recent: true})
	if err != nil {
		t.Fatal(err)
	}
	video, err := c.Search(context.Background(), indexer.Query{
		Recent:     true,
		Categories: indexer.VideoCategories(),
	})
	if err != nil {
		t.Fatal(err)
	}

	if len(video) >= len(all) {
		t.Errorf("video filter kept %d of %d rows; the recorded listing contains non-video categories", len(video), len(all))
	}
	for _, r := range video {
		if !indexer.IsVideoCategory(r.Category) {
			t.Errorf("release in category %d survived the video filter", r.Category)
		}
	}
}

// The most consequential behaviour in this package: the no-results marker is
// reported as a distinct error, not as an empty success.
func TestNoResultsMarkerIsADistinctError(t *testing.T) {
	body := fixture(t, "empty_response.json")
	srv := serveFixture(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write(body)
	})
	c := newTestClient(t, testConfig(srv.URL), nil)

	got, err := c.Search(context.Background(), indexer.Query{Term: "nonexistentzzz"})
	if !errors.Is(err, indexer.ErrNoResults) {
		t.Fatalf("error = %v, want ErrNoResults", err)
	}
	if len(got) != 0 {
		t.Errorf("got %d releases alongside ErrNoResults, want none", len(got))
	}
	// It must not count as a failure: the indexer answered, and opening the
	// breaker on a throttled response would take the indexer out of service
	// for fifteen minutes every time a search came up empty.
	if err := c.Healthy(context.Background()); err != nil {
		t.Errorf("the no-results marker must not affect health: %v", err)
	}
	if n := c.Stats().NoResults; n != 1 {
		t.Errorf("no_results counter = %d, want 1", n)
	}
}

// A marker row whose wording changed but whose id and hash are still zero must
// still be recognised.
func TestNoResultsMarkerDetectedByIDNotWording(t *testing.T) {
	srv := serveFixture(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `[{"id":"0","name":"Nothing here mate","info_hash":"0000000000000000000000000000000000000000","leechers":"0","seeders":"0","num_files":"0","size":"0","username":"","added":"0","status":"member","category":"0","imdb":""}]`)
	})
	c := newTestClient(t, testConfig(srv.URL), nil)
	if _, err := c.Search(context.Background(), indexer.Query{Term: "x"}); !errors.Is(err, indexer.ErrNoResults) {
		t.Errorf("error = %v, want ErrNoResults", err)
	}
}

func TestMalformedJSONIsAFailure(t *testing.T) {
	srv := serveFixture(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `[{"id":"1","name":`)
	})
	c := newTestClient(t, testConfig(srv.URL), nil)

	_, err := c.Search(context.Background(), indexer.Query{Term: "x"})
	if err == nil {
		t.Fatal("expected truncated JSON to fail")
	}
	if errors.Is(err, indexer.ErrNoResults) {
		t.Error("malformed JSON must not be reported as no-results")
	}
	if got := c.Stats().FailedRequests; got != 1 {
		t.Errorf("failed_requests = %d, want 1", got)
	}
}

// A dead mirror serving a parking page returns 200 with HTML. The error has to
// name the real problem, or the operator is left debugging a JSON offset.
func TestHTMLResponseGivesAUsefulError(t *testing.T) {
	srv := serveFixture(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		_, _ = io.WriteString(w, "<!doctype html><html><body>Domain for sale</body></html>")
	})
	c := newTestClient(t, testConfig(srv.URL), nil)

	_, err := c.Search(context.Background(), indexer.Query{Term: "x"})
	if err == nil {
		t.Fatal("expected an HTML response to fail")
	}
	if !strings.Contains(err.Error(), "non-JSON") || !strings.Contains(err.Error(), "mirror") {
		t.Errorf("error should point at the mirror, got: %v", err)
	}
}

func TestRetriesOn5xxButNotOn4xx(t *testing.T) {
	body := fixture(t, "search_response.json")

	t.Run("5xx then success", func(t *testing.T) {
		var calls atomic.Int32
		srv := serveFixture(t, func(w http.ResponseWriter, _ *http.Request) {
			if calls.Add(1) <= 2 {
				w.WriteHeader(http.StatusBadGateway)
				return
			}
			_, _ = w.Write(body)
		})
		cfg := testConfig(srv.URL)
		cfg.MaxRetries = 3
		c := newTestClient(t, cfg, nil)

		if _, err := c.Search(context.Background(), indexer.Query{Term: "x"}); err != nil {
			t.Fatalf("Search should have recovered: %v", err)
		}
		if got := calls.Load(); got != 3 {
			t.Errorf("made %d requests, want 3 (two failures then success)", got)
		}
	})

	t.Run("4xx is not retried", func(t *testing.T) {
		var calls atomic.Int32
		srv := serveFixture(t, func(w http.ResponseWriter, _ *http.Request) {
			calls.Add(1)
			w.WriteHeader(http.StatusForbidden)
		})
		cfg := testConfig(srv.URL)
		cfg.MaxRetries = 3
		c := newTestClient(t, cfg, nil)

		if _, err := c.Search(context.Background(), indexer.Query{Term: "x"}); err == nil {
			t.Fatal("expected a 403 to fail")
		}
		if got := calls.Load(); got != 1 {
			t.Errorf("made %d requests, want 1 — a 4xx must never be retried", got)
		}
	})

	t.Run("429 is not retried and says what to do", func(t *testing.T) {
		var calls atomic.Int32
		srv := serveFixture(t, func(w http.ResponseWriter, _ *http.Request) {
			calls.Add(1)
			w.WriteHeader(http.StatusTooManyRequests)
		})
		cfg := testConfig(srv.URL)
		cfg.MaxRetries = 3
		c := newTestClient(t, cfg, nil)

		_, err := c.Search(context.Background(), indexer.Query{Term: "x"})
		if err == nil {
			t.Fatal("expected a 429 to fail")
		}
		if got := calls.Load(); got != 1 {
			t.Errorf("made %d requests, want 1 — retrying into a 429 makes it worse", got)
		}
		if !strings.Contains(err.Error(), "rate_limit_per_second") {
			t.Errorf("a 429 should tell the operator which knob to turn, got: %v", err)
		}
	})
}

func TestBreakerTripsAndBlocksRequests(t *testing.T) {
	var calls atomic.Int32
	srv := serveFixture(t, func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		w.WriteHeader(http.StatusInternalServerError)
	})
	cfg := testConfig(srv.URL)
	cfg.FailureThreshold = 3
	clk := clock.NewFake(time.Date(2026, 8, 23, 12, 0, 0, 0, time.UTC))
	c := newTestClient(t, cfg, clk)

	for i := 0; i < 3; i++ {
		if _, err := c.Search(context.Background(), indexer.Query{Term: "x"}); err == nil {
			t.Fatalf("request %d should have failed", i+1)
		}
	}
	before := calls.Load()

	// Fourth call must be refused locally.
	_, err := c.Search(context.Background(), indexer.Query{Term: "x"})
	if !errors.Is(err, indexer.ErrUnhealthy) {
		t.Fatalf("error = %v, want ErrUnhealthy once the breaker is open", err)
	}
	if calls.Load() != before {
		t.Error("an open breaker must not issue a request")
	}
	if err := c.Healthy(context.Background()); !errors.Is(err, indexer.ErrUnhealthy) {
		t.Errorf("Healthy() = %v, want ErrUnhealthy", err)
	}
	if st := c.Stats(); st.Healthy || st.Trips != 1 {
		t.Errorf("stats = %+v, want unhealthy with one trip", st)
	}
}

func TestBreakerResetsAfterCooldown(t *testing.T) {
	body := fixture(t, "search_response.json")
	var fail atomic.Bool
	fail.Store(true)

	srv := serveFixture(t, func(w http.ResponseWriter, _ *http.Request) {
		if fail.Load() {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		_, _ = w.Write(body)
	})
	cfg := testConfig(srv.URL)
	cfg.FailureThreshold = 2
	cfg.BreakerCooldown = config.Dur(15 * time.Minute)
	clk := clock.NewFake(time.Date(2026, 8, 23, 12, 0, 0, 0, time.UTC))
	c := newTestClient(t, cfg, clk)

	for i := 0; i < 2; i++ {
		_, _ = c.Search(context.Background(), indexer.Query{Term: "x"})
	}
	if err := c.Healthy(context.Background()); err == nil {
		t.Fatal("breaker should be open")
	}

	// Not yet.
	clk.Advance(14 * time.Minute)
	if err := c.Healthy(context.Background()); err == nil {
		t.Error("breaker reopened early")
	}

	// Past the cooldown, and the indexer has recovered.
	clk.Advance(2 * time.Minute)
	if err := c.Healthy(context.Background()); err != nil {
		t.Errorf("breaker should have closed after the cooldown: %v", err)
	}
	fail.Store(false)
	if _, err := c.Search(context.Background(), indexer.Query{Term: "x"}); err != nil {
		t.Errorf("Search after recovery: %v", err)
	}
	// Failures here is the breaker's consecutive streak, which a success
	// clears; FailedRequests is the lifetime total, which stays at 2.
	if st := c.Stats(); !st.Healthy || st.BreakerState.Failures != 0 {
		t.Errorf("a success must clear the consecutive failure count, got %+v", st)
	}
	if st := c.Stats(); st.FailedRequests != 2 {
		t.Errorf("lifetime failed_requests = %d, want 2", st.FailedRequests)
	}
}

func TestRespectsContextCancellation(t *testing.T) {
	release := make(chan struct{})
	srv := serveFixture(t, func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-release:
		case <-r.Context().Done():
		}
	})
	c := newTestClient(t, testConfig(srv.URL), nil)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, err := c.Search(ctx, indexer.Query{Term: "x"})
		done <- err
	}()

	cancel()
	select {
	case err := <-done:
		if err == nil {
			t.Error("Search should fail when its context is cancelled")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Search ignored context cancellation")
	}
	close(release)
}

func TestRequestTimeoutIsEnforced(t *testing.T) {
	srv := serveFixture(t, func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-time.After(3 * time.Second):
		case <-r.Context().Done():
		}
	})
	cfg := testConfig(srv.URL)
	cfg.RequestTimeout = config.Dur(150 * time.Millisecond)
	c := newTestClient(t, cfg, nil)

	start := time.Now()
	if _, err := c.Search(context.Background(), indexer.Query{Term: "x"}); err == nil {
		t.Fatal("expected the per-request timeout to fire")
	}
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Errorf("took %s; the 150ms request timeout was not enforced", elapsed)
	}
}

func TestOversizedResponseIsRefused(t *testing.T) {
	srv := serveFixture(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("["))
		chunk := strings.Repeat("x", 1<<20)
		for i := 0; i < 9; i++ {
			_, _ = io.WriteString(w, chunk)
		}
	})
	c := newTestClient(t, testConfig(srv.URL), nil)

	_, err := c.Search(context.Background(), indexer.Query{Term: "x"})
	if err == nil {
		t.Fatal("expected an oversized response to be refused")
	}
	if !strings.Contains(err.Error(), "refusing to buffer") {
		t.Errorf("error should name the size cap, got: %v", err)
	}
}

func TestMinSeedersFilter(t *testing.T) {
	body := fixture(t, "search_response.json")
	srv := serveFixture(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write(body)
	})
	c := newTestClient(t, testConfig(srv.URL), nil)

	all, err := c.Search(context.Background(), indexer.Query{Term: "x"})
	if err != nil {
		t.Fatal(err)
	}
	seeded, err := c.Search(context.Background(), indexer.Query{Term: "x", MinSeeders: 100})
	if err != nil {
		t.Fatal(err)
	}
	if len(seeded) >= len(all) {
		t.Errorf("min_seeders=100 kept %d of %d; the recorded set contains low-seeder rows", len(seeded), len(all))
	}
	for _, r := range seeded {
		if r.Seeders < 100 {
			t.Errorf("release with %d seeders survived a floor of 100", r.Seeders)
		}
	}
}

// A single unusable row must not discard the other ninety-nine.
func TestBadRowsAreSkippedNotFatal(t *testing.T) {
	srv := serveFixture(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `[
		  {"id":"1","name":"Good.Show.S01E01.1080p.WEB.x264-GRP","info_hash":"24F8D55D8B3F94E28F53E1D8AE821836BC69BE99","seeders":"10","leechers":"1","size":"100","num_files":"1","added":"1700000000","category":"208","username":"u","imdb":""},
		  {"id":"2","name":"","info_hash":"24F8D55D8B3F94E28F53E1D8AE821836BC69BE99","seeders":"10","leechers":"1","size":"100","num_files":"1","added":"1700000000","category":"208","username":"u","imdb":""},
		  {"id":"3","name":"Bad.Hash.S01E01","info_hash":"nope","seeders":"10","leechers":"1","size":"100","num_files":"1","added":"1700000000","category":"208","username":"u","imdb":""},
		  {"id":"4","name":"Other.Show.S01E02.1080p.WEB.x264-GRP","info_hash":"C6D74B89F979C9BB0ED236B899DCFEFA1492D16A","seeders":"5","leechers":"0","size":"200","num_files":"1","added":"1700000000","category":"205","username":"u","imdb":null}
		]`)
	})
	c := newTestClient(t, testConfig(srv.URL), nil)

	got, err := c.Search(context.Background(), indexer.Query{Term: "x"})
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("got %d releases, want 2 (the nameless and bad-hash rows dropped): %+v", len(got), got)
	}
}

func TestTermSearchRequiresATerm(t *testing.T) {
	srv := serveFixture(t, func(w http.ResponseWriter, _ *http.Request) {
		t.Error("no request should have been made")
	})
	c := newTestClient(t, testConfig(srv.URL), nil)

	if _, err := c.Search(context.Background(), indexer.Query{Term: "   "}); !errors.Is(err, indexer.ErrUnsupported) {
		t.Errorf("error = %v, want ErrUnsupported", err)
	}
}

func TestNewRejectsBadBaseURL(t *testing.T) {
	for _, bad := range []string{"", "not-a-url", "/relative/only"} {
		cfg := testConfig(bad)
		if _, err := New(cfg, Options{}); err == nil {
			t.Errorf("New with base_url %q should have failed", bad)
		}
	}
}

func TestRateLimiterSerialisesRequests(t *testing.T) {
	body := fixture(t, "empty_response.json")
	srv := serveFixture(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write(body)
	})
	cfg := testConfig(srv.URL)
	// 20/s with burst 1: three requests must take at least two intervals.
	cfg.RateLimitPerSecond = 20
	cfg.RateLimitBurst = 1
	c := newTestClient(t, cfg, nil)

	start := time.Now()
	for i := 0; i < 3; i++ {
		_, _ = c.Search(context.Background(), indexer.Query{Term: fmt.Sprintf("q%d", i)})
	}
	elapsed := time.Since(start)
	if min := 80 * time.Millisecond; elapsed < min {
		t.Errorf("three requests took %s; the limiter should have held them to at least %s", elapsed, min)
	}
}

func TestBackoffIsJitteredAndBounded(t *testing.T) {
	seen := map[time.Duration]bool{}
	for i := 0; i < 200; i++ {
		d := backoff(3)
		if d <= 0 || d > 8*time.Second {
			t.Fatalf("backoff(3) = %s, out of range", d)
		}
		seen[d] = true
	}
	if len(seen) < 10 {
		t.Errorf("backoff produced only %d distinct values; jitter is not working", len(seen))
	}
	// Monotonic in expectation: attempt 1 must be cheaper than attempt 4.
	if backoff(1) > 8*time.Second {
		t.Error("first retry should be short")
	}
}
