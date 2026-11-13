package modules

import (
	"bufio"
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"testing/fstest"
	"time"

	"github.com/pressly/goose/v3"
)

// testMigrations is a migration set of two files; first holds only the
// first of them.
func testMigrations(firstSQL string) (first, both fstest.MapFS) {
	one := &fstest.MapFile{Data: []byte("-- +goose Up\n" + firstSQL + "\n")}
	two := &fstest.MapFile{Data: []byte("-- +goose Up\nCREATE TABLE added (id INTEGER PRIMARY KEY);\n")}
	return fstest.MapFS{"0001_init.sql": one}, fstest.MapFS{"0001_init.sql": one, "0002_added.sql": two}
}

// dbState is what a test compares to see that a database was not changed:
// its user_version, its schema and the migrations goose records.
func dbState(t *testing.T, path string) string {
	t.Helper()
	db, err := sql.Open("sqlite", "file:"+path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var uv int64
	if err := db.QueryRow(`PRAGMA user_version`).Scan(&uv); err != nil {
		t.Fatal(err)
	}
	var b strings.Builder
	b.WriteString("user_version " + strconv.FormatInt(uv, 10) + "\n")
	rows, err := db.Query(`SELECT type, name, COALESCE(sql, '') FROM sqlite_master ORDER BY type, name`)
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var typ, name, text string
		if err := rows.Scan(&typ, &name, &text); err != nil {
			t.Fatal(err)
		}
		b.WriteString(typ + " " + name + " " + text + "\n")
	}
	rows.Close()
	if versions, err := appliedVersions(db); err != nil {
		t.Fatal(err)
	} else {
		var list []int64
		for v := range versions {
			list = append(list, v)
		}
		sort.Slice(list, func(i, j int) bool { return list[i] < list[j] })
		for _, v := range list {
			b.WriteString("applied " + strconv.FormatInt(v, 10) + "\n")
		}
	}
	return b.String()
}

func userVersion(t *testing.T, db *sql.DB) int64 {
	t.Helper()
	var uv int64
	if err := db.QueryRow(`PRAGMA user_version`).Scan(&uv); err != nil {
		t.Fatal(err)
	}
	return uv
}

func wantOtherBuild(t *testing.T, err error, path string) {
	t.Helper()
	var exitErr *ExitError
	if !IsOtherBuildDB(err) && !errors.As(err, &exitErr) {
		t.Fatalf("want the refusal of another build, got %v", err)
	}
	if want := "database " + path + " was created by another build of the program"; !strings.HasPrefix(err.Error(), want) {
		t.Fatalf("message %q does not start with %q", err.Error(), want)
	}
}

// A new database gets the fingerprint of its migrations, opens again, and a
// migration file added later is applied and updates the fingerprint.
func TestMigrationFingerprint(t *testing.T) {
	first, both := testMigrations("CREATE TABLE t (id INTEGER PRIMARY KEY, a TEXT);")
	path := filepath.Join(t.TempDir(), "x.db")
	db, err := openSQLite(path, first)
	if err != nil {
		t.Fatal(err)
	}
	fp1 := userVersion(t, db)
	want, err := migrationFingerprint(first, sourcesOf(t, first))
	if err != nil {
		t.Fatal(err)
	}
	if fp1 == 0 || fp1 != want || fp1 > 0x7fffffff {
		t.Fatalf("user_version %d, want %d (non-zero, 31 bits)", fp1, want)
	}
	db.Close()
	before := dbState(t, path)
	db, err = openSQLite(path, first)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	db.Close()
	if after := dbState(t, path); after != before {
		t.Fatalf("reopening changed the database:\n%s\n%s", before, after)
	}
	db, err = openSQLite(path, both)
	if err != nil {
		t.Fatalf("added migration: %v", err)
	}
	if _, err := db.Exec(`INSERT INTO added (id) VALUES (1)`); err != nil {
		t.Fatalf("added table: %v", err)
	}
	fp2 := userVersion(t, db)
	if fp2 == 0 || fp2 == fp1 {
		t.Fatalf("fingerprint after the added migration: %d (before %d)", fp2, fp1)
	}
	db.Close()
	if db, err = openSQLite(path, both); err != nil {
		t.Fatalf("reopen after the added migration: %v", err)
	}
	db.Close()
	// A build without the added migration refuses this database.
	before = dbState(t, path)
	_, err = openSQLite(path, first)
	wantOtherBuild(t, err, path)
	if after := dbState(t, path); after != before {
		t.Fatalf("the refused open changed the database:\n%s\n%s", before, after)
	}
}

