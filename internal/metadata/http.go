package metadata

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"time"
)

const maxMetadataResponse = 16 << 20

type httpJSONClient struct {
	base      *url.URL
	http      *http.Client
	timeout   time.Duration
	userAgent string
}

func newHTTPJSONClient(baseURL string, client *http.Client, timeout time.Duration) (httpJSONClient, error) {
	base, err := url.Parse(baseURL)
	if err != nil || base.Scheme == "" || base.Host == "" {
		return httpJSONClient{}, fmt.Errorf("metadata: invalid base url %q", baseURL)
	}
	if client == nil {
		client = &http.Client{}
	}
	if timeout <= 0 {
		timeout = 15 * time.Second
	}
	return httpJSONClient{base: base, http: client, timeout: timeout, userAgent: "Reelay/1.0"}, nil
}

func (c httpJSONClient) get(ctx context.Context, path string, query url.Values, dst any) ([]byte, error) {
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
