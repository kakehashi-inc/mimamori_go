package modules

import (
	"bufio"
	"database/sql"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"testing/fstest"
	"time"

	"modernc.org/sqlite"

	"mimamori/app/models"
)

// handleMigrations makes the database of the tests of the handle: one
// table with one row (id 1, v 0).
var handleMigrations = fstest.MapFS{"0001_init.sql": &fstest.MapFile{Data: []byte(
	"-- +goose Up\nCREATE TABLE t (id INTEGER PRIMARY KEY, v INTEGER NOT NULL);\nINSERT INTO t (id, v) VALUES (1, 0);\n")}}

// openHandle opens a new database of handleMigrations with the options o
// (the zero value of a field means its default).
func openHandle(t testing.TB, o openOptions) *DB {
	t.Helper()
	def := defaultOpenOptions()
	if o.lockWaitMS == 0 {
		o.lockWaitMS = def.lockWaitMS
	}
	if o.readConns == 0 {
		o.readConns = def.readConns
	}
	d, err := openDB(filepath.Join(t.TempDir(), "x.db"), handleMigrations, o, false)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { d.Close() })
	return d
}

// rowV returns the value of the row id (the error of a missing row
// included).
func rowV(r models.Reader, id int) (v int, err error) {
	err = r.QueryRow(`SELECT v FROM t WHERE id = ?`, id).Scan(&v)
	return v, err
}

// wantV fails the test unless row 1 holds want on the read side.
func wantV(t *testing.T, d *DB, want int) {
	t.Helper()
	if v, err := rowV(d.Read(), 1); err != nil || v != want {
		t.Fatalf("v = %d (%v), want %d", v, err, want)
	}
}

// setV stores v in row 1.
func setV(tx models.Writer, v int) error {
	_, err := tx.Exec(`UPDATE t SET v = ? WHERE id = 1`, v)
	return err
}

// wantPanic runs fn and fails the test unless it panics with a text that
// contains every part.
func wantPanic(t *testing.T, fn func(), parts ...string) {
	t.Helper()
	defer func() {
		t.Helper()
		p := recover()
		if p == nil {
			t.Fatalf("no panic, want one containing %q", parts)
		}
		for _, part := range parts {
			if !strings.Contains(fmt.Sprint(p), part) {
				t.Fatalf("panic %q does not contain %q", fmt.Sprint(p), part)
			}
		}
	}()
	fn()
}

// wantWriteConnFree fails the test unless another flow gets the write
// connection of d (it is not left held after a panic).
func wantWriteConnFree(t *testing.T, d *DB) {
	t.Helper()
	done := make(chan error, 1)
	go func() { done <- d.Write(func(models.Writer) error { return nil }) }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the write connection stayed held")
	}
}

// isBusy reports whether err is SQLite's "database is locked" (code 5).
func isBusy(err error) bool {
	var se *sqlite.Error
	return errors.As(err, &se) && se.Code()&0xff == 5
}

// The guard against nested use needs the number of the calling goroutine.
// When this test fails, the toolchain prints stack traces in another form
// and goroutineNumber has to learn it.
func TestGoroutineNumber(t *testing.T) {
	here := goroutineNumber()
	other := make(chan uint64)
	go func() { other <- goroutineNumber() }()
	there := <-other
	if here == 0 || there == 0 || here == there {
		t.Fatalf("goroutine numbers %d and %d, want two different numbers above 0", here, there)
	}
	if again := goroutineNumber(); again != here {
		t.Fatalf("goroutine number changed from %d to %d within one goroutine", here, again)
	}
}

