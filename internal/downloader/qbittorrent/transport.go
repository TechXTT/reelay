package qbittorrent

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"

	"github.com/TechXTT/reelay/internal/downloader"
)

type request struct {
	method string
	path   string
	query  url.Values

	// form is sent url-encoded; body+contentType is sent as given. Only one of
	// the two is used.
	form        url.Values
	body        io.Reader
	contentType string

	// okStatus overrides the default of "200 only".
	okStatus []int

	// noRetryAuth stops the login path from recursing into itself.
	noRetryAuth bool
}

// do issues one API call, logging in first if needed and re-authenticating once
// on a 403.
//
// A 403 mid-session is routine rather than exceptional: qBittorrent's session
// cookie expires after an hour by default, and the status loop polls for as
// long as the service runs. Surfacing that as an error would mean every install
// logs a failure once an hour and drops one poll cycle.
func (c *Client) do(ctx context.Context, req request) ([]byte, error) {
	var sessionGeneration uint64
	if !req.noRetryAuth {
		var err error
		sessionGeneration, err = c.ensureLogin(ctx)
		if err != nil {
			return nil, err
		}
	}

	body, status, err := c.roundTrip(ctx, req)
	if err != nil {
		return nil, err
	}

	if status == http.StatusForbidden && !req.noRetryAuth {
		c.log.Debug("session expired, re-authenticating", "path", req.path)
		if err := c.reauthenticate(ctx, sessionGeneration); err != nil {
			return nil, err
		}
		body, status, err = c.roundTrip(ctx, req)
		if err != nil {
			return nil, err
		}
	}

	if !statusOK(status, req.okStatus) {
		return nil, c.statusError(req, status, body)
	}
	return body, nil
}

func (c *Client) statusError(req request, status int, body []byte) error {
	snippet := strings.TrimSpace(string(body))
	if len(snippet) > 200 {
		snippet = snippet[:200] + "..."
	}
	switch status {
	case http.StatusForbidden:
		return fmt.Errorf("qbittorrent: %s %s: %w (check downloader.username and downloader.password)",
			req.method, req.path, downloader.ErrAuth)
	case http.StatusNotFound:
		return fmt.Errorf("qbittorrent: %s %s: %w", req.method, req.path, downloader.ErrNotFound)
	default:
		return fmt.Errorf("qbittorrent: %s %s: unexpected status %d: %s",
			req.method, req.path, status, snippet)
	}
}

func statusOK(status int, allowed []int) bool {
	if len(allowed) == 0 {
		return status == http.StatusOK
	}
	for _, a := range allowed {
		if status == a {
			return true
		}
	}
	return false
}

func (c *Client) roundTrip(ctx context.Context, req request) ([]byte, int, error) {
	reqCtx, cancel := context.WithTimeout(ctx, c.timeout)
	defer cancel()

	u := *c.baseURL
	u.Path = req.path
	if len(req.query) > 0 {
		u.RawQuery = req.query.Encode()
	}

	var bodyReader io.Reader
	contentType := req.contentType
	switch {
	case req.body != nil:
		bodyReader = req.body
	case len(req.form) > 0:
		bodyReader = strings.NewReader(req.form.Encode())
		contentType = "application/x-www-form-urlencoded"
	}

	httpReq, err := http.NewRequestWithContext(reqCtx, req.method, u.String(), bodyReader)
	if err != nil {
		return nil, 0, fmt.Errorf("qbittorrent: build request: %w", err)
	}
	if contentType != "" {
		httpReq.Header.Set("Content-Type", contentType)
	}
	// qBittorrent validates the Referer against its own origin by default
	// (WebUI\HostHeaderValidation and CSRFProtection are both on out of the
	// box). Without this header every POST is rejected with a 403 that looks
	// exactly like an authentication failure.
	httpReq.Header.Set("Referer", c.baseURL.String())
	httpReq.Header.Set("Origin", c.baseURL.String())

	resp, err := c.http.Do(httpReq)
	if err != nil {
		return nil, 0, fmt.Errorf("qbittorrent: %s %s: %w", req.method, req.path, err)
	}
	defer func() {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4<<10))
		_ = resp.Body.Close()
	}()

	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes+1))
	if err != nil {
		return nil, resp.StatusCode, fmt.Errorf("qbittorrent: read response: %w", err)
	}
	if len(raw) > maxResponseBytes {
		return nil, resp.StatusCode, fmt.Errorf(
			"qbittorrent: response from %s exceeds %d bytes; refusing to buffer it",
			req.path, maxResponseBytes)
	}
	return raw, resp.StatusCode, nil
}

// ensureLogin authenticates if we do not already hold a session.
func (c *Client) ensureLogin(ctx context.Context) (uint64, error) {
	c.loginMu.Lock()
	defer c.loginMu.Unlock()
	if c.loggedIn {
		return c.sessionGeneration, nil
	}
	if err := c.loginLocked(ctx); err != nil {
		return 0, err
	}
	return c.sessionGeneration, nil
}

// reauthenticate replaces the session only if the 403 came from the current
// generation. Concurrent callers that received a 403 from that same stale
// session reuse the first caller's replacement instead of logging in again.
func (c *Client) reauthenticate(ctx context.Context, staleGeneration uint64) error {
	c.loginMu.Lock()
	defer c.loginMu.Unlock()
	if c.loggedIn && c.sessionGeneration != staleGeneration {
		return nil
	}
	c.loggedIn = false
	return c.loginLocked(ctx)
}

// loginLocked performs the login. Caller holds loginMu, which is what makes a
// burst of concurrent 403s produce one login rather than one per caller.
func (c *Client) loginLocked(ctx context.Context) error {
	body, status, err := c.roundTrip(ctx, request{
		method:      http.MethodPost,
		path:        "/api/v2/auth/login",
		form:        url.Values{"username": {c.username}, "password": {c.password}},
		noRetryAuth: true,
	})
	if err != nil {
		return err
	}

	answer := strings.TrimSpace(string(body))
	switch {
	case status == http.StatusOK && strings.EqualFold(answer, "Ok."):
		c.loggedIn = true
		c.sessionGeneration++
	case status == http.StatusOK && strings.EqualFold(answer, "Fails."):
		// A 200 carrying "Fails." is how qBittorrent reports bad credentials.
		return fmt.Errorf("qbittorrent: %w (the client answered %q; check downloader.username and downloader.password)",
			downloader.ErrAuth, answer)
	case status == http.StatusForbidden:
		// After too many failed attempts the client bans the IP, by default
		// for an hour. Say so, because the obvious next move — retrying — is
		// the one that keeps the ban alive.
		return fmt.Errorf("qbittorrent: %w: too many failed logins, the client has banned this IP "+
			"(WebUI\\BanDuration, default 3600s). Fix the credentials and wait it out rather than retrying",
			downloader.ErrAuth)
	default:
		return fmt.Errorf("qbittorrent: login returned status %d: %s", status, answer)
	}

	// The Web API version tells us which field names this server expects.
	if v, _, err := c.roundTrip(ctx, request{
		method:      http.MethodGet,
		path:        "/api/v2/app/webapiVersion",
		noRetryAuth: true,
	}); err == nil {
		c.apiVersion = strings.TrimSpace(string(v))
	}

	c.log.Debug("authenticated", "user", c.username, "web_api", c.apiVersion)
	return nil
}
