package modules

import (
	"context"
	"database/sql"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"

	"github.com/pressly/goose/v3"
	_ "modernc.org/sqlite" // pure-Go SQLite driver, registered as "sqlite"
)

// Schema names select the migration directory under app/migrations.
const (
	SchemaCentral    = "central"    // central management database
	SchemaAgentDB    = "agentdb"    // per-target database on the central host
	SchemaAgentLocal = "agentlocal" // the agent's local database on each host
)

// OpenSQLite opens (creating when missing) the SQLite database at path,
// restricts the file to its owner and applies the migrations of schema.
func OpenSQLite(path, schema string) (*sql.DB, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, err
	}
	dsn := "file:" + path + "?_pragma=busy_timeout(10000)&_pragma=journal_mode(WAL)&_pragma=foreign_keys(1)&_pragma=synchronous(NORMAL)&_pragma=temp_store(MEMORY)"
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
	for _, p := range []string{path, path + "-wal", path + "-shm"} {
		if _, err := os.Stat(p); err == nil {
			_ = os.Chmod(p, 0o600)
		}
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
