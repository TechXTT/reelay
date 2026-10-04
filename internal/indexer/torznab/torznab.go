// Package torznab implements a bounded Torznab adapter. Results with a magnet or
// info hash become magnet releases; results offering only a .torrent link become
// download-URL releases that FetchTorrent resolves at grab time.
package torznab

import (
	"context"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/TechXTT/reelay/internal/clock"
	"github.com/TechXTT/reelay/internal/config"
	"github.com/TechXTT/reelay/internal/indexer"
	"github.com/TechXTT/reelay/internal/indexer/tpb"
	"golang.org/x/time/rate"
)

type Options struct {
	APIKey     string
	HTTPClient *http.Client
	Clock      clock.Clock
}

type Client struct {
	cfg     config.Indexer
	key     string
	base    *url.URL
	http    *http.Client
	limiter *rate.Limiter
	breaker *indexer.Breaker
}

func New(cfg config.Indexer, opt Options) (*Client, error) {
	var base, err = url.Parse(cfg.BaseURL)

	if err != nil || base.Host == "" || (base.Scheme != "http" && base.Scheme != "https") || base.User != nil || base.RawQuery != "" || base.Fragment != "" {
		return nil, errors.New("torznab requires an absolute HTTP(S) API endpoint")
	}
	if cfg.RateLimitPerSecond <= 0 || cfg.RateLimitBurst < 1 || cfg.RequestTimeout.Duration <= 0 {
		return nil, errors.New("torznab requires positive rate limits and timeout")
	}
	if opt.HTTPClient == nil {
		opt.HTTPClient = &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	}
	if opt.APIKey == "" {
		opt.APIKey = cfg.APIKey
		if cfg.APIKeyEnv != "" {
			opt.APIKey = os.Getenv(cfg.APIKeyEnv)
			if opt.APIKey == "" && cfg.Enabled {
				return nil, errors.New("torznab api_key_env is not set")
			}
		}
	}
	return &Client{cfg: cfg, key: opt.APIKey, base: base, http: opt.HTTPClient, limiter: rate.NewLimiter(rate.Limit(cfg.RateLimitPerSecond), cfg.RateLimitBurst), breaker: indexer.NewBreaker(indexer.BreakerOptions{Name: cfg.Name, Threshold: cfg.FailureThreshold, Cooldown: cfg.BreakerCooldown.Duration, Clock: opt.Clock})}, nil
}

func (c *Client) Name() string                  { return c.cfg.Name }
func (c *Client) Healthy(context.Context) error { return c.breaker.Allow() }

func (c *Client) get(ctx context.Context, query url.Values) (*http.Response, error) {
	var endpoint = *c.base

	if err := c.breaker.Allow(); err != nil {
		return nil, err
	}
	if err := c.limiter.Wait(ctx); err != nil {
		return nil, err
	}
	if c.key != "" {
		query.Set("apikey", c.key)
	}
	endpoint.RawQuery = query.Encode()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint.String(), nil)
	if err != nil {
		return nil, errors.New("invalid Torznab request")
	}
	req.Header.Set("User-Agent", c.cfg.UserAgent)
	response, err := c.http.Do(req)
	if err != nil {
		return nil, errors.New("torznab endpoint could not be reached")
	}
	if response.StatusCode != http.StatusOK {
		_ = response.Body.Close()
		return nil, fmt.Errorf("torznab returned HTTP %d", response.StatusCode)
	}
	return response, nil
}

func (c *Client) Probe(ctx context.Context) error {
	var bounded, cancel = context.WithTimeout(ctx, c.cfg.RequestTimeout.Duration)
	var payload struct {
		XMLName xml.Name `xml:"caps"`
		Search  struct {
			Available string `xml:"available,attr"`
		} `xml:"searching>search"`
	}

	defer cancel()
	response, err := c.get(bounded, url.Values{"t": {"caps"}})
	if err != nil {
		c.breaker.Failure(err)
		return err
	}
	defer response.Body.Close()
	if err := xml.NewDecoder(io.LimitReader(response.Body, 1<<20)).Decode(&payload); err != nil {
		c.breaker.Failure(err)
		return errors.New("invalid Torznab capabilities response")
	}
	if payload.Search.Available != "yes" {
		return indexer.ErrUnsupported
	}
	c.breaker.Success()
	return nil
}

// maxItems bounds how many feed items one search decodes and considers.
const maxItems = 100

type feedItem struct {
	Title     string `xml:"title"`
	Link      string `xml:"link"`
	Published string `xml:"pubDate"`
	Enclosure struct {
		URL    string `xml:"url,attr"`
		Length int64  `xml:"length,attr"`
	} `xml:"enclosure"`
	Attributes []struct {
		Name  string `xml:"name,attr"`
		Value string `xml:"value,attr"`
	} `xml:"attr"`
}

func (c *Client) Search(ctx context.Context, query indexer.Query) (result []indexer.Release, searchErr error) {
	var bounded, cancel = context.WithTimeout(ctx, c.cfg.RequestTimeout.Duration)
	var parameters = url.Values{"t": {"search"}, "q": {query.Term}, "cat": {"2000,5000"}, "limit": {"100"}, "extended": {"1"}}
	var releases = make([]indexer.Release, 0)
	var seen = map[string]bool{}

	defer cancel()
	defer func() {
		if searchErr != nil {
			c.breaker.Failure(searchErr)
		} else {
			c.breaker.Success()
		}
	}()
	if query.Recent {
		parameters.Del("q")
	}
	response, err := c.get(bounded, parameters)
	if err != nil {
		return nil, err
	}
	defer response.Body.Close()
	items, err := decodeFeed(io.LimitReader(response.Body, 4<<20))
	if err != nil {
		return nil, err
	}
	for _, item := range items {
		release, ok := c.toRelease(item, query)
		if !ok || seen[release.InfoHash] {
			continue
		}
		seen[release.InfoHash] = true
		releases = append(releases, release)
	}
	return releases, nil
}

