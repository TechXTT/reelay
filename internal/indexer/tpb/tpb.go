// Package tpb implements the Indexer interface against The Pirate Bay's JSON
// API (the apibay.org-style q.php endpoint).
//
// HTML scraping is out of scope by design. If the JSON endpoint is unavailable
// the indexer reports itself unhealthy and the engine skips it.
//
// Three properties of the live API drive most of the code here, all measured
// rather than documented:
//
//  1. The `name` field is truncated at 80 characters, mid-word, and dots are
//     flattened to spaces. Release groups and trailing quality tokens are
//     routinely lost. The parser copes; scoring has to know it may be looking
//     at an incomplete name.
//
//  2. There is no empty array. A search with nothing to return sends exactly
//     one row reading "No results returned" — and it sends the same row when
//     it is rate-limiting you. See indexer.ErrNoResults.
//
//  3. Rate limiting is much tighter than it looks. Novel queries a few seconds
//     apart start coming back as the no-results marker; a 45-second pause
//     clears it. Repeated identical queries are served from cache and never
//     trip it, which makes the throttling easy to miss in testing.
package tpb

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"math/rand/v2"
	"net/http"
	"net/url"
	"strings"
	"sync/atomic"
	"time"

	"golang.org/x/time/rate"

	"github.com/TechXTT/reelay/internal/clock"
	"github.com/TechXTT/reelay/internal/config"
	"github.com/TechXTT/reelay/internal/indexer"
)

// maxResponseBytes caps how much we will read from the indexer.
//
// A 100-row response measures about 29 KB, so 8 MB is generous. The cap exists
// because a mirror serving an HTML error page or a hijacked DNS entry serving
// something enormous must not be able to exhaust a 256 MB NAS's memory.
const maxResponseBytes = 8 << 20

// resultCap is the number of rows the API returns at most. Measured: every
// non-empty response is exactly 100 rows or fewer.
const resultCap = 100

type Client struct {
	name     string
	baseURL  *url.URL
	ua       string
	trackers []string

	http       *http.Client
	limiter    *rate.Limiter
	breaker    *indexer.Breaker
	maxRetries int
	timeout    time.Duration
	clock      clock.Clock
	log        *slog.Logger

	// Counters for the health endpoint. The no-results tally is the one that
	// matters: a high ratio is the visible symptom of rate limiting, and
	// without it the operator sees only "nothing was found, ever".
	searches  atomic.Int64
	noResults atomic.Int64
	failures  atomic.Int64
	rows      atomic.Int64
}

type Options struct {
	HTTPClient *http.Client
	Clock      clock.Clock
	Logger     *slog.Logger
}

// New builds a client from the indexer's config section.
func New(cfg config.Indexer, opt Options) (*Client, error) {
	base, err := url.Parse(strings.TrimRight(cfg.BaseURL, "/"))
	if err != nil {
		return nil, fmt.Errorf("tpb %s: parse base_url %q: %w", cfg.Name, cfg.BaseURL, err)
	}
	if base.Scheme == "" || base.Host == "" {
		return nil, fmt.Errorf("tpb %s: base_url %q is not absolute", cfg.Name, cfg.BaseURL)
	}

	if opt.Clock == nil {
		opt.Clock = clock.Real{}
	}
	if opt.Logger == nil {
		opt.Logger = slog.Default()
	}
	httpClient := opt.HTTPClient
	if httpClient == nil {
		httpClient = &http.Client{
			// No client-level timeout: each request gets its own context
			// deadline so a cancelled parent context is honoured immediately
			// rather than at the end of a fixed window.
			Transport: &http.Transport{
				MaxIdleConns:        4,
				MaxIdleConnsPerHost: 2,
				IdleConnTimeout:     90 * time.Second,
			},
		}
	}

	return &Client{
		name:     cfg.Name,
		baseURL:  base,
		ua:       cfg.UserAgent,
		trackers: cfg.Trackers,
		http:     httpClient,
		limiter: rate.NewLimiter(
			rate.Limit(cfg.RateLimitPerSecond), cfg.RateLimitBurst),
		breaker: indexer.NewBreaker(indexer.BreakerOptions{
			Name:      cfg.Name,
			Threshold: cfg.FailureThreshold,
			Cooldown:  cfg.BreakerCooldown.Duration,
			Clock:     opt.Clock,
		}),
		maxRetries: cfg.MaxRetries,
		timeout:    cfg.RequestTimeout.Duration,
		clock:      opt.Clock,
		log:        opt.Logger.With("indexer", cfg.Name),
	}, nil
}

func (c *Client) Name() string { return c.name }

// Healthy reports the breaker state without making a request. The health
// endpoint is polled by browsers and container probes; turning that into
// indexer traffic is a good way to get rate-limited by your own dashboard.
func (c *Client) Healthy(context.Context) error { return c.breaker.Allow() }

