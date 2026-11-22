package api

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/TechXTT/reelay/internal/clock"
	"github.com/TechXTT/reelay/internal/config"
	"github.com/TechXTT/reelay/internal/jellyfinauth"
	"github.com/TechXTT/reelay/internal/model"
	"github.com/TechXTT/reelay/internal/store"
)

type fakeVerifier struct {
	users map[string]jellyfinauth.User // "username:password"
	calls atomic.Int32
}

func (f *fakeVerifier) Authenticate(_ context.Context, _, username, password string) (jellyfinauth.User, error) {
	f.calls.Add(1)
	user, ok := f.users[username+":"+password]
	if !ok {
		return jellyfinauth.User{}, jellyfinauth.ErrInvalidCredentials
	}
	return user, nil
}

type authFixture struct {
	srv      *Server
	st       *store.Store
	verifier *fakeVerifier
}

func newAuthFixture(t *testing.T, mutate func(*config.Config)) *authFixture {
	t.Helper()
	srv, st := newTestServer(t, func(c *config.Config) {
		c.Library.MovieRoot = t.TempDir()
		c.Library.TVRoot = t.TempDir()
		c.Availability.JellyfinServers = map[string]string{"server": "http://jellyfin.invalid"}
		c.Auth = config.Auth{LoginsEnabled: true, SessionTTL: config.Dur(24 * time.Hour)}
		if mutate != nil {
			mutate(c)
		}
	})
	verifier := &fakeVerifier{users: map[string]jellyfinauth.User{
		"alice:pw": {ID: "alice", Name: "Alice"},
		"bob:pw":   {ID: "bob", Name: "Bob"},
		"carol:pw": {ID: "carol", Name: "Carol"},
		"admin:pw": {ID: "admin", Name: "Admin", Administrator: true},
	}}
	srv.loginVerifier = verifier
	srv.movies = recommendationMovieProvider{}
	srv.discovery, srv.externalSeries = requestDiscoveryStub{}, requestSeriesStub{}
	addRequestUser(t, st, "server", "alice")
	addRequestUser(t, st, "server", "bob")
	seedRequestProfile(t, st)
	return &authFixture{srv: srv, st: st, verifier: verifier}
}

type call struct {
	method  string
	path    string
	body    any
	cookie  string
	bearer  bool
	csrf    bool
	headers map[string]string
}

func (f *authFixture) do(t *testing.T, c call) *httptest.ResponseRecorder {
	t.Helper()
	var body bytes.Reader
	if c.body != nil {
		raw, err := json.Marshal(c.body)
		if err != nil {
			t.Fatal(err)
		}
		body = *bytes.NewReader(raw)
	}
	req := httptest.NewRequest(c.method, c.path, &body)
	req.Header.Set("Content-Type", "application/json")
	if c.bearer {
		req.Header.Set("Authorization", "Bearer "+testToken)
	}
	if c.cookie != "" {
		req.AddCookie(&http.Cookie{Name: sessionCookie, Value: c.cookie})
	}
	if c.csrf {
		req.Header.Set(csrfHeader, "1")
	}
	for k, v := range c.headers {
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	f.srv.Handler().ServeHTTP(rec, req)
	return rec
}

// login signs in and returns the session cookie value.
func (f *authFixture) login(t *testing.T, username string) string {
	t.Helper()
	rec := f.do(t, call{method: http.MethodPost, path: pathAuthLogin, body: map[string]string{"username": username, "password": "pw"}})
	if rec.Code != http.StatusOK {
		t.Fatalf("login %s: %d %s", username, rec.Code, rec.Body.String())
	}
	for _, c := range rec.Result().Cookies() {
		if c.Name == sessionCookie {
			return c.Value
		}
	}
	t.Fatalf("login %s set no session cookie", username)
	return ""
}

func (f *authFixture) member(t *testing.T, cookie, method, path string, body any) *httptest.ResponseRecorder {
	t.Helper()
	return f.do(t, call{method: method, path: path, body: body, cookie: cookie, csrf: true})
}

func (f *authFixture) seedRecommendations(t *testing.T, userID, mediaType string, tmdbIDs ...int) []model.Recommendation {
	t.Helper()
	var recs []model.Recommendation
	for _, id := range tmdbIDs {
		recs = append(recs, model.Recommendation{TMDBID: id, Title: "Title " + strconv.Itoa(id), Score: 70, Reasons: []string{"reason"},
			GeneratedAt: time.Now(), ExpiresAt: time.Now().Add(time.Hour)})
	}
	if err := f.st.Recommendations().Replace(context.Background(), "server", userID, mediaType, recs); err != nil {
		t.Fatal(err)
	}
	values, err := f.st.Recommendations().List(context.Background(), "server", userID, mediaType, "active", 50, 0)
	if err != nil || len(values) != len(tmdbIDs) {
		t.Fatalf("seeded recommendations = %+v, %v", values, err)
	}
	return values
}

func recPath(rec model.Recommendation) string {
	return fmt.Sprintf("/api/v1/recommendations/%d/actions", rec.ID)
}

func decode[T any](t *testing.T, rec *httptest.ResponseRecorder) T {
	t.Helper()
	var out T
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode %q: %v", rec.Body.String(), err)
	}
	return out
}

