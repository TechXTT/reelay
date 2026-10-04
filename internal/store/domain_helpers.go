package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
)

var (
	ErrConflict          = errors.New("conflict")
	ErrInvalidTransition = errors.New("invalid state transition")
	ErrLocked            = errors.New("item is locked")
	ErrItemBusy          = errors.New("item already has work in progress")
)

func encodeJSON(v any) (string, error) {
	b, err := json.Marshal(v)
	if err != nil {
		return "", fmt.Errorf("encode json: %w", err)
	}
	return string(b), nil
}

func decodeJSON(raw string, dst any) error {
	if err := json.Unmarshal([]byte(raw), dst); err != nil {
		return fmt.Errorf("decode stored json: %w", err)
	}
	return nil
}

func nullTime(t *time.Time) any {
	if t == nil || t.IsZero() {
		return nil
	}
	return FormatTime(*t)
}

func scanNullTime(v sql.NullString) (*time.Time, error) {
	if !v.Valid || strings.TrimSpace(v.String) == "" {
		return nil, nil
	}
	t, err := ParseTime(v.String)
	if err != nil {
		return nil, err
	}
	return &t, nil
}

func requiredText(field, value string) error {
	if strings.TrimSpace(value) == "" {
		return fmt.Errorf("%s is required", field)
	}
	return nil
}

func collectRows[T any](rows *sql.Rows, scan func(scanner) (T, error)) ([]T, error) {
	defer rows.Close()
	var values []T
	for rows.Next() {
		value, err := scan(rows)
		if err != nil {
			return nil, err
		}
		values = append(values, value)
	}
	return values, rows.Err()
}

func scanColumn[T any](row scanner) (T, error) {
	var value T
	err := row.Scan(&value)
	return value, err
}

func collectSet[T comparable](rows *sql.Rows) (map[T]bool, error) {
	values, err := collectRows(rows, scanColumn[T])
	if err != nil {
		return nil, err
	}
	set := make(map[T]bool, len(values))
	for _, value := range values {
		set[value] = true
	}
	return set, nil
}

func findOne[T any](row scanner, scan func(scanner) (T, error), label string) (T, error) {
	value, err := scan(row)
	if errors.Is(err, sql.ErrNoRows) {
		return value, fmt.Errorf("%s: %w", label, ErrNotFound)
	}
	return value, err
}

func (s *Store) deleteOne(ctx context.Context, query, label string, id int64) error {
	result, err := s.rw.ExecContext(ctx, query, id)
	if err != nil {
		return fmt.Errorf("delete %s %d: %w", label, id, err)
	}
	if count, _ := result.RowsAffected(); count != 1 {
		return fmt.Errorf("%s %d: %w", label, id, ErrNotFound)
	}
	return nil
}

func placeholders(count int) string {
	return strings.TrimSuffix(strings.Repeat("?,", count), ",")
}

// maxSQLVariables keeps IN (...) lists well under the SQLite bound-variable limit.
const maxSQLVariables = 500

// chunkIDs splits ids into slices small enough for one IN (...) query each.
func chunkIDs(ids []int64) [][]int64 {
	var chunks [][]int64
	for len(ids) > 0 {
		size := min(maxSQLVariables, len(ids))
		chunks = append(chunks, ids[:size])
		ids = ids[size:]
	}
	return chunks
}

func int64Args(ids []int64) []any {
	args := make([]any, len(ids))
	for i, id := range ids {
		args[i] = id
	}
	return args
}
