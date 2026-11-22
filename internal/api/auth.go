package api

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"net"
	"net/http"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/TechXTT/reelay/internal/jellyfinauth"
	"github.com/TechXTT/reelay/internal/model"
	"github.com/TechXTT/reelay/internal/store"
)

const (
	sessionCookie = "reelay_session"
	// csrfHeader must accompany every state-changing request authenticated by
	// the session cookie. A cross-site form or fetch cannot set a custom header
	// without a CORS preflight, which only configured origins pass.
	csrfHeader = "X-Reelay-Request"

	roleOperator = "operator"
	roleMember   = "member"

	pathAuthConfig = "/api/v1/auth/config"
	pathAuthLogin  = "/api/v1/auth/login"

	loginMaxFailures = 5
	loginWindow      = 10 * time.Minute
	loginLimiterCap  = 1024
	maxUsernameLen   = 256
	maxPasswordLen   = 1024
)

// access is the lowest role allowed to call a route. The zero value is the
// strictest, so a route added without a level is operator-only.
type access int

const (
	accessOperator access = iota
	accessMember
	accessPublic
)

// Principal is who a request acts as. Operators (the static token, or a
// signed-in Jellyfin administrator) may do anything; members see only their own
// recommendations and requests.
type Principal struct {
	Role     string `json:"role"`
	ServerID string `json:"server_id,omitempty"`
	UserID   string `json:"user_id,omitempty"`
	Name     string `json:"name"`
	// Session is true when the principal came from the session cookie rather
	// than the static token.
	Session bool `json:"session"`

	tokenHash string
}

// LoginVerifier checks a Jellyfin username and password.
type LoginVerifier interface {
	Authenticate(ctx context.Context, baseURL, username, password string) (jellyfinauth.User, error)
}

func principalFromContext(ctx context.Context) Principal {
	p, _ := ctx.Value(ctxPrincipal).(Principal)
	return p
}

func principalFrom(r *http.Request) Principal { return principalFromContext(r.Context()) }

func hashToken(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

// authenticate resolves the request's Principal. It runs outside the router so
// that unknown /api paths are authenticated too; per-route role checks are in
// guard. The static token keeps its original behavior: it is checked first and
// maps to an operator.
func (s *Server) authenticate(next http.Handler) http.Handler {
	token := s.cfg.Server.AuthToken

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// The SPA shell contains no operator data and must be reachable before
		// the browser can sign in. Every data and command route stays under /api.
		// CORS preflight carries no credentials by design.
		if !strings.HasPrefix(r.URL.Path, "/api/") || s.publicPaths[r.URL.Path] || r.Method == http.MethodOptions {
			next.ServeHTTP(w, r)
			return
		}
		if token == "" {
			next.ServeHTTP(w, withPrincipal(r, Principal{Role: roleOperator, Name: "local"}))
			return
		}

		if presented := presentedToken(r); presented != "" {
			if subtle.ConstantTimeCompare([]byte(presented), []byte(token)) != 1 {
				writeError(w, loggerFrom(r), Unauthorized("invalid bearer token"))
				return
			}
			next.ServeHTTP(w, withPrincipal(r, Principal{Role: roleOperator, Name: "API token"}))
			return
		}

		if s.cfg.Auth.LoginsEnabled {
			if cookie, err := r.Cookie(sessionCookie); err == nil && cookie.Value != "" {
				principal, err := s.sessionPrincipal(r, cookie.Value)
				if err != nil {
					if errors.Is(err, store.ErrNotFound) {
						http.SetCookie(w, s.sessionCookieFor(r, "", -1))
						writeError(w, loggerFrom(r), Unauthorized("session expired; sign in again"))
						return
					}
					writeError(w, loggerFrom(r), err)
					return
				}
				if !safeMethod(r.Method) && r.Header.Get(csrfHeader) != "1" {
					writeError(w, loggerFrom(r), Forbidden("missing "+csrfHeader+" header"))
					return
				}
				next.ServeHTTP(w, withPrincipal(r, principal))
				return
			}
		}

		w.Header().Set("WWW-Authenticate", `Bearer realm="reelay"`)
		writeError(w, loggerFrom(r), Unauthorized("missing bearer token"))
	})
}

