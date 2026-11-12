package modules

import (
	"bytes"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/alecthomas/kong"
)

// The command set commands take their arguments and flags as documented.
func TestCtlCommandSetsArguments(t *testing.T) {
	var cli struct {
		Ctl CtlCmd `cmd:""`
	}
	parser, err := kong.New(&cli)
	if err != nil {
		t.Fatal(err)
	}
	parse := func(args ...string) string {
		t.Helper()
		ctx, err := parser.Parse(append([]string{"ctl", "command-sets"}, args...))
		if err != nil {
			t.Fatalf("%v: %v", args, err)
		}
		return ctx.Command()
	}
	if got := parse("list", "3"); got != "ctl command-sets list <agent>" || cli.Ctl.CommandSets.List.Agent != 3 {
		t.Errorf("list: %q %+v", got, cli.Ctl.CommandSets.List)
	}
	if parse("show", "3", "7"); cli.Ctl.CommandSets.Show.Agent != 3 || cli.Ctl.CommandSets.Show.Set != 7 {
		t.Errorf("show: %+v", cli.Ctl.CommandSets.Show)
	}
	if parse("export", "3", "7"); cli.Ctl.CommandSets.Export.Set != 7 {
		t.Errorf("export: %+v", cli.Ctl.CommandSets.Export)
	}
	if parse("create", "3", "--file", "def.json"); cli.Ctl.CommandSets.Create.Agent != 3 || cli.Ctl.CommandSets.Create.File != "def.json" {
		t.Errorf("create: %+v", cli.Ctl.CommandSets.Create)
	}
	if parse("update", "3", "7", "--file", "-"); cli.Ctl.CommandSets.Update.Set != 7 || cli.Ctl.CommandSets.Update.File != "-" {
		t.Errorf("update: %+v", cli.Ctl.CommandSets.Update)
	}
	if parse("delete", "3", "7"); cli.Ctl.CommandSets.Delete.Set != 7 {
		t.Errorf("delete: %+v", cli.Ctl.CommandSets.Delete)
	}
	if parse("import", "3", "--from", "2", "5", "6=web, copy"); cli.Ctl.CommandSets.Import.From != 2 ||
		len(cli.Ctl.CommandSets.Import.Items) != 2 || cli.Ctl.CommandSets.Import.Items[1] != "6=web, copy" {
		t.Errorf("import: %+v", cli.Ctl.CommandSets.Import)
	}
	if parse("run", "3", "7"); cli.Ctl.CommandSets.Run.Set != 7 {
		t.Errorf("run: %+v", cli.Ctl.CommandSets.Run)
	}
	if parse("discard", "3", "11"); cli.Ctl.CommandSets.Discard.Request != 11 {
		t.Errorf("discard: %+v", cli.Ctl.CommandSets.Discard)
	}
	if parse("runs", "3", "7"); cli.Ctl.CommandSets.Runs.Limit != 50 || cli.Ctl.CommandSets.Runs.Before != 0 {
		t.Errorf("runs defaults: %+v", cli.Ctl.CommandSets.Runs)
	}
	if parse("runs", "3", "7", "--limit", "10", "--before", "99"); cli.Ctl.CommandSets.Runs.Limit != 10 || cli.Ctl.CommandSets.Runs.Before != 99 {
		t.Errorf("runs: %+v", cli.Ctl.CommandSets.Runs)
	}
	if parse("show-run", "3", "42"); cli.Ctl.CommandSets.ShowRun.RunID != 42 {
		t.Errorf("show-run: %+v", cli.Ctl.CommandSets.ShowRun)
	}

	// Missing or malformed arguments are refused by the parser.
	for _, args := range [][]string{
		{"create", "3"},                // --file is required
		{"update", "3", "--file", "f"}, // set id missing
		{"import", "3", "5"},           // --from is required
		{"import", "3", "--from", "2"}, // no item
		{"show", "x", "1"},             // not a number
		{"runs", "3"},                  // set id missing
	} {
		var fresh struct {
			Ctl CtlCmd `cmd:""`
		}
		p, err := kong.New(&fresh)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := p.Parse(append([]string{"ctl", "command-sets"}, args...)); err == nil {
			t.Errorf("%v accepted", args)
		}
	}
}