func errorCode(t *testing.T, rec *httptest.ResponseRecorder) string {
	t.Helper()
	return decode[errorEnvelope](t, rec).Error.Code
}

func TestBearerTokenIsAnOperator(t *testing.T) {
	fx := newAuthFixture(t, nil)

	rec := fx.do(t, call{method: http.MethodGet, path: "/api/v1/auth/me", bearer: true})
	got := decode[Principal](t, rec)
	if rec.Code != http.StatusOK || got.Role != roleOperator || got.Session {
		t.Fatalf("me = %d %+v", rec.Code, got)
	}
	if rec := fx.do(t, call{method: http.MethodGet, path: "/api/v1/auth/me"}); rec.Code != http.StatusUnauthorized {
		t.Errorf("anonymous me = %d, want 401", rec.Code)
	}
	if rec := fx.do(t, call{method: http.MethodGet, path: "/api/v1/queue", headers: map[string]string{"Authorization": "Bearer wrong"}}); rec.Code != http.StatusUnauthorized {
		t.Errorf("wrong bearer = %d, want 401", rec.Code)
	}
	if rec := fx.do(t, call{method: http.MethodGet, path: "/api/v1/queue", bearer: true}); rec.Code != http.StatusOK {
		t.Errorf("operator queue = %d", rec.Code)
	}
}

func TestNoTokenModeStaysOperator(t *testing.T) {
	srv, _ := newTestServer(t, func(c *config.Config) { c.Server.AuthToken = "" })
	rec := do(t, srv.Handler(), http.MethodGet, "/api/v1/queue", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("queue without any token = %d", rec.Code)
	}
}

func TestLoginCreatesAMemberSession(t *testing.T) {
	fx := newAuthFixture(t, nil)

	rec := fx.do(t, call{method: http.MethodPost, path: pathAuthLogin, body: map[string]string{"username": "alice", "password": "pw"},
		headers: map[string]string{"X-Forwarded-Proto": "https"}})
	if rec.Code != http.StatusOK {
		t.Fatalf("login = %d %s", rec.Code, rec.Body.String())
	}
	got := decode[Principal](t, rec)
	if got.Role != roleMember || got.ServerID != "server" || got.UserID != "alice" || got.Name != "Alice" {
		t.Errorf("login principal = %+v (single configured server should be implied)", got)
	}
	var cookie *http.Cookie
	for _, c := range rec.Result().Cookies() {
		if c.Name == sessionCookie {
			cookie = c
		}
	}
	if cookie == nil || !cookie.HttpOnly || !cookie.Secure || cookie.SameSite != http.SameSiteStrictMode || cookie.Path != "/" || cookie.MaxAge != 24*3600 {
		t.Fatalf("cookie = %+v", cookie)
	}
	if len(cookie.Value) < 40 || strings.Contains(rec.Body.String(), cookie.Value) {
		t.Errorf("session token looks weak or leaked into the body: %q", cookie.Value)
	}
	stored, err := fx.st.Sessions().Get(context.Background(), hashToken(cookie.Value), fx.srv.clock.Now())
	if err != nil || stored.Role != roleMember || stored.TokenHash == cookie.Value {
		t.Fatalf("stored session = %+v, %v (only the hash may be stored)", stored, err)
	}

	me := fx.do(t, call{method: http.MethodGet, path: "/api/v1/auth/me", cookie: cookie.Value})
	if p := decode[Principal](t, me); me.Code != http.StatusOK || p.Role != roleMember || p.UserID != "alice" || !p.Session {
		t.Errorf("me via cookie = %d %+v", me.Code, p)
	}
}

func TestPlainHTTPLoginIsNotSecureCookie(t *testing.T) {
	fx := newAuthFixture(t, nil)
	rec := fx.do(t, call{method: http.MethodPost, path: pathAuthLogin, body: map[string]string{"username": "alice", "password": "pw"}})
	for _, c := range rec.Result().Cookies() {
		if c.Name == sessionCookie && c.Secure {
			t.Error("cookie marked Secure on a plain HTTP request")
		}
	}
}

