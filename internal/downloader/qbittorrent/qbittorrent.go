// Package qbittorrent implements the Downloader interface against qBittorrent's
// Web API v2.
//
// Verified against a live qBittorrent 4.4.1 / Web API 2.8.5, and written to
// tolerate 5.x, which renamed several fields. Three things about this API drive
// the design:
//
//  1. torrents/add does NOT return the hash of what it just added. The hash has
//     to be computed from the magnet on this side and used as the key. Get that
//     wrong and every grab looks stalled forever, because the status loop polls
//     for something the client never registered under that name.
//
//  2. The session cookie expires (default 3600s). A 403 mid-poll is routine, not
//     exceptional, so re-authentication is transparent rather than an error.
//
//  3. Every torrent in the client is visible to this API, including the
//     operator's own. The category is the only thing separating ours from
//     theirs, so it is enforced on every read and every destructive call.
package qbittorrent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"mime/multipart"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/TechXTT/reelay/internal/config"
	"github.com/TechXTT/reelay/internal/downloader"
	"github.com/TechXTT/reelay/internal/indexer/tpb"
)

// maxResponseBytes caps a torrents/info response. A client with thousands of
// torrents produces a large document, but not a 32 MB one.
const maxResponseBytes = 32 << 20

type Client struct {
	baseURL  *url.URL
	username string
	password string

	// ownedCategories is the safety boundary: a torrent whose category is not
	// in here is invisible to reads and untouchable by writes.
	ownedCategories map[string]bool

	http    *http.Client
	timeout time.Duration
	log     *slog.Logger

	// loginMu serialises re-authentication so a burst of concurrent 403s
	// produces one login rather than one per caller.
	loginMu  sync.Mutex
	loggedIn bool
	// sessionGeneration distinguishes the stale session that produced a 403
	// from a newer session another caller may already have established.
	sessionGeneration uint64
	apiVersion        string
}

type Options struct {
	HTTPClient *http.Client
	Logger     *slog.Logger
	Timeout    time.Duration
}

func New(cfg config.Downloader, opt Options) (*Client, error) {
	base, err := url.Parse(strings.TrimRight(cfg.URL, "/"))
	if err != nil {
		return nil, fmt.Errorf("qbittorrent: parse url %q: %w", cfg.URL, err)
	}
	if base.Scheme == "" || base.Host == "" {
		return nil, fmt.Errorf("qbittorrent: url %q is not absolute", cfg.URL)
	}

	owned := map[string]bool{}
	for _, c := range []string{cfg.CategoryTV, cfg.CategoryMovies} {
		if c = strings.TrimSpace(c); c != "" {
			owned[c] = true
		}
	}
	if len(owned) == 0 {
		return nil, errors.New("qbittorrent: no categories configured; " +
			"the category is the only thing separating Reelay's torrents from yours")
	}

	if opt.Logger == nil {
		opt.Logger = slog.Default()
	}
	if opt.Timeout <= 0 {
		opt.Timeout = 30 * time.Second
	}

	httpClient := opt.HTTPClient
	if httpClient == nil {
		// A cookie jar is what makes the SID cookie persist across calls.
		jar, err := cookiejar.New(nil)
		if err != nil {
			return nil, fmt.Errorf("qbittorrent: cookie jar: %w", err)
		}
		httpClient = &http.Client{Jar: jar}
	} else if httpClient.Jar == nil {
		jar, err := cookiejar.New(nil)
		if err != nil {
			return nil, fmt.Errorf("qbittorrent: cookie jar: %w", err)
		}
		httpClient.Jar = jar
	}

	return &Client{
		baseURL:         base,
		username:        cfg.Username,
		password:        cfg.Password,
		ownedCategories: owned,
		http:            httpClient,
		timeout:         opt.Timeout,
		log:             opt.Logger.With("component", "qbittorrent"),
	}, nil
}

// OwnedCategories lists the categories this client will act on.
func (c *Client) OwnedCategories() []string {
	out := make([]string, 0, len(c.ownedCategories))
	for k := range c.ownedCategories {
		out = append(out, k)
	}
	return out
}

// APIVersion is the Web API version reported at login, for the health endpoint.
func (c *Client) APIVersion() string {
	c.loginMu.Lock()
	defer c.loginMu.Unlock()
	return c.apiVersion
}

// Healthy checks the client is reachable and we are authenticated.
func (c *Client) Healthy(ctx context.Context) error {
	body, err := c.do(ctx, request{method: http.MethodGet, path: "/api/v2/app/version"})
	if err != nil {
		return err
	}
	if v := strings.TrimSpace(string(body)); v == "" {
		return errors.New("qbittorrent: app/version returned an empty response")
	}
	return nil
}