// Every kind of write sent through the reading side fails, recognizably,
// and changes nothing.
func TestReadSideRefusesWrites(t *testing.T) {
	d := openHandle(t, openOptions{})
	state := func() string {
		var version, rows, tables int
		v, err := rowV(d.Read(), 1)
		if err == nil {
			err = d.Read().QueryRow(`PRAGMA user_version`).Scan(&version)
		}
		if err == nil {
			err = d.Read().QueryRow(`SELECT COUNT(*) FROM t`).Scan(&rows)
		}
		if err == nil {
			err = d.Read().QueryRow(`SELECT COUNT(*) FROM sqlite_master`).Scan(&tables)
		}
		if err != nil {
			t.Fatal(err)
		}
		return fmt.Sprintf("v %d, user_version %d, %d rows, %d objects", v, version, rows, tables)
	}
	before := state()
	for _, q := range []string{
		`INSERT INTO t (id, v) VALUES (2, 1)`,
		`UPDATE t SET v = 9`,
		`DELETE FROM t`,
		`CREATE TABLE u (id INTEGER PRIMARY KEY)`,
		`PRAGMA user_version = 5`,
		`DELETE FROM t RETURNING v`,
	} {
		rows, err := d.Read().Query(q)
		if err == nil {
			for rows.Next() {
			}
			err = rows.Err()
			rows.Close()
		}
		if !IsWriteThroughRead(err) {
			t.Errorf("%s through Query of the read side: %v, want the refusal of a write", q, err)
		}
	}
	var v int
	err := d.Read().QueryRow(`UPDATE t SET v = v + 1 WHERE id = 1 RETURNING v`).Scan(&v)
	if !IsWriteThroughRead(err) {
		t.Errorf("UPDATE ... RETURNING through QueryRow of the read side: %v, want the refusal of a write", err)
	}
	if !IsWriteThroughRead(fmt.Errorf("stored: %w", err)) {
		t.Error("the refusal is not recognized when it is wrapped")
	}
	if after := state(); after != before {
		t.Errorf("the refused writes changed the database: %s, before %s", after, before)
	}
	// Other errors are not taken for the refusal.
	_, missing := rowV(d.Read(), 99)
	_, noTable := d.Read().Query(`SELECT 1 FROM nothing`)
	for _, err := range []error{nil, missing, noTable, errors.New("attempt to write a readonly database")} {
		if IsWriteThroughRead(err) {
			t.Errorf("%v is taken for the refusal of a write", err)
		}
	}
	if !errors.Is(missing, sql.ErrNoRows) || noTable == nil {
		t.Errorf("errors of the read side: %v, %v", missing, noTable)
	}
}

