package modules

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"testing"
	"time"

	"mimamori/app/models"
)

func TestWalkLess(t *testing.T) {
	// The order of filepath.WalkDir: a directory before its contents, then
	// the next name.
	ordered := []string{"/b", "/b/a", "/b/a/z", "/b/a.c", "/b/a0", "/b/b", "/c"}
	for i := range ordered {
		for j := range ordered {
			if got := walkLess(ordered[i], ordered[j]); got != (i < j) {
				t.Errorf("walkLess(%s, %s) = %v", ordered[i], ordered[j], got)
			}
		}
	}
	if !entirelyBefore("/b/a", "/b/a.c") || entirelyBefore("/b", "/b/a") || entirelyBefore("/b/a", "/b/a") {
		t.Error("entirelyBefore")
	}
}

func TestMarkScanned(t *testing.T) {
	segs := []sinceSegment{{From: "", Since: 10}}
	// A first check scans up to /b/m.
	segs = markScanned(segs, "", "/b/m", 20)
	if fmt.Sprint(segs) != "[{ 20} {/b/m 10}]" {
		t.Fatalf("first part: %v", segs)
	}
	// The next one continues there and stops at /c.
	segs = markScanned(segs, "/b/m", "/c", 30)
	if fmt.Sprint(segs) != "[{ 20} {/b/m 30} {/c 10}]" {
		t.Fatalf("second part: %v", segs)
	}
	for p, want := range map[string]int64{"/a": 20, "/b": 20, "/b/a/z": 20, "/b/m": 30, "/b/m/x": 30, "/b/z": 30, "/c": 10, "/c/x": 10, "/d": 10} {
		if got := sinceAt(segs, p); got != want {
			t.Errorf("sinceAt(%s) = %d, want %d", p, got, want)
		}
	}
	// The end is reached: the rest gets the time of this check.
	segs = markScanned(segs, "/c", "", 40)
	if fmt.Sprint(segs) != "[{ 20} {/b/m 30} {/c 40}]" {
		t.Fatalf("third part: %v", segs)
	}
	// A new pass that reaches the end in one check.
	if got := markScanned(segs, "", "", 50); fmt.Sprint(got) != "[{ 50}]" {
		t.Fatalf("complete pass: %v", got)
	}
	// Adjacent equal times merge.
	if got := markScanned([]sinceSegment{{"", 5}, {"/m", 7}}, "", "/m", 7); fmt.Sprint(got) != "[{ 7}]" {
		t.Fatalf("merge: %v", got)
	}
}

func mkFile(t *testing.T, p string, mode os.FileMode) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte("x"), mode); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(p, mode); err != nil {
		t.Fatal(err)
	}
}

