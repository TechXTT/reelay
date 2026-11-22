// Package jellyfinauth verifies a username and password against a Jellyfin
// server. Reelay never stores the password and never keeps the Jellyfin
// session the check creates.
package jellyfinauth

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/TechXTT/reelay/internal/buildinfo"
)

const (
	requestTimeout = 10 * time.Second
	logoutTimeout  = 5 * time.Second
	maxBodyBytes   = 1 << 20
)

// ErrInvalidCredentials means Jellyfin rejected the username or password.
var ErrInvalidCredentials = errors.New("jellyfin rejected the username or password")

// User is the verified identity. ID uses the plugin's format: 32 lowercase hex
// characters without dashes.
type User struct {
	ID            string
	Name          string
	ServerID      string
	Administrator bool
}

type authResult struct {
	AccessToken string `json:"AccessToken"`
	ServerID    string `json:"ServerId"`
	User        struct {
		ID       string `json:"Id"`
		Name     string `json:"Name"`
		ServerID string `json:"ServerId"`
		Policy   struct {
			IsAdministrator bool `json:"IsAdministrator"`
		} `json:"Policy"`
	} `json:"User"`
}

// Client talks to Jellyfin. The zero value is not usable; call New.
type Client struct {
	http *http.Client
	log  *slog.Logger
}

func New(log *slog.Logger) *Client {
	return &Client{
		http: &http.Client{
			// Never replay the credentials to wherever a redirect points.
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		},
		log: log,
	}
}

// Authenticate verifies the credentials against the Jellyfin server at baseURL
// using the default client.
func Authenticate(ctx context.Context, baseURL, username, password string) (User, error) {
	return New(slog.Default()).Authenticate(ctx, baseURL, username, password)
}

func (c *Client) Authenticate(ctx context.Context, baseURL, username, password string) (User, error) {
	var result authResult
	var user User

	base := strings.TrimRight(baseURL, "/")
	ctx, cancel := context.WithTimeout(ctx, requestTimeout)
	defer cancel()

	body, err := json.Marshal(map[string]string{"Username": username, "Pw": password})
	if err != nil {
		return user, fmt.Errorf("encode jellyfin login: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, base+"/Users/AuthenticateByName", bytes.NewReader(body))
	if err != nil {
		return user, fmt.Errorf("build jellyfin login request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", authHeader(""))

	resp, err := c.http.Do(req)
	if err != nil {
		return user, fmt.Errorf("jellyfin login request: %w", err)
	}
	defer resp.Body.Close()

	switch {
	case resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden:
		return user, ErrInvalidCredentials
	case resp.StatusCode != http.StatusOK:
		return user, fmt.Errorf("jellyfin login returned status %d", resp.StatusCode)
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, maxBodyBytes)).Decode(&result); err != nil {
		return user, fmt.Errorf("decode jellyfin login response: %w", err)
	}

	// Drop the Jellyfin device session the check created, whatever follows.
	if result.AccessToken != "" {
		c.logout(ctx, base, result.AccessToken)
	}

	user.ID = normalizeID(result.User.ID)
	if len(user.ID) != 32 || strings.Trim(user.ID, "0123456789abcdef") != "" {
		return User{}, fmt.Errorf("jellyfin returned an unusable user id %q", result.User.ID)
	}
	user.Name = result.User.Name
	user.ServerID = normalizeID(result.User.ServerID)
	if user.ServerID == "" {
		user.ServerID = normalizeID(result.ServerID)
	}
	user.Administrator = result.User.Policy.IsAdministrator
	return user, nil
}

// logout is best effort: the credentials were already verified, so a failure
// only leaves a stale device entry in Jellyfin.
func (c *Client) logout(ctx context.Context, base, token string) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), logoutTimeout)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, base+"/Sessions/Logout", nil)
	if err != nil {
		c.log.Debug("build jellyfin logout request", "error", err)
		return
	}
	req.Header.Set("Authorization", authHeader(token))
	resp, err := c.http.Do(req)
	if err != nil {
		c.log.Debug("jellyfin logout failed", "error", err)
		return
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, maxBodyBytes))
	if resp.StatusCode >= 300 {
		c.log.Debug("jellyfin logout failed", "status", resp.StatusCode)
	}
}

func authHeader(token string) string {
	header := fmt.Sprintf(`MediaBrowser Client="Reelay", Device="Reelay", DeviceId="reelay-login", Version=%q`, buildinfo.Version)
	if token != "" {
		header += fmt.Sprintf(`, Token=%q`, token)
	}
	return header
}

func normalizeID(id string) string {
	return strings.ToLower(strings.ReplaceAll(strings.TrimSpace(id), "-", ""))
}
