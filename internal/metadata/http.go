package metadata

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"time"

	"golang.org/x/time/rate"
)

const maxMetadataResponse = 16 << 20

// cachedJSONClient is the shared HTTP layer of the metadata providers: an
// optional rate limiter, a bounded JSON GET, and a read-through cache.
type cachedJSONClient struct {
	provider  string
	base      *url.URL
	http      *http.Client
	timeout   time.Duration
	userAgent string
	limiter   *rate.Limiter
	cache     Cache
	ttl       time.Duration
	now       func() time.Time
}

type cachedJSONOptions struct {
	Provider string
	BaseURL  string
	Client   *http.Client
	Timeout  time.Duration
	Limiter  *rate.Limiter
	Cache    Cache
	CacheTTL time.Duration
	Now      func() time.Time
}

func newCachedJSONClient(opt cachedJSONOptions) (*cachedJSONClient, error) {
	base, err := url.Parse(opt.BaseURL)
	if err != nil || base.Scheme == "" || base.Host == "" {
		return nil, fmt.Errorf("metadata: invalid base url %q", opt.BaseURL)
	}
	if opt.Client == nil {
		opt.Client = &http.Client{}
	}
	if opt.Timeout <= 0 {
		opt.Timeout = 15 * time.Second
	}
	if opt.CacheTTL <= 0 {
		opt.CacheTTL = 24 * time.Hour
	}
	if opt.Now == nil {
		opt.Now = time.Now
	}
	return &cachedJSONClient{
		provider: opt.Provider, base: base, http: opt.Client, timeout: opt.Timeout, userAgent: "Reelay/1.0",
		limiter: opt.Limiter, cache: opt.Cache, ttl: opt.CacheTTL, now: opt.Now,
	}, nil
}

// cachedGET fills dst from the cache, or from a rate-limited GET that is then
// cached.
func (c *cachedJSONClient) cachedGET(ctx context.Context, cacheKey, path string, query url.Values, dst any) error {
	if c.cache != nil {
		raw, hit, err := c.cache.Get(ctx, c.provider, cacheKey, c.now())
		if err != nil {
			return err
		}
		if hit {
			if err := json.Unmarshal(raw, dst); err != nil {
				return fmt.Errorf("metadata: decode cached %s/%s: %w", c.provider, cacheKey, err)
			}
			return nil
		}
	}
	if c.limiter != nil {
		if err := c.limiter.Wait(ctx); err != nil {
			return fmt.Errorf("%s: rate limit wait: %w", c.provider, err)
		}
	}
	raw, err := c.get(ctx, path, query, dst)
	if err != nil {
		return err
	}
	if c.cache == nil {
		return nil
	}
	return c.cache.Put(ctx, c.provider, cacheKey, raw, c.now(), c.ttl)
}

func (c *cachedJSONClient) get(ctx context.Context, path string, query url.Values, dst any) ([]byte, error) {
	reqCtx, cancel := context.WithTimeout(ctx, c.timeout)
	defer cancel()
	u := *c.base
	u.Path = c.base.Path + path
	u.RawQuery = query.Encode()
	req, err := http.NewRequestWithContext(reqCtx, http.MethodGet, u.String(), nil)
	if err != nil {
		return nil, fmt.Errorf("metadata: build request: %w", err)
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", c.userAgent)
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("metadata: GET %s: %w", path, err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxMetadataResponse+1))
	if err != nil {
		return nil, fmt.Errorf("metadata: read %s: %w", path, err)
	}
	if len(raw) > maxMetadataResponse {
		return nil, fmt.Errorf("metadata: response from %s exceeds %d bytes", path, maxMetadataResponse)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("metadata: GET %s returned %d: %s", path, resp.StatusCode, snippet(raw))
	}
	if err := json.Unmarshal(raw, dst); err != nil {
		return nil, fmt.Errorf("metadata: decode %s: %w", path, err)
	}
	return raw, nil
}

func snippet(raw []byte) string {
	if len(raw) > 200 {
		raw = raw[:200]
	}
	return string(raw)
}