func TestLoginRoles(t *testing.T) {
	fx := newAuthFixture(t, nil)

	admin := fx.login(t, "admin")
	if rec := fx.do(t, call{method: http.MethodGet, path: "/api/v1/auth/me", cookie: admin}); decode[Principal](t, rec).Role != roleOperator {
		t.Error("a Jellyfin administrator did not become an operator")
	}
	if rec := fx.do(t, call{method: http.MethodGet, path: "/api/v1/queue", cookie: admin}); rec.Code != http.StatusOK {
		t.Errorf("operator session queue = %d", rec.Code)
	}

	rec := fx.do(t, call{method: http.MethodPost, path: pathAuthLogin, body: map[string]string{"username": "carol", "password": "pw"}})
	if rec.Code != http.StatusForbidden || !strings.Contains(rec.Body.String(), "not enabled in Reelay") {
		t.Errorf("unknown Jellyfin user = %d %s", rec.Code, rec.Body.String())
	}
	if _, err := fx.st.Writer().ExecContext(context.Background(), `UPDATE jellyfin_users SET enabled=0 WHERE user_id='bob'`); err != nil {
		t.Fatal(err)
	}
	if rec := fx.do(t, call{method: http.MethodPost, path: pathAuthLogin, body: map[string]string{"username": "bob", "password": "pw"}}); rec.Code != http.StatusForbidden {
		t.Errorf("disabled user login = %d, want 403", rec.Code)
	}
}

func TestDisablingAUserRevokesTheirSession(t *testing.T) {
	fx := newAuthFixture(t, nil)
	cookie := fx.login(t, "bob")
	if _, err := fx.st.Writer().ExecContext(context.Background(), `UPDATE jellyfin_users SET enabled=0 WHERE user_id='bob'`); err != nil {
		t.Fatal(err)
	}
	if rec := fx.do(t, call{method: http.MethodGet, path: "/api/v1/auth/me", cookie: cookie}); rec.Code != http.StatusUnauthorized {
		t.Errorf("session of a disabled user = %d, want 401", rec.Code)
	}
}

func TestLoginDisabled(t *testing.T) {
	fx := newAuthFixture(t, func(c *config.Config) { c.Auth.LoginsEnabled = false })

	rec := fx.do(t, call{method: http.MethodPost, path: pathAuthLogin, body: map[string]string{"username": "alice", "password": "pw"}})
	if rec.Code != http.StatusNotFound || fx.verifier.calls.Load() != 0 {
		t.Errorf("login with logins disabled = %d, verifier calls = %d", rec.Code, fx.verifier.calls.Load())
	}
	cfg := decode[authConfigResponse](t, fx.do(t, call{method: http.MethodGet, path: pathAuthConfig}))
	if cfg.LoginsEnabled || len(cfg.Servers) != 0 {
		t.Errorf("auth config = %+v, want disabled and no server ids", cfg)
	}
	// A cookie from an earlier configuration must not authenticate.
	if err := fx.st.Sessions().Create(context.Background(), store.Session{TokenHash: hashToken("stale"), Role: roleOperator, ServerID: "server",
		UserID: "admin", DisplayName: "Admin", CreatedAt: fx.srv.clock.Now(), ExpiresAt: fx.srv.clock.Now().Add(time.Hour), LastSeenAt: fx.srv.clock.Now()}); err != nil {
		t.Fatal(err)
	}
	if rec := fx.do(t, call{method: http.MethodGet, path: "/api/v1/queue", cookie: "stale"}); rec.Code != http.StatusUnauthorized {
		t.Errorf("cookie with logins disabled = %d, want 401", rec.Code)
	}
}

func TestAuthConfigListsServersWhenEnabled(t *testing.T) {
	fx := newAuthFixture(t, nil)
	cfg := decode[authConfigResponse](t, fx.do(t, call{method: http.MethodGet, path: pathAuthConfig}))
	if !cfg.LoginsEnabled || len(cfg.Servers) != 1 || cfg.Servers[0] != "server" {
		t.Errorf("auth config = %+v", cfg)
	}
}

func TestLoginRequiresServerWhenSeveralAreConfigured(t *testing.T) {
	fx := newAuthFixture(t, func(c *config.Config) {
		c.Availability.JellyfinServers = map[string]string{"server": "http://a.invalid", "other": "http://b.invalid"}
	})
	rec := fx.do(t, call{method: http.MethodPost, path: pathAuthLogin, body: map[string]string{"username": "alice", "password": "pw"}})
	if rec.Code != http.StatusBadRequest {
		t.Errorf("ambiguous server = %d, want 400", rec.Code)
	}
	rec = fx.do(t, call{method: http.MethodPost, path: pathAuthLogin, body: map[string]string{"server_id": "server", "username": "alice", "password": "pw"}})
	if rec.Code != http.StatusOK {
		t.Errorf("explicit server = %d %s", rec.Code, rec.Body.String())
	}
}