// A writable scan larger than the entry limit is continued by the following
// checks: every file is visited (and reported once) within the expected
// number of checks, and a complete pass reports nothing again.
func TestWritableScanContinues(t *testing.T) {
	rt, done := openAgentDB(t)
	defer done()
	base := t.TempDir()
	var files []string
	for d := 0; d < 5; d++ {
		for f := 0; f < 5; f++ {
			p := filepath.Join(base, fmt.Sprintf("d%d", d), fmt.Sprintf("f%d", f))
			mkFile(t, p, 0o666)
			files = append(files, p)
		}
	}
	c := &RuleContent{Writable: WritableSpec{Base: base, Category: CategoryAlert}, ScanIntervalSec: 3600}
	res := &ResolvedRule{Content: c}
	rt.scanLimit = 7
	// 1 base + 5 directories + 25 files = 31 entries: 5 checks of 7.
	const checks = 5
	start := time.Now().Unix()
	seen := map[string]int{}
	for i := 0; i < checks; i++ {
		for _, d := range rt.scanFiles(c, res, time.Unix(start+int64(i)*10, 0)) {
			seen[d.Target]++
		}
		resume := models.GetLocalState(rt.DB.Read(), stateWritableResume)
		if (i < checks-1) != (resume != "") {
			t.Fatalf("check %d: resume %q", i, resume)
		}
		if i < checks-1 {
			sc := rt.incompleteScans()
			if len(sc) != 1 || sc[0].Kind != ScanKindWritable || sc[0].ResumeAt != resume || sc[0].Limit != 7 || sc[0].StartedAt != start {
				t.Fatalf("check %d: incomplete scans %+v", i, sc)
			}
		}
	}
	for _, f := range files {
		if seen[f] != 1 {
			t.Fatalf("%s reported %d times (all: %v)", f, seen[f], seen)
		}
	}
	if len(seen) != len(files) {
		t.Fatalf("reported: %v", seen)
	}
	if got := models.GetLocalState(rt.DB.Read(), stateLastFullScan); got != fmt.Sprint(start) {
		t.Fatalf("last full scan %s, want the start of the pass %d", got, start)
	}
	if sc := rt.incompleteScans(); len(sc) != 0 {
		t.Fatalf("complete: %+v", sc)
	}
	// Before the interval: nothing scanned.
	if out := rt.scanFiles(c, res, time.Unix(start+60, 0)); len(out) != 0 || models.GetLocalState(rt.DB.Read(), stateWritableResume) != "" {
		t.Fatal("scanned before the interval")
	}
	// The next pass reports nothing again (the findings of the parts
	// visited first are not forgotten in the middle of a pass).
	for i := 0; i < checks; i++ {
		if out := rt.scanFiles(c, res, time.Unix(start+3600+int64(i)*10, 0)); len(out) != 0 {
			t.Fatalf("second pass check %d reported %+v", i, out)
		}
	}
	if models.GetLocalState(rt.DB.Read(), stateWritableResume) != "" {
		t.Fatal("the second pass did not complete")
	}
}

// The executed-file walk continues where it stopped and covers every root
// once per pass.
func TestExecScanFromResumes(t *testing.T) {
	base := t.TempDir()
	var all []string
	var roots []string
	for _, r := range []string{"c/storage", "a/storage", "b/storage"} {
		root := filepath.Join(base, r)
		roots = append(roots, root)
		for f := 0; f < 4; f++ {
			p := filepath.Join(root, fmt.Sprintf("f%d.php", f))
			mkFile(t, p, 0o644)
			all = append(all, p)
		}
	}
	c := &RuleContent{Writable: WritableSpec{Base: base, AllowedPaths: []string{"storage"}},
		ExecFiles: ExecFilesSpec{Extensions: []string{".php"}, OtherCategory: CategoryAlert}}
	since := func(string, string) int64 { return 1 }
	got := map[string]int{}
	resume := ""
	var stops []string
	for i := 0; i < 10; i++ {
		found, _, stop, err := ScanExecFilesFrom(base, roots, c, since, resume, 4)
		if err != nil {
			t.Fatal(err)
		}
		for _, f := range found {
			got[f.Path]++
		}
		stops = append(stops, stop)
		resume = stop
		if stop == "" {
			break
		}
	}
	// 3 roots of 1 directory and 4 files: 15 entries, 4 walks of 4.
	if len(stops) != 4 || stops[3] != "" {
		t.Fatalf("stops: %v", stops)
	}
	for _, f := range all {
		if got[f] != 1 {
			t.Fatalf("%s found %d times: %v", f, got[f], got)
		}
	}
	if !sort.SliceIsSorted(stops[:3], func(i, j int) bool { return walkLess(stops[i], stops[j]) }) {
		t.Fatalf("stops out of order: %v", stops)
	}
}