func TestParseImportItems(t *testing.T) {
	got, err := parseImportItems([]string{"5", " 6 =web=2", "7=  spaced name "})
	if err != nil {
		t.Fatal(err)
	}
	want := []map[string]any{{"set_id": int64(5)}, {"set_id": int64(6), "name": "web=2"}, {"set_id": int64(7), "name": "  spaced name "}}
	gj, _ := json.Marshal(got)
	wj, _ := json.Marshal(want)
	if !bytes.Equal(gj, wj) {
		t.Errorf("items = %s, want %s", gj, wj)
	}
	for _, bad := range []string{"", "x", "0", "-1", "5=", "5=  ", "=name", "1.5"} {
		_, err := parseImportItems([]string{bad})
		var ee *ExitError
		if err == nil || !errors.As(err, &ee) || ee.Code != ExitArgument {
			t.Errorf("%q: %v", bad, err)
		}
	}
}

func TestCommandRunsQuery(t *testing.T) {
	cases := []struct {
		limit  int
		before int64
		want   string
	}{
		{50, 0, "limit=50"},
		{0, 0, "limit=1"},
		{1000, 0, "limit=500"},
		{10, 99, "before=99&limit=10"},
		{10, -1, "limit=10"},
	}
	for _, c := range cases {
		if got := commandRunsQuery(c.limit, c.before); got != c.want {
			t.Errorf("(%d, %d) = %q, want %q", c.limit, c.before, got, c.want)
		}
	}
}

// A once time without an offset is read in the CLI's local time; any other
// value is sent as written.
func TestLocalizeScheduleAt(t *testing.T) {
	loc := time.FixedZone("JST", 9*3600)
	conv := func(in string) (map[string]any, error) {
		out, err := localizeScheduleAt(json.RawMessage(in), loc)
		if err != nil {
			return nil, err
		}
		var m map[string]any
		if err := json.Unmarshal(out, &m); err != nil {
			t.Fatal(err)
		}
		return m, nil
	}
	m, err := conv(`{"name":"n","schedule":{"kind":"once","at":"2026-12-01 03:00"}}`)
	if err != nil {
		t.Fatal(err)
	}
	if at := m["schedule"].(map[string]any)["at"]; at != "2026-12-01T03:00:00+09:00" {
		t.Errorf("local form: %v", at)
	}
	for _, in := range []string{
		`{"name":"n","schedule":{"kind":"once","at":"2026-12-01T03:00:00Z"}}`,
		`{"name":"n","schedule":null}`,
		`{"name":"n"}`,
		`{"name":"n","schedule":{"kind":"daily","time":"03:00"}}`,
		`{"name":"n","schedule":"odd"}`,
	} {
		out, err := localizeScheduleAt(json.RawMessage(in), loc)
		if err != nil || string(out) != in {
			t.Errorf("%s changed: %s %v", in, out, err)
		}
	}
	if _, err := conv(`{"schedule":{"kind":"once","at":"tomorrow"}}`); err == nil || !strings.Contains(err.Error(), "schedule.at") {
		t.Errorf("invalid time: %v", err)
	}
}

// export prints exactly the definition, which create and update accept.
func TestCommandSetDefinitionExport(t *testing.T) {
	saved := time.Local
	time.Local = time.FixedZone("JST", 9*3600)
	defer func() { time.Local = saved }()
	stored := map[string]any{
		"id": float64(4), "revision": float64(2), "name": "disk", "description": "d", "purpose": "collect_output", "os_user": "svc",
		"work_dir": "", "run_mode": "schedule", "schedule": map[string]any{"kind": "once", "at": "2026-12-01T03:00:00Z"},
		"keep_runs": float64(100), "joiner": "comma",
		"commands": []any{map[string]any{"command_line": "df -h", "note": "", "success": map[string]any{"kind": "single", "a": float64(0)},
			"failure": map[string]any{"kind": "min", "a": float64(1)}, "on_problems": "stop", "timeout_sec": float64(300), "line_limit": float64(20)}},
		"schedule_summary": "once", "imported_from": "", "created_at": "2026-11-01T00:00:00Z", "request": nil, "last_run": nil,
		"running": false, "agent_problems": []any{},
	}
	def, err := commandSetDefinition(stored)
	if err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{`"id"`, `"revision"`, "schedule_summary", "created_at", "agent_problems"} {
		if strings.Contains(def, key) {
			t.Errorf("export contains %s:\n%s", key, def)
		}
	}
	if !strings.Contains(def, `"at": "2026-12-01T12:00:00+09:00"`) || !strings.Contains(def, `"patterns": []`) {
		t.Errorf("export:\n%s", def)
	}
	spec, err := ParseCommandSetSpec([]byte(def))
	if err != nil {
		t.Fatalf("export is not accepted as a definition: %v", err)
	}
	if spec.Name != "disk" || len(spec.Commands) != 1 || spec.Schedule.At != "2026-12-01T03:00:00Z" {
		t.Errorf("round trip: %+v", spec)
	}
}

