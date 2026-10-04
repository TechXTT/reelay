package torznab

import (
	"context"
	"crypto/sha1"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"

	"github.com/TechXTT/reelay/internal/indexer"
	"github.com/TechXTT/reelay/internal/indexer/tpb"
	"github.com/TechXTT/reelay/internal/torrentfile"
)

// maxRedirects bounds same-host redirects followed while fetching a torrent.
const maxRedirects = 5

// magnetRedirect is returned by the redirect policy when a download link
// redirects to a magnet URI, which an HTTP client cannot follow.
type magnetRedirect struct{ magnet string }

func (m *magnetRedirect) Error() string { return "download link redirected to a magnet" }

// defaultPort returns the explicit port of u or the scheme default.
func defaultPort(u *url.URL) string {
	if port := u.Port(); port != "" {
		return port
	}
	if strings.EqualFold(u.Scheme, "https") {
		return "443"
	}
	return "80"
}

// sameOrigin reports whether u has the same scheme, host and port as the
// indexer endpoint. The API key is only ever sent to such URLs.
func (c *Client) sameOrigin(u *url.URL) bool {
	return strings.EqualFold(u.Scheme, c.base.Scheme) &&
		strings.EqualFold(u.Hostname(), c.base.Hostname()) &&
		defaultPort(u) == defaultPort(c.base)
}

// stripAPIKey removes every apikey query parameter, matched case-insensitively.
func stripAPIKey(u *url.URL) {
	var query = u.Query()

	for name := range query {
		if strings.EqualFold(name, "apikey") {
			query.Del(name)
		}
	}
	u.RawQuery = query.Encode()
}

// cleanDownloadURL validates a feed-provided .torrent link and returns it
// without the API key. Links to any other origin are rejected so feed content
// cannot make Reelay send the key elsewhere or reach arbitrary internal hosts.
func (c *Client) cleanDownloadURL(raw string) (string, bool) {
	var link, err = url.Parse(raw)

	if err != nil || (link.Scheme != "http" && link.Scheme != "https") || link.User != nil || !c.sameOrigin(link) {
		return "", false
	}
	link.Fragment = ""
	stripAPIKey(link)
	return link.String(), true
}

// standInKey is the identity of a download-link-only release until it is
// grabbed: the lowercase hex SHA-1 of the indexer name and the cleaned URL.
// It is NOT a real info hash. It only gives de-duplication, storage and
// blacklisting a stable key; the real hash is computed from the .torrent file
// at grab time.
func standInKey(indexerName, cleanedURL string) string {
	var sum = sha1.Sum([]byte(indexerName + "\x00" + cleanedURL))

	return hex.EncodeToString(sum[:])
}

// FetchTorrent resolves a release DownloadURL produced by this client into a
// .torrent file or, when the link redirects to one, a magnet.
func (c *Client) FetchTorrent(ctx context.Context, downloadURL string) (payload indexer.TorrentPayload, fetchErr error) {
	var bounded, cancel = context.WithTimeout(ctx, c.cfg.RequestTimeout.Duration)
	var client = *c.http
	var redirects int

	defer cancel()
	defer func() {
		if fetchErr != nil {
			c.breaker.Failure(fetchErr)
		} else {
			c.breaker.Success()
		}
	}()
	link, err := url.Parse(downloadURL)
	if err != nil || (link.Scheme != "http" && link.Scheme != "https") || link.User != nil || !c.sameOrigin(link) {
		return payload, errors.New("torznab download link is not on the indexer host")
	}
	if err := c.breaker.Allow(); err != nil {
		return payload, err
	}
	if err := c.limiter.Wait(bounded); err != nil {
		return payload, err
	}
	stripAPIKey(link)
	if c.key != "" {
		var query = link.Query()

		query.Set("apikey", c.key)
		link.RawQuery = query.Encode()
	}
	client.CheckRedirect = func(req *http.Request, _ []*http.Request) error {
		var location = req.Response.Header.Get("Location")

		if strings.HasPrefix(strings.ToLower(location), "magnet:") {
			return &magnetRedirect{magnet: location}
		}
		redirects++
		if redirects > maxRedirects {
			return errors.New("too many redirects")
		}
		if !c.sameOrigin(req.URL) {
			return errors.New("redirect leaves the indexer host")
		}
		return nil
	}
	req, err := http.NewRequestWithContext(bounded, http.MethodGet, link.String(), nil)
	if err != nil {
		return payload, errors.New("invalid Torznab download request")
	}
	req.Header.Set("User-Agent", c.cfg.UserAgent)
	response, err := client.Do(req)
	if err != nil {
		var redirect *magnetRedirect

		if errors.As(err, &redirect) {
			if _, err := tpb.InfoHashFromMagnet(redirect.magnet); err != nil {
				return payload, errors.New("torznab download link redirected to an invalid magnet")
			}
			return indexer.TorrentPayload{Magnet: redirect.magnet}, nil
		}
		if bounded.Err() != nil {
			return payload, errors.New("torznab download timed out")
		}
		return payload, errors.New("torznab download could not be completed")
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return payload, fmt.Errorf("torznab download returned HTTP %d", response.StatusCode)
	}
	data, err := io.ReadAll(io.LimitReader(response.Body, torrentfile.MaxSize+1))
	if err != nil {
		return payload, errors.New("torznab download could not be read")
	}
	if len(data) > torrentfile.MaxSize {
		return payload, fmt.Errorf("torznab torrent file exceeds %d bytes", torrentfile.MaxSize)
	}
	if _, err := torrentfile.InfoHash(data); err != nil {
		return payload, fmt.Errorf("torznab returned an unusable torrent file: %w", err)
	}
	return indexer.TorrentPayload{File: data}, nil
}

var _ indexer.TorrentFetcher = (*Client)(nil)
