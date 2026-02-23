package store

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"
)

// encodeJSON serializes value as JSON and returns it as a SQL argument.
// When nullIf returns true, NULL is stored instead of JSON.
func encodeJSON[T any](value T, nullIf func(T) bool) (any, error) {
	if nullIf != nil && nullIf(value) {
		return nil, nil
	}

	data, err := json.Marshal(value)
	if err != nil {
		return nil, err
	}
	return string(data), nil
}

// DecodeJSON deserializes a nullable JSON SQL value into T.
// For NULL/blank values it returns the zero value of T and nil error.
func DecodeJSON[T any](raw sql.NullString) (T, error) {
	var out T
	if !raw.Valid || strings.TrimSpace(raw.String) == "" {
		return out, nil
	}

	if err := json.Unmarshal([]byte(raw.String), &out); err != nil {
		return out, err
	}
	return out, nil
}

func nullWhenEmptySlice[T any](values []T) bool {
	return len(values) == 0
}

func nullWhenEmptyMap[K comparable, V any](values map[K]V) bool {
	return len(values) == 0
}

func nullWhenNilMap[K comparable, V any](values map[K]V) bool {
	return values == nil
}

// scanList scans all rows with scanFn, wraps scan/iteration errors with
// provided operation names and always closes rows before returning.
func scanList[T any](
	rows *sql.Rows,
	scanFn func(rowScanner) (T, error),
	scanOp string,
	iterOp string,
) ([]T, error) {
	defer rows.Close()

	var result []T
	for rows.Next() {
		item, err := scanFn(rows)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", scanOp, err)
		}
		result = append(result, item)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("%s: %w", iterOp, err)
	}
	return result, nil
}