func TestExpiredSessionIsRejected(t *testing.T) {
	fx := newAuthFixture(t, nil)
	now := fx.srv.clock.Now()
	err := fx.st.Sessions().Create(context.Background(), store.Session{TokenHash: hashToken("expired"), Role: roleMember, ServerID: "server",
		UserID: "alice", DisplayName: "Alice", CreatedAt: now.Add(-2 * time.Hour), ExpiresAt: now.Add(-time.Hour), LastSeenAt: now.Add(-2 * time.Hour)})
	if err != nil {
		t.Fatal(err)
	}
	rec := fx.do(t, call{method: http.MethodGet, path: "/api/v1/recommendations", cookie: "expired"})
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("expired session = %d, want 401", rec.Code)
	}
	cleared := false
	for _, c := range rec.Result().Cookies() {
		cleared = cleared || (c.Name == sessionCookie && c.MaxAge < 0)
	}
	if !cleared {
		t.Error("expired session did not clear the cookie")
	}
	if rec := fx.do(t, call{method: http.MethodGet, path: "/api/v1/recommendations", cookie: "never-issued"}); rec.Code != http.StatusUnauthorized {
		t.Errorf("unknown session = %d, want 401", rec.Code)
	}
}

func TestCSRFHeaderRequiredForCookiePostsOnly(t *testing.T) {
	fx := newAuthFixture(t, nil)
	cookie := fx.login(t, "alice")
	rec := fx.seedRecommendations(t, "alice", "movie", 11)[0]
	path := recPath(rec)
	body := map[string]string{"action_id": "dismiss-1", "action": "dismiss"}

	denied := fx.do(t, call{method: http.MethodPost, path: path, body: body, cookie: cookie})
	if denied.Code != http.StatusForbidden || errorCode(t, denied) != CodeForbidden {
		t.Fatalf("cookie POST without the header = %d %s", denied.Code, denied.Body.String())
	}
	if rec := fx.do(t, call{method: http.MethodPost, path: path, body: body, cookie: cookie, csrf: true}); rec.Code != http.StatusOK {
		t.Errorf("cookie POST with the header = %d %s", rec.Code, rec.Body.String())
	}
	// Reads need no header.
	if rec := fx.do(t, call{method: http.MethodGet, path: "/api/v1/recommendations", cookie: cookie}); rec.Code != http.StatusOK {
		t.Errorf("cookie GET = %d", rec.Code)
	}
	// Bearer requests are exempt.
	if rec := fx.do(t, call{method: http.MethodPost, path: path, body: map[string]string{"action_id": "undo-1", "action": "undo"}, bearer: true}); rec.Code != http.StatusOK {
		t.Errorf("bearer POST without the header = %d %s", rec.Code, rec.Body.String())
	}
}

func TestMemberCannotReachOperatorRoutes(t *testing.T) {
	fx := newAuthFixture(t, nil)
	cookie := fx.login(t, "alice")

	for _, route := range []string{
		"GET /api/v1/health", "GET /api/v1/events", "GET /api/v1/metrics", "GET /api/v1/setup", "GET /api/v1/settings",
		"GET /api/v1/queue", "POST /api/v1/queue/pause", "GET /api/v1/history", "GET /api/v1/series", "POST /api/v1/series",
		"GET /api/v1/movies", "GET /api/v1/profiles", "GET /api/v1/search", "GET /api/v1/metadata/search",
		"GET /api/v1/upgrades", "POST /api/v1/upgrades/movie/1/search", "POST /api/v1/database/backup",
		"POST /api/v1/system/trigger/search", "POST /api/v1/integrations/jellyfin/sync", "POST /api/v1/integrations/jellyfin/events",
		"POST /api/v1/integrations/jellyfin/trial-playback", "POST /api/v1/recommendations/generate",
		"POST /api/v1/requests/1/grab", "POST /api/v1/episodes/1/grab", "GET /api/v1/episodes/1/candidates",
		"POST /api/v1/movies/1/search", "DELETE /api/v1/movies/1",
	} {
		method, path, _ := strings.Cut(route, " ")
		rec := fx.member(t, cookie, method, path, map[string]string{})
		if rec.Code != http.StatusForbidden || errorCode(t, rec) != CodeForbidden {
			t.Errorf("member %s = %d %s, want 403 forbidden", route, rec.Code, rec.Body.String())
		}
	}
	// An operator session passes the same gate.
	admin := fx.login(t, "admin")
	if rec := fx.member(t, admin, http.MethodGet, "/api/v1/metrics", nil); rec.Code == http.StatusForbidden {
		t.Errorf("operator session blocked from metrics: %d", rec.Code)
	}
}

