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

// queuedDetections returns the targets of the detections in the queued
// reports.
func queuedDetections(t *testing.T, rt *AgentRuntime) []string {
	t.Helper()
	var out []string
	for _, r := range queuedReports(t, rt) {
		for _, d := range r.Detections {
			out = append(out, d.Target)
		}
	}
	return out
}

// TestDetectionsKeptWhenThePeriodicReportFails: what makes a detection known
// (the fingerprint of a scan, the position in the audit log) is stored
// together with the detection; when the periodic report of that step cannot
// be queued, the detection is kept and goes with the report of the next
// step, once.
func TestDetectionsKeptWhenThePeriodicReportFails(t *testing.T) {
	rt, done := openAgentDB(t)
	defer done()
	var logged bytes.Buffer
	rt.Logger = log.New(&logged, "", 0)
	// After the stop no tool runs: no rule application, no host information;
	// the audit log is read by a fake ausearch below in its own step.
	rt.stop = closedChan()
	base := t.TempDir()
	file := filepath.Join(base, "d1/f")
	mkFile(t, file, 0o666)
	rt.mon.rule = &RuleAssignment{ID: 1, Name: "r", Version: 1, Content: RuleContent{
		Writable: WritableSpec{Base: base, AllowedPaths: []string{"allowed"}, Category: CategoryAlert}, ScanIntervalSec: 3600}}
	if err := execWrite(rt.DB, `CREATE TRIGGER fail_outbox BEFORE INSERT ON outbox BEGIN SELECT RAISE(ABORT, 'test: outbox write fails'); END`); err != nil {
		t.Fatal(err)
	}
	rt.monitorStep()
	if !strings.Contains(logged.String(), "the detections found are kept for the next report") {
		t.Fatalf("log of the failing step: %q", logged.String())
	}
	if err := execWrite(rt.DB, `DROP TRIGGER fail_outbox`); err != nil {
		t.Fatal(err)
	}
	for range 3 {
		rt.monitorStep()
	}
	if got := queuedDetections(t, rt); len(got) != 1 || got[0] != file {
		t.Fatalf("queued detections %v, want %s once", got, file)
	}

	// The audit log: the position moves with the detections of the records
	// read, and the step's report fails.
	fakeTool(t, "ausearch", `cat <<'X'
type=SYSCALL msg=audit(1700000000.100:10): arch=c000003e syscall=59 success=yes exit=0 euid=0 comm="tool" exe="/usr/bin/tool" key="mimamori"
type=EXECVE msg=audit(1700000000.100:10): argc=2 a0="tool" a1="-v"
X
`)
	c := &RuleContent{Users: []Element{{Value: "root"}}, Exec: CategoryOnly{Category: CategoryAlert}}
	rt.stop = nil
	if ds := rt.collectAudit(c, map[int]string{0: "root"}); len(ds) != 1 {
		t.Fatalf("detections of the audit log: %+v", ds)
	}
	rt.stop = closedChan()
	if err := execWrite(rt.DB, `CREATE TRIGGER fail_outbox BEFORE INSERT ON outbox BEGIN SELECT RAISE(ABORT, 'test: outbox write fails'); END`); err != nil {
		t.Fatal(err)
	}
	rt.report()
	if err := execWrite(rt.DB, `DROP TRIGGER fail_outbox`); err != nil {
		t.Fatal(err)
	}
	rt.stop = nil
	if ds := rt.collectAudit(c, map[int]string{0: "root"}); len(ds) != 0 {
		t.Fatalf("the records read before were read again: %+v", ds)
	}
	rt.stop = closedChan()
	rt.report()
	if got := queuedDetections(t, rt); len(got) != 2 || got[1] != "tool -v" {
		t.Fatalf("queued detections %v, want the execution once more", got)
	}
}
