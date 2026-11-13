package modules

import (
	"bytes"
	"database/sql"
	"errors"
	"fmt"
	"path/filepath"
	"runtime"
	"strconv"
	"sync"
	"sync/atomic"

	"modernc.org/sqlite"
	sqlite3 "modernc.org/sqlite/lib"

	"mimamori/app/models"
)

// DB is the handle of one SQLite database file: everything this process
// does with the file goes through it (OpenSQLite opens it). The handle owns
// every connection the process has to the file:
//
//   - one write connection, opened read-write with the handle and kept
//     until the handle is closed. One flow at a time uses it. A flow that
//     finds it in use waits for its turn, without a time limit, and never
//     fails because of waiting;
//   - read connections, opened read-only and kept open, used by many flows
//     at the same time, also while a write transaction is open (WAL). Each
//     statement sees everything committed before it began. A writing
//     statement sent through one of them fails every time
//     (IsWriteThroughRead).
//
// The rules for the code that uses a handle:
//
//   - every access says what it is: Read for reading, Write for writing.
//     Every write is a transaction; a single statement is a transaction of
//     one statement;
//   - what decides a write is read inside the same Write, through its
//     transaction. Only when something slow lies between the read and the
//     write (password hashing, a mail, a request, an external program, a
//     file walk) the read stays on the read side, and the writing statement
//     carries its own condition or the row is read again inside the Write;
//   - inside a Write callback the flow uses only the transaction it was
//     given: no Read, no Write, no OpenSQLite and no Close, of this or of
//     any other file. What it needs from another file it reads before, what
//     it writes to another file it writes after;
//   - inside a Write callback nothing waits on the outside: no response is
//     written, no mail or request sent, no external program run, no
//     password hashed, no sleep. Every other write to the file waits until
//     the callback has ended;
//   - a statement is finished where it is sent: the rows of a query are
//     read and closed before the flow does anything else (the functions of
//     app/models return values, never *sql.Rows). A flow so holds at most
//     one read connection, and none while it waits for something.
//
// Under these rules no flow can wait for ever: a flow waits for the write
// connection of a file while it holds nothing, a flow inside a transaction
// waits for nothing but its own statements, and a flow that holds a read
// connection waits for nothing. A flow that breaks the third rule would
// wait for a connection it holds itself, or two flows could wait for each
// other's files, so it is stopped with a panic at once (see writeFlows).
//
// Between processes SQLite's own lock decides: a command that writes next
// to the running central or agent meets it when a transaction begins,
// waits for it for DBLockWaitMS and then fails with SQLite's "database is
// locked". The read connections are not held up by a writer of another
// process.
type DB struct {
	path string  // as given to the opener
	key  string  // the absolute path while the handle is in openHandles
	w    *sql.DB // the write connection: exactly one, read-write
	r    *sql.DB // the read connections: read-only
	// turn is held by the flow that uses the write connection. It is the
	// handle's own lock and not the wait for a connection of database/sql:
	// once a flow has waited for a sync.Mutex for more than a millisecond,
	// the mutex goes to the flow that has waited longest, so every waiter
	// gets its turn, while database/sql gives a freed connection to any of
	// the waiters.
	turn sync.Mutex
}

// openHandles holds the handles opened with OpenSQLite that are not closed
// yet, by the absolute path of their file.
var openHandles = struct {
	sync.Mutex
	byPath map[string]*DB
}{byPath: map[string]*DB{}}

// register enters the handle in openHandles; it fails when the file already
// has a handle there.
func (d *DB) register() error {
	key, err := filepath.Abs(d.path)
	if err != nil {
		return err
	}
	openHandles.Lock()
	defer openHandles.Unlock()
	if openHandles.byPath[key] != nil {
		return fmt.Errorf("database %s is already open in this process", d.path)
	}
	d.key = key
	openHandles.byPath[key] = d
	return nil
}

// unregister removes the handle from openHandles (when it is there).
func (d *DB) unregister() {
	openHandles.Lock()
	defer openHandles.Unlock()
	if d.key != "" && openHandles.byPath[d.key] == d {
		delete(openHandles.byPath, d.key)
	}
}

// Path returns the path of the database file as it was given to the opener.
func (d *DB) Path() string { return d.path }

// KeepReadConns sets how many read connections stay open while no flow
// uses them (all of them when it is not called). Further connections are
// still opened when more flows read at the same moment, and closed when
// they come back while n are idle. The central calls it for the per-agent
// databases, of which it holds hundreds.
func (d *DB) KeepReadConns(n int) { d.r.SetMaxIdleConns(n) }

// Close closes every connection of the handle after the write transaction
// in progress, if there is one, has ended. The read side is closed first:
// the last connection to close is then the write connection, which can
// bring the write-ahead log into the database file and remove the -wal and
// -shm files (a read-only connection cannot). A flow that uses the handle
// afterwards gets database/sql's error of a closed database.
func (d *DB) Close() error {
	d.refuseInsideWrite(nestedClose)
	d.turn.Lock()
	defer d.turn.Unlock()
	err := errors.Join(d.r.Close(), d.w.Close())
	d.unregister()
	return err
}

// Read returns the reading side of the database: each statement sent
// through it runs on a free read connection, at the same time as the
// statements of other flows and as a write transaction. When every read
// connection is in use the statement waits for one; it does not fail
// because of waiting. Inside a Write callback the transaction is used
// instead; a statement through Read there panics.
func (d *DB) Read() models.Reader { return readSide{d} }

// readSide is what Read returns. It has no method that writes, so it cannot
// be given to a function that takes a models.Writer.
type readSide struct{ d *DB }