// Version returns the qBittorrent application version.
func (c *Client) Version(ctx context.Context) (string, error) {
	body, err := c.do(ctx, request{method: http.MethodGet, path: "/api/v2/app/version"})
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(body)), nil
}

// EnsureCategory creates a category if it does not already exist.
//
// Idempotent: qBittorrent answers a duplicate create with a 409, which is
// success as far as we are concerned.
func (c *Client) EnsureCategory(ctx context.Context, name, savePath string) error {
	if !c.ownedCategories[name] {
		return fmt.Errorf("qbittorrent: %q is not one of Reelay's configured categories", name)
	}
	_, err := c.do(ctx, request{
		method: http.MethodPost,
		path:   "/api/v2/torrents/createCategory",
		form:   url.Values{"category": {name}, "savePath": {savePath}},
		// 409 means "already exists".
		okStatus: []int{http.StatusOK, http.StatusConflict},
	})
	if err != nil {
		return err
	}
	c.log.Debug("category ensured", "category", name, "save_path", savePath)
	return nil
}

// Add hands a magnet to the client.
//
// The returned hash comes from the magnet, not from the client, because
// torrents/add returns an empty body. Computing it here is the only way to have
// a key to poll on, and it is why the magnet's hash is normalised to lowercase
// hex — qBittorrent keys on that form, and a base32 magnet would otherwise
// produce a hash the client has never heard of.
func (c *Client) Add(ctx context.Context, req downloader.AddRequest) (string, error) {
	if strings.TrimSpace(req.Magnet) == "" {
		return "", errors.New("qbittorrent: add: empty magnet")
	}
	if !c.ownedCategories[req.Category] {
		return "", fmt.Errorf("qbittorrent: add: refusing to add with category %q; "+
			"it is not one of Reelay's (%s)", req.Category, strings.Join(c.OwnedCategories(), ", "))
	}

	hash, err := tpb.InfoHashFromMagnet(req.Magnet)
	if err != nil {
		return "", fmt.Errorf("qbittorrent: add: %w", err)
	}

	fields := map[string]string{
		"urls":     req.Magnet,
		"category": req.Category,
		// qBittorrent 5.0 renamed `paused` to `stopped`. Sending both keeps one
		// binary working across versions; an unknown field is ignored.
		"paused":  strconv.FormatBool(req.Paused),
		"stopped": strconv.FormatBool(req.Paused),
	}
	if req.SavePath != "" {
		fields["savepath"] = req.SavePath
	}

	body, contentType, err := multipartBody(fields)
	if err != nil {
		return "", err
	}

	resp, err := c.do(ctx, request{
		method:      http.MethodPost,
		path:        "/api/v2/torrents/add",
		body:        body,
		contentType: contentType,
	})
	if err != nil {
		return "", err
	}
	// The API answers "Ok." on success and "Fails." when it rejects the
	// payload, both with a 200, so the status code alone proves nothing.
	if trimmed := strings.TrimSpace(string(resp)); trimmed != "" &&
		!strings.EqualFold(trimmed, "Ok.") {
		return "", fmt.Errorf("qbittorrent: add was rejected: %s", trimmed)
	}

	c.log.Info("torrent added", "hash", hash, "category", req.Category, "save_path", req.SavePath)
	return hash, nil
}

// Status polls the given hashes.
//
// Hashes not present in the client are simply absent from the result, as are
// torrents that do not carry one of our categories. The caller compares what it
// asked for against what came back.
func (c *Client) Status(ctx context.Context, hashes []string) ([]downloader.TorrentStatus, error) {
	q := url.Values{}
	if len(hashes) > 0 {
		normalised := make([]string, 0, len(hashes))
		for _, h := range hashes {
			if h = strings.ToLower(strings.TrimSpace(h)); h != "" {
				normalised = append(normalised, h)
			}
		}
		if len(normalised) == 0 {
			return nil, nil
		}
		q.Set("hashes", strings.Join(normalised, "|"))
	}

	body, err := c.do(ctx, request{
		method: http.MethodGet,
		path:   "/api/v2/torrents/info",
		query:  q,
	})
	if err != nil {
		return nil, err
	}

	var rows []infoRow
	if err := json.Unmarshal(body, &rows); err != nil {
		return nil, fmt.Errorf("qbittorrent: decode torrents/info: %w", err)
	}

	out := make([]downloader.TorrentStatus, 0, len(rows))
	skipped := 0
	for _, r := range rows {
		if !c.ownedCategories[r.Category] {
			skipped++
			continue
		}
		out = append(out, r.toStatus())
	}
	if skipped > 0 {
		// Not a warning: it is the normal state of a shared client, and the
		// count is useful when someone wonders why a torrent is invisible.
		c.log.Debug("ignored torrents outside Reelay's categories", "count", skipped)
	}
	return out, nil
}

