package workers

import (
	"bytes"
	"database/sql"
	"log"
	"net/http"
	"testing"

	"mimamori/app/models"
	"mimamori/app/modules"
)

// A target database of another build does not stop the central: the start
// skips it, a request about that target answers 500 and the lists of every
// target show the others.
func TestTargetDBOfOtherBuild(t *testing.T) {
	e := newTestEnv(t)
	var logged bytes.Buffer
	e.c.logger = log.New(&logged, "", 0)
	agents, err := models.ListAgents(e.c.db.Read())
	if err != nil || len(agents) == 0 {
		t.Fatalf("agents: %v %d", err, len(agents))
	}
	a := agents[0]
	if _, err := e.c.targetDB(a); err != nil {
		t.Fatal(err)
	}
	e.c.closeTargets()
	path := modules.TargetDBPath(e.c.dataDir, a.DBFile)
	raw, err := sql.Open("sqlite", "file:"+path)
	if err != nil {
		t.Fatal(err)
	}
	// A database without the fingerprint, as a build before it made them.
	if _, err := raw.Exec(`PRAGMA user_version = 0`); err != nil {
		t.Fatal(err)
	}
	raw.Close()

	if err := e.c.openAllTargets(); err != nil {
		t.Fatalf("start with a target database of another build: %v", err)
	}
	if !bytes.Contains(logged.Bytes(), []byte(path+" was created by another build")) {
		t.Fatalf("log: %q", logged.String())
	}
	if code := e.adminStatus("GET", "/detections?agent_id="+itoa64(a.ID), nil); code != http.StatusInternalServerError {
		t.Fatalf("detections of the refused target: %d, want 500", code)
	}
	for _, p := range []string{"/detections", "/overview", "/agents"} {
		if code := e.adminStatus("GET", p, nil); code != http.StatusOK {
			t.Fatalf("GET %s: %d, want 200", p, code)
		}
	}
}
