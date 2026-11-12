package modules

import (
	"encoding/json"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"testing/fstest"
	"time"

	"mimamori/app/models"
)

func TestMergeTemplates(t *testing.T) {
	base, _ := FindBuiltinTemplate("base")
	apache, _ := FindBuiltinTemplate("apache")
	nginx, _ := FindBuiltinTemplate("nginx")
	merged, conflicts := MergeTemplates([]NamedContent{
		{Name: "base", Content: &base.Content}, {Name: "apache", Content: &apache.Content}, {Name: "nginx", Content: &nginx.Content},
	}, nil)
	if len(conflicts) != 0 {
		t.Fatalf("unexpected conflicts: %+v", conflicts)
	}
	var users []string
	for _, u := range merged.Users {
		users = append(users, u.Value)
	}
	if strings.Join(users, ",") != "apache,www-data,nginx" {
		t.Fatalf("users not united without duplicates: %v", users)
	}
	if merged.Writable.Base != "/opt" || merged.Exec.Category != CategoryAlert || len(merged.Read.Dirs) != 5 {
		t.Fatalf("merged: %s", merged.JSON())
	}

	a := &RuleContent{Writable: WritableSpec{Base: "/opt"}, CheckIntervalSec: 60}
	b := &RuleContent{Writable: WritableSpec{Base: "/srv"}, CheckIntervalSec: 60}
	_, conflicts = MergeTemplates([]NamedContent{{Name: "a", Content: a}, {Name: "b", Content: b}}, nil)
	if len(conflicts) != 1 || conflicts[0].Field != "writable.base" {
		t.Fatalf("conflicts: %+v", conflicts)
	}
	merged, conflicts = MergeTemplates([]NamedContent{{Name: "a", Content: a}, {Name: "b", Content: b}}, map[string]string{"writable.base": "b"})
	if len(conflicts) != 0 || merged.Writable.Base != "/srv" {
		t.Fatalf("choice not applied: %+v %s", conflicts, merged.Writable.Base)
	}
	// The merged content is independent of the templates.
	merged.Users = append(merged.Users, Element{Value: "x"})
	if again, _ := FindBuiltinTemplate("apache"); len(again.Content.Users) != 2 {
		t.Fatal("built-in template changed")
	}
}

func TestValidate(t *testing.T) {
	c := &RuleContent{
		Users:     []Element{{Value: "Bad User"}},
		Read:      ReadSpec{Dirs: []Element{{Value: "/etc/../root"}, {Value: "relative"}}},
		Writable:  WritableSpec{Base: "/opt", AllowedPaths: []string{"../x"}},
		ExecFiles: ExecFilesSpec{Extensions: []string{".p hp"}},
		Exec:      CategoryOnly{Category: "urgent"},
	}
	c.Normalize()
	errs := c.Validate()
	if len(errs) < 5 {
		t.Fatalf("expected several errors, got %v", errs)
	}
	if _, err := ParseRuleContent([]byte(`{"audit_rules":"-D"}`)); err == nil {
		t.Fatal("unknown fields such as raw auditd rules must be rejected")
	}
	ok := &RuleContent{}
	ok.Normalize()
	if errs := ok.Validate(); len(errs) != 0 {
		t.Fatalf("empty rule must be valid: %v", errs)
	}
}

func TestPathSequence(t *testing.T) {
	seq := SplitComponents("storage")
	for _, p := range []string{"/opt/a/storage", "/opt/a/prod/storage", "/opt/x/a/prod/storage"} {
		if !EndsWithSeq(SplitComponents(p), seq) {
			t.Errorf("%s should match storage", p)
		}
	}
	bc := SplitComponents("bootstrap/cache")
	if MatchSeqAt(SplitComponents("a/bootstrap/cache/x"), bc) != 3 {
		t.Error("bootstrap/cache inside path")
	}
	if MatchSeqAt(SplitComponents("a/bootstrap/x/cache"), bc) != -1 {
		t.Error("non-consecutive names must not match")
	}
}

func TestGlobAndDiff(t *testing.T) {
	if !GlobMatch("/usr/bin/*", "/usr/bin/curl") || GlobMatch("/usr/bin/*", "/bin/sh") || !GlobMatch("", "x") || !GlobMatch("*cache*", "/a/cache/b") {
		t.Fatal("glob")
	}
	d := DiffLines("a\nb\nc", "a\nc\nd")
	var ops []string
	for _, l := range d {
		ops = append(ops, l.Op+l.Text)
	}
	if strings.Join(ops, ",") != " a,-b, c,+d" {
		t.Fatalf("diff: %v", ops)
	}
}

