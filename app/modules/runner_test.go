package modules

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"mimamori/app/models"
)

// closedChan returns a stop channel that is already closed.
func closedChan() chan struct{} {
	ch := make(chan struct{})
	close(ch)
	return ch
}

// closeAfter closes a new stop channel after d.
func closeAfter(d time.Duration) chan struct{} {
	ch := make(chan struct{})
	time.AfterFunc(d, func() { close(ch) })
	return ch
}

// A tool has no limit on its total running time.
func TestRunToolNoTimeLimit(t *testing.T) {
	start := time.Now()
	res, err := RunTool(nil, 1<<20, "sh", "-c", "sleep 3; echo done")
	if err != nil || strings.TrimSpace(string(res.Stdout)) != "done" || res.ExitCode != 0 {
		t.Fatalf("run: %+v %v", res, err)
	}
	if el := time.Since(start); el < 3*time.Second {
		t.Fatalf("returned after %v, before the tool ended", el)
	}
}

// The output, the standard error and the exit code are kept; output much
// larger than a pipe buffer does not block the tool and is cut at the limit.
func TestRunToolOutputLimit(t *testing.T) {
	res, err := RunTool(nil, 1<<20, "sh", "-c", "echo hello; echo err >&2; exit 3")
	if err != nil || strings.TrimSpace(string(res.Stdout)) != "hello" || res.ExitCode != 3 || strings.TrimSpace(string(res.Stderr)) != "err" {
		t.Fatalf("run: %+v %v", res, err)
	}
	big, err := RunTool(nil, 1<<20, "head", "-c", "5000000", "/dev/zero")
	if err != nil || big.ExitCode != 0 || len(big.Stdout) != 1<<20 {
		t.Fatalf("large output: exit %v len %d err %v", big.ExitCode, len(big.Stdout), err)
	}
}

// A stop ends a running tool with SIGTERM.
func TestRunToolStop(t *testing.T) {
	start := time.Now()
	_, err := RunTool(closeAfter(200*time.Millisecond), 1<<20, "sleep", "60")
	if !errors.Is(err, ErrToolStopped) {
		t.Fatalf("want ErrToolStopped, got %v", err)
	}
	// Generous margin: other tests may run on the machine at the same time.
	if el := time.Since(start); el > 3*time.Second {
		t.Fatalf("the stop took %v", el)
	}
}

// A tool that ignores SIGTERM gets SIGKILL after the grace.
func TestRunToolStopGrace(t *testing.T) {
	prev := toolKillGrace
	toolKillGrace = 300 * time.Millisecond
	t.Cleanup(func() { toolKillGrace = prev })
	start := time.Now()
	res, err := RunTool(closeAfter(100*time.Millisecond), 1<<20, "sh", "-c", `trap "" TERM; echo ready; sleep 60`)
	el := time.Since(start)
	if !errors.Is(err, ErrToolStopped) {
		t.Fatalf("want ErrToolStopped, got %v", err)
	}
	if res == nil || strings.TrimSpace(string(res.Stdout)) != "ready" || res.ExitCode != 128+9 {
		t.Fatalf("result: %+v", res)
	}
	if el < 400*time.Millisecond || el > 3*time.Second {
		t.Fatalf("SIGKILL after the grace expected about 400 ms after the start, got %v", el)
	}
}

// With the stop already closed the tool is not started.
func TestRunToolStoppedBeforeStart(t *testing.T) {
	marker := filepath.Join(t.TempDir(), "started")
	res, err := RunTool(closedChan(), 1<<20, "sh", "-c", "touch "+marker)
	if !errors.Is(err, ErrToolStopped) || res != nil {
		t.Fatalf("want ErrToolStopped and no result, got %+v %v", res, err)
	}
	if _, err := os.Stat(marker); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("the tool must not be started: %v", err)
	}
	if _, err := RunSimple(closedChan(), "true"); !errors.Is(err, ErrToolStopped) {
		t.Fatalf("RunSimple: %v", err)
	}
}

