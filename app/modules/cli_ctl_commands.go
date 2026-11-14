package modules

import (
	"encoding/json"
	"fmt"
	"io"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"
)

// --- command sets ---

// CtlCommandSetsCmd groups the command set commands: definitions, run
// requests and results of one agent. The management API never asks for a
// re-authentication code.
type CtlCommandSetsCmd struct {
	List    CtlCommandSetsListCmd    `cmd:"" help:"List the command sets of an agent"`
	Show    CtlCommandSetsShowCmd    `cmd:"" help:"Show a command set with its definition and state"`
	Export  CtlCommandSetsExportCmd  `cmd:"" help:"Print the definition of a command set as JSON (the shape create and update read)"`
	Create  CtlCommandSetsCreateCmd  `cmd:"" help:"Create a command set from a JSON definition file"`
	Update  CtlCommandSetsUpdateCmd  `cmd:"" help:"Replace the definition of a command set with a JSON definition file"`
	Delete  CtlCommandSetsDeleteCmd  `cmd:"" help:"Delete a command set (its runs are kept until the retention removes them)"`
	Import  CtlCommandSetsImportCmd  `cmd:"" help:"Copy command sets of an agent (or of the same agent) into an agent"`
	Run     CtlCommandSetsRunCmd     `cmd:"" help:"Request a run of a command set (it waits for a free worker of the central and goes to the agent with its next sync)"`
	Discard CtlCommandSetsDiscardCmd `cmd:"" help:"Discard a run request that was not yet handed to the agent"`
	Runs    CtlCommandSetsRunsCmd    `cmd:"" help:"List the runs of a command set, newest first"`
	ShowRun CtlCommandSetsShowRunCmd `cmd:"" name:"show-run" help:"Show a run with every command's result and console output"`
}

type CtlCommandSetsListCmd struct {
	Agent int64 `arg:"" help:"Agent id"`
}
type CtlCommandSetsShowCmd struct {
	Agent int64 `arg:"" help:"Agent id"`
	Set   int64 `arg:"" help:"Command set id"`
}
type CtlCommandSetsExportCmd struct {
	Agent int64 `arg:"" help:"Agent id"`
	Set   int64 `arg:"" help:"Command set id"`
}
type CtlCommandSetsDeleteCmd struct {
	Agent int64 `arg:"" help:"Agent id"`
	Set   int64 `arg:"" help:"Command set id"`
}
type CtlCommandSetsRunCmd struct {
	Agent int64 `arg:"" help:"Agent id"`
	Set   int64 `arg:"" help:"Command set id"`
}
type CtlCommandSetsCreateCmd struct {
	Agent int64  `arg:"" help:"Agent id"`
	File  string `help:"Definition JSON file ('-' for standard input)" required:""`
}
type CtlCommandSetsUpdateCmd struct {
	Agent int64  `arg:"" help:"Agent id"`
	Set   int64  `arg:"" help:"Command set id"`
	File  string `help:"Definition JSON file ('-' for standard input)" required:""`
}
type CtlCommandSetsImportCmd struct {
	Agent int64    `arg:"" help:"Agent id (destination)"`
	Items []string `arg:"" help:"SET_ID or SET_ID=NEW_NAME of the source agent"`
	From  int64    `help:"Source agent id" required:""`
}
type CtlCommandSetsDiscardCmd struct {
	Agent   int64 `arg:"" help:"Agent id"`
	Request int64 `arg:"" help:"Run request id"`
}
type CtlCommandSetsRunsCmd struct {
	Agent  int64 `arg:"" help:"Agent id"`
	Set    int64 `arg:"" help:"Command set id"`
	Limit  int   `help:"Maximum rows (1 to 500)" default:"50"`
	Before int64 `help:"List only runs older than this run id (the value printed after the table)"`
}
type CtlCommandSetsShowRunCmd struct {
	Agent int64 `arg:"" help:"Agent id"`
	RunID int64 `arg:"" name:"run" help:"Run id"`
}

func commandSetsPath(agent int64) string {
	return fmt.Sprintf("/agents/%d/command-sets", agent)
}

func (c *CtlCommandSetsListCmd) Run(p *CtlCmd) error {
	cl, err := p.client()
	if err != nil {
		return err
	}
	out, err := cl.call("GET", commandSetsPath(c.Agent), nil)
	if err != nil {
		return err
	}
	if p.JSON {
		cl.show(out)
		return nil
	}
	m, _ := out.(map[string]any)
	fmt.Print(commandSetsHeader(m))
	cl.show(m["sets"], "id", "name", "purpose", "schedule_summary", "os_user", "running", "request.state", "last_run.state", "last_run.finished_at")
	return nil
}