func TestMemberRoutesAreReachable(t *testing.T) {
	fx := newAuthFixture(t, nil)
	cookie := fx.login(t, "alice")

	for _, route := range []string{
		"GET /api/v1/ping", "GET /api/v1/auth/me", "GET /api/v1/recommendations", "GET /api/v1/recommendations/stats",
		"GET /api/v1/recommendations/history?media_type=movie", "GET /api/v1/requests", "GET /api/v1/trials",
		"GET /api/v1/integrations/jellyfin/users",
	} {
		method, path, _ := strings.Cut(route, " ")
		if rec := fx.member(t, cookie, method, path, nil); rec.Code != http.StatusOK {
			t.Errorf("member %s = %d %s", route, rec.Code, rec.Body.String())
		}
	}
	users := decode[struct {
		Items []model.JellyfinUser `json:"items"`
	}](t, fx.member(t, cookie, http.MethodGet, "/api/v1/integrations/jellyfin/users", nil))
	if len(users.Items) != 1 || users.Items[0].UserID != "alice" {
		t.Errorf("member user list = %+v, want only themselves", users.Items)
	}
	all := decode[struct {
		Items []model.JellyfinUser `json:"items"`
	}](t, fx.do(t, call{method: http.MethodGet, path: "/api/v1/integrations/jellyfin/users", bearer: true}))
	if len(all.Items) != 2 {
		t.Errorf("operator user list = %+v, want everyone", all.Items)
	}
	prefs := fx.member(t, cookie, http.MethodPut, "/api/v1/recommendations/preferences", map[string]any{})
	if prefs.Code == http.StatusForbidden || prefs.Code == http.StatusUnauthorized {
		t.Errorf("member preferences = %d %s", prefs.Code, prefs.Body.String())
	}
}

func TestMemberScopeIsEnforced(t *testing.T) {
	fx := newAuthFixture(t, nil)
	cookie := fx.login(t, "alice")
	fx.seedRecommendations(t, "alice", "movie", 21)
	fx.seedRecommendations(t, "bob", "movie", 22)

	for _, query := range []string{
		"?user_id=bob", "?server_id=server&user_id=bob", "?server_id=elsewhere&user_id=alice", "?server_id=elsewhere",
	} {
		for _, path := range []string{"/api/v1/recommendations", "/api/v1/requests", "/api/v1/trials", "/api/v1/recommendations/stats", "/api/v1/recommendations/preferences"} {
			if rec := fx.member(t, cookie, http.MethodGet, path+query, nil); rec.Code != http.StatusForbidden {
				t.Errorf("member GET %s%s = %d, want 403", path, query, rec.Code)
			}
		}
	}
	if rec := fx.member(t, cookie, http.MethodGet, "/api/v1/recommendations/history?user_id=bob&media_type=movie", nil); rec.Code != http.StatusForbidden {
		t.Errorf("member history of another user = %d, want 403", rec.Code)
	}

	for _, query := range []string{"", "?server_id=server&user_id=alice", "?user_id=alice"} {
		rec := fx.member(t, cookie, http.MethodGet, "/api/v1/recommendations"+query, nil)
		got := decode[struct {
			Items []model.Recommendation `json:"items"`
		}](t, rec)
		if rec.Code != http.StatusOK || len(got.Items) != 1 || got.Items[0].UserID != "alice" {
			t.Errorf("member own list%s = %d %+v", query, rec.Code, got.Items)
		}
	}
	// Operators still have to name the user.
	if rec := fx.do(t, call{method: http.MethodGet, path: "/api/v1/recommendations", bearer: true}); rec.Code != http.StatusBadRequest {
		t.Errorf("operator without scope = %d, want 400", rec.Code)
	}
	if rec := fx.do(t, call{method: http.MethodGet, path: "/api/v1/recommendations?server_id=server&user_id=bob", bearer: true}); rec.Code != http.StatusOK {
		t.Errorf("operator with scope = %d", rec.Code)
	}
}

