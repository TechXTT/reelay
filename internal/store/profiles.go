package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/TechXTT/reelay/internal/model"
)

type ProfileRepository struct{ s *Store }

func (s *Store) Profiles() *ProfileRepository { return &ProfileRepository{s: s} }

// Seed inserts config profiles only into a new database. Subsequent config
// edits do not overwrite profiles the user may have changed through the API.
func (r *ProfileRepository) Seed(ctx context.Context, profiles []model.QualityProfile) (bool, error) {
	if len(profiles) == 0 {
		return false, errors.New("seed profiles: empty profile list")
	}
	seeded := false
	err := r.s.InTx(ctx, func(tx *sql.Tx) error {
		var count int
		if err := tx.QueryRowContext(ctx, "SELECT COUNT(*) FROM quality_profiles").Scan(&count); err != nil {
			return fmt.Errorf("count profiles: %w", err)
		}
		if count > 0 {
			return nil
		}

		hasDefault := false
		for _, p := range profiles {
			if p.IsDefault {
				hasDefault = true
				break
			}
		}
		now := FormatTime(r.s.nowUTC())
		for i, p := range profiles {
			if err := validateProfile(p); err != nil {
				return fmt.Errorf("seed profile %d: %w", i, err)
			}
			if !hasDefault && i == 0 {
				p.IsDefault = true
			}
			args, err := profileArgs(p, now)
			if err != nil {
				return err
			}
			if _, err := tx.ExecContext(ctx, insertProfileSQL, args...); err != nil {
				return fmt.Errorf("insert profile %q: %w", p.Name, err)
			}
		}
		seeded = true
		return nil
	})
	return seeded, err
}

func (r *ProfileRepository) Get(ctx context.Context, id int64) (model.QualityProfile, error) {
	return findOne(r.s.ro.QueryRowContext(ctx, selectProfileSQL+" WHERE id = ?", id), scanProfile, fmt.Sprintf("profile %d", id))
}

func (r *ProfileRepository) Default(ctx context.Context) (model.QualityProfile, error) {
	return findOne(r.s.ro.QueryRowContext(ctx, selectProfileSQL+" ORDER BY is_default DESC, id LIMIT 1"), scanProfile, "default profile")
}

func (r *ProfileRepository) List(ctx context.Context) ([]model.QualityProfile, error) {
	rows, err := r.s.ro.QueryContext(ctx, selectProfileSQL+" ORDER BY is_default DESC, name")
	if err != nil {
		return nil, fmt.Errorf("list profiles: %w", err)
	}
	out, err := collectRows(rows, scanProfile)
	if err != nil {
		return nil, fmt.Errorf("list profiles: %w", err)
	}
	return out, nil
}

func (r *ProfileRepository) Create(ctx context.Context, in model.QualityProfile) (model.QualityProfile, error) {
	if err := validateProfile(in); err != nil {
		return in, err
	}
	now := FormatTime(r.s.nowUTC())
	args, err := profileArgs(in, now)
	if err != nil {
		return in, err
	}
	res, err := r.s.rw.ExecContext(ctx, insertProfileSQL, args...)
	if err != nil {
		return in, fmt.Errorf("create profile: %w", err)
	}
	in.ID, err = res.LastInsertId()
	if err != nil {
		return in, err
	}
	return r.Get(ctx, in.ID)
}

func (r *ProfileRepository) Update(ctx context.Context, in model.QualityProfile) (model.QualityProfile, error) {
	if in.ID <= 0 {
		return in, errors.New("update profile requires id")
	}
	if err := validateProfile(in); err != nil {
		return in, err
	}
	args, err := profileArgs(in, FormatTime(r.s.nowUTC()))
	if err != nil {
		return in, err
	}
	// profileArgs includes created_at; updates preserve it and use only the
	// mutable values plus the generated updated timestamp.
	res, err := r.s.rw.ExecContext(ctx, `UPDATE quality_profiles SET name=?, is_default=?,
 allowed_resolutions_json=?, allowed_sources_json=?, min_size_mb=?, max_size_mb=?,
 min_seeders=?, required_terms_json=?, banned_terms_json=?, preferred_groups_json=?,
 language_prefs_json=?, hdr_prefs_json=?, upgrade_until=NULLIF(?,''), updated_at=? WHERE id=?`,
		args[0], args[1], args[2], args[3], args[4], args[5], args[6], args[7],
		args[8], args[9], args[10], args[11], args[12], args[14], in.ID)
	if err != nil {
		return in, fmt.Errorf("update profile %d: %w", in.ID, err)
	}
	if n, _ := res.RowsAffected(); n != 1 {
		return in, fmt.Errorf("profile %d: %w", in.ID, ErrNotFound)
	}
	return r.Get(ctx, in.ID)
}

