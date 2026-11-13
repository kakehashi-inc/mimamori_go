package workers

import (
	"os"
	"path/filepath"
	"sync"
	"syscall"
	"testing"
	"time"

	"mimamori/app/models"
	"mimamori/app/modules"
)

// While the open of one agent's database is held up, the central goes on
// with the other agents: a report for another agent is stored and a third
// agent is read. Every flow that asked for the held agent gets the one
// handle once the open ends, and a failed open is tried again by the next
// call.
func TestTargetOpenDoesNotBlockOthers(t *testing.T) {
	e := newTestEnv(t)
	_, reported, _ := commandAgent(t, e, "host-report")
	_, read, readDB := commandAgent(t, e, "host-read")

	// An agent whose database is not open yet (as after a restart before
	// openAllTargets reached it).
	held := &models.Agent{Kind: models.AgentToken}
	if err := models.InsertAgent(e.c.db, held); err != nil {
		t.Fatal(err)
	}

	// The open of a database takes the lock of its directory; holding it
	// from a second descriptor holds every open in targets/ up, the way
	// another process creating a database there would.
	dir, err := os.Open(filepath.Join(e.c.dataDir, modules.TargetDBDirName))
	if err != nil {
		t.Fatal(err)
	}
	defer dir.Close()
	if err := syscall.Flock(int(dir.Fd()), syscall.LOCK_EX); err != nil {
		t.Fatal(err)
	}
	locked := true
	unlock := func() {
		if locked {
			locked = false
			_ = syscall.Flock(int(dir.Fd()), syscall.LOCK_UN)
		}
	}
	defer unlock()

	const waiters = 8
	type got struct {
		db  *modules.DB
		err error
	}
	results := make(chan got, waiters)
	for range waiters {
		go func() {
			// Each flow has its own copy of the row, as each request reads
			// its own.
			a := *held
			db, err := e.c.targetDB(&a)
			results <- got{db, err}
		}()
	}
	// Wait until the open has begun (its entry is in the set).
	deadline := time.Now().Add(10 * time.Second)
	for {
		e.c.targetsMu.Lock()
		_, begun := e.c.targets[held.ID]
		e.c.targetsMu.Unlock()
		if begun {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the open of the held agent did not begin")
		}
		time.Sleep(time.Millisecond)
	}

	others := make(chan error, 2)
	go func() {
		others <- e.c.centralReport(reported, &modules.ReportRequest{Detections: []modules.DetectionReport{
			{Type: "exec", Category: "alert", OSUser: "nginx", Exe: "/usr/bin/curl", Target: "while held"},
		}})
	}()
	go func() {
		db, err := e.c.targetDB(read)
		if err == nil && db != readDB {
			t.Error("another handle for an open agent database")
		}
		if err == nil {
			_, _, err = models.ListDetections(db.Read(), models.DetectionFilter{}, 10)
		}
		others <- err
	}()
	for range 2 {
		select {
		case err := <-others:
			if err != nil {
				t.Fatal(err)
			}
		case <-time.After(10 * time.Second):
			t.Fatal("a flow for another agent waited for the open of the held one")
		}
	}
	select {
	case r := <-results:
		t.Fatalf("the open of the held agent ended while its directory was locked: %v", r.err)
	default:
	}
	rdb, err := e.c.targetDB(reported)
	if err != nil {
		t.Fatal(err)
	}
	list, _, err := models.ListDetections(rdb.Read(), models.DetectionFilter{}, 10)
	if err != nil || len(list) != 1 || list[0].Target != "while held" {
		t.Fatalf("report stored while another open was held: %v %+v", err, list)
	}

	unlock()
	var first *modules.DB
	for range waiters {
		select {
		case r := <-results:
			if r.err != nil {
				t.Fatal(r.err)
			}
			if first == nil {
				first = r.db
			} else if r.db != first {
				t.Fatal("flows that asked for one agent got different handles")
			}
		case <-time.After(10 * time.Second):
			t.Fatal("a flow for the held agent did not get its handle")
		}
	}
	if _, _, err := models.ListDetections(first.Read(), models.DetectionFilter{}, 10); err != nil {
		t.Fatal(err)
	}

	// A failed open is not remembered: a directory where the database
	// file belongs makes it fail, and once it is gone the next call opens
	// the file.
	failing := &models.Agent{Kind: models.AgentToken}
	if err := models.InsertAgent(e.c.db, failing); err != nil {
		t.Fatal(err)
	}
	failing.DBFile = "agent-failing.db"
	if err := models.SetAgentDBFile(e.c.db, failing.ID, failing.DBFile); err != nil {
		t.Fatal(err)
	}
	path := modules.TargetDBPath(e.c.dataDir, failing.DBFile)
	if err := os.Mkdir(path, 0o700); err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	for range 2 {
		wg.Go(func() {
			if _, err := e.c.targetDB(failing); err == nil {
				t.Error("open of a directory succeeded")
			}
		})
	}
	wg.Wait()
	if _, err := e.c.targetDB(failing); err == nil {
		t.Fatal("open of a directory succeeded")
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	db, err := e.c.targetDB(failing)
	if err != nil {
		t.Fatalf("open after the cause was removed: %v", err)
	}
	if again, _ := e.c.targetDB(failing); again != db {
		t.Fatal("a second handle for the same agent")
	}
}
