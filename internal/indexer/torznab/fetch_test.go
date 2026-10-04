package torznab

import (
	"bytes"
	"context"
	"crypto/sha1"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/TechXTT/reelay/internal/config"
	"github.com/TechXTT/reelay/internal/indexer"
)

const fetchTestInfo = "d6:lengthi12345e4:name8:test.mkv12:piece lengthi16384e6:pieces20:01234567890123456789e"

var fetchTestTorrent = []byte("d4:info" + fetchTestInfo + "e")

func newFetchClient(t *testing.T, baseURL string) *Client {
	t.Helper()
	client, err := New(config.Indexer{Name: "fixture", BaseURL: baseURL, RateLimitPerSecond: 1000, RateLimitBurst: 10,
		RequestTimeout: config.Dur(2 * time.Second), FailureThreshold: 5, BreakerCooldown: config.Dur(time.Minute)},
		Options{APIKey: "fixture-key"})
	if err != nil {
		t.Fatal(err)
	}
	return client
}

func feed(items ...string) string {
	return `<rss xmlns:torznab="http://torznab.com/schemas/2015/feed"><channel>` + strings.Join(items, "") + `</channel></rss>`
}

func linkItem(url string) string {
	return `<item><title>Arrival.2016.1080p.WEB-DL</title><enclosure url="` + url + `" length="1000000000"/>` +
		`<torznab:attr name="category" value="2000"/><torznab:attr name="seeders" value="20"/></item>`
}

func searchOnce(t *testing.T, body string) (*Client, []indexer.Release) {
	t.Helper()
	var server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(body))
	}))

	t.Cleanup(server.Close)
	var client = newFetchClient(t, server.URL)
	releases, err := client.Search(context.Background(), indexer.Query{Term: "Arrival"})
	if err != nil {
		t.Fatal(err)
	}
	return client, releases
}

func TestDownloadLinkOnlyItemBecomesDownloadURLRelease(t *testing.T) {
	var server *httptest.Server

	server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(feed(linkItem(server.URL + "/dl/1.torrent?file=a&amp;APIKEY=fixture-key&amp;x=1"))))
	}))
	defer server.Close()
	var client = newFetchClient(t, server.URL)

	first, err := client.Search(context.Background(), indexer.Query{Term: "Arrival"})
	if err != nil || len(first) != 1 {
		t.Fatalf("releases=%v err=%v", first, err)
	}
	var release = first[0]
	var wantURL = server.URL + "/dl/1.torrent?file=a&x=1"
	var sum = sha1.Sum([]byte("fixture\x00" + wantURL))

	if release.DownloadURL != wantURL || strings.Contains(strings.ToLower(release.DownloadURL), "apikey") {
		t.Fatalf("download url = %q, want %q", release.DownloadURL, wantURL)
	}
	if release.Magnet != "" || release.InfoHash != hex.EncodeToString(sum[:]) {
		t.Fatalf("release = %+v", release)
	}
	second, err := client.Search(context.Background(), indexer.Query{Term: "Arrival"})
	if err != nil || len(second) != 1 || second[0].InfoHash != release.InfoHash {
		t.Fatalf("stand-in hash is not stable: %+v err=%v", second, err)
	}
}

func TestDownloadLinkOnDifferentHostIsRejected(t *testing.T) {
	_, releases := searchOnce(t, feed(
		linkItem("http://other.example/dl/1.torrent?apikey=x"),
		linkItem("ftp://example.org/1.torrent")))

	if len(releases) != 0 {
		t.Fatalf("releases = %+v, want none", releases)
	}
}

func TestDownloadLinkStillSubjectToFilters(t *testing.T) {
	var server *httptest.Server

	server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(feed(`<item><title>Arrival.2016</title><enclosure url="` + server.URL +
			`/dl/1.torrent" length="1000"/><torznab:attr name="category" value="2000"/><torznab:attr name="seeders" value="1"/></item>`)))
	}))
	defer server.Close()
	var client = newFetchClient(t, server.URL)

	releases, err := client.Search(context.Background(), indexer.Query{Term: "Arrival", MinSeeders: 5})
	if err != nil || len(releases) != 0 {
		t.Fatalf("releases=%v err=%v", releases, err)
	}
}

