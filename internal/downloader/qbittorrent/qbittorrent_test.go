package qbittorrent

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"mime"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/TechXTT/reelay/internal/config"
	"github.com/TechXTT/reelay/internal/downloader"
)

const (
	testUser = "admin"
	testPass = "correct-horse"
	testSID  = "abc123sid"
)

// fakeQB is a stand-in qBittorrent. Hand-written rather than generated: the
// interesting behaviours here are the session cookie expiring and the "Fails."
// body on a 200, and a mocking framework would let both be faked away.
type fakeQB struct {
	t *testing.T

	mu sync.Mutex
	// expireAfter counts down authenticated requests before the session is
	// treated as stale, which is how cookie expiry is simulated.
	expireAfter int
	sessions    map[string]bool
	// expireBarrier makes several requests observe the same expired session
	// before any of them can re-authenticate. It catches redundant-login races
	// that a sequential expiry test cannot expose.
	expireBarrier chan struct{}
	expireTarget  int
	expireWaiters int

	infoBody []byte

	// Recorded for assertions.
	Logins            atomic.Int32
	Adds              atomic.Int32
	Deletes           atomic.Int32
	Controls          atomic.Int32
	Infos             atomic.Int32
	LastAdd           map[string]string
	LastQuery         string
	LastControlPath   string
	LastControlHashes string
	LegacyControlOnly bool
	addMu             sync.Mutex
}

func newFakeQB(t *testing.T, infoBody []byte) (*fakeQB, *httptest.Server) {
	f := &fakeQB{t: t, sessions: map[string]bool{}, infoBody: infoBody, expireAfter: -1}
	srv := httptest.NewServer(f)
	t.Cleanup(srv.Close)
	return f, srv
}

// ExpireSessionAfter makes the next n authenticated requests succeed, then
// start answering 403 until a fresh login.
func (f *fakeQB) ExpireSessionAfter(n int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.expireAfter = n
}

func (f *fakeQB) ExpireSessionConcurrently(callers int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.expireBarrier = make(chan struct{})
	f.expireTarget = callers
	f.expireWaiters = 0
}

func (f *fakeQB) authorised(r *http.Request) bool {
	c, err := r.Cookie("SID")
	if err != nil {
		return false
	}
	f.mu.Lock()
	if f.expireBarrier != nil {
		barrier := f.expireBarrier
		f.expireWaiters++
		if f.expireWaiters == f.expireTarget {
			close(barrier)
			f.expireBarrier = nil
		}
		f.mu.Unlock()
		<-barrier
		return false
	}
	defer f.mu.Unlock()
	if !f.sessions[c.Value] {
		return false
	}
	if f.expireAfter == 0 {
		delete(f.sessions, c.Value)
		return false
	}
	if f.expireAfter > 0 {
		f.expireAfter--
	}
	return true
}