func withPrincipal(r *http.Request, p Principal) *http.Request {
	return r.WithContext(context.WithValue(r.Context(), ctxPrincipal, p))
}

func safeMethod(method string) bool {
	return method == http.MethodGet || method == http.MethodHead || method == http.MethodOptions
}

// sessionPrincipal loads the session behind a cookie value. It returns
// store.ErrNotFound for an unknown or expired session, and for a member who has
// since been disabled in Reelay, so disabling a user revokes their sessions.
func (s *Server) sessionPrincipal(r *http.Request, value string) (Principal, error) {
	var principal Principal

	ctx := r.Context()
	now := s.clock.Now()
	hash := hashToken(value)
	session, err := s.store.Sessions().Get(ctx, hash, now)
	if err != nil {
		return principal, err
	}
	if session.Role == roleMember {
		enabled, err := s.store.Recommendations().UserEnabled(ctx, session.ServerID, session.UserID)
		if err != nil {
			return principal, Internal(err)
		}
		if !enabled {
			if err := s.store.Sessions().Delete(ctx, hash); err != nil {
				loggerFrom(r).Warn("delete session of disabled user", "error", err)
			}
			return principal, store.ErrNotFound
		}
	}
	if err := s.store.Sessions().Touch(ctx, hash, now); err != nil {
		loggerFrom(r).Warn("touch session", "error", err)
	}
	return Principal{Role: session.Role, ServerID: session.ServerID, UserID: session.UserID,
		Name: session.DisplayName, Session: true, tokenHash: hash}, nil
}

// guard rejects a request whose principal is below the route's access level.
func (s *Server) guard(level access, h handler) handler {
	return func(w http.ResponseWriter, r *http.Request) error {
		role := principalFrom(r).Role
		if level != accessPublic && role == "" {
			return Unauthorized("authentication required")
		}
		if level == accessOperator && role != roleOperator {
			return Forbidden("this action requires an operator")
		}
		return h(w, r)
	}
}

// scopeFor resolves which Jellyfin user a request acts on. Operators must name
// the user. Members always act as themselves: omitted parameters are filled in,
// and naming anyone else is refused.
func scopeFor(r *http.Request, serverID, userID string) (string, string, error) {
	p := principalFrom(r)
	serverID, userID = strings.TrimSpace(serverID), strings.TrimSpace(userID)

	if p.Role == roleOperator {
		if serverID == "" || userID == "" {
			return "", "", BadRequest("server_id and user_id are required")
		}
		return serverID, userID, nil
	}
	if p.UserID == "" || p.ServerID == "" {
		return "", "", Forbidden("no user is associated with this session")
	}
	if (serverID != "" && serverID != p.ServerID) || (userID != "" && userID != p.UserID) {
		return "", "", Forbidden("you can only access your own data")
	}
	return p.ServerID, p.UserID, nil
}

// ownsRecord reports whether the principal may see data owned by the given
// Jellyfin user. Callers answer 404, not 403, so ids are not probed.
func ownsRecord(r *http.Request, serverID, userID string) bool {
	p := principalFrom(r)
	return p.Role == roleOperator || (p.UserID != "" && p.ServerID == serverID && p.UserID == userID)
}

// checkMemberQuota refuses a member's request when it would create a new
// request row beyond auth.member_request_quota in auth.member_quota_window.
// Re-requesting a title the member already has reopens the existing row and is
// never blocked. Operators are not limited.
func (s *Server) checkMemberQuota(ctx context.Context, rec model.Recommendation) error {
	quota := s.cfg.Auth.MemberRequestQuota
	if principalFromContext(ctx).Role != roleMember || quota <= 0 {
		return nil
	}
	exists, err := s.store.Requests().RequestExists(ctx, rec.ServerID, rec.UserID, rec.MediaType, rec.TMDBID)
	if err != nil || exists {
		return err
	}
	now := s.clock.Now()
	window := s.cfg.Auth.MemberQuotaWindow.Duration
	count, oldest, err := s.store.Requests().CountRequestedSince(ctx, rec.ServerID, rec.UserID, now.Add(-window))
	if err != nil || count < quota {
		return err
	}
	next := oldest.Add(window)
	return TooManyRequests(CodeQuotaExceeded,
		fmt.Sprintf("request quota reached (%d per %s); the next request is allowed at %s", quota, window, next.UTC().Format(time.RFC3339)),
		next.Sub(now))
}

