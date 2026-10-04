package store

import (
	"context"
	"database/sql"
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
	stored, err := scanRelease(r.s.rw.QueryRowContext(ctx, `INSERT INTO releases (
 indexer, raw_title, info_hash, magnet, size_bytes, seeders, leechers,
 published_at, category, parsed_json, score, seen_at
) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
ON CONFLICT (indexer, info_hash) DO UPDATE SET
 raw_title=excluded.raw_title, magnet=excluded.magnet, size_bytes=excluded.size_bytes,
 seeders=excluded.seeders, leechers=excluded.leechers,
 published_at=excluded.published_at, category=excluded.category,
 parsed_json=excluded.parsed_json, score=excluded.score, seen_at=excluded.seen_at
RETURNING `+releaseColumns,
		in.Indexer, in.RawTitle, in.InfoHash, in.Magnet, in.SizeBytes, in.Seeders,
		in.Leechers, nullTime(&in.PublishedAt), in.Category, in.ParsedJSON,
		in.Score, FormatTime(in.SeenAt)))
	if err != nil {
		return in, fmt.Errorf("upsert release %s/%s: %w", in.Indexer, in.InfoHash, err)
	}
	return stored, nil
}

func (r *ReleaseRepository) Get(ctx context.Context, id int64) (model.StoredRelease, error) {
	return findOne(r.s.ro.QueryRowContext(ctx, selectReleaseSQL+" WHERE id = ?", id), scanRelease, fmt.Sprintf("release %d", id))
}

func (r *ReleaseRepository) ByIndexerHash(ctx context.Context, indexer, hash string) (model.StoredRelease, error) {
	return findOne(r.s.ro.QueryRowContext(ctx, selectReleaseSQL+
		" WHERE indexer = ? AND info_hash = ?", indexer, strings.ToLower(hash)), scanRelease,
		fmt.Sprintf("release %s/%s", indexer, hash))
}

const releaseColumns = `id, indexer, raw_title, info_hash, magnet,
 size_bytes, seeders, leechers, published_at, category, parsed_json, score, seen_at`

const selectReleaseSQL = "SELECT " + releaseColumns + " FROM releases"

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

// GetMany returns the stored releases among ids keyed by ID; unknown ids are absent.
func (r *ReleaseRepository) GetMany(ctx context.Context, ids []int64) (map[int64]model.StoredRelease, error) {
	releases := make(map[int64]model.StoredRelease, len(ids))
	for _, chunk := range chunkIDs(ids) {
		rows, err := r.s.ro.QueryContext(ctx, selectReleaseSQL+" WHERE id IN ("+placeholders(len(chunk))+")", int64Args(chunk)...)
		if err != nil {
			return nil, fmt.Errorf("list releases: %w", err)
		}
		values, err := collectRows(rows, scanRelease)
		if err != nil {
			return nil, err
		}
		for _, value := range values {
			releases[value.ID] = value
		}
	}
	return releases, nil
}