func (f *fakeQB) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	// Every POST must carry a Referer, or the real client rejects it with a
	// 403 that is indistinguishable from an auth failure.
	if r.Method == http.MethodPost && r.Header.Get("Referer") == "" {
		f.t.Errorf("POST %s arrived with no Referer header; the real client 403s on that", r.URL.Path)
	}

	switch r.URL.Path {
	case "/api/v2/auth/login":
		f.Logins.Add(1)
		_ = r.ParseForm()
		if r.PostFormValue("username") != testUser || r.PostFormValue("password") != testPass {
			// qBittorrent answers bad credentials with 200 + "Fails.", not 401.
			w.WriteHeader(http.StatusOK)
			_, _ = io.WriteString(w, "Fails.")
			return
		}
		f.mu.Lock()
		f.sessions[testSID] = true
		// A fresh login gives a fresh session. Without this reset the
		// simulated expiry fires again immediately and the client, which
		// retries exactly once, can never recover — the test would be
		// asserting a limitation of the fake rather than of the client.
		f.expireAfter = -1
		f.mu.Unlock()
		http.SetCookie(w, &http.Cookie{Name: "SID", Value: testSID, Path: "/"})
		_, _ = io.WriteString(w, "Ok.")

	case "/api/v2/app/version":
		if !f.authorised(r) {
			w.WriteHeader(http.StatusForbidden)
			return
		}
		_, _ = io.WriteString(w, "v4.4.1")

	case "/api/v2/app/webapiVersion":
		if !f.authorised(r) {
			w.WriteHeader(http.StatusForbidden)
			return
		}
		_, _ = io.WriteString(w, "2.8.5")

	case "/api/v2/torrents/info":
		if !f.authorised(r) {
			w.WriteHeader(http.StatusForbidden)
			return
		}
		f.Infos.Add(1)
		f.addMu.Lock()
		f.LastQuery = r.URL.RawQuery
		f.addMu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		// The hashes filter has to be honoured, not ignored. A fake that
		// returns the whole list regardless made the Remove ownership test
		// pass for entirely the wrong reason: the client asked about a foreign
		// torrent, got our own back as row zero, and happily deleted.
		_, _ = w.Write(f.filterByHashes(r.URL.Query().Get("hashes")))

	case "/api/v2/torrents/add":
		if !f.authorised(r) {
			w.WriteHeader(http.StatusForbidden)
			return
		}
		f.Adds.Add(1)
		fields := parseMultipart(f.t, r)
		f.addMu.Lock()
		f.LastAdd = fields
		f.addMu.Unlock()
		_, _ = io.WriteString(w, "Ok.")

	case "/api/v2/torrents/delete":
		if !f.authorised(r) {
			w.WriteHeader(http.StatusForbidden)
			return
		}
		f.Deletes.Add(1)
		_ = r.ParseForm()
		_, _ = io.WriteString(w, "")

	case "/api/v2/torrents/stop", "/api/v2/torrents/start":
		if !f.authorised(r) {
			w.WriteHeader(http.StatusForbidden)
			return
		}
		if f.LegacyControlOnly {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		f.recordControl(r)

	case "/api/v2/torrents/pause", "/api/v2/torrents/resume":
		if !f.authorised(r) {
			w.WriteHeader(http.StatusForbidden)
			return
		}
		f.recordControl(r)

	case "/api/v2/torrents/createCategory":
		if !f.authorised(r) {
			w.WriteHeader(http.StatusForbidden)
			return
		}
		_ = r.ParseForm()
		if r.PostFormValue("category") == "already-there" {
			w.WriteHeader(http.StatusConflict)
			return
		}
		_, _ = io.WriteString(w, "")

	default:
		w.WriteHeader(http.StatusNotFound)
	}
}

func (f *fakeQB) recordControl(r *http.Request) {
	f.Controls.Add(1)
	_ = r.ParseForm()
	f.addMu.Lock()
	f.LastControlPath = r.URL.Path
	f.LastControlHashes = r.PostFormValue("hashes")
	f.addMu.Unlock()
}

// filterByHashes mimics the real endpoint's hashes= behaviour: an empty filter
// returns everything, otherwise only the pipe-separated hashes listed.
func (f *fakeQB) filterByHashes(filter string) []byte {
	if strings.TrimSpace(filter) == "" {
		return f.infoBody
	}
	wanted := map[string]bool{}
	for _, h := range strings.Split(filter, "|") {
		if h = strings.ToLower(strings.TrimSpace(h)); h != "" {
			wanted[h] = true
		}
	}

	var rows []map[string]any
	if err := json.Unmarshal(f.infoBody, &rows); err != nil {
		f.t.Fatalf("fake: decode fixture: %v", err)
	}
	kept := make([]map[string]any, 0, len(rows))
	for _, r := range rows {
		h, _ := r["hash"].(string)
		if wanted[strings.ToLower(h)] {
			kept = append(kept, r)
		}
	}
	out, err := json.Marshal(kept)
	if err != nil {
		f.t.Fatalf("fake: encode filtered rows: %v", err)
	}
	return out
}

func parseMultipart(t *testing.T, r *http.Request) map[string]string {
	t.Helper()
	ct := r.Header.Get("Content-Type")
	mediaType, params, err := mime.ParseMediaType(ct)
	if err != nil || !strings.HasPrefix(mediaType, "multipart/") {
		t.Errorf("torrents/add Content-Type = %q, want multipart/form-data", ct)
		return nil
	}
	mr := multipart.NewReader(r.Body, params["boundary"])
	out := map[string]string{}
	for {
		part, err := mr.NextPart()
		if err != nil {
			break
		}
		b, _ := io.ReadAll(part)
		out[part.FormName()] = string(b)
	}
	return out
}