func TestFetchTorrentReturnsFileAndReaddsAPIKey(t *testing.T) {
	var seenKey atomic.Value
	var server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seenKey.Store(r.URL.Query().Get("apikey"))
		if r.URL.Query().Get("file") != "a" {
			t.Error("lost query parameter")
		}
		_, _ = w.Write(fetchTestTorrent)
	}))

	defer server.Close()
	var client = newFetchClient(t, server.URL)
	payload, err := client.FetchTorrent(context.Background(), server.URL+"/dl/1.torrent?file=a&ApiKey=stale")
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(payload.File, fetchTestTorrent) || payload.Magnet != "" {
		t.Fatalf("payload = %+v", payload)
	}
	if seenKey.Load() != "fixture-key" {
		t.Fatalf("server saw apikey %v", seenKey.Load())
	}
}

func TestFetchTorrentFollowsSameHostRedirect(t *testing.T) {
	var server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/dl/start" {
			http.Redirect(w, r, "/dl/real.torrent", http.StatusFound)
			return
		}
		_, _ = w.Write(fetchTestTorrent)
	}))

	defer server.Close()
	payload, err := newFetchClient(t, server.URL).FetchTorrent(context.Background(), server.URL+"/dl/start")
	if err != nil || !bytes.Equal(payload.File, fetchTestTorrent) {
		t.Fatalf("payload=%+v err=%v", payload, err)
	}
}

func TestFetchTorrentCapturesMagnetRedirect(t *testing.T) {
	var magnet = "magnet:?xt=urn:btih:0123456789abcdef0123456789abcdef01234567&dn=Arrival"
	var server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Location", magnet)
		w.WriteHeader(http.StatusFound)
	}))

	defer server.Close()
	payload, err := newFetchClient(t, server.URL).FetchTorrent(context.Background(), server.URL+"/dl/1")
	if err != nil || payload.Magnet != magnet || len(payload.File) != 0 {
		t.Fatalf("payload=%+v err=%v", payload, err)
	}
}

func TestFetchTorrentRejectsInvalidMagnetRedirect(t *testing.T) {
	var server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Location", "magnet:?dn=nohash")
		w.WriteHeader(http.StatusFound)
	}))

	defer server.Close()
	if _, err := newFetchClient(t, server.URL).FetchTorrent(context.Background(), server.URL+"/dl/1"); err == nil {
		t.Fatal("invalid magnet accepted")
	}
}

func TestFetchTorrentRejectsCrossHostRedirect(t *testing.T) {
	var other atomic.Int32
	var otherServer = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		other.Add(1)
		_, _ = w.Write(fetchTestTorrent)
	}))
	var server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, otherServer.URL+"/steal?apikey=x", http.StatusFound)
	}))

	defer server.Close()
	defer otherServer.Close()
	// Both servers are on 127.0.0.1 with different ports, so the port check
	// is what separates them.
	_, err := newFetchClient(t, server.URL).FetchTorrent(context.Background(), server.URL+"/dl/1")
	if err == nil || other.Load() != 0 {
		t.Fatalf("err=%v other server hits=%d", err, other.Load())
	}
	if strings.Contains(err.Error(), "fixture-key") {
		t.Fatalf("error leaks the API key: %v", err)
	}
}

func TestFetchTorrentRejectsTooManyRedirects(t *testing.T) {
	var server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/dl/loop", http.StatusFound)
	}))

	defer server.Close()
	if _, err := newFetchClient(t, server.URL).FetchTorrent(context.Background(), server.URL+"/dl/loop"); err == nil {
		t.Fatal("redirect loop accepted")
	}
}

func TestFetchTorrentRejectsOversizeAndInvalid(t *testing.T) {
	var body []byte
	var server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write(body)
	}))

	defer server.Close()
	var client = newFetchClient(t, server.URL)

	body = append([]byte("d1:a"), bytes.Repeat([]byte("1"), 4<<20)...)
	if _, err := client.FetchTorrent(context.Background(), server.URL+"/dl/big"); err == nil || !strings.Contains(err.Error(), "exceeds") {
		t.Fatalf("oversize err = %v", err)
	}
	body = []byte("<html>login</html>")
	if _, err := client.FetchTorrent(context.Background(), server.URL+"/dl/html"); err == nil {
		t.Fatal("non-torrent accepted")
	}
}

func TestFetchTorrentRejectsForeignHostAndBadStatus(t *testing.T) {
	var server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
	}))

	defer server.Close()
	var client = newFetchClient(t, server.URL)

	if _, err := client.FetchTorrent(context.Background(), "http://other.example/dl/1"); err == nil {
		t.Fatal("foreign host accepted")
	}
	if _, err := client.FetchTorrent(context.Background(), server.URL+"/dl/1"); err == nil || strings.Contains(err.Error(), "fixture-key") {
		t.Fatalf("status err = %v", err)
	}
}