// Stats is the snapshot the API exposes.
//
// FailedRequests is deliberately not called Failures: the embedded
// BreakerState already has a Failures field holding the *consecutive* count,
// and two fields with one name means the outer shadows the inner and readers
// silently get the wrong number. These measure different things — lifetime
// total versus current streak — so they get different names.
type Stats struct {
	indexer.BreakerState
	Searches       int64 `json:"searches"`
	NoResults      int64 `json:"no_results"`
	FailedRequests int64 `json:"failed_requests"`
	Rows           int64 `json:"rows_returned"`
}

func (c *Client) Stats() Stats {
	return Stats{
		BreakerState:   c.breaker.State(),
		Searches:       c.searches.Load(),
		NoResults:      c.noResults.Load(),
		FailedRequests: c.failures.Load(),
		Rows:           c.rows.Load(),
	}
}

// Search runs one query and returns the candidates it produced.
//
// Returns a nil slice wrapped with indexer.ErrNoResults when the API answers
// with its no-results marker, which the caller must not read as a definitive
// "this release does not exist" — see the package comment.
func (c *Client) Search(ctx context.Context, q indexer.Query) ([]indexer.Release, error) {
	if err := c.breaker.Allow(); err != nil {
		return nil, err
	}

	endpoint, err := c.endpointFor(q)
	if err != nil {
		return nil, err
	}

	body, err := c.fetch(ctx, endpoint)
	if err != nil {
		c.failures.Add(1)
		c.breaker.Failure(err)
		return nil, err
	}
	c.breaker.Success()
	c.searches.Add(1)

	var rows []row
	if err := json.Unmarshal(body, &rows); err != nil {
		// Malformed JSON is a failure of the mirror, not of the query, so it
		// counts toward the breaker.
		wrapped := fmt.Errorf("tpb %s: decode response from %s: %w", c.name, endpoint, err)
		c.failures.Add(1)
		c.breaker.Failure(wrapped)
		return nil, wrapped
	}

	if isNoResultsMarker(rows) {
		c.noResults.Add(1)
		c.log.Debug("indexer returned its no-results marker",
			"query", q.String(),
			"no_results_total", c.noResults.Load(),
			"searches_total", c.searches.Load())
		return nil, fmt.Errorf("tpb %s: query %q: %w", c.name, q.String(), indexer.ErrNoResults)
	}

	releases := c.toReleases(rows, q)
	c.rows.Add(int64(len(rows)))

	if len(rows) >= resultCap {
		// Worth saying out loud: the best release may simply not be in the
		// window we were given, and a more specific term would find it.
		c.log.Debug("result set hit the indexer's cap; results may be truncated",
			"query", q.String(), "rows", len(rows), "cap", resultCap)
	}
	return releases, nil
}

// endpointFor builds the URL for a query.
//
// Note what is NOT here: a cat= parameter. Omitting it searches every category
// in a single request — verified: a term search with no cat returns rows from
// categories 208, 212, 205, 505 and 601 at once — and Query.Categories filters
// the result afterwards.
//
// The reason to prefer that over cat= is the request budget, not any defect in
// cat=. Both forms work. But the spec's anime rule wants categories 205, 208
// and 299 searched, cat= accepts only one value per request, and this API's
// throttling is severe enough that turning one search into three is the
// expensive choice. Filtering afterwards also cannot lose a correctly-named
// release that was filed under the wrong category, which on this indexer is
// common.
func (c *Client) endpointFor(q indexer.Query) (string, error) {
	u := *c.baseURL

	if q.Recent {
		// The precompiled listing is a static file: cheap, cache-friendly, and
		// the only way to catch a release within minutes of it appearing.
		u.Path = "/precompiled/data_top100_recent.json"
		return u.String(), nil
	}

	term := strings.TrimSpace(q.Term)
	if term == "" {
		return "", fmt.Errorf("tpb %s: %w: a term search needs a term", c.name, indexer.ErrUnsupported)
	}
	u.Path = "/q.php"
	u.RawQuery = url.Values{"q": []string{term}}.Encode()
	return u.String(), nil
}

func (c *Client) toReleases(rows []row, q indexer.Query) []indexer.Release {
	catFilter := make(map[int]bool, len(q.Categories))
	for _, cat := range q.Categories {
		catFilter[cat] = true
	}

	out := make([]indexer.Release, 0, len(rows))
	for _, r := range rows {
		if !r.valid() {
			continue
		}
		if len(catFilter) > 0 && !catFilter[r.Category.Int()] {
			continue
		}
		if r.Seeders.Int() < q.MinSeeders {
			continue
		}

		magnet, err := BuildMagnet(r.InfoHash, r.Name, c.trackers)
		if err != nil {
			// One unusable hash must not discard the rest of the response.
			c.log.Debug("skipping row with unusable info hash",
				"name", r.Name, "info_hash", r.InfoHash, "error", err)
			continue
		}
		hash, err := NormalizeInfoHash(r.InfoHash)
		if err != nil {
			continue
		}

		rel := indexer.Release{
			Title:     r.Name,
			InfoHash:  hash,
			Magnet:    magnet,
			SizeBytes: r.Size.Int64(),
			Seeders:   r.Seeders.Int(),
			Leechers:  r.Leechers.Int(),
			Indexer:   c.name,
			Category:  r.Category.Int(),
			Files:     r.NumFiles.Int(),
			IMDBID:    r.IMDB.String(),
			Uploader:  r.Username,
		}
		if ts := r.Added.Int64(); ts > 0 {
			rel.PublishedAt = time.Unix(ts, 0).UTC()
		}
		out = append(out, rel)
	}
	return out
}

