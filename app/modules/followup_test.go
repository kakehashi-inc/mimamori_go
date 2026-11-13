package modules

import (
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"mimamori/app/models"
)

// A root that appears after the scan configuration changed is scanned like
// the roots of a first scan: the files already in it are reported once,
// files in legitimate locations are not, and known roots keep their mark.
func TestScanFilesAfterScanConfigChange(t *testing.T) {
	rt, done := openAgentDB(t)
	defer done()
	mk := func(base, p string) string {
		full := filepath.Join(base, p)
		_ = os.MkdirAll(filepath.Dir(full), 0o755)
		if err := os.WriteFile(full, []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
		return full
	}
	rule := func(base string, allowed ...string) (*RuleContent, *ResolvedRule) {
		c := &RuleContent{
			Writable: WritableSpec{Base: base, AllowedPaths: allowed},
			ExecFiles: ExecFilesSpec{Extensions: []string{".php"}, OtherCategory: CategoryAlert,
				Legit: []LegitLocation{{Path: "storage/views", Extensions: []string{".php"}, Category: CategoryNormal}}},
		}
		return c, &ResolvedRule{Content: c}
	}
	sig := func(c *RuleContent) string { return c.Writable.Base + "|" + filepath.Join(c.Writable.AllowedPaths...) }

	base1, base2 := t.TempDir(), t.TempDir()
	old1 := mk(base1, "a/storage/old1.php")
	old2 := mk(base2, "b/storage/old2.php")
	mk(base2, "b/storage/views/legit.php")
	inUploads := mk(base2, "b/uploads/up.php")
	now := ctimeOf(t, inUploads) + 5

	c1, res1 := rule(base1, "storage")
	rt.noteScanConfig(sig(c1))
	if out := rt.scanFiles(c1, res1, time.Unix(now, 0)); len(out) != 1 || out[0].Target != old1 {
		t.Fatalf("first scan: %+v", out)
	}

	// Another base directory: its existing file is reported, the legitimate
	// one is not, and nothing is reported twice.
	c2, res2 := rule(base2, "storage")
	rt.noteScanConfig(sig(c2))
	if out := rt.scanFiles(c2, res2, time.Unix(now+10, 0)); len(out) != 1 || out[0].Target != old2 {
		t.Fatalf("scan after the base changed: %+v", out)
	}
	if out := rt.scanFiles(c2, res2, time.Unix(now+20, 0)); len(out) != 0 {
		t.Fatalf("reported again: %+v", out)
	}

	// One more allowed path under the same base: only the file in the new
	// root is reported.
	c3, res3 := rule(base2, "storage", "uploads")
	rt.noteScanConfig(sig(c3))
	if out := rt.scanFiles(c3, res3, time.Unix(now+30, 0)); len(out) != 1 || out[0].Target != inUploads {
		t.Fatalf("scan after an allowed path was added: %+v", out)
	}
	if models.GetLocalState(rt.DB.Read(), stateScanPrevRoots) != "" {
		t.Error("the remembered roots were not cleared after the full scan")
	}
}

// Transactions of the agent's runtime that read and then write wait for
// each other instead of failing with "database is locked", and none loses
// the update of another.
func TestWriteTransactionsWait(t *testing.T) {
	rt, done := openAgentDB(t)
	defer done()
	var wg sync.WaitGroup
	errs := make(chan error, 64)
	for g := 0; g < 8; g++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < 20; i++ {
				err := rt.DB.Write(func(tx models.Writer) error {
					v := models.GetLocalState(tx, "counter")
					return models.SetLocalState(tx, "counter", v+"x")
				})
				if err != nil {
					errs <- err
					return
				}
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatalf("transaction failed: %v", err)
	}
	if got := len(models.GetLocalState(rt.DB.Read(), "counter")); got != 160 {
		t.Errorf("counter length = %d, want 160 (an update was lost)", got)
	}
}

func TestLoginAuthChallenges(t *testing.T) {
	a := &loginAuth{user: "u", pass: "p"}
	for challenge, want := range map[string]string{
		"Username:": "u", "User Name\x00": "u", "username": "u",
		"Password:": "p", "Password\x00": "p",
	} {
		got, err := a.Next([]byte(challenge), true)
		if err != nil || string(got) != want {
			t.Errorf("Next(%q) = %q, %v; want %q", challenge, got, err, want)
		}
	}
	if _, err := a.Next([]byte("Something else"), true); err == nil {
		t.Error("an unknown challenge was answered")
	}
}

func TestDefaultCheckIntervalLowerBound(t *testing.T) {
	c := DefaultCentralConfig()
	c.DefaultCheckIntervalSec = MinCheckInterval - 1
	if err := c.Validate(); err == nil {
		t.Error("a default check interval below the minimum was accepted")
	}
	c.DefaultCheckIntervalSec = MinCheckInterval
	if err := c.Validate(); err != nil {
		t.Errorf("the minimum was rejected: %v", err)
	}
}

func TestCtlHistoryDetailKeepsUserText(t *testing.T) {
	saved := time.Local
	time.Local = time.FixedZone("JST", 9*3600)
	defer func() { time.Local = saved }()

	user := "target=/var/backups/db-2026-11-11T03:00:00Z.sql.gz"
	row := map[string]any{"category": models.HistSuppression, "detail": user, "at": "2026-11-11T03:00:00Z"}
	out := localizeValue(row).(map[string]any)
	if out["detail"] != user {
		t.Errorf("user text changed: %q", out["detail"])
	}
	if out["at"] != "2026-11-11 12:00:00" {
		t.Errorf("at = %q", out["at"])
	}
	tok := map[string]any{"category": models.HistToken, "detail": "expires 2026-11-11T03:00:00Z"}
	if got := localizeValue(tok).(map[string]any)["detail"]; got != "expires 2026-11-11 12:00:00" {
		t.Errorf("token detail = %q", got)
	}
	if !keepsUserDetail(row) || keepsUserDetail(tok) || keepsUserDetail(map[string]any{"detail": "x"}) {
		t.Error("keepsUserDetail")
	}
}
