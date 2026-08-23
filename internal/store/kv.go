package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// ErrNotFound is returned by every repository lookup that finds nothing, so
// callers can branch without importing database/sql.
var ErrNotFound = errors.New("not found")

// KV is small durable bookkeeping: last run time per scheduler loop, first-run
// markers, the "profiles already seeded" flag. Not for domain data.
type KV struct{ s *Store }

func (s *Store) KV() *KV { return &KV{s: s} }

func (k *KV) Get(ctx context.Context, key string) (string, error) {
	var v string
	err := k.s.ro.QueryRowContext(ctx, "SELECT value FROM kv WHERE key = ?", key).Scan(&v)
	if errors.Is(err, sql.ErrNoRows) {
		return "", fmt.Errorf("kv %q: %w", key, ErrNotFound)
	}
	if err != nil {
		return "", fmt.Errorf("kv get %q: %w", key, err)
	}
	return v, nil
}

func (k *KV) Set(ctx context.Context, key, value string) error {
	_, err := k.s.rw.ExecContext(ctx, `
INSERT INTO kv (key, value, updated_at) VALUES (?, ?, ?)
ON CONFLICT (key) DO UPDATE SET value = excluded.value, updated_at = excluded.updated_at`,
		key, value, FormatTime(k.s.nowUTC()))
	if err != nil {
		return fmt.Errorf("kv set %q: %w", key, err)
	}
	return nil
}

func (k *KV) GetTime(ctx context.Context, key string) (time.Time, error) {
	raw, err := k.Get(ctx, key)
	if err != nil {
		return time.Time{}, err
	}
	return ParseTime(raw)
}

func (k *KV) SetTime(ctx context.Context, key string, t time.Time) error {
	return k.Set(ctx, key, FormatTime(t))
}

func (k *KV) Delete(ctx context.Context, key string) error {
	if _, err := k.s.rw.ExecContext(ctx, "DELETE FROM kv WHERE key = ?", key); err != nil {
		return fmt.Errorf("kv delete %q: %w", key, err)
	}
	return nil
}