// The compiler keeps the reading side out of every function that writes,
// and a transaction cannot be ended or begun by the function that gets it.
func TestReadSideIsNotAWriter(t *testing.T) {
	d := openHandle(t, openOptions{})
	side := reflect.TypeOf(d.Read())
	if !side.Implements(reflect.TypeFor[models.Reader]()) || side.Implements(reflect.TypeFor[models.Writer]()) {
		t.Fatalf("%v must be a models.Reader and no models.Writer", side)
	}
	if n := side.NumMethod(); n != 2 {
		t.Errorf("the reading side has %d methods, want Query and QueryRow only", n)
	}
	err := d.Write(func(tx models.Writer) error {
		typ := reflect.TypeOf(tx)
		for _, name := range []string{"Commit", "Rollback", "Begin", "BeginTx", "Prepare", "Stmt"} {
			if _, has := typ.MethodByName(name); has {
				t.Errorf("the transaction of Write has the method %s", name)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestWriteCommitsAndRollsBack(t *testing.T) {
	d := openHandle(t, openOptions{})
	// nil commits.
	if err := d.Write(func(tx models.Writer) error { return setV(tx, 1) }); err != nil {
		t.Fatal(err)
	}
	wantV(t, d, 1)
	// An error rolls back and comes back as it is.
	boom := errors.New("boom")
	err := d.Write(func(tx models.Writer) error {
		if err := setV(tx, 2); err != nil {
			return err
		}
		if v, err := rowV(tx, 1); err != nil || v != 2 {
			t.Errorf("the transaction does not read its own write: %d %v", v, err)
		}
		return boom
	})
	if err != boom {
		t.Fatalf("Write returned %v, want the error of the callback itself", err)
	}
	wantV(t, d, 1)
	// A panic rolls back, frees the connection and goes on.
	var tx models.Writer
	func() {
		defer func() {
			if p := recover(); p != "stop" {
				t.Errorf("recovered %v, want the panic of the callback", p)
			}
		}()
		_ = d.Write(func(w models.Writer) error {
			tx = w
			if err := setV(w, 3); err != nil {
				t.Error(err)
			}
			panic("stop")
		})
	}()
	wantV(t, d, 1)
	wantWriteConnFree(t, d)
	// The flow that panicked is no longer taken to be inside a Write.
	if err := d.Write(func(tx models.Writer) error { return setV(tx, 4) }); err != nil {
		t.Fatal(err)
	}
	wantV(t, d, 4)
	// A transaction that has ended takes no more statements.
	if err := setV(tx, 5); !errors.Is(err, sql.ErrTxDone) {
		t.Errorf("statement on an ended transaction: %v, want sql.ErrTxDone", err)
	}
	wantV(t, d, 4)
}

// A flow inside a Write that reaches for any other connection is stopped at
// once instead of waiting for ever.
func TestNestedUsePanics(t *testing.T) {
	a, b := openHandle(t, openOptions{}), openHandle(t, openOptions{})
	MigrationsFS = os.DirFS("../..")
	newPath := filepath.Join(t.TempDir(), "new.db")
	cases := []struct {
		name   string
		nested func()
		on     *DB
		what   string
	}{
		{"Write of the same handle", func() { _ = a.Write(func(models.Writer) error { return nil }) }, a, "write transaction asked for inside"},
		{"Write of another handle", func() { _ = b.Write(func(models.Writer) error { return nil }) }, b, "write transaction asked for inside"},
		{"QueryRow of the same read side", func() { _, _ = rowV(a.Read(), 1) }, a, "read through the read connections inside the write transaction of " + a.Path() + " (use the transaction)"},
		{"Query of another read side", func() { _, _ = b.Read().Query(`SELECT v FROM t`) }, b, "read through the read connections inside the write transaction of " + a.Path() + " (read it before the transaction)"},
		{"Close of the same handle", func() { _ = a.Close() }, a, "closed inside"},
		{"Close of another handle", func() { _ = b.Close() }, b, "closed inside"},
	}
	for _, tc := range cases {
		wantPanic(t, func() {
			_ = a.Write(func(tx models.Writer) error {
				if err := setV(tx, 77); err != nil {
					t.Error(err)
				}
				tc.nested()
				t.Errorf("%s inside a Write went on", tc.name)
				return nil
			})
		}, "database "+tc.on.Path()+": ", tc.what, "the write transaction of "+a.Path())
		// The transaction was rolled back and both connections are free.
		wantV(t, a, 0)
		wantWriteConnFree(t, a)
		wantWriteConnFree(t, b)
	}
	wantPanic(t, func() {
		_ = a.Write(func(models.Writer) error {
			_, _ = OpenSQLite(newPath, SchemaAgentLocal)
			t.Error("OpenSQLite inside a Write went on")
			return nil
		})
	}, "database "+newPath+": opened inside the write transaction of "+a.Path())
	if _, err := os.Stat(newPath); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("the refused open made the file: %v", err)
	}
	// Outside a Write all of it is allowed, also on the flow that panicked.
	if err := a.Write(func(tx models.Writer) error { return setV(tx, 1) }); err != nil {
		t.Fatal(err)
	}
	wantV(t, a, 1)
	wantV(t, b, 0)
	d, err := OpenSQLite(newPath, SchemaAgentLocal)
	if err != nil {
		t.Fatal(err)
	}
	if err := d.Close(); err != nil {
		t.Fatal(err)
	}
	// Another flow is not held up by a Write in progress: it reads at once.
	inside, release := make(chan struct{}), make(chan struct{})
	done := make(chan error, 1)
	go func() {
		done <- a.Write(func(models.Writer) error {
			close(inside)
			<-release
			return nil
		})
	}()
	<-inside
	wantV(t, a, 1)
	wantV(t, b, 0)
	close(release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

// Writes of one process wait for their turn at the write connection and do
// not meet at SQLite's lock: with a lock wait of 50 ms, a flow that waited
// on SQLite's lock behind a transaction of 2 s would fail.
func TestWritesWaitTheirTurn(t *testing.T) {
	d := openHandle(t, openOptions{lockWaitMS: 50})
	const waiters = 64
	var finished atomic.Int64
	holding := make(chan struct{})
	errs := make(chan error, waiters+1)
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		errs <- d.Write(func(tx models.Writer) error {
			close(holding)
			<-time.After(2 * time.Second)
			if n := finished.Load(); n != 0 {
				return fmt.Errorf("%d writes ended while another transaction was open", n)
			}
			_, err := tx.Exec(`UPDATE t SET v = v + 1 WHERE id = 1`)
			return err
		})
	}()
	<-holding
	for i := 0; i < waiters; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			errs <- d.Write(func(tx models.Writer) error {
				_, err := tx.Exec(`UPDATE t SET v = v + 1 WHERE id = 1`)
				return err
			})
			finished.Add(1)
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Error(err)
		}
	}
	wantV(t, d, waiters+1)
}

// Reads go on while a write transaction is open and see the state before
// it; the first read after the commit sees the change.
func TestReadsDuringWrite(t *testing.T) {
	d := openHandle(t, openOptions{})
	written, release := make(chan struct{}), make(chan struct{})
	done := make(chan error, 1)
	go func() {
		done <- d.Write(func(tx models.Writer) error {
			err := setV(tx, 5)
			close(written)
			<-release
			return err
		})
	}()
	<-written
	var wg sync.WaitGroup
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 200; j++ {
				if v, err := rowV(d.Read(), 1); err != nil || v != 0 {
					t.Errorf("read during the open transaction: %d %v, want 0", v, err)
					return
				}
			}
		}()
	}
	wg.Wait()
	close(release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	wantV(t, d, 5)
}

// What a Write committed is seen by the next read, which runs on another
// connection.
func TestReadAfterWriteSeesIt(t *testing.T) {
	d := openHandle(t, openOptions{})
	for i := 1; i <= 2000; i++ {
		if err := d.Write(func(tx models.Writer) error { return setV(tx, i) }); err != nil {
			t.Fatal(err)
		}
		if v, err := rowV(d.Read(), 1); err != nil || v != i {
			t.Fatalf("read after the write of %d: %d %v", i, v, err)
		}
	}
}

// Many flows that read and then write in one transaction lose no update
// and never fail with "database is locked", while other flows read: no
// read fails and no reader sees the value go back.
func TestManyFlows(t *testing.T) {
	d := openHandle(t, openOptions{})
	const writers, each, readers = 32, 50, 16
	stop := make(chan struct{})
	var reading, writing sync.WaitGroup
	for i := 0; i < readers; i++ {
		reading.Add(1)
		go func() {
			defer reading.Done()
			last := 0
			for {
				select {
				case <-stop:
					return
				default:
				}
				v, err := rowV(d.Read(), 1)
				if err != nil || v < last {
					t.Errorf("read %d (%v) after %d", v, err, last)
					return
				}
				last = v
			}
		}()
	}
	for i := 0; i < writers; i++ {
		writing.Add(1)
		go func() {
			defer writing.Done()
			for j := 0; j < each; j++ {
				stored := 0
				err := d.Write(func(tx models.Writer) error {
					v, err := rowV(tx, 1)
					if err != nil {
						return err
					}
					stored = v + 1
					return setV(tx, stored)
				})
				if err != nil {
					t.Error(err)
					return
				}
				if v, err := rowV(d.Read(), 1); err != nil || v < stored {
					t.Errorf("read %d (%v) after the write of %d", v, err, stored)
					return
				}
			}
		}()
	}
	writing.Wait()
	close(stop)
	reading.Wait()
	wantV(t, d, writers*each)
}

// A read that finds every read connection in use waits for one; it does
// not fail, and no further connection is opened.
func TestReadersWaitForAConnection(t *testing.T) {
	d := openHandle(t, openOptions{readConns: 2})
	// Two unfinished statements hold both connections.
	var held []*sql.Rows
	for i := 0; i < 2; i++ {
		rows, err := d.Read().Query(`SELECT v FROM t`)
		if err != nil {
			t.Fatal(err)
		}
		held = append(held, rows)
	}
	third := make(chan error, 1)
	go func() {
		_, err := rowV(d.Read(), 1)
		third <- err
	}()
	select {
	case err := <-third:
		t.Fatalf("a third read ended while both read connections were in use: %v", err)
	case <-time.After(300 * time.Millisecond):
	}
	for _, rows := range held {
		rows.Close()
	}
	if err := <-third; err != nil {
		t.Fatalf("the read that waited: %v", err)
	}
	var wg sync.WaitGroup
	for i := 0; i < 200; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 20; j++ {
				if _, err := rowV(d.Read(), 1); err != nil {
					t.Error(err)
					return
				}
				if n := d.r.Stats().OpenConnections; n > 2 {
					t.Errorf("%d read connections open, want at most 2", n)
					return
				}
			}
		}()
	}
	wg.Wait()
}