func sourcesOf(t *testing.T, fsys fstest.MapFS) []*goose.Source {
	t.Helper()
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	p, err := goose.NewProvider(goose.DialectSQLite3, db, fsys)
	if err != nil {
		t.Fatal(err)
	}
	return p.ListSources()
}

// A database whose migration file had another content, or that has no
// fingerprint, is refused and not modified.
func TestMigrationFingerprintRefused(t *testing.T) {
	first, _ := testMigrations("CREATE TABLE t (id INTEGER PRIMARY KEY, a TEXT);")
	edited, _ := testMigrations("CREATE TABLE t (id INTEGER PRIMARY KEY, a TEXT, b TEXT);")
	path := filepath.Join(t.TempDir(), "x.db")
	db, err := openSQLite(path, first)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO t (a) VALUES ('kept')`); err != nil {
		t.Fatal(err)
	}
	db.Close()
	before := dbState(t, path)
	_, err = openSQLite(path, edited)
	wantOtherBuild(t, err, path)
	if after := dbState(t, path); after != before {
		t.Fatalf("the refused open changed the database:\n%s\n%s", before, after)
	}

	// Without the fingerprint (a database of a build before it existed).
	raw, err := sql.Open("sqlite", "file:"+path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := raw.Exec(`PRAGMA user_version = 0`); err != nil {
		t.Fatal(err)
	}
	raw.Close()
	before = dbState(t, path)
	_, err = openSQLite(path, first)
	wantOtherBuild(t, err, path)
	if after := dbState(t, path); after != before {
		t.Fatalf("the refused open changed the database:\n%s\n%s", before, after)
	}
	if strings.Contains(before, gooseVersionTable) && !strings.Contains(before, "applied 1") {
		t.Fatalf("state: %s", before)
	}

	// A file of tables that goose never made.
	other := filepath.Join(t.TempDir(), "y.db")
	raw, err = sql.Open("sqlite", "file:"+other)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := raw.Exec(`CREATE TABLE foreign_data (x); PRAGMA user_version = 7`); err != nil {
		t.Fatal(err)
	}
	raw.Close()
	before = dbState(t, other)
	_, err = openSQLite(other, first)
	wantOtherBuild(t, err, other)
	if after := dbState(t, other); after != before {
		t.Fatalf("the refused open changed the database:\n%s\n%s", before, after)
	}
}

// A fingerprint changed by hand is refused like another build.
func TestMigrationFingerprintEdited(t *testing.T) {
	first, _ := testMigrations("CREATE TABLE t (id INTEGER PRIMARY KEY);")
	path := filepath.Join(t.TempDir(), "x.db")
	db, err := openSQLite(path, first)
	if err != nil {
		t.Fatal(err)
	}
	fp := userVersion(t, db)
	if _, err := db.Exec(`PRAGMA user_version = ` + strconv.FormatInt(fp^1, 10)); err != nil {
		t.Fatal(err)
	}
	db.Close()
	before := dbState(t, path)
	_, err = openSQLite(path, first)
	wantOtherBuild(t, err, path)
	if after := dbState(t, path); after != before {
		t.Fatalf("the refused open changed the database:\n%s\n%s", before, after)
	}
}

// When a later migration of a new database fails, the fingerprint covers
// the applied ones, so the next open applies the corrected file instead of
// refusing the database.
func TestMigrationFingerprintPartial(t *testing.T) {
	first, both := testMigrations("CREATE TABLE t (id INTEGER PRIMARY KEY);")
	broken := fstest.MapFS{"0001_init.sql": first["0001_init.sql"],
		"0002_added.sql": &fstest.MapFile{Data: []byte("-- +goose Up\nCREATE TABLE t (id INTEGER);\n")}}
	path := filepath.Join(t.TempDir(), "x.db")
	if _, err := openSQLite(path, broken); err == nil || IsOtherBuildDB(err) {
		t.Fatalf("want the failure of the second migration, got %v", err)
	}
	want, err := migrationFingerprint(first, sourcesOf(t, first))
	if err != nil {
		t.Fatal(err)
	}
	if state := dbState(t, path); !strings.HasPrefix(state, "user_version "+strconv.FormatInt(want, 10)+"\n") {
		t.Fatalf("state after the failed migration:\n%s", state)
	}
	db, err := openSQLite(path, both)
	if err != nil {
		t.Fatalf("open with the corrected migration: %v", err)
	}
	db.Close()
}

// Opens of a new database wait while another process holds the lock of its
// directory, and many opens at the same moment all find it complete.
func TestOpenSQLiteConcurrent(t *testing.T) {
	first, _ := testMigrations("CREATE TABLE t (id INTEGER PRIMARY KEY);")
	dir := t.TempDir()
	path := filepath.Join(dir, "x.db")
	unlock, err := lockDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	opened := make(chan error, 1)
	go func() {
		db, err := openSQLite(path, first)
		if err == nil {
			db.Close()
		}
		opened <- err
	}()
	select {
	case err := <-opened:
		unlock()
		t.Fatalf("the open did not wait for the lock: %v", err)
	case <-time.After(300 * time.Millisecond):
	}
	unlock()
	if err := <-opened; err != nil {
		t.Fatalf("open after the lock was dropped: %v", err)
	}

	path = filepath.Join(dir, "y.db")
	var wg sync.WaitGroup
	errs := make(chan error, 8)
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			db, err := openSQLite(path, first)
			if err == nil {
				db.Close()
			}
			errs <- err
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatalf("concurrent open: %v", err)
		}
	}
}

// The databases of this build open and reopen, and the fingerprint does not
// depend on the line ends of the migration files.
func TestOpenSQLiteSchemas(t *testing.T) {
	MigrationsFS = os.DirFS("../..")
	dir := t.TempDir()
	for _, schema := range []string{SchemaCentral, SchemaAgentDB, SchemaAgentLocal} {
		path := filepath.Join(dir, schema+".db")
		for i := 0; i < 2; i++ {
			db, err := OpenSQLite(path, schema)
			if err != nil {
				t.Fatalf("%s open %d: %v", schema, i+1, err)
			}
			if userVersion(t, db) == 0 {
				t.Fatalf("%s: no fingerprint", schema)
			}
			db.Close()
		}
	}
	lf := fstest.MapFS{"0001_a.sql": {Data: []byte("-- +goose Up\nCREATE TABLE t (x);\n")}}
	crlf := fstest.MapFS{"0001_a.sql": {Data: []byte("-- +goose Up\r\nCREATE TABLE t (x);\r\n")}}
	a, err := migrationFingerprint(lf, sourcesOf(t, lf))
	if err != nil {
		t.Fatal(err)
	}
	b, err := migrationFingerprint(crlf, sourcesOf(t, crlf))
	if err != nil {
		t.Fatal(err)
	}
	if a != b {
		t.Fatalf("fingerprints differ by line ends: %d %d", a, b)
	}
}

// The commands on the central host that change something write the change
// and its history row together: when the history cannot be written, nothing
// is changed and the command fails with ExitConfig. A central database of
// another build is refused by every command with its message.
func TestCLIChangeNeedsHistory(t *testing.T) {
	MigrationsFS = os.DirFS("../..")
	prevDir := dataDirOverride
	prevStdin := stdin
	t.Cleanup(func() { SetDataDir(prevDir); stdin = prevStdin })

	refuseHistory := func(t *testing.T, dir string) {
		t.Helper()
		db, err := OpenSQLite(filepath.Join(dir, CentralDBFileName), SchemaCentral)
		if err != nil {
			t.Fatal(err)
		}
		defer db.Close()
		if _, err := db.Exec(`CREATE TRIGGER refuse_history BEFORE INSERT ON history BEGIN SELECT RAISE(ABORT, 'history refused'); END`); err != nil {
			t.Fatal(err)
		}
	}
	snapshot := func(t *testing.T, dir string) string {
		t.Helper()
		db, err := OpenSQLite(filepath.Join(dir, CentralDBFileName), SchemaCentral)
		if err != nil {
			t.Fatal(err)
		}
		defer db.Close()
		var b strings.Builder
		for _, q := range []string{
			`SELECT email || ' ' || password_hash || ' ' || disabled FROM accounts ORDER BY id`,
			`SELECT key || '=' || value FROM settings WHERE key LIKE 'config.%' ORDER BY key`,
			`SELECT category || ' ' || action || ' ' || target FROM history ORDER BY id`,
		} {
			rows, err := db.Query(q)
			if err != nil {
				t.Fatal(err)
			}
			for rows.Next() {
				var s string
				if err := rows.Scan(&s); err != nil {
					t.Fatal(err)
				}
				b.WriteString(s + "\n")
			}
			rows.Close()
		}
		return b.String()
	}
	wantCode := func(t *testing.T, err error, code int) {
		t.Helper()
		var exitErr *ExitError
		if !errors.As(err, &exitErr) || exitErr.Code != code {
			t.Fatalf("want exit code %d, got %v", code, err)
		}
	}

	t.Run("init-admin", func(t *testing.T) {
		dir := t.TempDir()
		SetDataDir(dir)
		refuseHistory(t, dir)
		before := snapshot(t, dir)
		stdin = bufio.NewReader(strings.NewReader("Admin-pass-123\n"))
		err := (&CentralInitAdminCmd{Email: "root@example.com", DisplayName: "Root", Language: "auto"}).Run()
		wantCode(t, err, ExitConfig)
		if after := snapshot(t, dir); after != before {
			t.Fatalf("changed without history:\n%s\n%s", before, after)
		}
	})

	t.Run("others", func(t *testing.T) {
		dir := t.TempDir()
		SetDataDir(dir)
		stdin = bufio.NewReader(strings.NewReader("Admin-pass-123\n"))
		if err := (&CentralInitAdminCmd{Email: "root@example.com", DisplayName: "Root", Language: "auto"}).Run(); err != nil {
			t.Fatal(err)
		}
		if err := (&CentralConfigSetCmd{Name: "server_names", Value: "a.example.com"}).Run(); err != nil {
			t.Fatal(err)
		}
		db, err := OpenSQLite(filepath.Join(dir, CentralDBFileName), SchemaCentral)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := db.Exec(`UPDATE accounts SET disabled = 1`); err != nil {
			t.Fatal(err)
		}
		db.Close()
		refuseHistory(t, dir)
		before := snapshot(t, dir)
		stdin = bufio.NewReader(strings.NewReader("Other-pass-456\n"))
		wantCode(t, (&CentralResetPasswordCmd{Email: "root@example.com"}).Run(), ExitConfig)
		wantCode(t, (&CentralConfigSetCmd{Name: "server_names", Value: "b.example.com"}).Run(), ExitConfig)
		wantCode(t, (&CentralConfigUnsetCmd{Name: "server_names"}).Run(), ExitConfig)
		if after := snapshot(t, dir); after != before {
			t.Fatalf("changed without history:\n%s\n%s", before, after)
		}
		// An unchanged value writes nothing and needs no history.
		if err := (&CentralConfigSetCmd{Name: "server_names", Value: "a.example.com"}).Run(); err != nil {
			t.Fatalf("unchanged value: %v", err)
		}
	})

	t.Run("other build", func(t *testing.T) {
		dir := t.TempDir()
		SetDataDir(dir)
		path := filepath.Join(dir, CentralDBFileName)
		db, err := OpenSQLite(path, SchemaCentral)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := db.Exec(`PRAGMA user_version = 0`); err != nil {
			t.Fatal(err)
		}
		db.Close()
		before := dbState(t, path)
		stdin = bufio.NewReader(strings.NewReader("Admin-pass-123\nAdmin-pass-123\n"))
		for _, run := range []func() error{
			(&CentralInitAdminCmd{Email: "root@example.com", DisplayName: "Root", Language: "auto"}).Run,
			(&CentralResetPasswordCmd{Email: "root@example.com"}).Run,
			(&CentralConfigSetCmd{Name: "server_names", Value: "a.example.com"}).Run,
			(&CentralConfigUnsetCmd{Name: "server_names"}).Run,
			(&CentralConfigListCmd{}).Run,
			(&CentralConfigGetCmd{Name: "web_listen"}).Run,
		} {
			err := run()
			wantCode(t, err, ExitConfig)
			wantOtherBuild(t, err, path)
		}
		if after := dbState(t, path); after != before {
			t.Fatalf("a refused command changed the database:\n%s\n%s", before, after)
		}
		// On a screen the refusal is a coded message.
		if m := AsMsg(otherBuildDB(path)); m.Code != "common.db_other_build" {
			t.Fatalf("the refusal is not a coded message: %+v", m)
		}
	})
}