const sampleAudit = `type=SYSCALL msg=audit(1700000000.100:10): arch=c000003e syscall=59 success=yes exit=0 a0=55 ppid=1 pid=200 auid=4294967295 uid=48 gid=48 euid=48 comm="curl" exe="/usr/bin/curl" key="mimamori"
type=EXECVE msg=audit(1700000000.100:10): argc=2 a0="curl" a1="http://example.com"
type=PATH msg=audit(1700000000.100:10): item=0 name="/usr/bin/curl" nametype=NORMAL
type=SYSCALL msg=audit(1700000001.100:11): arch=c000003e syscall=59 success=yes exit=0 euid=48 comm="curl" exe="/usr/bin/curl" key="mimamori"
type=EXECVE msg=audit(1700000001.100:11): argc=2 a0="curl" a1="http://example.com"
type=SYSCALL msg=audit(1700000002.000:12): arch=c000003e syscall=257 success=no exit=-13 euid=48 comm="php-fpm" exe=2F7573722F7362696E2F7068702D66706D key="mimamori"
type=CWD msg=audit(1700000002.000:12): cwd="/var/www"
type=PATH msg=audit(1700000002.000:12): item=0 name="/root/" nametype=PARENT
type=PATH msg=audit(1700000002.000:12): item=1 name="secret.txt" nametype=NORMAL
type=SYSCALL msg=audit(1700000003.000:13): arch=c000003e syscall=42 success=no exit=-115 euid=0 comm="ffmpeg" exe="/opt/ffmpeg/bin/ffmpeg" key="mimamori"
type=SOCKADDR msg=audit(1700000003.000:13): saddr=020001BBC00002010000000000000000
type=SYSCALL msg=audit(1700000003.500:15): arch=c000003e syscall=59 success=no exit=-2 euid=48 comm="setpriv" exe="/usr/bin/setpriv" key="mimamori"
type=SYSCALL msg=audit(1700000004.000:14): arch=c000003e syscall=257 success=yes exit=3 euid=48 comm="cat" exe="/usr/bin/cat" key="other"
`

func TestAuditParsing(t *testing.T) {
	events := ParseAuditRaw(sampleAudit)
	if len(events) != 6 {
		t.Fatalf("events: %d", len(events))
	}
	c := &RuleContent{Users: []Element{{Value: "apache"}}, Exec: CategoryOnly{Category: "alert"}, Denied: CategoryOnly{Category: "normal"},
		Network: NetworkSpec{Executables: []Element{{Value: "/opt/ffmpeg/bin/ffmpeg"}}, Category: "alert"}}
	got := AggregateAuditEvents(events, c, map[int]string{48: "apache", 0: "root"})
	byType := map[string]DetectionReport{}
	for _, d := range got {
		byType[d.Type] = d
	}
	if e := byType[TypeExec]; e.Count != 2 || e.OSUser != "apache" || e.Target != "curl http://example.com" || e.FirstAt != 1700000000 || e.LastAt != 1700000001 {
		t.Fatalf("exec: %+v", e)
	}
	if d := byType[TypeDenied]; d.Target != "/var/www/secret.txt" || d.Exe != "/usr/sbin/php-fpm" || d.Category != "normal" {
		t.Fatalf("denied: %+v", d)
	}
	if n := byType[TypeNetwork]; n.Target != "192.0.2.1:443" {
		t.Fatalf("network: %+v", n)
	}
	if _, ok := byType[TypeRead]; ok {
		t.Fatal("events without the mimamori key or disabled types must be ignored")
	}
}

func TestGenerateAuditRules(t *testing.T) {
	c := &RuleContent{Users: []Element{{Value: "apache"}}, Exec: CategoryOnly{Category: "alert"}, Read: ReadSpec{Category: "alert"}}
	r := &ResolvedRule{Content: c, UIDs: []int{48}, Dirs: []string{"/etc/ssh"}}
	lines := GenerateAuditRules(r)
	if len(lines) != 2 {
		t.Fatalf("read without dirs in content must be disabled: %v", lines)
	}
	if !strings.Contains(lines[0], "-S execve -S execveat ") || !strings.Contains(lines[1], "arch=b32 -S execve -S execveat ") {
		t.Fatalf("execution rules must audit execve and execveat: %v", lines)
	}
	for _, l := range lines {
		if !strings.HasSuffix(l, "-k "+AuditKey) || !strings.Contains(l, "auid=4294967295") {
			t.Fatalf("rule %q", l)
		}
	}
}

