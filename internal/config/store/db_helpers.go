package store

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/nupi-ai/nupi/internal/config/store/dbutil"
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

// encodeJSONString serializes value as JSON and returns it as a string.
// When nullIf returns true, it returns "" and a true null flag.
func encodeJSONString[T any](value T, nullIf func(T) bool) (string, bool, error) {
	if nullIf != nil && nullIf(value) {
		return "", true, nil
	}

	data, err := json.Marshal(value)
	if err != nil {
		return "", false, err
	}
	return string(data), false, nil
}

type upsertOptions struct {
	InsertCreatedAt bool
	InsertUpdatedAt bool
	UpdateUpdatedAt bool
	CreatedAtExpr   string
	UpdatedAtExpr   string
}

type insertOptions struct {
	InsertCreatedAt bool
	InsertUpdatedAt bool
	CreatedAtExpr   string
	UpdatedAtExpr   string
}

func buildUpsertSQL(table string, insertCols []string, conflictCols []string, updateCols []string, opts upsertOptions) string {
	createdExpr := strings.TrimSpace(opts.CreatedAtExpr)
	if createdExpr == "" {
		createdExpr = "CURRENT_TIMESTAMP"
	}
	updatedExpr := strings.TrimSpace(opts.UpdatedAtExpr)
	if updatedExpr == "" {
		updatedExpr = "CURRENT_TIMESTAMP"
	}

	cols := append([]string{}, insertCols...)
	values := make([]string, 0, len(cols)+2)
	for range insertCols {
		values = append(values, "?")
	}
	if opts.InsertCreatedAt {
		cols = append(cols, "created_at")
		values = append(values, createdExpr)
	}
	if opts.InsertUpdatedAt {
		cols = append(cols, "updated_at")
		values = append(values, updatedExpr)
	}

	assignments := make([]string, 0, len(updateCols)+1)
	for _, col := range updateCols {
		assignments = append(assignments, fmt.Sprintf("%s = excluded.%s", col, col))
	}
	if opts.UpdateUpdatedAt {
		assignments = append(assignments, fmt.Sprintf("updated_at = %s", updatedExpr))
	}

	return fmt.Sprintf(
		"INSERT INTO %s (%s) VALUES (%s) ON CONFLICT(%s) DO UPDATE SET %s",
		table,
		strings.Join(cols, ", "),
		strings.Join(values, ", "),
		strings.Join(conflictCols, ", "),
		strings.Join(assignments, ", "),
	)
}

func buildInsertDoNothingSQL(table string, insertCols []string, conflictCols []string, opts insertOptions) string {
	createdExpr := strings.TrimSpace(opts.CreatedAtExpr)
	if createdExpr == "" {
		createdExpr = "CURRENT_TIMESTAMP"
	}
	updatedExpr := strings.TrimSpace(opts.UpdatedAtExpr)
	if updatedExpr == "" {
		updatedExpr = "CURRENT_TIMESTAMP"
	}

	cols := append([]string{}, insertCols...)
	values := make([]string, 0, len(cols)+2)
	for range insertCols {
		values = append(values, "?")
	}
	if opts.InsertCreatedAt {
		cols = append(cols, "created_at")
		values = append(values, createdExpr)
	}
	if opts.InsertUpdatedAt {
		cols = append(cols, "updated_at")
		values = append(values, updatedExpr)
	}

	return fmt.Sprintf(
		"INSERT INTO %s (%s) VALUES (%s) ON CONFLICT(%s) DO NOTHING",
		table,
		strings.Join(cols, ", "),
		strings.Join(values, ", "),
		strings.Join(conflictCols, ", "),
	)
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
	scanFn func(dbutil.RowScanner) (T, error),
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
