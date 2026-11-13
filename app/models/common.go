// Package models holds one file per database table. Tables of the central
// management database, of the per-target databases and of the agent's local
// database live here. Each function receives the side of the database
// handle it works on: a Reader when it only reads, the Writer of a write
// transaction when it writes.
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

// collect reads all rows with scan and closes rows. Rows never leave this
// package: a function has finished its statement and given its connection
// back when it returns, so a flow holds at most one connection at a time and
// never while it waits for something else.
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