func (s *Server) sessionCookieFor(r *http.Request, value string, maxAge int) *http.Cookie {
	return &http.Cookie{
		Name:     sessionCookie,
		Value:    value,
		Path:     "/",
		MaxAge:   maxAge,
		HttpOnly: true,
		SameSite: http.SameSiteStrictMode,
		Secure:   r.TLS != nil || strings.EqualFold(r.Header.Get("X-Forwarded-Proto"), "https"),
	}
}

type authConfigResponse struct {
	LoginsEnabled bool     `json:"logins_enabled"`
	Servers       []string `json:"servers"`
}

// handleAuthConfig tells the sign-in screen whether logins exist. Server ids
// are listed only when logins are enabled.
func (s *Server) handleAuthConfig(w http.ResponseWriter, r *http.Request) error {
	servers := make([]string, 0, len(s.cfg.Availability.JellyfinServers))
	if s.cfg.Auth.LoginsEnabled {
		for id := range s.cfg.Availability.JellyfinServers {
			servers = append(servers, id)
		}
		slices.Sort(servers)
	}
	return reply(w, r, http.StatusOK, authConfigResponse{LoginsEnabled: s.cfg.Auth.LoginsEnabled, Servers: servers})
}

type loginRequest struct {
	ServerID string `json:"server_id"`
	Username string `json:"username"`
	Password string `json:"password"`
}

// handleLogin verifies the credentials against the chosen Jellyfin server.
// With logins disabled the endpoint answers 404, as if it did not exist.
func (s *Server) handleLogin(w http.ResponseWriter, r *http.Request) error {
	if !s.cfg.Auth.LoginsEnabled {
		return NotFound("logins are not enabled")
	}
	req, err := decodeJSON[loginRequest](r)
	if err != nil {
		return err
	}

	now := s.clock.Now()
	ip := clientIP(r)
	if wait := s.logins.blocked(ip, now); wait > 0 {
		return TooManyRequests(CodeRateLimited, "too many failed sign-in attempts; try again later", wait)
	}

	servers := s.cfg.Availability.JellyfinServers
	serverID := strings.TrimSpace(req.ServerID)
	if serverID == "" && len(servers) == 1 {
		for id := range servers {
			serverID = id
		}
	}
	baseURL, known := servers[serverID]
	if serverID == "" || !known {
		return BadRequest("server_id must name a configured Jellyfin server")
	}
	username := strings.TrimSpace(req.Username)
	if username == "" || len(username) > maxUsernameLen || len(req.Password) > maxPasswordLen {
		return BadRequest("a username and a password of reasonable length are required")
	}

	user, err := s.loginVerifier.Authenticate(r.Context(), baseURL, username, req.Password)
	if errors.Is(err, jellyfinauth.ErrInvalidCredentials) {
		s.logins.fail(ip, now)
		return Unauthorized("invalid Jellyfin username or password")
	}
	if err != nil {
		return Unavailable("the Jellyfin server could not verify the sign-in").WithCause(err)
	}

	role := roleOperator
	if !user.Administrator {
		role = roleMember
		enabled, err := s.store.Recommendations().UserEnabled(r.Context(), serverID, user.ID)
		if err != nil {
			return err
		}
		if !enabled {
			return Forbidden("this Jellyfin user is not enabled in Reelay")
		}
	}

	var raw [32]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return Internal(err)
	}
	token := base64.RawURLEncoding.EncodeToString(raw[:])
	ttl := s.cfg.Auth.SessionTTL.Duration
	err = s.store.Sessions().Create(r.Context(), store.Session{TokenHash: hashToken(token), Role: role,
		ServerID: serverID, UserID: user.ID, DisplayName: user.Name,
		CreatedAt: now, ExpiresAt: now.Add(ttl), LastSeenAt: now})
	if err != nil {
		return Internal(err)
	}
	s.logins.reset(ip)

	http.SetCookie(w, s.sessionCookieFor(r, token, int(ttl/time.Second)))
	loggerFrom(r).Info("login", "role", role, "server_id", serverID, "user_id", user.ID)
	return reply(w, r, http.StatusOK, Principal{Role: role, ServerID: serverID, UserID: user.ID, Name: user.Name, Session: true})
}

