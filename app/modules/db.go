package modules

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/binary"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"sync"

	"github.com/pressly/goose/v3"
	_ "modernc.org/sqlite" // pure-Go SQLite driver, registered as "sqlite"
)

// Schema names select the migration directory under app/migrations.
const (
	SchemaCentral    = "central"    // central management database
	SchemaAgentDB    = "agentdb"    // per-target database on the central host
	SchemaAgentLocal = "agentlocal" // the agent's local database on each host
)

// sqliteURIPath escapes the characters that have a meaning in an SQLite URI
// filename ("%" starts an escape, "?" the query and "#" the fragment), so
// that any path names exactly that file.
var sqliteURIPath = strings.NewReplacer("%", "%25", "?", "%3f", "#", "%23")

// OpenSQLite opens (creating when missing) the SQLite database at path and
// applies the migrations of schema. Transactions take the write lock when
// they begin (_txlock=immediate) and wait for it (busy_timeout), so a
// transaction that reads and then writes never fails because another one
// wrote in between. A new file gets its mode from the umask
// (the -wal and -shm files take the mode of the database file); the mode of
// an existing file is not changed.
//
// A database that was not created by the migrations of this build (see
// migrate) is refused with the message "common.db_other_build" and is not
// changed; IsOtherBuildDB recognizes that error.
func OpenSQLite(path, schema string) (*sql.DB, error) {
	if MigrationsFS == nil {
		return nil, fmt.Errorf("migrations filesystem not initialized")
	}
	sub, err := fs.Sub(MigrationsFS, "app/migrations/"+schema)
	if err != nil {
		return nil, err
	}
	return openSQLite(path, sub)
}

// openSQLite opens the database at dbPath with the migrations in fsys.
func openSQLite(dbPath string, fsys fs.FS) (*sql.DB, error) {
	if err := os.MkdirAll(filepath.Dir(dbPath), 0o700); err != nil {
		return nil, err
	}
	dsn := "file:" + sqliteURIPath.Replace(dbPath) + "?_txlock=immediate&_pragma=busy_timeout(10000)&_pragma=journal_mode(WAL)&_pragma=foreign_keys(1)&_pragma=synchronous(NORMAL)&_pragma=temp_store(MEMORY)"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	// Each flow uses at most one connection at a time; a small pool is enough.
	db.SetMaxOpenConns(4)
	db.SetMaxIdleConns(2)
	if err := db.Ping(); err != nil {
		db.Close()
		return nil, err
	}
	if err := migrate(db, dbPath, fsys); err != nil {
		db.Close()
		if IsOtherBuildDB(err) {
			return nil, err
		}
		return nil, fmt.Errorf("migrate %s: %w", dbPath, err)
	}
	return db, nil
}

// IsOtherBuildDB reports whether err is (or wraps) the refusal of a
// database created by another build of the program.
func IsOtherBuildDB(err error) bool {
	var m Msg
	return errors.As(err, &m) && m.Code == "common.db_other_build"
}

// otherBuildDB is the refusal of the database at dbPath.
func otherBuildDB(dbPath string) error {
	return M("common.db_other_build", "path", dbPath)
}

// migrateMu serializes the check and migration of the databases opened by
// this process, so that two opens of a new file never see each other's
// half-made state.
var migrateMu sync.Mutex

// goose's table of applied migrations (its default name).
const gooseVersionTable = "goose_db_version"