// Another process that holds SQLite's write lock (here a second connection
// outside the handle, which SQLite treats the same way) does not hold up
// reads; a Write waits for it and fails only when the lock wait has passed.
func TestOtherProcessWrite(t *testing.T) {
	d := openHandle(t, openOptions{lockWaitMS: 50})
	slow := openHandle(t, openOptions{})
	for _, h := range []*DB{d, slow} {
		other, err := sql.Open("sqlite", writeDSN(h.Path(), DBLockWaitMS))
		if err != nil {
			t.Fatal(err)
		}
		defer other.Close()
		tx, err := other.Begin()
		if err != nil {
			t.Fatal(err)
		}
		defer tx.Rollback()
		if _, err := tx.Exec(`INSERT INTO t (id, v) VALUES (2, 7)`); err != nil {
			t.Fatal(err)
		}
		// Reads go on and do not see what is not committed.
		wantV(t, h, 0)
		if _, err := rowV(h.Read(), 2); !errors.Is(err, sql.ErrNoRows) {
			t.Fatalf("read of the uncommitted row: %v, want no row", err)
		}
		write := func() error {
			return h.Write(func(tx models.Writer) error {
				_, err := tx.Exec(`UPDATE t SET v = v + 1 WHERE id = 2`)
				return err
			})
		}
		if h == d {
			// With a lock wait of 50 ms the Write gives up, and the handle
			// goes on working.
			start := time.Now()
			err := write()
			if !isBusy(err) {
				t.Fatalf("Write during the transaction of another process: %v, want database is locked", err)
			}
			if waited := time.Since(start); waited < 40*time.Millisecond {
				t.Errorf("Write gave up after %v, want the lock wait of 50 ms", waited)
			}
			wantV(t, h, 0)
			if err := tx.Commit(); err != nil {
				t.Fatal(err)
			}
			if err := write(); err != nil {
				t.Fatalf("Write after the other process committed: %v", err)
			}
		} else {
			// With the lock wait of production the Write waits until the
			// other transaction has ended.
			done := make(chan error, 1)
			go func() { done <- write() }()
			select {
			case err := <-done:
				t.Fatalf("Write did not wait for the transaction of another process: %v", err)
			case <-time.After(300 * time.Millisecond):
			}
			if err := tx.Commit(); err != nil {
				t.Fatal(err)
			}
			if err := <-done; err != nil {
				t.Fatalf("Write that waited for another process: %v", err)
			}
		}
		if v, err := rowV(h.Read(), 2); err != nil || v != 8 {
			t.Fatalf("row of the other process after both commits: %d %v, want 8", v, err)
		}
	}
}