// handleLogout ends the session behind the cookie. The CSRF header rule in
// authenticate already applies, since this is a cookie-authenticated POST.
func (s *Server) handleLogout(w http.ResponseWriter, r *http.Request) error {
	p := principalFrom(r)
	if !p.Session {
		return BadRequest("this request is not authenticated by a session")
	}
	if err := s.store.Sessions().Delete(r.Context(), p.tokenHash); err != nil {
		return Internal(err)
	}
	http.SetCookie(w, s.sessionCookieFor(r, "", -1))
	return reply(w, r, http.StatusOK, map[string]any{"logged_out": true})
}

func (s *Server) handleAuthMe(w http.ResponseWriter, r *http.Request) error {
	return reply(w, r, http.StatusOK, principalFrom(r))
}

// handleJellyfinUsers lists the synced Jellyfin users; a member sees only
// themselves.
func (s *Server) handleJellyfinUsers(w http.ResponseWriter, r *http.Request) error {
	users, err := s.store.Recommendations().Users(r.Context())
	if err != nil {
		return err
	}
	if p := principalFrom(r); p.Role != roleOperator {
		users = slices.DeleteFunc(users, func(u model.JellyfinUser) bool {
			return u.ServerID != p.ServerID || u.UserID != p.UserID
		})
	}
	return reply(w, r, http.StatusOK, map[string]any{"items": users})
}

// clientIP is the peer address. X-Forwarded-For is deliberately ignored: it is
// client-controlled unless a trusted proxy rewrites it, so honoring it would
// let an attacker dodge the limiter by rotating the header.
func clientIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

type loginAttempts struct {
	failures int
	since    time.Time
}

// loginLimiter counts failed sign-ins per client address in a fixed window. It
// is in-memory and bounded; losing it on restart only forgives attackers.
type loginLimiter struct {
	mu      sync.Mutex
	entries map[string]*loginAttempts
}

func newLoginLimiter() *loginLimiter {
	return &loginLimiter{entries: make(map[string]*loginAttempts)}
}

// blocked returns how long the address must wait, or zero when it may try.
func (l *loginLimiter) blocked(ip string, now time.Time) time.Duration {
	l.mu.Lock()
	defer l.mu.Unlock()

	e := l.entries[ip]
	if e == nil || e.failures < loginMaxFailures || now.Sub(e.since) >= loginWindow {
		return 0
	}
	return loginWindow - now.Sub(e.since)
}

func (l *loginLimiter) fail(ip string, now time.Time) {
	l.mu.Lock()
	defer l.mu.Unlock()

	if e := l.entries[ip]; e != nil && now.Sub(e.since) < loginWindow {
		e.failures++
		return
	}
	if _, tracked := l.entries[ip]; !tracked && len(l.entries) >= loginLimiterCap {
		l.evict(now)
	}
	l.entries[ip] = &loginAttempts{failures: 1, since: now}
}

func (l *loginLimiter) reset(ip string) {
	l.mu.Lock()
	defer l.mu.Unlock()

	delete(l.entries, ip)
}

// evict drops expired windows, then the oldest window if the map is still full.
func (l *loginLimiter) evict(now time.Time) {
	var oldest string
	var oldestSince time.Time

	for ip, e := range l.entries {
		if now.Sub(e.since) >= loginWindow {
			delete(l.entries, ip)
			continue
		}
		if oldest == "" || e.since.Before(oldestSince) {
			oldest, oldestSince = ip, e.since
		}
	}
	if len(l.entries) >= loginLimiterCap {
		delete(l.entries, oldest)
	}
}
