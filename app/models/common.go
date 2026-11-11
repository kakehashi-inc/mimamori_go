// Package models holds one file per database table. Tables of the central
// management database, of the per-target databases and of the agent's local
// database live here; each function receives the *sql.DB it works on.
package models

import (
	"database/sql"
	"time"
)

// rowScanner abstracts *sql.Row and *sql.Rows.
type rowScanner interface {
	Scan(dest ...any) error
}

// Execer is satisfied by *sql.DB and *sql.Tx.
type Execer interface {
	Exec(query string, args ...any) (sql.Result, error)
	Query(query string, args ...any) (*sql.Rows, error)
	QueryRow(query string, args ...any) *sql.Row
}

// Now returns the current Unix time in seconds.
func Now() int64 { return time.Now().Unix() }

func boolToInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

// collect reads all rows with scan and closes rows. Reading everything before
// issuing another query keeps the connection usage of each flow at one.
func collect[T any](rows *sql.Rows, err error, scan func(rowScanner) (*T, error)) ([]*T, error) {
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*T
	for rows.Next() {
		v, err := scan(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, rows.Err()
}