// decodeFeed streams the RSS document and returns at most maxItems items. It
// stops reading at the cap, so the closing tag is only required when the feed
// ended before it.
func decodeFeed(reader io.Reader) ([]feedItem, error) {
	var decoder = xml.NewDecoder(reader)
	var items []feedItem
	var rootSeen, rootClosed bool

	for len(items) < maxItems {
		token, err := decoder.Token()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, errors.New("invalid or oversized Torznab XML response")
		}
		if end, ok := token.(xml.EndElement); ok && end.Name.Local == "rss" {
			rootClosed = true
		}
		start, ok := token.(xml.StartElement)
		if !ok {
			continue
		}
		if !rootSeen {
			if start.Name.Local != "rss" {
				return nil, errors.New("torznab response is not RSS")
			}
			rootSeen = true
		}
		if start.Name.Local == "error" {
			return nil, errors.New("torznab reported an API error")
		}
		if start.Name.Local != "item" {
			continue
		}
		var item feedItem
		if err := decoder.DecodeElement(&item, &start); err != nil {
			return nil, errors.New("invalid Torznab item")
		}
		items = append(items, item)
	}
	if !rootSeen {
		return nil, errors.New("empty Torznab XML response")
	}
	if len(items) < maxItems && !rootClosed {
		return nil, errors.New("incomplete Torznab XML response")
	}
	return items, nil
}

// toRelease converts a feed item into a release, reporting false when the item
// is unusable or filtered out by the query. Deduplication is the caller's job.
func (c *Client) toRelease(item feedItem, query indexer.Query) (indexer.Release, bool) {
	var release = indexer.Release{Title: item.Title, SizeBytes: item.Enclosure.Length, Indexer: c.cfg.Name}
	var magnet, hash string

	for _, attribute := range item.Attributes {
		switch attribute.Name {
		case "magneturl":
			magnet = attribute.Value
		case "infohash":
			hash = attribute.Value
		case "size":
			release.SizeBytes, _ = strconv.ParseInt(attribute.Value, 10, 64)
		case "seeders":
			release.Seeders, _ = strconv.Atoi(attribute.Value)
		case "leechers":
			release.Leechers, _ = strconv.Atoi(attribute.Value)
		case "files":
			release.Files, _ = strconv.Atoi(attribute.Value)
		case "imdb":
			release.IMDBID = "tt" + strings.TrimPrefix(attribute.Value, "tt")
		case "category":
			category, _ := strconv.Atoi(attribute.Value)
			if category >= 2000 && category < 3000 {
				release.Category = indexer.CatMoviesHD
			}
			if category >= 5000 && category < 6000 {
				release.Category = indexer.CatTVShowsHD
			}
		}
	}
	if magnet == "" && strings.HasPrefix(item.Enclosure.URL, "magnet:") {
		magnet = item.Enclosure.URL
	}
	if magnet == "" && strings.HasPrefix(item.Link, "magnet:") {
		magnet = item.Link
	}
	if magnet != "" {
		uri, err := url.Parse(magnet)
		if err != nil || uri.Scheme != "magnet" {
			return indexer.Release{}, false
		}
		magnetHash := strings.TrimPrefix(uri.Query().Get("xt"), "urn:btih:")
		if hash != "" {
			expected, err := tpb.NormalizeInfoHash(hash)
			actual, otherErr := tpb.NormalizeInfoHash(magnetHash)
			if err != nil || otherErr != nil || expected != actual {
				return indexer.Release{}, false
			}
		}
		hash = magnetHash
	}
	if magnet == "" && hash == "" {
		var link = item.Enclosure.URL

		if !strings.HasPrefix(link, "http://") && !strings.HasPrefix(link, "https://") {
			link = item.Link
		}
		if strings.HasPrefix(link, "http://") || strings.HasPrefix(link, "https://") {
			cleaned, ok := c.cleanDownloadURL(link)
			if !ok {
				return indexer.Release{}, false
			}
			// A stand-in identity until grab time; see standInKey.
			release.DownloadURL = cleaned
			hash = standInKey(c.cfg.Name, cleaned)
		}
	}
	hash, err := tpb.NormalizeInfoHash(hash)
	if err != nil || release.SizeBytes <= 0 || release.Title == "" || !indexer.IsVideoCategory(release.Category) || release.Seeders < query.MinSeeders {
		return indexer.Release{}, false
	}
	if len(query.Categories) > 0 && !slices.Contains(query.Categories, release.Category) {
		return indexer.Release{}, false
	}
	if magnet == "" && release.DownloadURL == "" {
		magnet, err = tpb.BuildMagnet(hash, item.Title, c.cfg.Trackers)
		if err != nil {
			return indexer.Release{}, false
		}
	}
	release.InfoHash, release.Magnet = hash, magnet
	release.PublishedAt, _ = time.Parse(time.RFC1123Z, item.Published)
	if release.PublishedAt.IsZero() {
		release.PublishedAt, _ = time.Parse(time.RFC1123, item.Published)
	}
	return release, true
}