func TestMemberCannotSeeAnotherMembersRecords(t *testing.T) {
	fx := newAuthFixture(t, nil)
	ctx := context.Background()
	alice := fx.login(t, "alice")
	bob := fx.login(t, "bob")
	bobMovie := fx.seedRecommendations(t, "bob", "movie", 31)[0]
	bobSeries := fx.seedRecommendations(t, "bob", "series", 32)[0]

	// Recommendations belong to their owner.
	for _, rec := range []*httptest.ResponseRecorder{
		fx.member(t, alice, http.MethodPost, recPath(bobMovie), map[string]string{"action_id": "x", "action": "dismiss"}),
		fx.member(t, alice, http.MethodGet, fmt.Sprintf("/api/v1/recommendations/%d/preview", bobMovie.ID), nil),
	} {
		if rec.Code != http.StatusNotFound {
			t.Errorf("foreign recommendation = %d %s, want 404", rec.Code, rec.Body.String())
		}
	}

	// Bob requests a movie and starts a series trial as himself.
	if rec := fx.member(t, bob, http.MethodPost, recPath(bobMovie), map[string]string{"action_id": "bob-movie", "action": "request"}); rec.Code != http.StatusOK {
		t.Fatalf("bob request = %d %s", rec.Code, rec.Body.String())
	}
	if rec := fx.member(t, bob, http.MethodPost, recPath(bobSeries), map[string]string{"action_id": "bob-trial", "action": "request", "trial": "one_episode"}); rec.Code != http.StatusOK {
		t.Fatalf("bob trial = %d %s", rec.Code, rec.Body.String())
	}
	requests, err := fx.st.Requests().List(ctx, "server", "bob")
	if err != nil || len(requests) != 2 {
		t.Fatalf("bob requests = %+v, %v", requests, err)
	}
	trial, err := fx.st.Requests().TrialForTitle(ctx, "server", "bob", 32)
	if err != nil || trial == nil {
		t.Fatalf("bob trial = %+v, %v", trial, err)
	}

	for _, r := range requests {
		for _, rec := range []*httptest.ResponseRecorder{
			fx.member(t, alice, http.MethodPost, fmt.Sprintf("/api/v1/requests/%d/actions", r.ID), map[string]string{"action": "cancel"}),
			fx.member(t, alice, http.MethodGet, fmt.Sprintf("/api/v1/requests/%d/diagnostics", r.ID), nil),
		} {
			if rec.Code != http.StatusNotFound {
				t.Errorf("foreign request %d = %d %s, want 404", r.ID, rec.Code, rec.Body.String())
			}
		}
		if rec := fx.member(t, bob, http.MethodGet, fmt.Sprintf("/api/v1/requests/%d/diagnostics", r.ID), nil); rec.Code == http.StatusNotFound || rec.Code == http.StatusForbidden {
			t.Errorf("own request %d diagnostics = %d %s", r.ID, rec.Code, rec.Body.String())
		}
	}
	vote := map[string]any{"decision": "continue", "rating": 4}
	votePath := fmt.Sprintf("/api/v1/trials/%d/vote", trial.RequestID)
	if rec := fx.member(t, alice, http.MethodPost, votePath, vote); rec.Code != http.StatusNotFound {
		t.Errorf("foreign trial vote = %d %s, want 404", rec.Code, rec.Body.String())
	}
	if rec := fx.member(t, bob, http.MethodPost, votePath, vote); rec.Code != http.StatusConflict {
		t.Errorf("own trial vote before watching = %d %s, want the normal 409", rec.Code, rec.Body.String())
	}
	if rec := fx.member(t, alice, http.MethodPost, "/api/v1/trials/99999/vote", vote); rec.Code != http.StatusNotFound {
		t.Errorf("unknown trial vote = %d, want 404", rec.Code)
	}
	// The owner can still manage their request.
	if rec := fx.member(t, bob, http.MethodPost, fmt.Sprintf("/api/v1/requests/%d/actions", requests[0].ID), map[string]string{"action": "cancel"}); rec.Code != http.StatusOK {
		t.Errorf("owner cancel = %d %s", rec.Code, rec.Body.String())
	}
}

