package modules

import (
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
)

// central config set and unset end with exit code 2 for a refused item or
// value and with exit code 3 when the database fails, as the other commands
// do; nothing is changed in either case.
func TestConfigChangeExitCodes(t *testing.T) {
	MigrationsFS = os.DirFS("../..")
	prevDir := dataDirOverride
	t.Cleanup(func() { SetDataDir(prevDir) })
	dir := t.TempDir()
	SetDataDir(dir)
	// The commands open the database themselves (once per process).
	withDB := func(f func(db *DB)) {
		t.Helper()
		db, err := OpenSQLite(filepath.Join(dir, CentralDBFileName), SchemaCentral)
		if err != nil {
			t.Fatal(err)
		}
		defer db.Close()
		f(db)
	}
	if err := (&CentralConfigSetCmd{Name: "server_names", Value: "a.example.com"}).Run(); err != nil {
		t.Fatal(err)
	}
	stored := func() string {
		t.Helper()
		var cfg *CentralConfig
		var set map[string]bool
		var err error
		withDB(func(db *DB) { cfg, set, err = CentralConfigState(db.Read()) })
		if err != nil {
			t.Fatal(err)
		}
		names := slices.Sorted(maps.Keys(set))
		return strings.Join(cfg.ServerNames, ",") + " " + cfg.WebListen + " " + strconv.Itoa(cfg.RetentionDays) + " " + strings.Join(names, ",")
	}
	before := stored()

	refused := []struct {
		name string
		run  func() error
	}{
		{"unknown item set", (&CentralConfigSetCmd{Name: "no_such_item", Value: "1"}).Run},
		{"unknown item unset", (&CentralConfigUnsetCmd{Name: "no_such_item"}).Run},
		{"value of the item", (&CentralConfigSetCmd{Name: "retention_days", Value: "-1"}).Run},
		{"configuration as a whole", (&CentralConfigSetCmd{Name: "web_listen", Value: "nowhere"}).Run},
	}
	for _, c := range refused {
		if ee := wantExit(t, c.run(), ExitArgument); ee.Message == "" {
			t.Errorf("%s: refused without a message", c.name)
		}
		if after := stored(); after != before {
			t.Fatalf("%s: a refused change changed the configuration: %s -> %s", c.name, before, after)
		}
	}

	withDB(func(db *DB) {
		for _, op := range []string{"INSERT", "DELETE"} {
			if err := execWrite(db, `CREATE TRIGGER refuse_`+op+` BEFORE `+op+` ON settings BEGIN SELECT RAISE(ABORT, 'settings refused'); END`); err != nil {
				t.Fatal(err)
			}
		}
	})
	for _, c := range []struct {
		name string
		run  func() error
	}{
		{"set", (&CentralConfigSetCmd{Name: "retention_days", Value: "30"}).Run},
		{"unset", (&CentralConfigUnsetCmd{Name: "server_names"}).Run},
	} {
		ee := wantExit(t, c.run(), ExitConfig)
		if !strings.Contains(ee.Error(), "settings refused") {
			t.Errorf("%s: the database's error is not shown: %v", c.name, ee)
		}
		if after := stored(); after != before {
			t.Fatalf("%s: a failed change changed the configuration: %s -> %s", c.name, before, after)
		}
	}
}