func testCfg(url string) config.Downloader {
	return config.Downloader{
		Type:           "qbittorrent",
		URL:            url,
		Username:       testUser,
		Password:       testPass,
		CategoryTV:     "reelay-tv",
		CategoryMovies: "reelay-movies",
	}
}

func newClient(t *testing.T, cfg config.Downloader) *Client {
	t.Helper()
	c, err := New(cfg, Options{
		Logger:  slog.New(slog.NewTextHandler(io.Discard, nil)),
		Timeout: 5 * time.Second,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return c
}

func fixtureBody(t *testing.T) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", "torrents_info.json"))
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}
	return b
}

const testMagnet = "magnet:?xt=urn:btih:24F8D55D8B3F94E28F53E1D8AE821836BC69BE99&dn=Some.Show.S01E01"

// ---------------------------------------------------------------------------

func TestLoginAndHealthy(t *testing.T) {
	fake, srv := newFakeQB(t, fixtureBody(t))
	c := newClient(t, testCfg(srv.URL))

	if err := c.Healthy(context.Background()); err != nil {
		t.Fatalf("Healthy: %v", err)
	}
	if got := fake.Logins.Load(); got != 1 {
		t.Errorf("made %d logins, want 1", got)
	}
	if got := c.APIVersion(); got != "2.8.5" {
		t.Errorf("APIVersion = %q, want 2.8.5", got)
	}

	// A second call reuses the session rather than logging in again.
	if err := c.Healthy(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := fake.Logins.Load(); got != 1 {
		t.Errorf("made %d logins across two calls, want 1 — the session should be reused", got)
	}
}

// Bad credentials arrive as 200 + "Fails.", so a status-code-only check would
// treat them as success.
func TestBadCredentials(t *testing.T) {
	_, srv := newFakeQB(t, nil)
	cfg := testCfg(srv.URL)
	cfg.Password = "wrong"
	c := newClient(t, cfg)

	err := c.Healthy(context.Background())
	if !errors.Is(err, downloader.ErrAuth) {
		t.Fatalf("error = %v, want ErrAuth", err)
	}
	if !strings.Contains(err.Error(), "downloader.password") {
		t.Errorf("error should name the config key to fix, got: %v", err)
	}
}

// The behaviour that matters most in a long-running process: the session
// expires roughly hourly and the poll must recover without surfacing an error.
func TestTransparentReauthOnSessionExpiry(t *testing.T) {
	fake, srv := newFakeQB(t, fixtureBody(t))
	c := newClient(t, testCfg(srv.URL))

	if err := c.Healthy(context.Background()); err != nil {
		t.Fatal(err)
	}
	loginsBefore := fake.Logins.Load()

	// Expire the session, then poll. The caller must see success.
	fake.ExpireSessionAfter(0)
	got, err := c.Status(context.Background(), nil)
	if err != nil {
		t.Fatalf("Status after session expiry should have recovered, got: %v", err)
	}
	if len(got) == 0 {
		t.Error("Status returned nothing after re-auth")
	}
	if after := fake.Logins.Load(); after != loginsBefore+1 {
		t.Errorf("logins went %d -> %d, want exactly one re-auth", loginsBefore, after)
	}
}

// One login, not one per caller, when several goroutines hit an expired
// session simultaneously.
func TestConcurrentReauthLogsInOnce(t *testing.T) {
	fake, srv := newFakeQB(t, fixtureBody(t))
	c := newClient(t, testCfg(srv.URL))
	if err := c.Healthy(context.Background()); err != nil {
		t.Fatal(err)
	}
	loginsBefore := fake.Logins.Load()
	const callers = 8
	fake.ExpireSessionConcurrently(callers)

	var wg sync.WaitGroup
	errs := make(chan error, callers)
	for i := 0; i < callers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := c.Status(context.Background(), nil)
			errs <- err
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Errorf("Status after concurrent expiry: %v", err)
		}
	}

	if got := fake.Logins.Load(); got != loginsBefore+1 {
		t.Errorf("%d callers observing one expired session caused %d new logins, want 1",
			callers, got-loginsBefore)
	}
}