// fakeTool puts an executable script with the given name first in PATH.
func fakeTool(t *testing.T, name, script string) {
	t.Helper()
	bin := filepath.Join(t.TempDir(), "bin")
	if err := os.MkdirAll(bin, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(bin, name), []byte("#!/bin/sh\n"+script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
}

// A stop while ausearch runs ends it; the stored position stays and no
// detection comes from it. After the stop ausearch is not started at all.
func TestCollectAuditStoppedKeepsPosition(t *testing.T) {
	rt, done := openAgentDB(t)
	defer done()
	calls := filepath.Join(t.TempDir(), "calls")
	fakeTool(t, "ausearch", "echo run >> '"+calls+"'\nsleep 60\n")
	prev := toolKillGrace
	toolKillGrace = 300 * time.Millisecond
	t.Cleanup(func() { toolKillGrace = prev })
	pos := `{"sec":1700000000,"msec":5,"serial":7}`
	setLocalState(t, rt.DB, stateLastAuditEvent, pos)
	var c RuleContent

	rt.stop = closeAfter(200 * time.Millisecond)
	start := time.Now()
	if ds := rt.collectAudit(&c, nil); len(ds) != 0 {
		t.Fatalf("no detection expected: %+v", ds)
	}
	if el := time.Since(start); el > 3*time.Second {
		t.Fatalf("the stop took %v", el)
	}
	if got := models.GetLocalState(rt.DB.Read(), stateLastAuditEvent); got != pos {
		t.Fatalf("the position moved: %s", got)
	}
	if got := readFile(t, calls); got != "run\n" {
		t.Fatalf("ausearch calls: %q", got)
	}

	// The stop has come: ausearch is not started again.
	if ds := rt.collectAudit(&c, nil); len(ds) != 0 {
		t.Fatalf("no detection expected: %+v", ds)
	}
	if got := readFile(t, calls); got != "run\n" {
		t.Fatalf("ausearch must not be started after the stop: %q", got)
	}
}

// After the stop the report is queued and sent without host information and
// without running auditctl; the detections of the step are in it.
func TestReportAfterStopWithoutHostInfo(t *testing.T) {
	rt, done := openAgentDB(t)
	defer done()
	_, calls := fakeAuditctl(t, "1", nil)
	tr := &fakeTransport{}
	rt.Transport = tr
	rt.stop = closedChan()
	det := DetectionReport{Type: TypeExec, Category: CategoryAlert, Exe: "/usr/bin/x", Count: 1, FirstAt: 1, LastAt: 1}
	rt.report([]DetectionReport{det})
	if len(tr.reports) != 1 {
		t.Fatalf("reports sent: %d", len(tr.reports))
	}
	r := tr.reports[0]
	if r.HostInfo != nil {
		t.Fatalf("host information must be absent after the stop: %+v", r.HostInfo)
	}
	if len(r.Detections) != 1 || r.Detections[0] != det {
		t.Fatalf("detections: %+v", r.Detections)
	}
	b, _ := json.Marshal(r)
	if strings.Contains(string(b), "host_info") {
		t.Fatalf("the report carries host_info: %s", b)
	}
	if _, err := os.Stat(calls); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("auditctl must not run after the stop: %q", readFile(t, calls))
	}

	// Without the stop the host information is collected with auditctl.
	rt.stop = nil
	rt.report(nil)
	if len(tr.reports) != 2 || tr.reports[1].HostInfo == nil || !strings.Contains(readFile(t, calls), "-s") {
		t.Fatalf("host information expected without the stop")
	}
}

// A stop while auditctl runs for the host information drops the host
// information.
func TestHostInfoCutByStop(t *testing.T) {
	prev := toolKillGrace
	toolKillGrace = 300 * time.Millisecond
	t.Cleanup(func() { toolKillGrace = prev })
	fakeTool(t, "auditctl", "sleep 60\n")
	hi, err := CollectHostInfo(HostInfoInput{Stop: closeAfter(200 * time.Millisecond)})
	if !errors.Is(err, ErrToolStopped) || hi != nil {
		t.Fatalf("want ErrToolStopped and no host information, got %v %v", hi, err)
	}
}
