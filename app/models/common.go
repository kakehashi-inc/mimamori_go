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

// Reader runs reading statements. A function that only reads takes one.
type Reader interface {
	Query(query string, args ...any) (*sql.Rows, error)
	QueryRow(query string, args ...any) *sql.Row
}

// Writer runs the statements of one write transaction. A function that
// writes takes one, also when it reads first: what a transaction reads, it
// reads through itself.
//
// *sql.DB and *sql.Tx satisfy both interfaces, and the callers still pass
// them.
type Writer interface {
	Reader
	Exec(query string, args ...any) (sql.Result, error)
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