// commandSetsHeader is the text printed above the set table: the agent,
// the OS user new sets default to, the host's time zone, whether the agent
// holds the current definitions and whether it is responding (its run
// requests take no worker of the central while it is not).
func commandSetsHeader(m map[string]any) string {
	user, _ := m["default_os_user"].(string)
	if user == "" {
		user = "(not reported yet)"
	}
	zone, _ := m["time_zone"].(string)
	if zone == "" {
		zone = "(not reported yet)"
	} else if off, ok := m["utc_offset_sec"].(float64); ok {
		zone += ", " + FormatUTCOffset(int(off))
	}
	delivered := "no (the agent receives them with its next sync)"
	if m["definitions_delivered"] == true {
		delivered = "yes"
	}
	responding := "no (its run requests wait without a worker of the central until it syncs)"
	if m["agent_responding"] == true {
		responding = "yes"
	}
	return fmt.Sprintf("agent:                 %v (id %s)\ndefault OS user:       %s\nhost time zone:        %s\ndefinitions delivered: %s\nagent responding:      %s\n\n",
		m["agent_name"], cell("agent_id", m["agent_id"], true), user, zone, delivered, responding)
}

func (c *CtlCommandSetsShowCmd) Run(p *CtlCmd) error {
	return runCtl(p, "GET", fmt.Sprintf("%s/%d", commandSetsPath(c.Agent), c.Set), nil)
}

// Run of export prints only the definition, with a once time in local time
// (RFC 3339 with the local offset), so that the output can be edited and
// given to update. It is JSON with or without --json.
func (c *CtlCommandSetsExportCmd) Run(p *CtlCmd) error {
	cl, err := p.client()
	if err != nil {
		return err
	}
	out, err := cl.call("GET", fmt.Sprintf("%s/%d", commandSetsPath(c.Agent), c.Set), nil)
	if err != nil {
		return err
	}
	def, err := commandSetDefinition(out)
	if err != nil {
		return NewExitError(ExitGeneral, err.Error())
	}
	fmt.Println(def)
	return nil
}

// commandSetDefinition extracts the definition from a stored set as the API
// returns it and renders it as indented JSON.
func commandSetDefinition(out any) (string, error) {
	data, err := json.Marshal(out)
	if err != nil {
		return "", err
	}
	var spec CommandSetSpec
	if err := json.Unmarshal(data, &spec); err != nil {
		return "", err
	}
	if spec.Commands == nil {
		spec.Commands = []CommandSpec{}
	}
	for i := range spec.Commands {
		if spec.Commands[i].Patterns == nil {
			spec.Commands[i].Patterns = []string{}
		}
	}
	if spec.Schedule != nil && spec.Schedule.At != "" {
		if t, err := ParseScheduleAt(spec.Schedule.At, nil); err == nil {
			spec.Schedule.At = t.Local().Format(time.RFC3339)
		}
	}
	return marshalIndent(spec), nil
}

// readCommandSetFile reads a definition file. The time of a once schedule
// may be written in local time without an offset ("2026-12-01 03:00"); it
// is sent with the local offset. Everything else is sent as written, and
// the central checks it.
func readCommandSetFile(path string) (json.RawMessage, error) {
	raw, err := readJSONFile(path)
	if err != nil {
		return nil, err
	}
	return localizeScheduleAt(raw, time.Local)
}

// localizeScheduleAt rewrites schedule.at of a definition from a local form
// without an offset to RFC 3339 in loc. Other values are kept.
func localizeScheduleAt(raw json.RawMessage, loc *time.Location) (json.RawMessage, error) {
	var top map[string]json.RawMessage
	if err := json.Unmarshal(raw, &top); err != nil {
		return nil, NewExitErrorf(ExitArgument, "the definition must be a JSON object: %v", err)
	}
	var sched map[string]json.RawMessage
	if s, ok := top["schedule"]; !ok || json.Unmarshal(s, &sched) != nil || sched == nil {
		return raw, nil
	}
	var at string
	if a, ok := sched["at"]; !ok || json.Unmarshal(a, &at) != nil || at == "" {
		return raw, nil
	}
	if _, err := time.Parse(time.RFC3339, strings.TrimSpace(at)); err == nil {
		return raw, nil
	}
	t, err := ParseScheduleAt(at, loc)
	if err != nil {
		return nil, NewExitErrorf(ExitArgument, "schedule.at: %v", err)
	}
	sched["at"], _ = json.Marshal(t.Format(time.RFC3339))
	top["schedule"], _ = json.Marshal(sched)
	out, err := json.Marshal(top)
	if err != nil {
		return nil, err
	}
	return out, nil
}

func (c *CtlCommandSetsCreateCmd) Run(p *CtlCmd) error {
	raw, err := readCommandSetFile(c.File)
	if err != nil {
		return err
	}
	return runCtl(p, "POST", commandSetsPath(c.Agent), raw)
}