func (r readSide) Query(query string, args ...any) (*sql.Rows, error) {
	r.d.refuseInsideWrite(nestedRead)
	return r.d.r.Query(query, args...)
}

func (r readSide) QueryRow(query string, args ...any) *sql.Row {
	r.d.refuseInsideWrite(nestedRead)
	return r.d.r.QueryRow(query, args...)
}

// writeTx is the transaction a Write callback gets: the statements of a
// transaction without the means to end it or to begin another one.
type writeTx struct{ tx *sql.Tx }

func (t writeTx) Exec(query string, args ...any) (sql.Result, error) {
	return t.tx.Exec(query, args...)
}

func (t writeTx) Query(query string, args ...any) (*sql.Rows, error) {
	return t.tx.Query(query, args...)
}

func (t writeTx) QueryRow(query string, args ...any) *sql.Row {
	return t.tx.QueryRow(query, args...)
}

// Write runs fn in one write transaction on the write connection. It waits
// until the connection is free; the transaction takes SQLite's write lock
// when it begins, so what fn reads through tx cannot change before fn
// writes. When fn returns nil the transaction is committed (the error of
// the commit is returned); when fn returns an error the transaction is
// rolled back and that error is returned unchanged; when fn panics the
// transaction is rolled back, the connection is released and the panic goes
// on. tx must not be used after fn has returned.
//
// Errors that do not come from fn are those of the begin and of the commit:
// another process held SQLite's write lock for longer than DBLockWaitMS, the
// handle is closed, or the file could not be written.
func (d *DB) Write(fn func(tx models.Writer) error) error {
	leave := d.enterWrite()
	defer leave()
	d.turn.Lock()
	defer d.turn.Unlock()
	tx, err := d.w.Begin()
	if err != nil {
		return err
	}
	committed := false
	defer func() {
		// Also reached when fn panicked.
		if !committed {
			_ = tx.Rollback()
		}
	}()
	if err := fn(writeTx{tx}); err != nil {
		return err
	}
	// After a failed commit the driver has rolled the transaction back, so
	// nothing is left to undo here.
	committed = true
	return tx.Commit()
}

// writeFlows records the flows (goroutines) that are inside a Write of any
// handle of the process, from before they wait for their turn until the
// Write has ended: goroutine number -> *DB. writeFlowCount is the number of
// entries.
//
// A flow in this set that asks for the write connection again would wait
// for itself for ever, and one that asks for any other connection breaks
// the rule that makes waiting for each other impossible (see DB). Write,
// a statement through Read, OpenSQLite and Close therefore panic when the
// calling flow is in the set. The check is always made, not only in tests:
// a nested use on a path no test runs ends that one request with a logged
// panic and a rolled back transaction, instead of stopping every write to
// the file for ever.
var (
	writeFlows     sync.Map
	writeFlowCount atomic.Int64
)

// What a flow inside a Write must not do, as the panic names it.
const (
	nestedWrite = "write transaction asked for"
	nestedRead  = "read through the read connections"
	nestedOpen  = "opened"
	nestedClose = "closed"
)

// goroutineNumber returns the number of the calling goroutine as the first
// line of its stack trace gives it ("goroutine 12 [running]:"), the way the
// standard library reads it for its own lock checks. It returns 0 when the
// line has another form; the guard against nested use then does nothing
// (TestGoroutineNumber fails in such a toolchain).
func goroutineNumber() uint64 {
	var buf [64]byte
	line := buf[:runtime.Stack(buf[:], false)]
	line, ok := bytes.CutPrefix(line, []byte("goroutine "))
	if !ok {
		return 0
	}
	end := bytes.IndexByte(line, ' ')
	if end < 0 {
		return 0
	}
	n, err := strconv.ParseUint(string(line[:end]), 10, 64)
	if err != nil {
		return 0
	}
	return n
}

// enterWrite records the calling flow as being inside a Write of d and
// returns the function that removes it again. It panics when the flow is
// inside a Write already.
func (d *DB) enterWrite() (leave func()) {
	g := goroutineNumber()
	if g == 0 {
		return func() {}
	}
	if held, inside := writeFlows.LoadOrStore(g, d); inside {
		panic(nestedUse(d, nestedWrite, held.(*DB)))
	}
	writeFlowCount.Add(1)
	return func() {
		writeFlowCount.Add(-1)
		writeFlows.Delete(g)
	}
}

// refuseInsideWrite panics when the calling flow is inside a Write of any
// handle; what is the thing it is doing to d (nestedRead, nestedOpen or
// nestedClose). While no write transaction is open or waited for anywhere
// in the process this costs one atomic load; only otherwise the goroutine
// number is read.
func (d *DB) refuseInsideWrite(what string) {
	if writeFlowCount.Load() == 0 {
		return
	}
	g := goroutineNumber()
	if g == 0 {
		return
	}
	if held, inside := writeFlows.Load(g); inside {
		panic(nestedUse(d, what, held.(*DB)))
	}
}

// nestedUse is the text of the panic for a flow that does what to d while
// it is inside a Write of held.
func nestedUse(d *DB, what string, held *DB) string {
	text := fmt.Sprintf("database %s: %s inside the write transaction of %s", d.path, what, held.path)
	if what == nestedRead {
		if d == held {
			text += " (use the transaction)"
		} else {
			text += " (read it before the transaction)"
		}
	}
	return text
}

// IsWriteThroughRead reports whether err is SQLite's refusal of a writing
// statement that was sent through a read connection (Read): a programming
// error, never a state of the data.
func IsWriteThroughRead(err error) bool {
	var se *sqlite.Error
	// The low byte is the primary result code; the rest tells which kind of
	// read-only refusal it is.
	return errors.As(err, &se) && se.Code()&0xff == sqlite3.SQLITE_READONLY
}
