package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"

	"github.com/TechXTT/reelay/internal/model"
)

type ReleaseRepository struct{ s *Store }

func (s *Store) Releases() *ReleaseRepository { return &ReleaseRepository{s: s} }

func (r *ReleaseRepository) Upsert(ctx context.Context, in model.StoredRelease) (model.StoredRelease, error) {
	if err := requiredText("release.indexer", in.Indexer); err != nil {
		return in, err
	}
	if err := requiredText("release.info_hash", in.InfoHash); err != nil {
		return in, err
	}
	if err := requiredText("release.raw_title", in.RawTitle); err != nil {
		return in, err
	}
	if in.ParsedJSON == "" {
		in.ParsedJSON = "{}"
	}
	in.InfoHash = strings.ToLower(in.InfoHash)
	if in.SeenAt.IsZero() {
		in.SeenAt = r.s.nowUTC()
	}
	_, err := r.s.rw.ExecContext(ctx, `INSERT INTO releases (
 indexer, raw_title, info_hash, magnet, size_bytes, seeders, leechers,
 published_at, category, parsed_json, score, seen_at
) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
ON CONFLICT (indexer, info_hash) DO UPDATE SET
 raw_title=excluded.raw_title, magnet=excluded.magnet, size_bytes=excluded.size_bytes,
 seeders=excluded.seeders, leechers=excluded.leechers,
 published_at=excluded.published_at, category=excluded.category,
 parsed_json=excluded.parsed_json, score=excluded.score, seen_at=excluded.seen_at`,
		in.Indexer, in.RawTitle, in.InfoHash, in.Magnet, in.SizeBytes, in.Seeders,
		in.Leechers, nullTime(&in.PublishedAt), in.Category, in.ParsedJSON,
		in.Score, FormatTime(in.SeenAt))
	if err != nil {
		return in, fmt.Errorf("upsert release %s/%s: %w", in.Indexer, in.InfoHash, err)
	}
	return r.ByIndexerHash(ctx, in.Indexer, in.InfoHash)
}

func (r *ReleaseRepository) Get(ctx context.Context, id int64) (model.StoredRelease, error) {
	v, err := scanRelease(r.s.ro.QueryRowContext(ctx, selectReleaseSQL+" WHERE id = ?", id))
	if errors.Is(err, sql.ErrNoRows) {
		return v, fmt.Errorf("release %d: %w", id, ErrNotFound)
	}
	return v, err
}

func (r *ReleaseRepository) ByIndexerHash(ctx context.Context, indexer, hash string) (model.StoredRelease, error) {
	v, err := scanRelease(r.s.ro.QueryRowContext(ctx, selectReleaseSQL+
		" WHERE indexer = ? AND info_hash = ?", indexer, strings.ToLower(hash)))
	if errors.Is(err, sql.ErrNoRows) {
		return v, fmt.Errorf("release %s/%s: %w", indexer, hash, ErrNotFound)
	}
	return v, err
}

const selectReleaseSQL = `SELECT id, indexer, raw_title, info_hash, magnet,
 size_bytes, seeders, leechers, published_at, category, parsed_json, score, seen_at
 FROM releases`

func scanRelease(row scanner) (model.StoredRelease, error) {
	var v model.StoredRelease
	var published sql.NullString
	var seen string
	err := row.Scan(&v.ID, &v.Indexer, &v.RawTitle, &v.InfoHash, &v.Magnet,
		&v.SizeBytes, &v.Seeders, &v.Leechers, &published, &v.Category,
		&v.ParsedJSON, &v.Score, &seen)
	if err != nil {
		return v, err
	}
	if t, err := scanNullTime(published); err != nil {
		return v, err
	} else if t != nil {
		v.PublishedAt = *t
	}
	v.SeenAt, err = ParseTime(seen)
	return v, err
}