// With a small entry limit, the executed-file scan of the agent covers the
// roots over several checks; a file changed in a part not reached yet is
// detected once, when that part is reached, and the since time of each
// part is the time the part was last scanned.
func TestExecScanContinues(t *testing.T) {
	rt, done := openAgentDB(t)
	defer done()
	base := t.TempDir()
	for _, r := range []string{"a/storage", "b/storage", "c/storage"} {
		for f := 0; f < 4; f++ {
			mkFile(t, filepath.Join(base, r, fmt.Sprintf("f%d.php", f)), 0o644)
		}
	}
	c := &RuleContent{Writable: WritableSpec{Base: base, AllowedPaths: []string{"storage"}}, ScanIntervalSec: 3600,
		ExecFiles: ExecFilesSpec{Extensions: []string{".php"}, OtherCategory: CategoryAlert}}
	res := &ResolvedRule{Content: c}
	// The checks run at times before the files' status changes, so every
	// file is newer than its part's since time and only the fingerprints
	// keep them from being reported again.
	t0 := time.Now().Unix() - 1000
	if out := rt.scanFiles(c, res, time.Unix(t0, 0)); len(out) != 12 {
		t.Fatalf("first scan reports the existing files: %d", len(out))
	}
	rt.scanLimit = 4
	// 15 entries (3 roots of 1 directory and 4 files): checks of 4 stop at
	// a/storage/f3.php, b/storage/f2.php, c/storage/f1.php, then reach the
	// end.
	if out := rt.scanFiles(c, res, time.Unix(t0+10, 0)); len(out) != 0 {
		t.Fatalf("check 1: %+v", out)
	}
	stop1 := models.GetLocalState(rt.DB.Read(), stateExecResume)
	if stop1 != filepath.Join(base, "a/storage/f3.php") {
		t.Fatalf("first stop %q", stop1)
	}
	var segs []sinceSegment
	_ = json.Unmarshal([]byte(models.GetLocalState(rt.DB.Read(), stateExecSince)), &segs)
	if len(segs) != 2 || segs[0].Since != t0+10-ctimeMargin || segs[1].From != stop1 || segs[1].Since != t0-ctimeMargin {
		t.Fatalf("since times after the first part: %+v", segs)
	}
	if sc := rt.incompleteScans(); len(sc) != 1 || sc[0].Kind != ScanKindExecFiles || sc[0].ResumeAt != stop1 || sc[0].StartedAt != t0+10 {
		t.Fatalf("incomplete scans: %+v", sc)
	}
	// A file in the part not reached yet changes.
	changed := filepath.Join(base, "c/storage/f3.php")
	if err := os.Chmod(changed, 0o640); err != nil {
		t.Fatal(err)
	}
	var reported []string
	for i := 2; i <= 4; i++ {
		for _, d := range rt.scanFiles(c, res, time.Unix(t0+int64(i)*10, 0)) {
			reported = append(reported, d.Target)
		}
	}
	if len(reported) != 1 || reported[0] != changed {
		t.Fatalf("the changed file must be reported once: %v", reported)
	}
	if models.GetLocalState(rt.DB.Read(), stateExecResume) != "" {
		t.Fatal("the pass did not complete")
	}
	// The next pass reports nothing again.
	for i := 5; i <= 9; i++ {
		if out := rt.scanFiles(c, res, time.Unix(t0+int64(i)*10, 0)); len(out) != 0 {
			t.Fatalf("check %d reported %+v", i, out)
		}
	}
}