func TestMemberRequestQuota(t *testing.T) {
	fx := newAuthFixture(t, func(c *config.Config) {
		c.Auth.SessionTTL = config.Dur(72 * time.Hour)
		c.Auth.MemberRequestQuota = 2
		c.Auth.MemberQuotaWindow = config.Dur(24 * time.Hour)
	})
	alice := fx.login(t, "alice")
	recs := fx.seedRecommendations(t, "alice", "movie", 41, 42, 43)
	request := func(rec model.Recommendation, actionID string) *httptest.ResponseRecorder {
		return fx.member(t, alice, http.MethodPost, recPath(rec), map[string]string{"action_id": actionID, "action": "request"})
	}

	for i, rec := range recs[:2] {
		if got := request(rec, "req-"+strconv.Itoa(i)); got.Code != http.StatusOK {
			t.Fatalf("request %d within quota = %d %s", i, got.Code, got.Body.String())
		}
	}
	blocked := request(recs[2], "req-2")
	if blocked.Code != http.StatusTooManyRequests || errorCode(t, blocked) != CodeQuotaExceeded {
		t.Fatalf("third request = %d %s, want 429 quota_exceeded", blocked.Code, blocked.Body.String())
	}
	if got := blocked.Header().Get("Retry-After"); got != strconv.Itoa(24*3600) {
		t.Errorf("Retry-After = %q, want %d (oldest request is now, window 24h)", got, 24*3600)
	}
	if !strings.Contains(blocked.Body.String(), "next request is allowed at 2026-08-24T12:00:00Z") {
		t.Errorf("message does not say when to retry: %s", blocked.Body.String())
	}
	if movies, _ := fx.st.Movies().List(context.Background()); len(movies) != 2 {
		t.Errorf("blocked request still created a movie: %d movies", len(movies))
	}

	// Replays and re-requests of existing titles are never blocked.
	if got := request(recs[0], "req-0"); got.Code != http.StatusOK {
		t.Errorf("idempotent replay = %d %s", got.Code, got.Body.String())
	}
	if got := request(recs[1], "req-again"); got.Code != http.StatusOK {
		t.Errorf("new action for an existing request = %d %s", got.Code, got.Body.String())
	}
	// Dismissing and rating are not requests.
	if got := fx.member(t, alice, http.MethodPost, recPath(recs[2]), map[string]string{"action_id": "dismiss-2", "action": "dismiss"}); got.Code != http.StatusOK {
		t.Errorf("dismiss over quota = %d", got.Code)
	}
	// Operators are never limited.
	if got := fx.do(t, call{method: http.MethodPost, path: recPath(recs[2]), bearer: true, body: map[string]string{"action_id": "op-2", "action": "request"}}); got.Code != http.StatusOK {
		t.Errorf("operator over quota = %d %s", got.Code, got.Body.String())
	}
	// Once the window passes the quota frees up.
	fx.srv.clock.(*clock.Fake).Advance(25 * time.Hour)
	next := fx.seedRecommendations(t, "alice", "movie", 44)[0]
	if got := request(next, "req-late"); got.Code != http.StatusOK {
		t.Errorf("request after the window = %d %s", got.Code, got.Body.String())
	}
}

func TestQuotaOffByDefault(t *testing.T) {
	fx := newAuthFixture(t, nil)
	alice := fx.login(t, "alice")
	recs := fx.seedRecommendations(t, "alice", "movie", 51, 52, 53)
	for i, rec := range recs {
		if got := fx.member(t, alice, http.MethodPost, recPath(rec), map[string]string{"action_id": "r" + strconv.Itoa(i), "action": "request"}); got.Code != http.StatusOK {
			t.Fatalf("request %d = %d %s", i, got.Code, got.Body.String())
		}
	}
}

func TestLoginRateLimit(t *testing.T) {
	fx := newAuthFixture(t, nil)
	bad := map[string]string{"username": "alice", "password": "wrong"}

	for i := 0; i < loginMaxFailures; i++ {
		if rec := fx.do(t, call{method: http.MethodPost, path: pathAuthLogin, body: bad}); rec.Code != http.StatusUnauthorized {
			t.Fatalf("failure %d = %d", i, rec.Code)
		}
	}
	calls := fx.verifier.calls.Load()
	rec := fx.do(t, call{method: http.MethodPost, path: pathAuthLogin, body: bad})
	if rec.Code != http.StatusTooManyRequests || errorCode(t, rec) != CodeRateLimited || rec.Header().Get("Retry-After") == "" {
		t.Fatalf("sixth attempt = %d %s", rec.Code, rec.Body.String())
	}
	good := map[string]string{"username": "alice", "password": "pw"}
	if rec := fx.do(t, call{method: http.MethodPost, path: pathAuthLogin, body: good}); rec.Code != http.StatusTooManyRequests {
		t.Errorf("correct password while blocked = %d, want 429", rec.Code)
	}
	if fx.verifier.calls.Load() != calls {
		t.Error("a blocked client still reached Jellyfin")
	}
	// X-Forwarded-For must not buy a fresh allowance.
	if rec := fx.do(t, call{method: http.MethodPost, path: pathAuthLogin, body: good, headers: map[string]string{"X-Forwarded-For": "203.0.113.9"}}); rec.Code != http.StatusTooManyRequests {
		t.Errorf("spoofed X-Forwarded-For = %d, want 429", rec.Code)
	}

	fx.srv.clock.(*clock.Fake).Advance(loginWindow + time.Second)
	if rec := fx.do(t, call{method: http.MethodPost, path: pathAuthLogin, body: good}); rec.Code != http.StatusOK {
		t.Errorf("after the window = %d %s", rec.Code, rec.Body.String())
	}
}