// Remove deletes a torrent, and refuses to touch one that is not ours.
//
// The ownership check costs an extra round trip, and it is not optional. This
// is the one call that can destroy the operator's data, so it verifies against
// the client rather than trusting a hash that was handed to it.
func (c *Client) Remove(ctx context.Context, hash string, deleteData bool) error {
	hash = strings.ToLower(strings.TrimSpace(hash))
	if hash == "" {
		return errors.New("qbittorrent: remove: empty hash")
	}

	body, err := c.do(ctx, request{
		method: http.MethodGet,
		path:   "/api/v2/torrents/info",
		query:  url.Values{"hashes": {hash}},
	})
	if err != nil {
		return err
	}
	var rows []infoRow
	if err := json.Unmarshal(body, &rows); err != nil {
		return fmt.Errorf("qbittorrent: decode torrents/info before remove: %w", err)
	}
	if len(rows) == 0 {
		return fmt.Errorf("qbittorrent: remove %s: %w", hash, downloader.ErrNotFound)
	}
	if !c.ownedCategories[rows[0].Category] {
		return fmt.Errorf("qbittorrent: remove %s (%q, category %q): %w",
			hash, rows[0].Name, rows[0].Category, downloader.ErrNotOurs)
	}

	if _, err := c.do(ctx, request{
		method: http.MethodPost,
		path:   "/api/v2/torrents/delete",
		form: url.Values{
			"hashes":      {hash},
			"deleteFiles": {strconv.FormatBool(deleteData)},
		},
	}); err != nil {
		return err
	}
	c.log.Info("torrent removed", "hash", hash, "name", rows[0].Name, "deleted_data", deleteData)
	return nil
}

// SetPaused pauses or resumes only torrents that Status confirms belong to
// one of Reelay's categories. qBittorrent 5 renamed pause/resume to stop/start;
// the legacy endpoint fallback keeps older supported clients working.
func (c *Client) SetPaused(ctx context.Context, hashes []string, paused bool) error {
	requested := make([]string, 0, len(hashes))
	requestedSet := make(map[string]bool, len(hashes))
	for _, hash := range hashes {
		hash = strings.ToLower(strings.TrimSpace(hash))
		if hash != "" && !requestedSet[hash] {
			requestedSet[hash] = true
			requested = append(requested, hash)
		}
	}
	// Status with an empty hash list intentionally means "list all". Returning
	// here prevents an empty queue operation from broadening into every owned
	// torrent in the client.
	if len(requested) == 0 {
		return nil
	}
	statuses, err := c.Status(ctx, requested)
	if err != nil {
		return fmt.Errorf("qbittorrent: verify torrents before changing pause state: %w", err)
	}
	owned := make([]string, 0, len(statuses))
	seen := make(map[string]bool, len(statuses))
	for _, status := range statuses {
		hash := strings.ToLower(strings.TrimSpace(status.Hash))
		if hash != "" && !seen[hash] {
			seen[hash] = true
			owned = append(owned, hash)
		}
	}
	if len(owned) == 0 {
		return nil
	}

	path, legacyPath := "/api/v2/torrents/start", "/api/v2/torrents/resume"
	if paused {
		path, legacyPath = "/api/v2/torrents/stop", "/api/v2/torrents/pause"
	}
	req := request{method: http.MethodPost, path: path,
		form: url.Values{"hashes": {strings.Join(owned, "|")}}}
	if _, err := c.do(ctx, req); errors.Is(err, downloader.ErrNotFound) {
		req.path = legacyPath
		if _, err = c.do(ctx, req); err != nil {
			return fmt.Errorf("qbittorrent: set paused=%t: %w", paused, err)
		}
	} else if err != nil {
		return fmt.Errorf("qbittorrent: set paused=%t: %w", paused, err)
	}
	c.log.Info("torrent pause state changed", "paused", paused, "count", len(owned))
	return nil
}

func multipartBody(fields map[string]string) (io.Reader, string, error) {
	var buf strings.Builder
	w := multipart.NewWriter(&buf)
	for k, v := range fields {
		if err := w.WriteField(k, v); err != nil {
			return nil, "", fmt.Errorf("qbittorrent: build multipart field %q: %w", k, err)
		}
	}
	if err := w.Close(); err != nil {
		return nil, "", fmt.Errorf("qbittorrent: close multipart body: %w", err)
	}
	return strings.NewReader(buf.String()), w.FormDataContentType(), nil
}

var _ downloader.Downloader = (*Client)(nil)