func (r *ProfileRepository) Delete(ctx context.Context, id int64) error {
	res, err := r.s.rw.ExecContext(ctx, "DELETE FROM quality_profiles WHERE id=? AND is_default=0", id)
	if err != nil {
		return fmt.Errorf("delete profile %d: %w", id, err)
	}
	if n, _ := res.RowsAffected(); n != 1 {
		return fmt.Errorf("profile %d not found or is the default profile", id)
	}
	return nil
}

func validateProfile(p model.QualityProfile) error {
	if err := requiredText("profile.name", p.Name); err != nil {
		return err
	}
	if len(p.AllowedResolutions) == 0 || len(p.AllowedSources) == 0 {
		return errors.New("profile resolutions and sources must not be empty")
	}
	if p.MinSizeMB < 0 || p.MaxSizeMB < p.MinSizeMB || p.MinSeeders < 0 {
		return errors.New("profile size and seeder limits are invalid")
	}
	return nil
}

const insertProfileSQL = `
INSERT INTO quality_profiles (
 name, is_default, allowed_resolutions_json, allowed_sources_json,
 min_size_mb, max_size_mb, min_seeders, required_terms_json,
 banned_terms_json, preferred_groups_json, language_prefs_json, hdr_prefs_json,
 upgrade_until, created_at, updated_at
) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, NULLIF(?, ''), ?, ?)`

func profileArgs(p model.QualityProfile, now string) ([]any, error) {
	res, err := encodeJSON(p.AllowedResolutions)
	if err != nil {
		return nil, err
	}
	sources, err := encodeJSON(p.AllowedSources)
	if err != nil {
		return nil, err
	}
	required, err := encodeJSON(p.RequiredTerms)
	if err != nil {
		return nil, err
	}
	banned, err := encodeJSON(p.BannedTerms)
	if err != nil {
		return nil, err
	}
	groups, err := encodeJSON(p.PreferredGroups)
	if err != nil {
		return nil, err
	}
	languages, err := encodeJSON(p.LanguagePrefs)
	if err != nil {
		return nil, err
	}
	hdr, err := encodeJSON(p.HDRPrefs)
	if err != nil {
		return nil, err
	}
	return []any{p.Name, p.IsDefault, res, sources, p.MinSizeMB, p.MaxSizeMB,
		p.MinSeeders, required, banned, groups, languages, hdr, p.UpgradeUntil, now, now}, nil
}

const selectProfileSQL = `SELECT id, name, is_default,
 allowed_resolutions_json, allowed_sources_json, min_size_mb, max_size_mb,
 min_seeders, required_terms_json, banned_terms_json, preferred_groups_json,
 language_prefs_json, hdr_prefs_json, COALESCE(upgrade_until, ''), created_at, updated_at
 FROM quality_profiles`

type scanner interface{ Scan(...any) error }

func scanProfile(row scanner) (model.QualityProfile, error) {
	var p model.QualityProfile
	var isDefault int
	var resolutions, sources, required, banned, groups, languages, hdr string
	var created, updated string
	err := row.Scan(&p.ID, &p.Name, &isDefault, &resolutions, &sources,
		&p.MinSizeMB, &p.MaxSizeMB, &p.MinSeeders, &required, &banned, &groups,
		&languages, &hdr, &p.UpgradeUntil, &created, &updated)
	if err != nil {
		return p, err
	}
	p.IsDefault = isDefault == 1
	values := []struct {
		raw string
		dst any
	}{
		{resolutions, &p.AllowedResolutions}, {sources, &p.AllowedSources},
		{required, &p.RequiredTerms}, {banned, &p.BannedTerms},
		{groups, &p.PreferredGroups}, {languages, &p.LanguagePrefs},
		{hdr, &p.HDRPrefs},
	}
	for _, value := range values {
		if err := decodeJSON(value.raw, value.dst); err != nil {
			return p, fmt.Errorf("scan profile %d: %w", p.ID, err)
		}
	}
	p.CreatedAt, err = ParseTime(created)
	if err != nil {
		return p, err
	}
	p.UpdatedAt, err = ParseTime(updated)
	return p, err
}
