package modules

import (
	"context"
	"database/sql"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

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
// applies the migrations of schema. A new file gets its mode from the umask
// (the -wal and -shm files take the mode of the database file); the mode of
// an existing file is not changed.
func OpenSQLite(path, schema string) (*sql.DB, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, err
	}
	dsn := "file:" + sqliteURIPath.Replace(path) + "?_pragma=busy_timeout(10000)&_pragma=journal_mode(WAL)&_pragma=foreign_keys(1)&_pragma=synchronous(NORMAL)&_pragma=temp_store(MEMORY)"
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
	if err := migrate(db, schema); err != nil {
		db.Close()
		return nil, fmt.Errorf("migrate %s: %w", path, err)
	}
	return db, nil
}

func migrate(db *sql.DB, schema string) error {
	if MigrationsFS == nil {
		return fmt.Errorf("migrations filesystem not initialized")
	}
	sub, err := fs.Sub(MigrationsFS, "app/migrations/"+schema)
	if err != nil {
		return err
	}
	p, err := goose.NewProvider(goose.DialectSQLite3, db, sub)
	if err != nil {
		return err
	}
	_, err = p.Up(context.Background())
	return err
}