func TestAddSendsMultipartAndComputesHash(t *testing.T) {
	fake, srv := newFakeQB(t, fixtureBody(t))
	c := newClient(t, testCfg(srv.URL))

	hash, err := c.Add(context.Background(), downloader.AddRequest{
		Magnet:   testMagnet,
		Category: "reelay-tv",
		SavePath: `\\Azeroth\Series\.reelay-downloads`,
		Paused:   false,
	})
	if err != nil {
		t.Fatalf("Add: %v", err)
	}

	// torrents/add returns no hash, so this must have come from the magnet.
	const want = "24f8d55d8b3f94e28f53e1d8ae821836bc69be99"
	if hash != want {
		t.Errorf("hash = %q, want %q computed from the magnet", hash, want)
	}

	fake.addMu.Lock()
	fields := fake.LastAdd
	fake.addMu.Unlock()

	if fields["urls"] != testMagnet {
		t.Errorf("urls = %q", fields["urls"])
	}
	if fields["category"] != "reelay-tv" {
		t.Errorf("category = %q", fields["category"])
	}
	if fields["savepath"] != `\\Azeroth\Series\.reelay-downloads` {
		t.Errorf("savepath = %q", fields["savepath"])
	}
	// Both spellings, so one binary works across the 4.x/5.x rename.
	if fields["paused"] != "false" || fields["stopped"] != "false" {
		t.Errorf("paused=%q stopped=%q; both should be sent", fields["paused"], fields["stopped"])
	}
}

func TestAddPausedSendsBothSpellings(t *testing.T) {
	fake, srv := newFakeQB(t, fixtureBody(t))
	c := newClient(t, testCfg(srv.URL))

	if _, err := c.Add(context.Background(), downloader.AddRequest{
		Magnet: testMagnet, Category: "reelay-movies", Paused: true,
	}); err != nil {
		t.Fatal(err)
	}
	fake.addMu.Lock()
	defer fake.addMu.Unlock()
	if fake.LastAdd["paused"] != "true" || fake.LastAdd["stopped"] != "true" {
		t.Errorf("paused=%q stopped=%q, want both true",
			fake.LastAdd["paused"], fake.LastAdd["stopped"])
	}
}

// The safety rule, on the way in: an unlabelled add would produce a torrent
// Reelay could never see again, and one it could not distinguish from the
// operator's own.
func TestAddRefusesForeignOrEmptyCategory(t *testing.T) {
	fake, srv := newFakeQB(t, fixtureBody(t))
	c := newClient(t, testCfg(srv.URL))

	for _, cat := range []string{"", "personal", "reelay-tv-typo"} {
		_, err := c.Add(context.Background(), downloader.AddRequest{Magnet: testMagnet, Category: cat})
		if err == nil {
			t.Errorf("Add with category %q should have been refused", cat)
		}
	}
	if got := fake.Adds.Load(); got != 0 {
		t.Errorf("%d adds reached the client; none should have", got)
	}
}

func TestAddRejectsBadMagnet(t *testing.T) {
	_, srv := newFakeQB(t, fixtureBody(t))
	c := newClient(t, testCfg(srv.URL))

	for _, m := range []string{"", "http://example.com/x.torrent", "magnet:?dn=NoHash"} {
		if _, err := c.Add(context.Background(), downloader.AddRequest{
			Magnet: m, Category: "reelay-tv",
		}); err == nil {
			t.Errorf("Add with magnet %q should have failed", m)
		}
	}
}

// The safety rule, on the way out. The recorded fixture deliberately contains
// two torrents that are not ours.
func TestStatusHidesTorrentsThatAreNotOurs(t *testing.T) {
	_, srv := newFakeQB(t, fixtureBody(t))
	c := newClient(t, testCfg(srv.URL))

	got, err := c.Status(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) == 0 {
		t.Fatal("no torrents returned")
	}
	for _, s := range got {
		if s.Category != "reelay-tv" && s.Category != "reelay-movies" {
			t.Errorf("torrent %q with category %q leaked through the ownership filter", s.Name, s.Category)
		}
	}
	// The fixture has 11 rows, 2 of which are not ours.
	if len(got) != 9 {
		t.Errorf("got %d torrents, want 9 of the fixture's 11", len(got))
	}
}