func TestSuccessfulLoginResetsTheFailureCount(t *testing.T) {
	fx := newAuthFixture(t, nil)
	bad := map[string]string{"username": "alice", "password": "wrong"}
	for round := 0; round < 3; round++ {
		for i := 0; i < loginMaxFailures-1; i++ {
			fx.do(t, call{method: http.MethodPost, path: pathAuthLogin, body: bad})
		}
		if rec := fx.do(t, call{method: http.MethodPost, path: pathAuthLogin, body: map[string]string{"username": "alice", "password": "pw"}}); rec.Code != http.StatusOK {
			t.Fatalf("round %d success = %d %s", round, rec.Code, rec.Body.String())
		}
	}
}

func TestLoginLimiterIsBounded(t *testing.T) {
	l := newLoginLimiter()
	now := time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)
	for i := 0; i < loginLimiterCap*2; i++ {
		l.fail(fmt.Sprintf("10.0.%d.%d", i/256, i%256), now.Add(time.Duration(i)*time.Millisecond))
	}
	if len(l.entries) > loginLimiterCap {
		t.Fatalf("limiter holds %d entries, cap is %d", len(l.entries), loginLimiterCap)
	}
	if _, kept := l.entries["10.0.7.255"]; !kept {
		t.Error("the newest address was evicted instead of the oldest")
	}
	if _, kept := l.entries["10.0.0.0"]; kept {
		t.Error("the oldest address survived eviction")
	}
}

func TestLogoutEndsTheSession(t *testing.T) {
	fx := newAuthFixture(t, nil)
	cookie := fx.login(t, "alice")

	if rec := fx.do(t, call{method: http.MethodPost, path: "/api/v1/auth/logout", cookie: cookie}); rec.Code != http.StatusForbidden {
		t.Errorf("logout without the CSRF header = %d, want 403", rec.Code)
	}
	if rec := fx.do(t, call{method: http.MethodPost, path: "/api/v1/auth/logout", bearer: true}); rec.Code != http.StatusBadRequest {
		t.Errorf("logout with the static token = %d, want 400", rec.Code)
	}
	rec := fx.member(t, cookie, http.MethodPost, "/api/v1/auth/logout", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("logout = %d %s", rec.Code, rec.Body.String())
	}
	cleared := false
	for _, c := range rec.Result().Cookies() {
		cleared = cleared || (c.Name == sessionCookie && c.MaxAge < 0 && c.Value == "")
	}
	if !cleared {
		t.Error("logout did not clear the cookie")
	}
	if _, err := fx.st.Sessions().Get(context.Background(), hashToken(cookie), fx.srv.clock.Now()); err == nil {
		t.Error("session row survived logout")
	}
	if rec := fx.do(t, call{method: http.MethodGet, path: "/api/v1/auth/me", cookie: cookie}); rec.Code != http.StatusUnauthorized {
		t.Errorf("reusing a logged-out cookie = %d, want 401", rec.Code)
	}
}

func TestPublicAuthRoutesNeedNoCredentials(t *testing.T) {
	fx := newAuthFixture(t, nil)
	for _, path := range []string{"/api/v1/ping", pathAuthConfig} {
		if rec := fx.do(t, call{method: http.MethodGet, path: path}); rec.Code != http.StatusOK {
			t.Errorf("GET %s = %d", path, rec.Code)
		}
	}
	// A stale cookie does not break the public endpoints either.
	if rec := fx.do(t, call{method: http.MethodPost, path: pathAuthLogin, cookie: "stale", body: map[string]string{"username": "alice", "password": "pw"}}); rec.Code != http.StatusOK {
		t.Errorf("login with a stale cookie = %d", rec.Code)
	}
}

func TestEveryRouteDeclaresAnAccessLevel(t *testing.T) {
	// Registering the routes fills publicPaths; only these may skip authentication.
	fx := newAuthFixture(t, nil)
	want := map[string]bool{"/api/v1/ping": true, pathAuthConfig: true, pathAuthLogin: true}
	if len(fx.srv.publicPaths) != len(want) {
		t.Errorf("public paths = %v, want %v", fx.srv.publicPaths, want)
	}
	for path := range want {
		if !fx.srv.publicPaths[path] {
			t.Errorf("%s is not public", path)
		}
	}
}
