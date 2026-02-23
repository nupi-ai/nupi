package dbutil

// RowScanner is implemented by *sql.Row and *sql.Rows.
type RowScanner interface {
	Scan(dest ...any) error
}
