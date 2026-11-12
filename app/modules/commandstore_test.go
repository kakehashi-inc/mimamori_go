package modules

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"mimamori/app/models"
)

func TestCommandSetRowsRoundTrip(t *testing.T) {
	MigrationsFS = os.DirFS("../..")
	db, err := OpenSQLite(filepath.Join(t.TempDir(), "target.db"), SchemaAgentDB)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	specs := []string{
		`{"name":"a","purpose":"collect_output","os_user":"app","work_dir":"/srv/app","joiner":"newline","keep_runs":5,
		  "schedule":{"kind":"weekly","weekdays":["fri","mon"],"time":"03:05"},
		  "commands":[{"command_line":"df -h","note":"n","patterns":["(?P<u>\\d+)%"],"line_limit":3},
		              {"command_line":"x","success":{"kind":"range","a":0,"b":2},"failure":{"kind":"min","a":3},"on_problems":"continue_always","timeout_sec":9}]}`,
		`{"name":"b","purpose":"run_only","os_user":"root","schedule":{"kind":"hourly_at","minute":0},"commands":[{"command_line":"true"}]}`,
		`{"name":"c","purpose":"run_only","os_user":"root","schedule":{"kind":"once","at":"2030-01-02T03:04:05+09:00"},"commands":[{"command_line":"true"}]}`,
		`{"name":"d","purpose":"run_only","os_user":"root","schedule":{"kind":"every_minutes","every":15},"commands":[{"command_line":"true"}]}`,
		`{"name":"e","purpose":"run_only","os_user":"root","commands":[{"command_line":"true"}]}`,
	}
	var empty string
	if empty, err = TargetCommandSignature(db); err != nil {
		t.Fatal(err)
	}
	for _, js := range specs {
		spec, err := ParseCommandSetSpec([]byte(js))
		if err != nil {
			t.Fatal(err)
		}
		if errs := spec.Validate(0); len(errs) != 0 {
			t.Fatalf("%s: %v", spec.Name, errs)
		}
		row, items := CommandSetRows(spec)
		if err := models.InsertCommandSet(db, row, 1); err != nil {
			t.Fatal(err)
		}
		if err := models.ReplaceCommandItems(db, row.ID, items); err != nil {
			t.Fatal(err)
		}
		def, _, err := LoadCommandSet(db, row.ID)
		if err != nil {
			t.Fatal(err)
		}
		if def.ID != row.ID || def.Revision != 1 || !reflect.DeepEqual(def.CommandSetSpec, *spec) {
			t.Fatalf("round trip of %s:\n got %+v\nwant %+v", spec.Name, def.CommandSetSpec, *spec)
		}
	}
	defs, err := LoadCommandDefinitions(db)
	if err != nil || len(defs.Sets) != len(specs) {
		t.Fatalf("definitions: %v %d", err, len(defs.Sets))
	}
	sig, _ := TargetCommandSignature(db)
	if sig != defs.Signature || sig == empty {
		t.Fatalf("signature %s / %s / %s", sig, defs.Signature, empty)
	}
	if err := models.DeleteCommandSet(db, defs.Sets[0].ID, "x", 2); err != nil {
		t.Fatal(err)
	}
	if after, _ := TargetCommandSignature(db); after == sig {
		t.Fatal("signature unchanged after a delete")
	}
}