func TestStatusNormalisesStates(t *testing.T) {
	_, srv := newFakeQB(t, fixtureBody(t))
	c := newClient(t, testCfg(srv.URL))

	got, err := c.Status(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}

	byName := map[string]downloader.TorrentStatus{}
	for _, s := range got {
		byName[s.Name] = s
	}

	cases := map[string]string{
		"Some.Show.S01E01.1080p.WEB-DL.x264-GRP": downloader.StateDownloading,
		"Some.Show.S01E02.1080p.WEB-DL.x264-GRP": downloader.StateStalled,
		"Some.Show.S01E03.1080p.WEB-DL.x264-GRP": downloader.StatePaused,
		"Some.Film.2024.1080p.BluRay.x264-GRP":   downloader.StateSeeding,
		"Some.Film.2023.1080p.BluRay.x264-GRP":   downloader.StateSeeding,
		"Some.Film.2022.1080p.BluRay.x264-GRP":   downloader.StateCompleted,
		"Broken.Release.S01E01.1080p-GRP":        downloader.StateError,
		"Vanished.Release.S01E01.1080p-GRP":      downloader.StateError,
		"Metadata.Only.S01E01.1080p-GRP":         downloader.StateDownloading,
	}
	for name, want := range cases {
		s, ok := byName[name]
		if !ok {
			t.Errorf("fixture torrent %q missing from the result", name)
			continue
		}
		if s.State != want {
			t.Errorf("%q: state = %q, want %q", name, s.State, want)
		}
	}

	// missingFiles must carry an explanation: "error" alone sends the operator
	// looking in the wrong place.
	if s := byName["Vanished.Release.S01E01.1080p-GRP"]; !strings.Contains(s.ErrorMessage, "missing files") {
		t.Errorf("missingFiles error message = %q", s.ErrorMessage)
	}
}

func TestCompleteRequiresBothStateAndProgress(t *testing.T) {
	_, srv := newFakeQB(t, fixtureBody(t))
	c := newClient(t, testCfg(srv.URL))
	got, _ := c.Status(context.Background(), nil)

	for _, s := range got {
		switch s.Name {
		case "Some.Film.2024.1080p.BluRay.x264-GRP", // uploading, progress 1
			"Some.Film.2023.1080p.BluRay.x264-GRP", // stalledUP, progress 1
			"Some.Film.2022.1080p.BluRay.x264-GRP": // pausedUP, progress 1
			if !s.Complete() {
				t.Errorf("%q should be complete (state %s, progress %v)", s.Name, s.State, s.Progress)
			}
		case "Some.Show.S01E01.1080p.WEB-DL.x264-GRP": // downloading, 0.42
			if s.Complete() {
				t.Errorf("%q is 42%% downloaded and must not be complete", s.Name)
			}
		}
	}

	// A seeding state with partial progress is a torrent still verifying.
	// Importing it would produce a corrupt library entry.
	partial := downloader.TorrentStatus{State: downloader.StateSeeding, Progress: 0.98}
	if partial.Complete() {
		t.Error("a seeding torrent at 98% must not report complete")
	}
}

func TestStatusPassesHashesAsPipeSeparated(t *testing.T) {
	fake, srv := newFakeQB(t, fixtureBody(t))
	c := newClient(t, testCfg(srv.URL))

	if _, err := c.Status(context.Background(), []string{"AAA111", "bbb222", "  ", ""}); err != nil {
		t.Fatal(err)
	}
	fake.addMu.Lock()
	q := fake.LastQuery
	fake.addMu.Unlock()

	// Lowercased, blanks dropped, pipe-separated.
	if !strings.Contains(q, "hashes=aaa111%7Cbbb222") {
		t.Errorf("query = %q, want lowercased pipe-separated hashes", q)
	}
}

func TestStatusWithNoHashesRequestsEverything(t *testing.T) {
	fake, srv := newFakeQB(t, fixtureBody(t))
	c := newClient(t, testCfg(srv.URL))

	if _, err := c.Status(context.Background(), nil); err != nil {
		t.Fatal(err)
	}
	fake.addMu.Lock()
	defer fake.addMu.Unlock()
	if strings.Contains(fake.LastQuery, "hashes=") {
		t.Errorf("query = %q, want no hashes filter", fake.LastQuery)
	}
}