// fetch performs one rate-limited, retried, deadline-bounded GET.
func (c *Client) fetch(ctx context.Context, endpoint string) ([]byte, error) {
	var lastErr error

	attempts := c.maxRetries + 1
	if attempts < 1 {
		attempts = 1
	}

	for attempt := 0; attempt < attempts; attempt++ {
		if attempt > 0 {
			delay := backoff(attempt)
			c.log.Debug("retrying indexer request",
				"attempt", attempt+1, "of", attempts, "delay", delay, "last_error", lastErr)
			select {
			case <-ctx.Done():
				return nil, fmt.Errorf("tpb %s: %w", c.name, ctx.Err())
			case <-time.After(delay):
			}
		}

		// Every outbound call passes through the limiter, retries included:
		// a burst of retries is exactly what gets us throttled.
		if err := c.limiter.Wait(ctx); err != nil {
			return nil, fmt.Errorf("tpb %s: rate limiter: %w", c.name, err)
		}

		body, retryable, err := c.doOnce(ctx, endpoint)
		if err == nil {
			return body, nil
		}
		lastErr = err
		if !retryable {
			return nil, err
		}
		// A cancelled parent context is never retryable, whatever the
		// transport reported.
		if ctx.Err() != nil {
			return nil, fmt.Errorf("tpb %s: %w", c.name, ctx.Err())
		}
	}
	return nil, fmt.Errorf("tpb %s: giving up after %d attempts: %w", c.name, attempts, lastErr)
}

// doOnce issues a single request. The bool reports whether a retry is
// worthwhile: 5xx and transport errors yes, 4xx never.
func (c *Client) doOnce(ctx context.Context, endpoint string) (body []byte, retryable bool, err error) {
	reqCtx, cancel := context.WithTimeout(ctx, c.timeout)
	defer cancel()

	req, err := http.NewRequestWithContext(reqCtx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, false, fmt.Errorf("tpb %s: build request: %w", c.name, err)
	}
	req.Header.Set("User-Agent", c.ua)
	req.Header.Set("Accept", "application/json, text/plain, */*")

	resp, err := c.http.Do(req)
	if err != nil {
		// Transport failures are worth retrying unless the caller gave up.
		return nil, ctx.Err() == nil, fmt.Errorf("tpb %s: GET %s: %w", c.name, endpoint, err)
	}
	defer func() {
		// Drain before closing so the connection can be reused.
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4<<10))
		_ = resp.Body.Close()
	}()

	switch {
	case resp.StatusCode == http.StatusOK:
	case resp.StatusCode >= 500:
		return nil, true, fmt.Errorf("tpb %s: GET %s: server error %s", c.name, endpoint, resp.Status)
	case resp.StatusCode == http.StatusTooManyRequests:
		// Retrying into a 429 makes it worse. Surface it so the breaker opens
		// and the operator sees a real reason to lower the rate limit.
		return nil, false, fmt.Errorf("tpb %s: GET %s: rate limited (%s); lower indexers[].rate_limit_per_second",
			c.name, endpoint, resp.Status)
	default:
		return nil, false, fmt.Errorf("tpb %s: GET %s: unexpected status %s", c.name, endpoint, resp.Status)
	}

	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes+1))
	if err != nil {
		return nil, ctx.Err() == nil, fmt.Errorf("tpb %s: read response: %w", c.name, err)
	}
	if len(raw) > maxResponseBytes {
		return nil, false, fmt.Errorf("tpb %s: response from %s exceeds %d bytes; refusing to buffer it",
			c.name, endpoint, maxResponseBytes)
	}

	// A mirror that has been replaced by a parking page returns 200 with HTML.
	// Saying "expected JSON, got HTML" beats a JSON syntax error at offset 1.
	if trimmed := strings.TrimLeft(string(raw), " \t\r\n"); trimmed != "" &&
		trimmed[0] != '[' && trimmed[0] != '{' {
		return nil, false, fmt.Errorf("tpb %s: %s returned non-JSON (starts with %.24q); is the base_url still a working mirror?",
			c.name, endpoint, trimmed)
	}
	return raw, false, nil
}

// backoff is exponential with full jitter: 1s, 2s, 4s base, randomised across
// the whole interval so concurrent searches that fail together do not retry in
// lockstep.
func backoff(attempt int) time.Duration {
	base := time.Second << (attempt - 1)
	if base > 8*time.Second {
		base = 8 * time.Second
	}
	return time.Duration(rand.Int64N(int64(base))) + base/2
}

// Compile-time proof that the interface is satisfied.
var _ indexer.Indexer = (*Client)(nil)