func (c *CtlCommandSetsUpdateCmd) Run(p *CtlCmd) error {
	raw, err := readCommandSetFile(c.File)
	if err != nil {
		return err
	}
	return runCtl(p, "PUT", fmt.Sprintf("%s/%d", commandSetsPath(c.Agent), c.Set), raw)
}

func (c *CtlCommandSetsDeleteCmd) Run(p *CtlCmd) error {
	return runCtl(p, "DELETE", fmt.Sprintf("%s/%d", commandSetsPath(c.Agent), c.Set), nil)
}

func (c *CtlCommandSetsRunCmd) Run(p *CtlCmd) error {
	return runCtl(p, "POST", fmt.Sprintf("%s/%d/run", commandSetsPath(c.Agent), c.Set), nil)
}

// parseImportItems reads SET_ID or SET_ID=NEW_NAME arguments. The id must
// be a positive integer; a new name, when given, must not be empty.
func parseImportItems(args []string) ([]map[string]any, error) {
	items := make([]map[string]any, 0, len(args))
	for _, a := range args {
		idText, name, renamed := strings.Cut(a, "=")
		id, err := strconv.ParseInt(strings.TrimSpace(idText), 10, 64)
		if err != nil || id <= 0 {
			return nil, NewExitErrorf(ExitArgument, "expected SET_ID or SET_ID=NEW_NAME with a positive integer SET_ID, got %q", a)
		}
		item := map[string]any{"set_id": id}
		if renamed {
			if strings.TrimSpace(name) == "" {
				return nil, NewExitErrorf(ExitArgument, "the new name after = is empty in %q", a)
			}
			item["name"] = name
		}
		items = append(items, item)
	}
	return items, nil
}

func (c *CtlCommandSetsImportCmd) Run(p *CtlCmd) error {
	items, err := parseImportItems(c.Items)
	if err != nil {
		return err
	}
	cl, err := p.client()
	if err != nil {
		return err
	}
	out, err := cl.call("POST", commandSetsPath(c.Agent)+"/import", map[string]any{"source_agent_id": c.From, "items": items})
	if err != nil {
		return err
	}
	if p.JSON {
		cl.show(out)
		return nil
	}
	m, _ := out.(map[string]any)
	cl.show(m["sets"], "id", "name", "purpose", "schedule_summary", "os_user", "imported_from")
	return nil
}

func (c *CtlCommandSetsDiscardCmd) Run(p *CtlCmd) error {
	return runCtl(p, "DELETE", fmt.Sprintf("/agents/%d/command-requests/%d", c.Agent, c.Request), nil)
}

// commandRunsQuery builds the query of the run list. The limit is held
// between 1 and 500 like the other list commands; before 0 or less means
// from the newest run.
func commandRunsQuery(limit int, before int64) string {
	q := url.Values{}
	q.Set("limit", strconv.Itoa(min(max(limit, 1), 500)))
	if before > 0 {
		q.Set("before", strconv.FormatInt(before, 10))
	}
	return q.Encode()
}

func (c *CtlCommandSetsRunsCmd) Run(p *CtlCmd) error {
	cl, err := p.client()
	if err != nil {
		return err
	}
	out, err := cl.call("GET", fmt.Sprintf("%s/%d/runs?%s", commandSetsPath(c.Agent), c.Set, commandRunsQuery(c.Limit, c.Before)), nil)
	if err != nil {
		return err
	}
	if p.JSON {
		cl.show(out)
		return nil
	}
	m, _ := out.(map[string]any)
	runs, _ := m["runs"].([]any)
	for _, r := range runs {
		if rm, ok := r.(map[string]any); ok {
			rm["values"] = oneLineDisplay(rm["display"])
		}
	}
	deleted := ""
	if m["deleted"] == true {
		deleted = " (deleted)"
	}
	fmt.Printf("%v / %v%s\n\n", m["agent_name"], m["set_name"], deleted)
	cl.show(runs, "id", "trigger", "scheduled_for", "started_at", "finished_at", "state", "reason", "values")
	if next, ok := m["next_before"].(float64); ok {
		fmt.Printf("(older runs: --before %s)\n", formatNumber(next))
	}
	return nil
}

// oneLineDisplay renders a display value for a table cell: rows as
// "name: value" separated by "; ", line breaks inside values as spaces.
func oneLineDisplay(v any) string {
	dv, ok := decodeDisplay(v)
	if !ok {
		return ""
	}
	parts := make([]string, 0, len(dv.Rows))
	for _, r := range dv.Rows {
		val := strings.Join(strings.Fields(strings.ReplaceAll(r.Value, "\n", " ")), " ")
		if r.Name == "" {
			parts = append(parts, val)
		} else {
			parts = append(parts, r.Name+": "+val)
		}
	}
	return strings.Join(parts, "; ")
}