// All-blank input must not degrade into "fetch everything", which for a delete
// path would be catastrophic and for a poll is merely wasteful.
func TestStatusWithOnlyBlankHashesRequestsNothing(t *testing.T) {
	fake, srv := newFakeQB(t, fixtureBody(t))
	c := newClient(t, testCfg(srv.URL))

	got, err := c.Status(context.Background(), []string{"", "   "})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 0 {
		t.Errorf("got %d torrents, want none", len(got))
	}
	if fake.Infos.Load() != 0 {
		t.Error("no request should have been made for an all-blank hash list")
	}
}

// The single most dangerous call in the codebase.
func TestRemoveRefusesTorrentsThatAreNotOurs(t *testing.T) {
	fake, srv := newFakeQB(t, fixtureBody(t))
	c := newClient(t, testCfg(srv.URL))

	// The fixture deliberately contains an uncategorised torrent and one
	// labelled "personal" — the shape of a real client, where 21 of the
	// operator's own torrents sit alongside ours.
	foreign := foreignHashFromFixture(t, fixtureBody(t))
	err := c.Remove(context.Background(), foreign, true)
	if !errors.Is(err, downloader.ErrNotOurs) {
		t.Fatalf("Remove on a foreign torrent = %v, want ErrNotOurs", err)
	}
	if fake.Deletes.Load() != 0 {
		t.Fatal("a delete reached the client for a torrent that is not ours")
	}
}

func TestRemoveOurOwnTorrent(t *testing.T) {
	fake, srv := newFakeQB(t, fixtureBody(t))
	c := newClient(t, testCfg(srv.URL))

	ours := ourHashFromFixture(t, fixtureBody(t))
	if err := c.Remove(context.Background(), ours, false); err != nil {
		t.Fatalf("Remove: %v", err)
	}
	if got := fake.Deletes.Load(); got != 1 {
		t.Errorf("%d deletes issued, want 1", got)
	}
}

func TestSetPausedOnlyActsOnOwnedTorrents(t *testing.T) {
	fake, srv := newFakeQB(t, fixtureBody(t))
	c := newClient(t, testCfg(srv.URL))
	ours := ourHashFromFixture(t, fixtureBody(t))
	foreign := foreignHashFromFixture(t, fixtureBody(t))

	if err := c.SetPaused(context.Background(), []string{ours, foreign}, true); err != nil {
		t.Fatal(err)
	}
	if fake.Controls.Load() != 1 || fake.LastControlPath != "/api/v2/torrents/stop" || fake.LastControlHashes != ours {
		t.Fatalf("controls=%d path=%q hashes=%q", fake.Controls.Load(), fake.LastControlPath, fake.LastControlHashes)
	}
	if err := c.SetPaused(context.Background(), []string{foreign}, false); err != nil {
		t.Fatal(err)
	}
	if fake.Controls.Load() != 1 {
		t.Fatal("foreign torrent reached a pause/resume endpoint")
	}
}

func TestSetPausedWithNoHashesDoesNotFetchOrMutateAll(t *testing.T) {
	fake, srv := newFakeQB(t, fixtureBody(t))
	c := newClient(t, testCfg(srv.URL))
	if err := c.SetPaused(context.Background(), nil, true); err != nil {
		t.Fatal(err)
	}
	if fake.Infos.Load() != 0 || fake.Controls.Load() != 0 {
		t.Fatalf("empty pause fetched=%d controlled=%d", fake.Infos.Load(), fake.Controls.Load())
	}
}

func TestSetPausedFallsBackForOlderQBittorrent(t *testing.T) {
	fake, srv := newFakeQB(t, fixtureBody(t))
	fake.LegacyControlOnly = true
	c := newClient(t, testCfg(srv.URL))
	if err := c.SetPaused(context.Background(), []string{ourHashFromFixture(t, fixtureBody(t))}, false); err != nil {
		t.Fatal(err)
	}
	if fake.Controls.Load() != 1 || fake.LastControlPath != "/api/v2/torrents/resume" {
		t.Fatalf("controls=%d path=%q", fake.Controls.Load(), fake.LastControlPath)
	}
}

