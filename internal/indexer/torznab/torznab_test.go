package torznab

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/TechXTT/reelay/internal/config"
	"github.com/TechXTT/reelay/internal/indexer"
)

func TestTorznabCapsAndMagnetOnlySearch(t *testing.T) {
	var server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("apikey") != "fixture-key" {
			t.Error("missing API key")
		}
		if r.URL.Query().Get("t") == "caps" {
			_, _ = w.Write([]byte(`<caps><searching><search available="yes" supportedParams="q"/></searching></caps>`))
			return
		}
		if r.URL.Query().Get("cat") != "2000,5000" || r.URL.Query().Get("q") != "Arrival" {
			t.Error("incorrect query parameters")
		}
		_, _ = w.Write([]byte(`<rss xmlns:torznab="http://torznab.com/schemas/2015/feed"><channel>
 <item><title>Arrival.2016.1080p.WEB-DL</title><enclosure length="1000000000"/><pubDate>Mon, 01 Jun 2026 10:00:00 +0000</pubDate><torznab:attr name="category" value="2000"/><torznab:attr name="category" value="2040"/><torznab:attr name="seeders" value="20"/><torznab:attr name="infohash" value="0123456789abcdef0123456789abcdef01234567"/></item>
 <item><title>Unsupported.torrent.only</title><enclosure url="http://example.invalid/file.torrent" length="100000"/><torznab:attr name="category" value="5000"/></item>
 </channel></rss>`))
	}))

	defer server.Close()
	client, err := New(config.Indexer{Name: "fixture", BaseURL: server.URL, RateLimitPerSecond: 1000, RateLimitBurst: 10, RequestTimeout: config.Dur(time.Second), FailureThreshold: 2, BreakerCooldown: config.Dur(time.Minute)}, Options{APIKey: "fixture-key"})
	if err != nil {
		t.Fatal(err)
	}
	if err := client.Probe(context.Background()); err != nil {
		t.Fatal(err)
	}
	values, err := client.Search(context.Background(), indexer.Query{Term: "Arrival", Categories: []int{indexer.CatMoviesHD}})
	if err != nil || len(values) != 1 {
		t.Fatalf("releases=%v err=%v", values, err)
	}
	if values[0].InfoHash != "0123456789abcdef0123456789abcdef01234567" || !strings.HasPrefix(values[0].Magnet, "magnet:") || values[0].Seeders != 20 || values[0].PublishedAt.IsZero() {
		t.Fatalf("incorrect release: %+v", values[0])
	}
}

// Opt-in live protocol check. Credentials stay in the process environment and
// are never logged or embedded in fixtures.
func TestTorznabLiveProtocol(t *testing.T) {
	endpoint := os.Getenv("REELAY_TORZNAB_LIVE_URL")
	if endpoint == "" {
		t.Skip("live Torznab endpoint is not configured")
	}
	client, err := New(config.Indexer{Name: "live-probe", BaseURL: endpoint, RateLimitPerSecond: 1, RateLimitBurst: 1, RequestTimeout: config.Dur(30 * time.Second), FailureThreshold: 3, BreakerCooldown: config.Dur(time.Minute)}, Options{APIKey: os.Getenv("REELAY_TORZNAB_LIVE_KEY")})
	if err != nil {
		t.Fatal(err)
	}
	if err := client.Probe(context.Background()); err != nil {
		t.Fatal(err)
	}
	releases, err := client.Search(context.Background(), indexer.Query{Recent: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(releases) == 0 {
		t.Fatal("live check needs at least one video result with a usable magnet or infohash")
	}
	t.Logf("validated capabilities and %d magnet video results", len(releases))
	if _, err := client.Search(context.Background(), indexer.Query{Term: "Sintel"}); err != nil {
		t.Fatalf("named search failed: %v", err)
	}
}

func TestTorznabHTTPFailuresOpenBreakerAndDoNotExposeKey(t *testing.T) {
	var server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(503) }))

	defer server.Close()
	client, err := New(config.Indexer{Name: "fixture", BaseURL: server.URL, RateLimitPerSecond: 1000, RateLimitBurst: 10, RequestTimeout: config.Dur(time.Second), FailureThreshold: 1, BreakerCooldown: config.Dur(time.Minute)}, Options{APIKey: "secret-key"})
	if err != nil {
		t.Fatal(err)
	}
	_, err = client.Search(context.Background(), indexer.Query{Term: "test"})
	if err == nil || strings.Contains(err.Error(), "secret-key") {
		t.Fatalf("unsafe error: %v", err)
	}
	if err := client.Healthy(context.Background()); err == nil {
		t.Fatal("breaker remained closed")
	}
}

func TestTorznabMalformedResponsesOpenBreaker(t *testing.T) {
	for _, body := range []string{"", `<error code="100"/>`, `<rss><channel><item>`, `<rss><channel>`} {
		t.Run(body, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte(body)) }))
			defer server.Close()
			client, err := New(config.Indexer{Name: "fixture", BaseURL: server.URL, RateLimitPerSecond: 1000, RateLimitBurst: 10, RequestTimeout: config.Dur(time.Second), FailureThreshold: 1, BreakerCooldown: config.Dur(time.Minute)}, Options{})
			if err != nil {
				t.Fatal(err)
			}
			if _, err := client.Search(context.Background(), indexer.Query{}); err == nil {
				t.Fatal("malformed response accepted")
			}
			if err := client.Healthy(context.Background()); err == nil {
				t.Fatal("malformed response did not open breaker")
			}
		})
	}
}

func TestTorznabEnvironmentCredentialAndMissingVariable(t *testing.T) {
	const variable = "REELAY_TEST_TORZNAB_KEY"
	cfg := config.Indexer{Name: "fixture", Type: "torznab", Enabled: true, BaseURL: "http://localhost:9696/1/api", APIKeyEnv: variable, RateLimitPerSecond: 1, RateLimitBurst: 1, RequestTimeout: config.Dur(time.Second)}
	t.Setenv(variable, "")
	if _, err := New(cfg, Options{}); err == nil {
		t.Fatal("missing credential variable accepted")
	}
	t.Setenv(variable, "fixture-key")
	client, err := New(cfg, Options{})
	if err != nil || client.key != "fixture-key" {
		t.Fatal("credential variable was not resolved")
	}
}