func TestScans(t *testing.T) {
	base := t.TempDir()
	mk := func(p string, mode os.FileMode) {
		full := filepath.Join(base, p)
		_ = os.MkdirAll(filepath.Dir(full), 0o755)
		if err := os.WriteFile(full, []byte("x"), mode); err != nil {
			t.Fatal(err)
		}
		_ = os.Chmod(full, mode)
	}
	mk("app/public/index.php", 0o644)
	mk("app/public/shell.php", 0o666)
	mk("app/storage/logs/a.log", 0o666)
	mk("app/storage/up/evil.php", 0o644)
	mk("x/prod/bootstrap/cache/config.php", 0o644)
	_ = os.Symlink("/etc", filepath.Join(base, "link"))
	res, err := ScanWritable(base, []string{"storage", "bootstrap/cache"}, map[int]bool{}, CategoryAlert)
	if err != nil {
		t.Fatal(err)
	}
	var found []string
	for _, f := range res.Findings {
		found = append(found, strings.TrimPrefix(f.Path, base))
	}
	if strings.Join(found, ",") != "/app/public/shell.php" {
		t.Fatalf("writable findings: %v", found)
	}
	if len(res.AllowedRoots) != 2 {
		t.Fatalf("allowed roots: %v", res.AllowedRoots)
	}
	laravel, _ := FindBuiltinTemplate("laravel")
	c := &RuleContent{Writable: WritableSpec{Base: base, AllowedPaths: []string{"storage", "bootstrap/cache"}},
		ExecFiles: ExecFilesSpec{Extensions: []string{".php"}, Legit: laravel.Content.ExecFiles.Legit, OtherCategory: CategoryAlert}}
	files, _, err := ScanExecFiles(base, res.AllowedRoots, c, sinceAll(1), false)
	if err != nil {
		t.Fatal(err)
	}
	cats := map[string]string{}
	for _, f := range files {
		cats[filepath.Base(f.Path)] = f.Category
	}
	if cats["evil.php"] != CategoryAlert || cats["config.php"] != CategoryNormal || len(cats) != 2 {
		t.Fatalf("exec files: %v", cats)
	}
	if files, skipped, _ := ScanExecFiles(base, res.AllowedRoots, c, sinceAll(0), true); len(files) != 1 || len(skipped) != 1 {
		t.Fatalf("first scan must skip legitimate locations: %d (skipped %d)", len(files), len(skipped))
	}
	if files, _, _ := ScanExecFiles(base, res.AllowedRoots, c, sinceAll(time.Now().Unix()+10), false); len(files) != 0 {
		t.Fatal("unchanged files must not be reported")
	}
}

type fakeTransport struct {
	fail    bool
	reports []*ReportRequest
}

func (f *fakeTransport) Sync(*SyncRequest) (*SyncResponse, error) {
	if f.fail {
		return nil, os.ErrDeadlineExceeded
	}
	return &SyncResponse{AgentName: "t"}, nil
}
func (f *fakeTransport) Report(r *ReportRequest) error {
	if f.fail {
		return os.ErrDeadlineExceeded
	}
	f.reports = append(f.reports, r)
	return nil
}

func TestOutboxKeepsReportsAndDropsNormalFirst(t *testing.T) {
	MigrationsFS = os.DirFS("../..")
	dir := t.TempDir()
	db, err := OpenSQLite(filepath.Join(dir, "agent.db"), SchemaAgentLocal)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	tr := &fakeTransport{fail: true}
	rt := NewAgentRuntime(db, tr, nil)
	mkReport := func(cat string, n int) {
		var ds []DetectionReport
		for i := 0; i < n; i++ {
			ds = append(ds, DetectionReport{Type: TypeExec, Category: cat, Target: cat})
		}
		if err := rt.enqueue(&ReportRequest{Detections: ds}); err != nil {
			t.Fatal(err)
		}
	}
	mkReport(CategoryNormal, DefaultOutboxMaxDetection/2)
	mkReport(CategoryAlert, DefaultOutboxMaxDetection/2)
	mkReport(CategoryAlert, 10)
	rt.enforceOutboxLimit()
	if n := models.OutboxDetectionCount(db); n != DefaultOutboxMaxDetection {
		t.Fatalf("outbox size %d", n)
	}
	if v := models.GetLocalState(db, stateDroppedPending); v != "10" {
		t.Fatalf("dropped count %q", v)
	}
	items, _ := models.ListOutbox(db)
	var first ReportRequest
	_ = json.Unmarshal([]byte(items[0].Payload), &first)
	if len(first.Detections) != DefaultOutboxMaxDetection/2-10 {
		t.Fatalf("normal detections must be dropped first: %d", len(first.Detections))
	}
	tr.fail = false
	rt.flushOutbox()
	if len(tr.reports) != 3 || models.OutboxDetectionCount(db) != 0 {
		t.Fatalf("flush: %d reports", len(tr.reports))
	}
}