// The words between TestOtherProcessReal and the child process it starts.
const (
	otherProcessChild   = "other-process-child"
	otherProcessHolding = "holding the write transaction"
)

// A real second process (this test binary, run again as
// TestOtherProcessRealChild) holds a write transaction through its own
// handle: reads here go on without seeing its row, a Write here waits until
// it has committed and then sees the row.
func TestOtherProcessReal(t *testing.T) {
	d := openHandle(t, openOptions{})
	child := exec.Command(os.Args[0], "-test.run=^TestOtherProcessRealChild$", "--", otherProcessChild, d.Path())
	child.Stderr = os.Stderr
	stdin, err := child.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	stdout, err := child.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := child.Start(); err != nil {
		t.Fatal(err)
	}
	// The child commits when its standard input is closed.
	released := false
	release := func() {
		if !released {
			released = true
			_ = stdin.Close()
		}
	}
	defer func() {
		release()
		_ = child.Wait()
	}()
	holding := false
	for lines := bufio.NewScanner(stdout); !holding && lines.Scan(); {
		holding = lines.Text() == otherProcessHolding
	}
	if !holding {
		t.Fatal("the child process ended without holding a write transaction")
	}
	wantV(t, d, 0)
	if _, err := rowV(d.Read(), 2); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("read of the row the other process has not committed: %v, want no row", err)
	}
	done := make(chan error, 1)
	go func() {
		done <- d.Write(func(tx models.Writer) error {
			_, err := tx.Exec(`UPDATE t SET v = v + 1 WHERE id = 2`)
			return err
		})
	}()
	select {
	case err := <-done:
		t.Fatalf("Write did not wait for the transaction of the other process: %v", err)
	case <-time.After(time.Second):
	}
	release()
	if err := <-done; err != nil {
		t.Fatalf("Write that waited for the other process: %v", err)
	}
	if v, err := rowV(d.Read(), 2); err != nil || v != 8 {
		t.Fatalf("row of the other process after both commits: %d %v, want 8", v, err)
	}
	if _, err := io.Copy(io.Discard, stdout); err != nil {
		t.Fatal(err)
	}
	if err := child.Wait(); err != nil {
		t.Fatalf("the child process: %v", err)
	}
}