func TestRemoveUnknownHash(t *testing.T) {
	// An empty info response means the client has never heard of it.
	_, srv := newFakeQB(t, []byte("[]"))
	c := newClient(t, testCfg(srv.URL))

	err := c.Remove(context.Background(), "0000000000000000000000000000000000000000", true)
	if !errors.Is(err, downloader.ErrNotFound) {
		t.Errorf("error = %v, want ErrNotFound", err)
	}
}

func TestRemoveRejectsEmptyHash(t *testing.T) {
	fake, srv := newFakeQB(t, fixtureBody(t))
	c := newClient(t, testCfg(srv.URL))

	if err := c.Remove(context.Background(), "   ", true); err == nil {
		t.Error("Remove with an empty hash should fail")
	}
	if fake.Deletes.Load() != 0 || fake.Infos.Load() != 0 {
		t.Error("an empty hash must not reach the client at all")
	}
}

func TestEnsureCategoryIsIdempotent(t *testing.T) {
	_, srv := newFakeQB(t, fixtureBody(t))
	cfg := testCfg(srv.URL)
	cfg.CategoryTV = "already-there"
	c := newClient(t, cfg)

	// The fake answers 409 for this name, which means "already exists" and
	// must not be an error.
	if err := c.EnsureCategory(context.Background(), "already-there", `D:\dl`); err != nil {
		t.Errorf("a duplicate category should be treated as success, got: %v", err)
	}
}

func TestEnsureCategoryRefusesForeignNames(t *testing.T) {
	_, srv := newFakeQB(t, fixtureBody(t))
	c := newClient(t, testCfg(srv.URL))

	if err := c.EnsureCategory(context.Background(), "someone-elses", `D:\dl`); err == nil {
		t.Error("EnsureCategory should refuse a category Reelay does not own")
	}
}

func TestNewValidation(t *testing.T) {
	cases := []struct {
		name string
		cfg  config.Downloader
	}{
		{"empty url", config.Downloader{CategoryTV: "a"}},
		{"relative url", config.Downloader{URL: "/api", CategoryTV: "a"}},
		{"no categories", config.Downloader{URL: "http://x:8080"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := New(tc.cfg, Options{}); err == nil {
				t.Error("expected New to reject this config")
			}
		})
	}
}

func TestContextCancellation(t *testing.T) {
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/v2/auth/login" {
			http.SetCookie(w, &http.Cookie{Name: "SID", Value: testSID, Path: "/"})
			_, _ = io.WriteString(w, "Ok.")
			return
		}
		select {
		case <-release:
		case <-r.Context().Done():
		}
	}))
	defer srv.Close()
	defer close(release)

	c := newClient(t, testCfg(srv.URL))
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, err := c.Status(ctx, nil)
		done <- err
	}()
	cancel()

	select {
	case err := <-done:
		if err == nil {
			t.Error("Status should fail on a cancelled context")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Status ignored context cancellation")
	}
}