func TestCommandSetsTextOutput(t *testing.T) {
	saved := time.Local
	time.Local = time.FixedZone("JST", 9*3600)
	defer func() { time.Local = saved }()

	head := commandSetsHeader(map[string]any{"agent_id": float64(3), "agent_name": "web1", "default_os_user": "",
		"time_zone": "JST", "utc_offset_sec": float64(32400), "definitions_delivered": true})
	for _, want := range []string{"web1 (id 3)", "(not reported yet)", "JST, UTC+09:00", "delivered: yes"} {
		if !strings.Contains(head, want) {
			t.Errorf("header lacks %q:\n%s", want, head)
		}
	}

	display := map[string]any{"rows": []any{
		map[string]any{"name": "version", "values": []any{"1.2", "1.3"}, "value": "1.2,1.3"},
		map[string]any{"name": "", "values": []any{"a\nb"}, "value": "a\nb"},
	}, "truncated": true}
	if got := oneLineDisplay(display); got != "version: 1.2,1.3; a b" {
		t.Errorf("one line = %q", got)
	}
	if got := oneLineDisplay(nil); got != "" {
		t.Errorf("no display = %q", got)
	}

	run := map[string]any{"id": float64(9), "agent_id": float64(3), "agent_name": "web1", "set_id": float64(4), "set_name": "disk",
		"trigger": "schedule", "scheduled_for": "2026-11-11T03:00:00Z", "request_id": float64(0), "commands_total": float64(3),
		"started_at": "2026-11-11T03:00:05Z", "finished_at": "2026-11-11T03:00:09Z", "state": "error", "reason": "",
		"os_user": "svc", "work_dir": "", "display": display, "not_executed": float64(1),
		"commands": []any{
			map[string]any{"position": float64(0), "kind": "success", "reason": "", "exit_code": float64(0), "started_at": "2026-11-11T03:00:05Z",
				"finished_at": "2026-11-11T03:00:06Z", "output": "line 1\nline 2", "output_bytes": float64(13), "output_truncated": false,
				"command_line": "echo a &&\necho b", "note": "first"},
			map[string]any{"position": float64(1), "kind": "error", "reason": "timeout", "exit_code": nil, "started_at": "2026-11-11T03:00:06Z",
				"finished_at": "2026-11-11T03:00:09Z", "output": "", "output_bytes": float64(2000000), "output_truncated": true},
		}}
	var b bytes.Buffer
	renderCommandRun(&b, run)
	out := b.String()
	for _, want := range []string{
		"command set:   disk (id 4)", "trigger:       schedule, due 2026-11-11 12:00:00", "OS user:       svc", "work dir:      (home directory)",
		"started:       2026-11-11 12:00:05", "state:         error", "  version: 1.2,1.3", "  (truncated by the line or size limit)",
		"--- command 1 of 3: success, exit code 0", "command line:\n  echo a &&\n  echo b\n", "note: first",
		"console output (13 bytes):\nline 1\nline 2\n", "--- command 2 of 3: error (timeout), no exit code",
		"only the first 1048576 bytes were kept", "1 command(s) not executed",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("run output lacks %q:\n%s", want, out)
		}
	}
	// A viewer-shaped answer (no command line, OS user) prints without them.
	delete(run, "os_user")
	cmds := run["commands"].([]any)
	delete(cmds[0].(map[string]any), "command_line")
	b.Reset()
	renderCommandRun(&b, run)
	if strings.Contains(b.String(), "OS user") || strings.Contains(b.String(), "command line") {
		t.Errorf("fields printed although absent:\n%s", b.String())
	}
}

// The example definition of the command reference is valid.
func TestCommandSetReferenceExample(t *testing.T) {
	example := `{
  "name": "disk usage",
  "description": "Usage of the root file system",
  "purpose": "collect_output",
  "work_dir": "",
  "schedule": {"kind": "daily", "time": "03:00"},
  "commands": [
    {"command_line": "df -h /", "patterns": ["(?P<use>\\d+%)"]}
  ]
}`
	raw, err := localizeScheduleAt(json.RawMessage(example), time.Local)
	if err != nil {
		t.Fatal(err)
	}
	spec, err := ParseCommandSetSpec(raw)
	if err != nil {
		t.Fatal(err)
	}
	spec.OSUser = "svc" // filled in by the central from the agent
	if problems := spec.Validate(time.Now().Unix()); len(problems) > 0 {
		t.Fatalf("example: %v", problems)
	}
	if spec.RunMode != "schedule" || spec.KeepRuns != 100 || spec.Joiner != JoinerComma || spec.Commands[0].TimeoutSec != 300 || spec.Commands[0].LineLimit != 20 {
		t.Errorf("defaults: %+v", spec)
	}
}