// migrate checks that the database was made by the migrations in fsys and
// applies the ones it lacks. Before the first release the schema of each
// database kind changes inside its migration file, so a database made by
// another build can record a migration as applied whose content differs.
// Therefore the database keeps a fingerprint of the migration files it was
// made with (migrationFingerprint, in SQLite's user_version):
//
//   - an empty database is migrated and gets the fingerprint of all files;
//   - otherwise the fingerprint of the files it records as applied must be
//     the stored one; then missing migrations (a file added later) are
//     applied and the fingerprint of all files is stored;
//   - a database without a fingerprint, with another one, or with an
//     applied migration this build does not have is refused (otherBuildDB)
//     before anything in it is written.
func migrate(db *sql.DB, dbPath string, fsys fs.FS) error {
	migrateMu.Lock()
	defer migrateMu.Unlock()
	p, err := goose.NewProvider(goose.DialectSQLite3, db, fsys)
	if err != nil {
		return err
	}
	sources := p.ListSources()
	var stored int64
	if err := db.QueryRow(`PRAGMA user_version`).Scan(&stored); err != nil {
		return err
	}
	var objects int
	if err := db.QueryRow(`SELECT COUNT(*) FROM sqlite_master`).Scan(&objects); err != nil {
		return err
	}
	if objects > 0 {
		if stored == 0 {
			return otherBuildDB(dbPath)
		}
		applied, err := appliedVersions(db)
		if err != nil {
			return err
		}
		if applied == nil {
			return otherBuildDB(dbPath)
		}
		var done []*goose.Source
		for _, s := range sources {
			if applied[s.Version] {
				done = append(done, s)
				delete(applied, s.Version)
			}
		}
		if len(applied) > 0 || len(done) == 0 {
			return otherBuildDB(dbPath)
		}
		fp, err := migrationFingerprint(fsys, done)
		if err != nil {
			return err
		}
		if fp != stored {
			return otherBuildDB(dbPath)
		}
		if len(done) == len(sources) {
			return nil
		}
	}
	if _, err := p.Up(context.Background()); err != nil {
		return err
	}
	fp, err := migrationFingerprint(fsys, sources)
	if err != nil {
		return err
	}
	// PRAGMA does not take parameters; fp is a number made here.
	_, err = db.Exec(fmt.Sprintf(`PRAGMA user_version = %d`, fp))
	return err
}

// appliedVersions returns the versions goose records as applied (version 0,
// goose's own start mark, left out), or nil when the database has no goose
// table. It only reads.
func appliedVersions(db *sql.DB) (map[int64]bool, error) {
	var n int
	if err := db.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE type = 'table' AND name = ?`, gooseVersionTable).Scan(&n); err != nil {
		return nil, err
	}
	if n == 0 {
		return nil, nil
	}
	rows, err := db.Query(`SELECT version_id FROM ` + gooseVersionTable + ` WHERE is_applied AND version_id > 0`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[int64]bool{}
	for rows.Next() {
		var v int64
		if err := rows.Scan(&v); err != nil {
			return nil, err
		}
		out[v] = true
	}
	return out, rows.Err()
}

// migrationFingerprint returns a non-zero 31-bit number derived from the
// SHA-256 of the names and contents of the migration files (in version
// order). Line ends are normalized to LF, so a checkout with CRLF line ends
// (a Windows host) builds the same fingerprint.
func migrationFingerprint(fsys fs.FS, sources []*goose.Source) (int64, error) {
	sorted := append([]*goose.Source(nil), sources...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].Version < sorted[j].Version })
	h := sha256.New()
	for _, s := range sorted {
		data, err := fs.ReadFile(fsys, s.Path)
		if err != nil {
			return 0, err
		}
		data = bytes.ReplaceAll(data, []byte("\r\n"), []byte("\n"))
		fmt.Fprintf(h, "%s\x00%d\x00", path.Base(s.Path), len(data))
		h.Write(data)
	}
	sum := h.Sum(nil)
	fp := int64(binary.BigEndian.Uint32(sum[:4]) & 0x7fffffff)
	if fp == 0 {
		fp = 1
	}
	return fp, nil
}

// DBOpenFailed is the error a command returns when OpenSQLite failed: a
// database of another build is reported with its own message and ExitConfig
// (the code of a database problem); any other error with the code and the
// prefix the command gives.
func DBOpenFailed(err error, code int, prefix string) error {
	var m Msg
	if errors.As(err, &m) && m.Code == "common.db_other_build" {
		return NewExitError(ExitConfig, m.Text())
	}
	return NewExitErrorf(code, "%s: %v", prefix, err)
}