// TestOtherProcessRealChild is the other process of TestOtherProcessReal.
// It does nothing when the test binary was not started by that test.
func TestOtherProcessRealChild(t *testing.T) {
	if flag.NArg() != 2 || flag.Arg(0) != otherProcessChild {
		t.Skip("runs only as the child process of TestOtherProcessReal")
	}
	d, err := openSQLite(flag.Arg(1), handleMigrations)
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	err = d.Write(func(tx models.Writer) error {
		if _, err := tx.Exec(`INSERT INTO t (id, v) VALUES (2, 7)`); err != nil {
			return err
		}
		// Holding the transaction open is what this process is for: it
		// waits here until the parent closes the standard input.
		fmt.Println(otherProcessHolding)
		_, err := io.Copy(io.Discard, os.Stdin)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
}

// A file has one handle per process: OpenSQLite refuses a path whose
// handle is open, however the path is written.
func TestOneHandlePerFile(t *testing.T) {
	MigrationsFS = os.DirFS("../..")
	dir := t.TempDir()
	path := filepath.Join(dir, "one.db")
	first, err := OpenSQLite(path, SchemaAgentLocal)
	if err != nil {
		t.Fatal(err)
	}
	defer first.Close()
	if first.Path() != path {
		t.Errorf("Path() = %q, want %q", first.Path(), path)
	}
	for _, again := range []string{path, filepath.Join(dir, "sub", "..", "one.db"), dir + string(filepath.Separator) + string(filepath.Separator) + "one.db"} {
		second, err := OpenSQLite(again, SchemaAgentLocal)
		if err == nil {
			second.Close()
			t.Fatalf("a second handle of %s was opened as %s", path, again)
		}
		if !strings.Contains(err.Error(), "already open") {
			t.Errorf("second open as %s: %v, want the refusal of a second handle", again, err)
		}
	}
	// The refusals closed nothing: the first handle works.
	if err := first.Write(func(tx models.Writer) error { return models.SetLocalState(tx, "k", "v") }); err != nil {
		t.Fatal(err)
	}
	if v := models.GetLocalState(first.Read(), "k"); v != "v" {
		t.Fatalf("state %q through the first handle", v)
	}
	// Another file is another handle.
	other, err := OpenSQLite(filepath.Join(dir, "other.db"), SchemaAgentLocal)
	if err != nil {
		t.Fatal(err)
	}
	defer other.Close()
	// After Close the path can be opened again.
	if err := first.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := rowV(first.Read(), 1); err == nil {
		t.Error("a read through a closed handle succeeded")
	}
	if err := first.Write(func(models.Writer) error { return nil }); err == nil {
		t.Error("a Write through a closed handle succeeded")
	}
	reopened, err := OpenSQLite(path, SchemaAgentLocal)
	if err != nil {
		t.Fatalf("open after Close: %v", err)
	}
	defer reopened.Close()
	if v := models.GetLocalState(reopened.Read(), "k"); v != "v" {
		t.Fatalf("state %q after reopening", v)
	}
	// Closing the old handle once more does not free the path of the new one.
	if err := first.Close(); err != nil {
		t.Fatal(err)
	}
	if second, err := OpenSQLite(path, SchemaAgentLocal); err == nil {
		second.Close()
		t.Fatal("a second handle was opened after the first one was closed twice")
	}
}

// An open that fails leaves nothing behind: no connection, and the path is
// free for the next open. A database of another build is refused unchanged.
func TestOpenSQLiteFailureFreesThePath(t *testing.T) {
	MigrationsFS = os.DirFS("../..")
	path := filepath.Join(t.TempDir(), "x.db")
	d, err := OpenSQLite(path, SchemaAgentDB)
	if err != nil {
		t.Fatal(err)
	}
	if err := d.Close(); err != nil {
		t.Fatal(err)
	}
	before := dbState(t, path)
	for i := 0; i < 2; i++ {
		_, err := OpenSQLite(path, SchemaAgentLocal)
		wantOtherBuild(t, err, path)
	}
	if after := dbState(t, path); after != before {
		t.Fatalf("the refused opens changed the database:\n%s\n%s", before, after)
	}
	if d, err = OpenSQLite(path, SchemaAgentDB); err != nil {
		t.Fatalf("open with its own schema after the refusals: %v", err)
	}
	if err := d.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := OpenSQLite(path, "no-such-schema"); err == nil {
		t.Fatal("a schema without migrations was opened")
	}
}

// The three schemas of the program open through the handle, also at a path
// with the characters that have a meaning in an SQLite URI, and both sides
// of the handle work on the same file.
func TestOpenSQLiteHandleSchemas(t *testing.T) {
	MigrationsFS = os.DirFS("../..")
	for _, schema := range []string{SchemaCentral, SchemaAgentDB, SchemaAgentLocal} {
		path := filepath.Join(t.TempDir(), "a#b?c%41 d", schema+".db")
		for i := 0; i < 2; i++ {
			d, err := OpenSQLite(path, schema)
			if err != nil {
				t.Fatalf("%s, open %d: %v", schema, i+1, err)
			}
			if _, err := os.Stat(path); err != nil {
				t.Fatalf("%s: the database is not at its path: %v", schema, err)
			}
			var foreignKeys int
			err = d.Write(func(tx models.Writer) error {
				if _, err := tx.Exec(fmt.Sprintf(`CREATE TABLE open%d (id INTEGER PRIMARY KEY)`, i)); err != nil {
					return err
				}
				return tx.QueryRow(`PRAGMA foreign_keys`).Scan(&foreignKeys)
			})
			if err != nil || foreignKeys != 1 {
				t.Errorf("%s: foreign_keys = %d on the write connection (%v)", schema, foreignKeys, err)
			}
			// The read side sees what the write connection made: both
			// are connections to the one file.
			var made int
			if err := d.Read().QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE name LIKE 'open%'`).Scan(&made); err != nil || made != i+1 {
				t.Errorf("%s: the read side sees %d of the %d tables made through the write connection (%v)", schema, made, i+1, err)
			}
			if err := d.Close(); err != nil {
				t.Fatal(err)
			}
		}
	}
}

// Close waits for the write transaction in progress, which is committed;
// a flow that comes afterwards gets the error of a closed database.
func TestCloseWaitsForWrite(t *testing.T) {
	d := openHandle(t, openOptions{})
	inside, release := make(chan struct{}), make(chan struct{})
	written, closed := make(chan error, 1), make(chan error, 1)
	go func() {
		written <- d.Write(func(tx models.Writer) error {
			close(inside)
			<-release
			return setV(tx, 6)
		})
	}()
	<-inside
	go func() { closed <- d.Close() }()
	select {
	case err := <-closed:
		t.Fatalf("Close did not wait for the write transaction: %v", err)
	case <-time.After(300 * time.Millisecond):
	}
	close(release)
	if err := <-written; err != nil {
		t.Fatalf("the Write that was in progress at Close: %v", err)
	}
	if err := <-closed; err != nil {
		t.Fatal(err)
	}
	if err := d.Write(func(tx models.Writer) error { return setV(tx, 7) }); err == nil {
		t.Error("a Write after Close succeeded")
	}
	again, err := openSQLite(d.Path(), handleMigrations)
	if err != nil {
		t.Fatal(err)
	}
	defer again.Close()
	wantV(t, again, 6)
}

// A clean Close leaves only the database file.
func TestCloseRemovesWalFiles(t *testing.T) {
	d := openHandle(t, openOptions{})
	if err := d.Write(func(tx models.Writer) error { return setV(tx, 1) }); err != nil {
		t.Fatal(err)
	}
	wantV(t, d, 1)
	dir := filepath.Dir(d.Path())
	names := func() string {
		entries, err := os.ReadDir(dir)
		if err != nil {
			t.Fatal(err)
		}
		var list []string
		for _, e := range entries {
			list = append(list, e.Name())
		}
		return strings.Join(list, " ")
	}
	if got := names(); got != "x.db x.db-shm x.db-wal" {
		t.Fatalf("files of the open database: %s", got)
	}
	if err := d.Close(); err != nil {
		t.Fatal(err)
	}
	if got := names(); got != "x.db" {
		t.Fatalf("files after Close: %s, want x.db only", got)
	}
}

// With KeepReadConns(1) the connections a burst of readers opened are
// closed when they come back; one stays.
func TestKeepReadConns(t *testing.T) {
	d := openHandle(t, openOptions{readConns: 8})
	if n := d.r.Stats().OpenConnections; n != 1 {
		t.Fatalf("%d read connections after the open, want the first one", n)
	}
	d.KeepReadConns(1)
	var held []*sql.Rows
	for i := 0; i < 8; i++ {
		rows, err := d.Read().Query(`SELECT v FROM t`)
		if err != nil {
			t.Fatal(err)
		}
		held = append(held, rows)
	}
	if n := d.r.Stats().OpenConnections; n != 8 {
		t.Fatalf("%d read connections during the burst, want 8", n)
	}
	for _, rows := range held {
		rows.Close()
	}
	if n := d.r.Stats().OpenConnections; n != 1 {
		t.Fatalf("%d read connections after the burst, want 1", n)
	}
	wantV(t, d, 0)
	if n := d.w.Stats().OpenConnections; n != 1 {
		t.Fatalf("%d write connections, want 1", n)
	}
}
