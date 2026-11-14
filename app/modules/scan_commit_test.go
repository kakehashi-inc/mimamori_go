package modules

import (
	"bytes"
	"log"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"mimamori/app/models"
)

// A writable scan whose transaction is rolled back (here: storing the roots
// fails after the findings were recorded) reports nothing and saves nothing
// of the walk, and the failure is logged; once storing works, the next
// check reports every file once.
func TestWritableScanRolledBackReportsNothing(t *testing.T) {
	rt, done := openAgentDB(t)
	defer done()
	var logged bytes.Buffer
	rt.Logger = log.New(&logged, "", 0)
	base := t.TempDir()
	var files []string
	for _, p := range []string{"allowed/f", "d1/f", "d2/f"} {
		full := filepath.Join(base, p)
		mkFile(t, full, 0o666)
		if !strings.HasPrefix(p, "allowed/") {
			files = append(files, full)
		}
	}
	c := &RuleContent{Writable: WritableSpec{Base: base, AllowedPaths: []string{"allowed"}, Category: CategoryAlert}, ScanIntervalSec: 3600}
	res := &ResolvedRule{Content: c}
	if err := execWrite(rt.DB, `CREATE TRIGGER fail_roots BEFORE INSERT ON scan_roots BEGIN SELECT RAISE(ABORT, 'test: roots not stored'); END`); err != nil {
		t.Fatal(err)
	}
	now := time.Now().Unix()
	if out := rt.scanFiles(c, res, time.Unix(now, 0)); len(out) != 0 {
		t.Fatalf("a rolled back scan reported %+v", out)
	}
	if !strings.Contains(logged.String(), "writable scan of "+base+": storing the result failed: ") {
		t.Fatalf("log: %q", logged.String())
	}
	if v := models.GetLocalState(rt.DB.Read(), stateLastFullScan); v != "" {
		t.Fatalf("last full scan saved by a rolled back scan: %q", v)
	}
	if err := execWrite(rt.DB, `DROP TRIGGER fail_roots`); err != nil {
		t.Fatal(err)
	}
	seen := map[string]int{}
	for i := range 2 {
		for _, d := range rt.scanFiles(c, res, time.Unix(now+int64(i+1)*3600, 0)) {
			seen[d.Target]++
		}
	}
	for _, f := range files {
		if seen[f] != 1 {
			t.Fatalf("%s reported %d times (all: %v)", f, seen[f], seen)
		}
	}
}

// An executed-file scan whose transaction is rolled back reports nothing,
// and the since time of a new root, which that walk would have removed,
// stays saved: the next check finds the file in the new root.
func TestExecScanRolledBackKeepsRootSince(t *testing.T) {
	rt, done := openAgentDB(t)
	defer done()
	var logged bytes.Buffer
	rt.Logger = log.New(&logged, "", 0)
	base := t.TempDir()
	c := &RuleContent{
		Writable:  WritableSpec{Base: base, AllowedPaths: []string{"storage"}},
		ExecFiles: ExecFilesSpec{Extensions: []string{".php"}, OtherCategory: CategoryAlert},
	}
	res := &ResolvedRule{Content: c}
	start := time.Now().Unix() - 100
	if out := rt.scanFiles(c, res, time.Unix(start, 0)); len(out) != 0 {
		t.Fatalf("first scan: %+v", out)
	}
	evil := filepath.Join(base, "b/storage/evil.php")
	mkFile(t, evil, 0o644)
	ctime := ctimeOf(t, evil)
	if out := rt.scanFiles(c, res, time.Unix(ctime+20, 0)); len(out) != 0 {
		t.Fatalf("the directory is not an allowed root yet: %+v", out)
	}
	// The next full scan finds the new root; storing the executed-file
	// walk fails.
	if err := execWrite(rt.DB, `CREATE TRIGGER fail_exec BEFORE INSERT ON findings WHEN NEW.fingerprint LIKE 'exec_file|%' BEGIN SELECT RAISE(ABORT, 'test: findings not stored'); END`); err != nil {
		t.Fatal(err)
	}
	full := start + int64(c.ScanInterval())
	if out := rt.scanFiles(c, res, time.Unix(full, 0)); len(out) != 0 {
		t.Fatalf("a rolled back scan reported %+v", out)
	}
	if !strings.Contains(logged.String(), "executed-file scan: storing the result failed: ") {
		t.Fatalf("log: %q", logged.String())
	}
	if v := models.GetLocalState(rt.DB.Read(), stateRootSince); !strings.Contains(v, filepath.Join(base, "b/storage")) {
		t.Fatalf("root since times after the rolled back walk: %q", v)
	}
	if err := execWrite(rt.DB, `DROP TRIGGER fail_exec`); err != nil {
		t.Fatal(err)
	}
	out := rt.scanFiles(c, res, time.Unix(full+10, 0))
	if len(out) != 1 || out[0].Target != evil {
		t.Fatalf("the next check: %+v", out)
	}
	_ = os.Remove(evil)
}
