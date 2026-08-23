package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

type MetadataCache struct{ s *Store }

func (s *Store) Metadata() *MetadataCache { return &MetadataCache{s: s} }

func (c *MetadataCache) Get(ctx context.Context, provider, key string, now time.Time) ([]byte, bool, error) {
	var payload []byte
	err := c.s.ro.QueryRowContext(ctx, `SELECT payload FROM metadata_cache
 WHERE provider=? AND key=? AND expires_at>?`, provider, key, FormatTime(now)).Scan(&payload)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, fmt.Errorf("metadata cache get %s/%s: %w", provider, key, err)
	}
	return payload, true, nil
}

func (c *MetadataCache) Put(ctx context.Context, provider, key string, payload []byte, fetchedAt time.Time, ttl time.Duration) error {
	if provider == "" || key == "" || len(payload) == 0 || ttl <= 0 {
		return errors.New("metadata cache put requires provider, key, payload, and positive ttl")
	}
	_, err := c.s.rw.ExecContext(ctx, `INSERT INTO metadata_cache
 (provider, key, payload, fetched_at, expires_at) VALUES (?, ?, ?, ?, ?)
 ON CONFLICT(provider, key) DO UPDATE SET payload=excluded.payload,
 fetched_at=excluded.fetched_at, expires_at=excluded.expires_at`, provider, key,
		payload, FormatTime(fetchedAt), FormatTime(fetchedAt.Add(ttl)))
	if err != nil {
		return fmt.Errorf("metadata cache put %s/%s: %w", provider, key, err)
	}
	return nil
}

func (c *MetadataCache) DeleteExpired(ctx context.Context, now time.Time, limit int) (int64, error) {
	if limit <= 0 {
		limit = 1000
	}
	res, err := c.s.rw.ExecContext(ctx, `DELETE FROM metadata_cache WHERE rowid IN (
 SELECT rowid FROM metadata_cache WHERE expires_at<=? LIMIT ?)`, FormatTime(now), limit)
	if err != nil {
		return 0, fmt.Errorf("delete expired metadata cache: %w", err)
	}
	n, err := res.RowsAffected()
	return n, err
}