// A root found by a split writable pass keeps its own since time until the
// executed-file scan walked it to the end.
func TestNewRootKeepsSinceUntilWalked(t *testing.T) {
	rt, done := openAgentDB(t)
	defer done()
	base := t.TempDir()
	mkFile(t, filepath.Join(base, "a/storage/f.php"), 0o644)
	c := &RuleContent{Writable: WritableSpec{Base: base, AllowedPaths: []string{"storage"}}, ScanIntervalSec: 200,
		ExecFiles: ExecFilesSpec{Extensions: []string{".php"}, OtherCategory: CategoryAlert}}
	res := &ResolvedRule{Content: c}
	t0 := time.Now().Unix() - 100
	if out := rt.scanFiles(c, res, time.Unix(t0, 0)); len(out) != 1 {
		t.Fatalf("first scan: %+v", out)
	}
	// A new allowed directory with a file older than the since time of its
	// part after the next check (created before it became a root).
	evil := filepath.Join(base, "z/storage/evil.php")
	mkFile(t, evil, 0o644)
	if out := rt.scanFiles(c, res, time.Unix(t0+110, 0)); len(out) != 0 {
		t.Fatalf("not a root yet: %+v", out)
	}
	// The next full pass is split: the writable scan finds the new root in
	// its second check, and the executed-file scan walks it after that.
	rt.scanLimit = 3
	var reported []string
	for i := 0; i < 6; i++ {
		for _, d := range rt.scanFiles(c, res, time.Unix(t0+200+int64(i)*10, 0)) {
			reported = append(reported, d.Target)
		}
	}
	if len(reported) != 1 || reported[0] != evil {
		t.Fatalf("file in a new root: %v", reported)
	}
	var rs map[string]int64
	_ = json.Unmarshal([]byte(models.GetLocalState(rt.DB.Read(), stateRootSince)), &rs)
	if len(rs) != 0 {
		t.Fatalf("own since times left: %v", rs)
	}
}

// ctimeSec returns the second of the ctime of p.
func ctimeSec(t *testing.T, p string) int64 {
	t.Helper()
	info, err := os.Stat(p)
	if err != nil {
		t.Fatal(err)
	}
	return statT(info).Ctim.Sec
}

// The kernel stamps a ctime from a clock that lags time.Now by a few
// milliseconds, so a file changed right after a check started in a new
// second can carry a ctime of the second before the check. Such a file is
// reported by the next check, both in a root known before and in a root
// that the next full pass finds. The files are made before the first check
// (outside the scanned tree) and moved in with their directory, which keeps
// their ctime; the first check then claims to start just after the second
// of their ctime, as a check would that started right before they were made.
func TestScanFindsFileWithLaggingCtime(t *testing.T) {
	rt, done := openAgentDB(t)
	defer done()
	base := t.TempDir()
	outside := t.TempDir()
	mkFile(t, filepath.Join(base, "a/storage/old.php"), 0o644)
	var late, evil string
	var sec int64
	for try := 0; ; try++ {
		dir := filepath.Join(outside, fmt.Sprint(try))
		late = filepath.Join(dir, "sub/late.php")
		evil = filepath.Join(dir, "n/storage/evil.php")
		mkFile(t, late, 0o644)
		mkFile(t, evil, 0o644)
		if sec = ctimeSec(t, late); sec == ctimeSec(t, evil) {
			break
		}
		if try == 5 {
			t.Fatal("the two files never got the same ctime second")
		}
	}
	c := &RuleContent{Writable: WritableSpec{Base: base, AllowedPaths: []string{"storage"}}, ScanIntervalSec: 1,
		ExecFiles: ExecFilesSpec{Extensions: []string{".php"}, OtherCategory: CategoryAlert}}
	res := &ResolvedRule{Content: c}
	// The first check (a full pass) starts 1 ms into the next second.
	if out := rt.scanFiles(c, res, time.Unix(sec+1, int64(time.Millisecond))); len(out) != 1 {
		t.Fatalf("first scan: %+v", out)
	}
	if err := os.Rename(filepath.Dir(late), filepath.Join(base, "a/storage/sub")); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(filepath.Dir(filepath.Dir(evil)), filepath.Join(base, "n")); err != nil {
		t.Fatal(err)
	}
	if got := ctimeSec(t, filepath.Join(base, "a/storage/sub/late.php")); got != sec {
		t.Skipf("this file system changes the ctime of files in a moved directory (%d, %d)", got, sec)
	}
	var reported []string
	for _, d := range rt.scanFiles(c, res, time.Unix(sec+3, 0)) {
		reported = append(reported, d.Target)
	}
	sort.Strings(reported)
	want := []string{filepath.Join(base, "a/storage/sub/late.php"), filepath.Join(base, "n/storage/evil.php")}
	if fmt.Sprint(reported) != fmt.Sprint(want) {
		t.Fatalf("reported %v, want %v", reported, want)
	}
}