func TestRunCommandTimeout(t *testing.T) {
	res, err := RunCommand(5*time.Second, 1<<20, "sh", "-c", "echo hello; echo err >&2; exit 3")
	if err != nil || strings.TrimSpace(string(res.Stdout)) != "hello" || res.ExitCode != 3 || strings.TrimSpace(string(res.Stderr)) != "err" {
		t.Fatalf("run: %+v %v", res, err)
	}
	// Output much larger than a pipe buffer must not block the command.
	big, err := RunCommand(10*time.Second, 1<<20, "head", "-c", "5000000", "/dev/zero")
	if err != nil || big.ExitCode != 0 || len(big.Stdout) != 1<<20 {
		t.Fatalf("large output: exit %v len %d err %v", big.ExitCode, len(big.Stdout), err)
	}
	start := time.Now()
	if _, err := RunCommand(300*time.Millisecond, 1<<20, "sleep", "5"); err == nil || time.Since(start) > 3*time.Second {
		t.Fatalf("timeout not enforced: %v", err)
	}
}

func TestSecrets(t *testing.T) {
	h, _ := HashPassword("correct horse battery")
	if !VerifyPassword(h, "correct horse battery") || VerifyPassword(h, "wrong") {
		t.Fatal("password hash")
	}
	a, b, c := DeriveRenewalToken([]byte("k"), 1, 2), DeriveRenewalToken([]byte("k"), 1, 2), DeriveRenewalToken([]byte("k"), 1, 3)
	if a != b || a == c {
		t.Fatal("renewal derivation")
	}
}

// TestMigrationsUpgradeExistingDatabase checks that a database created with
// the current schema is upgraded in place when a newer migration is added,
// for each of the three kinds of database files.
func TestMigrationsUpgradeExistingDatabase(t *testing.T) {
	repo := os.DirFS("../..")
	for _, schema := range []string{SchemaCentral, SchemaAgentDB, SchemaAgentLocal} {
		t.Run(schema, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), schema+".db")
			MigrationsFS = repo
			db, err := OpenSQLite(path, schema)
			if err != nil {
				t.Fatal(err)
			}
			db.Close()

			next := fstest.MapFS{}
			entries, _ := fs.ReadDir(repo, "app/migrations/"+schema)
			for _, e := range entries {
				data, _ := fs.ReadFile(repo, "app/migrations/"+schema+"/"+e.Name())
				next["app/migrations/"+schema+"/"+e.Name()] = &fstest.MapFile{Data: data}
			}
			next["app/migrations/"+schema+"/9999_test.sql"] = &fstest.MapFile{Data: []byte(
				"-- +goose Up\n-- +goose StatementBegin\nCREATE TABLE upgrade_check (id INTEGER PRIMARY KEY);\n-- +goose StatementEnd\n" +
					"-- +goose Down\n-- +goose StatementBegin\nDROP TABLE upgrade_check;\n-- +goose StatementEnd\n")}
			MigrationsFS = next
			defer func() { MigrationsFS = repo }()
			db, err = OpenSQLite(path, schema)
			if err != nil {
				t.Fatal(err)
			}
			defer db.Close()
			var v int64
			if err := db.QueryRow(`SELECT MAX(version_id) FROM goose_db_version`).Scan(&v); err != nil || v != 9999 {
				t.Fatalf("schema version %d, %v", v, err)
			}
			if _, err := db.Exec(`INSERT INTO upgrade_check (id) VALUES (1)`); err != nil {
				t.Fatalf("new table missing: %v", err)
			}
		})
	}
}

func TestLockHolder(t *testing.T) {
	path := filepath.Join(t.TempDir(), "x.lock")
	if _, running, err := LockHolder(path); err != nil || running {
		t.Fatalf("missing lock file: running=%v err=%v", running, err)
	}
	l, err := AcquireLock(path)
	if err != nil {
		t.Fatal(err)
	}
	pid, running, err := LockHolder(path)
	if err != nil || !running || pid != os.Getpid() {
		t.Fatalf("held lock: pid=%d running=%v err=%v", pid, running, err)
	}
	if _, err := AcquireLock(path); err == nil {
		t.Fatal("a second instance must not get the lock")
	}
	l.Release()
	if _, running, _ := LockHolder(path); running {
		t.Fatal("released lock still reported as running")
	}
}