func TestContentPathFallback(t *testing.T) {
	// content_path arrived in 4.x; an older client omits it, and an empty
	// path would send the importer looking in the working directory.
	windows := infoRow{SavePath: `D:\downloads\tv\`, Name: "Show.S01E01"}
	if got := contentPath(windows); got != `D:\downloads\tv\Show.S01E01` {
		t.Errorf("windows fallback = %q", got)
	}
	posix := infoRow{SavePath: "/downloads/tv", Name: "Show.S01E01"}
	if got := contentPath(posix); got != "/downloads/tv/Show.S01E01" {
		t.Errorf("posix fallback = %q", got)
	}
	explicit := infoRow{ContentPath: "/explicit/path", SavePath: "/ignored", Name: "x"}
	if got := contentPath(explicit); got != "/explicit/path" {
		t.Errorf("content_path should win, got %q", got)
	}
	if got := contentPath(infoRow{}); got != "" {
		t.Errorf("with nothing to work from the path should be empty, got %q", got)
	}
}

func TestInfiniteETAIsReportedAsNone(t *testing.T) {
	// Reported verbatim, qBittorrent's 8640000 sentinel renders as an ETA of
	// 2400 hours, which is worse than reporting none at all.
	if got := (infoRow{ETA: etaInfinite}).toStatus().ETA; got != 0 {
		t.Errorf("ETA = %s, want 0 for the infinite sentinel", got)
	}
	if got := (infoRow{ETA: 1830}).toStatus().ETA; got != 30*time.Minute+30*time.Second {
		t.Errorf("ETA = %s, want 30m30s", got)
	}
}

func TestPrefersInfohashV1(t *testing.T) {
	// For a hybrid torrent, `hash` can be the v2 hash while our magnets and
	// our database key on v1.
	r := infoRow{Hash: "BBBB", InfohashV1: "AAAA"}
	if got := r.toStatus().Hash; got != "aaaa" {
		t.Errorf("hash = %q, want the lowercased infohash_v1", got)
	}
	r = infoRow{Hash: "CCCC", InfohashV1: ""}
	if got := r.toStatus().Hash; got != "cccc" {
		t.Errorf("with no infohash_v1 it should fall back to hash, got %q", got)
	}
}

func TestUnknownStateIsNotGuessed(t *testing.T) {
	// Guessing "downloading" would make the engine wait forever on something
	// that will never finish.
	if got := normaliseState("someNewStateUpstreamAdded"); got != downloader.StateUnknown {
		t.Errorf("unknown state mapped to %q, want %q", got, downloader.StateUnknown)
	}
}

// qBittorrent 5.0 renamed pausedDL/pausedUP. One binary has to handle both.
func TestBothPausedSpellingsAreHandled(t *testing.T) {
	pairs := map[string]string{
		"pausedDL":  downloader.StatePaused,
		"stoppedDL": downloader.StatePaused,
		"pausedUP":  downloader.StateCompleted,
		"stoppedUP": downloader.StateCompleted,
	}
	for raw, want := range pairs {
		if got := normaliseState(raw); got != want {
			t.Errorf("normaliseState(%q) = %q, want %q", raw, got, want)
		}
	}
}

// A magnet whose metadata has not resolved yet reports total_size as -1, which
// rendered as "-1 B" in the progress line during the first live run.
func TestUnresolvedMetadataSizeIsNotNegative(t *testing.T) {
	st := (infoRow{TotalSize: -1, Size: -1, Downloaded: -1}).toStatus()
	if st.TotalBytes != 0 {
		t.Errorf("TotalBytes = %d, want 0 for unresolved metadata", st.TotalBytes)
	}
	if st.DownloadedBytes != 0 {
		t.Errorf("DownloadedBytes = %d, want 0", st.DownloadedBytes)
	}
	// total_size zero with a real size still uses the size.
	st = (infoRow{TotalSize: 0, Size: 4096}).toStatus()
	if st.TotalBytes != 4096 {
		t.Errorf("TotalBytes = %d, want the fallback size 4096", st.TotalBytes)
	}
}

func TestProgressIsClamped(t *testing.T) {
	for in, want := range map[float64]float64{-0.5: 0, 0: 0, 0.42: 0.42, 1: 1, 1.5: 1} {
		if got := clampProgress(in); got != want {
			t.Errorf("clampProgress(%v) = %v, want %v", in, got, want)
		}
	}
}

// Helpers that dig a hash out of the fixture, so the tests do not hard-code
// values the generator produced.

func foreignHashFromFixture(t *testing.T, body []byte) string {
	t.Helper()
	return hashWhere(t, body, func(cat string) bool {
		return cat != "reelay-tv" && cat != "reelay-movies"
	})
}

func ourHashFromFixture(t *testing.T, body []byte) string {
	t.Helper()
	return hashWhere(t, body, func(cat string) bool {
		return cat == "reelay-tv" || cat == "reelay-movies"
	})
}

func hashWhere(t *testing.T, body []byte, pred func(category string) bool) string {
	t.Helper()
	var rows []infoRow
	if err := json.Unmarshal(body, &rows); err != nil {
		t.Fatalf("decode fixture: %v", err)
	}
	for _, r := range rows {
		if pred(r.Category) {
			return r.Hash
		}
	}
	t.Fatal("fixture contains no matching row")
	return ""
}
