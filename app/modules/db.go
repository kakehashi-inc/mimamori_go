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
	"runtime"
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

// writeDSN names the database at dbPath for a connection that reads and
// writes. Its transactions take the write lock when they begin
// (_txlock=immediate), so a transaction that reads and then writes never
// fails because another one wrote in between. lockWaitMS is how long SQLite
// waits for a lock that another process holds (busy_timeout).
func writeDSN(dbPath string, lockWaitMS int) string {
	return fmt.Sprintf("file:%s?_txlock=immediate&_pragma=busy_timeout(%d)&_pragma=journal_mode(WAL)&_pragma=foreign_keys(1)&_pragma=synchronous(NORMAL)&_pragma=temp_store(MEMORY)",
		sqliteURIPath.Replace(dbPath), lockWaitMS)
}

// readDSN names the database at dbPath for a read-only connection
// (mode=ro): SQLite opens the file read-only and refuses every write through
// the connection. The journal mode is not set here: the file is already in
// WAL mode when a read connection is opened (the write connection of the
// handle set it), and the driver fails the open of a read-only connection
// that asks for WAL on a file that is not.
func readDSN(dbPath string, lockWaitMS int) string {
	return fmt.Sprintf("file:%s?mode=ro&_pragma=busy_timeout(%d)&_pragma=temp_store(MEMORY)",
		sqliteURIPath.Replace(dbPath), lockWaitMS)
}

// schemaMigrations returns the migration files of schema.
func schemaMigrations(schema string) (fs.FS, error) {
	if MigrationsFS == nil {
		return nil, fmt.Errorf("migrations filesystem not initialized")
	}
	return fs.Sub(MigrationsFS, "app/migrations/"+schema)
}

// openOptions are the values of an open that only tests set differently.
type openOptions struct {
	lockWaitMS int // SQLite's wait for a lock of another process
	readConns  int // read connections that can be open at the same time
}

// defaultOpenOptions returns the values every open outside tests uses. A
// file gets as many read connections as there are processors (at least
// ReadConnsMin): reading is bounded by the processors, and more connections
// than that only take processor time from each other and from the writer.
func defaultOpenOptions() openOptions {
	return openOptions{lockWaitMS: DBLockWaitMS, readConns: max(ReadConnsMin, runtime.GOMAXPROCS(0))}
}

// OpenSQLite opens (creating when missing) the SQLite database at path,
// applies the migrations of schema and returns the handle through which
// this process reads and writes the file (see DB). A new file gets its mode
// from the umask (the -wal and -shm files take the mode of the database
// file); the mode of an existing file is not changed.
//
// A file has one handle per process: opening a path whose handle is still
// open fails, because a second handle would be a second write connection
// and the two would wait on SQLite's lock inside the process.
//
// A database that was not created by the migrations of this build (see
// migrate) is refused with the message "common.db_other_build" and is not
// changed; IsOtherBuildDB recognizes that error.
func OpenSQLite(path, schema string) (*DB, error) {
	sub, err := schemaMigrations(schema)
	if err != nil {
		return nil, err
	}
	return openDB(path, sub, defaultOpenOptions(), true)
}

// openSQLite opens the database at dbPath with the migrations in fsys. It
// does not refuse a path that already has a handle: tests open one file
// from several flows with it, the way several processes would.
func openSQLite(dbPath string, fsys fs.FS) (*DB, error) {
	return openDB(dbPath, fsys, defaultOpenOptions(), false)
}

// openDB opens the handle of the database at dbPath: the write connection
// first, through which the database is checked and migrated, then the read
// side. With single it refuses a path that has an open handle in this
// process. When it fails, everything it opened is closed.
func openDB(dbPath string, fsys fs.FS, o openOptions, single bool) (_ *DB, err error) {
	d := &DB{path: dbPath}
	d.refuseInsideWrite(nestedOpen)
	if single {
		if err := d.register(); err != nil {
			return nil, err
		}
		defer func() {
			if err != nil {
				d.unregister()
			}
		}()
	}
	if err := os.MkdirAll(filepath.Dir(dbPath), 0o700); err != nil {
		return nil, err
	}
	// The check and the migration of the database happen while this process
	// holds migrateMu and the lock of the database's directory, so that two
	// opens of a new file (in this process or in two processes) never see
	// each other's half-made state: the second one waits and then finds the
	// database complete with its fingerprint.
	migrateMu.Lock()
	defer migrateMu.Unlock()
	unlock, err := lockDir(filepath.Dir(dbPath))
	if err != nil {
		return nil, err
	}
	defer unlock()
	// database/sql only keeps the one write connection (and opens it again
	// should the driver ever report it broken); whose turn it is, the
	// handle decides (DB.Write).
	if d.w, err = sql.Open("sqlite", writeDSN(dbPath, o.lockWaitMS)); err != nil {
		return nil, err
	}
	defer func() {
		if err != nil {
			d.w.Close()
		}
	}()
	d.w.SetMaxOpenConns(1)
	d.w.SetMaxIdleConns(1)
	if err := d.w.Ping(); err != nil {
		return nil, err
	}
	if err := migrate(d.w, dbPath, fsys); err != nil {
		if IsOtherBuildDB(err) {
			return nil, err
		}
		return nil, fmt.Errorf("migrate %s: %w", dbPath, err)
	}
	if d.r, err = sql.Open("sqlite", readDSN(dbPath, o.lockWaitMS)); err != nil {
		return nil, err
	}
	d.r.SetMaxOpenConns(o.readConns)
	d.r.SetMaxIdleConns(o.readConns)
	// The first read connection is opened now, so that a file that cannot
	// be opened read-only fails here and not at the first read.
	if err := d.r.Ping(); err != nil {
		d.r.Close()
		return nil, err
	}
	return d, nil
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
// this process (see openDB).
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
//
// The caller holds the locks that keep other opens out (see openDB).
func migrate(db *sql.DB, dbPath string, fsys fs.FS) error {
	p, err := goose.NewProvider(goose.DialectSQLite3, db, fsys)
	if err != nil {
		return err
	}
	sources := p.ListSources()
	var stored int64
	if err := db.QueryRow(`PRAGMA user_version`).Scan(&stored); err != nil {
		return err
	}
	var objects, appliedBefore int
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
		appliedBefore = len(done)
	}
	// Each migration is applied in its own transaction. The fingerprint is
	// written when this open applied at least one; when a later one failed,
	// it covers the ones that were applied, so the next open tries the
	// failed one again instead of refusing the database.
	results, upErr := p.Up(context.Background())
	done := sources
	if upErr != nil {
		applied, err := appliedVersions(db)
		if err != nil {
			return errors.Join(upErr, err)
		}
		done = nil
		for _, s := range sources {
			if applied[s.Version] {
				done = append(done, s)
			}
		}
		if len(done) == appliedBefore {
			return upErr
		}
	} else if len(results) == 0 {
		return nil
	}
	fp, err := migrationFingerprint(fsys, done)
	if err != nil {
		return errors.Join(upErr, err)
	}
	// PRAGMA does not take parameters; fp is a number made here.
	if _, err := db.Exec(fmt.Sprintf(`PRAGMA user_version = %d`, fp)); err != nil {
		return errors.Join(upErr, err)
	}
	return upErr
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