func decodeDisplay(v any) (DisplayValue, bool) {
	var dv DisplayValue
	if v == nil {
		return dv, false
	}
	data, err := json.Marshal(v)
	if err != nil || json.Unmarshal(data, &dv) != nil {
		return dv, false
	}
	return dv, true
}

func (c *CtlCommandSetsShowRunCmd) Run(p *CtlCmd) error {
	cl, err := p.client()
	if err != nil {
		return err
	}
	out, err := cl.call("GET", fmt.Sprintf("/agents/%d/command-runs/%d", c.Agent, c.RunID), nil)
	if err != nil {
		return err
	}
	if p.JSON {
		cl.show(out)
		return nil
	}
	m, _ := out.(map[string]any)
	renderCommandRun(os.Stdout, m)
	return nil
}

// renderCommandRun prints a run as text: its summary, its display value and
// every executed command with its result and console output. Times are in
// local time.
func renderCommandRun(w io.Writer, m map[string]any) {
	text := func(key string) string { return cell(key, m[key], true) }
	fmt.Fprintf(w, "run:           %s\n", text("id"))
	fmt.Fprintf(w, "agent:         %s (id %s)\n", text("agent_name"), text("agent_id"))
	fmt.Fprintf(w, "command set:   %s (id %s)\n", text("set_name"), text("set_id"))
	trigger := text("trigger")
	if m["trigger"] == TriggerSchedule {
		trigger += ", due " + text("scheduled_for")
	} else if id, ok := m["request_id"].(float64); ok && id > 0 {
		trigger += ", request " + formatNumber(id)
	}
	fmt.Fprintf(w, "trigger:       %s\n", trigger)
	if user, ok := m["os_user"].(string); ok {
		dir, _ := m["work_dir"].(string)
		if dir == "" {
			dir = "(home directory)"
		}
		fmt.Fprintf(w, "OS user:       %s\nwork dir:      %s\n", user, dir)
	}
	fmt.Fprintf(w, "started:       %s\n", text("started_at"))
	fmt.Fprintf(w, "finished:      %s\n", text("finished_at"))
	state := text("state")
	if reason, _ := m["reason"].(string); reason != "" {
		state += " (" + reason + ")"
	}
	fmt.Fprintf(w, "state:         %s\n", state)
	if dv, ok := decodeDisplay(m["display"]); ok {
		fmt.Fprintf(w, "\nDisplay value:\n")
		for _, l := range strings.Split(strings.TrimSuffix(RenderDisplayValue(dv), "\n"), "\n") {
			fmt.Fprintf(w, "  %s\n", l)
		}
	}
	total, _ := m["commands_total"].(float64)
	cmds, _ := m["commands"].([]any)
	for _, x := range cmds {
		cm, _ := x.(map[string]any)
		ctext := func(key string) string { return cell(key, cm[key], true) }
		pos, _ := cm["position"].(float64)
		head := fmt.Sprintf("command %d of %s: %s", int(pos)+1, formatNumber(total), ctext("kind"))
		if reason, _ := cm["reason"].(string); reason != "" {
			head += " (" + reason + ")"
		}
		if code, ok := cm["exit_code"].(float64); ok {
			head += ", exit code " + formatNumber(code)
		} else {
			head += ", no exit code"
		}
		fmt.Fprintf(w, "\n--- %s\n", head)
		fmt.Fprintf(w, "started %s, finished %s\n", ctext("started_at"), ctext("finished_at"))
		if line, ok := cm["command_line"].(string); ok {
			fmt.Fprintf(w, "command line:\n")
			for _, l := range strings.Split(line, "\n") {
				fmt.Fprintf(w, "  %s\n", l)
			}
		}
		if note, _ := cm["note"].(string); note != "" {
			fmt.Fprintf(w, "note: %s\n", note)
		}
		size, _ := cm["output_bytes"].(float64)
		fmt.Fprintf(w, "console output (%s bytes", formatNumber(size))
		if cm["output_truncated"] == true {
			fmt.Fprintf(w, ", only the first %d bytes were kept", MaxCommandOutputBytes)
		}
		fmt.Fprintf(w, "):\n")
		if out, _ := cm["output"].(string); out != "" {
			fmt.Fprint(w, out)
			if !strings.HasSuffix(out, "\n") {
				fmt.Fprintln(w)
			}
		}
	}
	if n, _ := m["not_executed"].(float64); n > 0 {
		fmt.Fprintf(w, "\n%s command(s) not executed (the set stopped)\n", formatNumber(n))
	}
}
