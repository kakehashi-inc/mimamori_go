package modules

import (
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"syscall"
	"text/tabwriter"
	"time"
)

// CtlCmd groups the management commands. They run on the central host and
// use the management API over the loopback address.
type CtlCmd struct {
	Config string `help:"Central configuration file (the address of the management API is taken from it)" default:"${central_config}" type:"path"`
	JSON   bool   `help:"Print raw JSON"`

	Agents       CtlAgentsCmd       `cmd:"" help:"Agents"`
	Tokens       CtlTokensCmd       `cmd:"" help:"Tokens"`
	Rules        CtlRulesCmd        `cmd:"" help:"Monitoring rules"`
	Templates    CtlTemplatesCmd    `cmd:"" help:"Templates"`
	Detections   CtlDetectionsCmd   `cmd:"" help:"Detections"`
	Suppressions CtlSuppressionsCmd `cmd:"" help:"Suppression conditions"`
	Accounts     CtlAccountsCmd     `cmd:"" help:"Accounts"`
	Certs        CtlCertsCmd        `cmd:"" help:"Certificate status"`
	History      CtlHistoryCmd      `cmd:"" help:"History"`
}

// ctlClient is the connected management API client.
type ctlClient struct {
	http *HTTPSClient
	json bool
}

func (c *CtlCmd) client() (*ctlClient, error) {
	// The management API is the central's own listener: its address comes
	// from the central configuration, its CA and the CLI secret from the
	// central's data directory. Whoever can read that directory can use the
	// CLI; it is not tied to an account.
	cc, _, err := LoadCentralConfig(c.Config)
	if err != nil {
		return nil, NewExitError(ExitConfig, err.Error())
	}
	_, port, err := net.SplitHostPort(cc.AgentListen)
	if err != nil {
		return nil, NewExitErrorf(ExitConfig, "agent_listen: %v", err)
	}
	caFile := filepath.Join(DataDir(), CABundleFileName)
	pemData, err := os.ReadFile(caFile)
	if err != nil {
		return nil, NewExitErrorf(ExitConfig, "CA certificate of the central: %v", err)
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(pemData) {
		return nil, NewExitErrorf(ExitConfig, "no certificate in %s", caFile)
	}
	hc, err := NewHTTPSClient("https://"+net.JoinHostPort("127.0.0.1", port), &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12}, 60*time.Second)
	if err != nil {
		return nil, NewExitError(ExitConfig, err.Error())
	}
	secret, err := os.ReadFile(filepath.Join(DataDir(), CLISecretFileName))
	if err != nil {
		return nil, NewExitErrorf(ExitAuth, "CLI secret of the central: %v (is the central running?)", err)
	}
	hc.Headers["Authorization"] = "Bearer " + strings.TrimSpace(string(secret))
	return &ctlClient{http: hc, json: c.JSON}, nil
}

func (cl *ctlClient) call(method, path string, in any) (any, error) {
	var out any
	if err := cl.http.JSON(method, "/admin/v1"+path, in, &out); err != nil {
		var he *HTTPError
		if errors.As(err, &he) {
			code := ExitGeneral
			switch he.Status {
			case 400, 404, 409:
				code = ExitArgument
			case 401, 403:
				code = ExitAuth
			}
			return nil, NewExitError(code, he.Error())
		}
		if errors.Is(err, syscall.ECONNREFUSED) {
			return nil, NewExitError(ExitExec, "the central is not running or not listening on "+cl.http.BaseURL.Host)
		}
		return nil, NewExitError(ExitExec, err.Error())
	}
	return out, nil
}

// show prints a value: a table when columns are given and --json is off.
func (cl *ctlClient) show(v any, columns ...string) {
	if cl.json {
		fmt.Println(marshalIndent(v))
		return
	}
	if len(columns) == 0 {
		fmt.Println(marshalIndent(localizeValue(v)))
		return
	}
	var rows []any
	switch t := v.(type) {
	case []any:
		rows = t
	case map[string]any:
		if items, ok := t["items"].([]any); ok {
			rows = items
			defer fmt.Printf("(%v total)\n", t["total"])
		} else {
			fmt.Println(marshalIndent(v))
			return
		}
	}
	tw := tabwriter.NewWriter(os.Stdout, 0, 2, 2, ' ', 0)
	fmt.Fprintln(tw, strings.ToUpper(strings.Join(columns, "\t")))
	for _, r := range rows {
		m, _ := r.(map[string]any)
		var cells []string
		for _, col := range columns {
			cells = append(cells, cell(lookup(m, col)))
		}
		fmt.Fprintln(tw, strings.Join(cells, "\t"))
	}
	tw.Flush()
}

// rfc3339Re finds RFC3339 timestamps (as returned by the API in UTC).
var rfc3339Re = regexp.MustCompile(`\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}(\.\d+)?(Z|[+-]\d{2}:\d{2})`)

// localTime renders an RFC3339 timestamp in local time.
func localTime(s string) string {
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		return s
	}
	return t.Local().Format("2006-01-02 15:04:05")
}

// localizeText replaces every RFC3339 timestamp inside a text with local time.
func localizeText(s string) string {
	return rfc3339Re.ReplaceAllStringFunc(s, localTime)
}

// localizeValue converts the timestamps of a decoded JSON value to local
// time for display (the --json output keeps the API's UTC values).
func localizeValue(v any) any {
	switch t := v.(type) {
	case map[string]any:
		out := make(map[string]any, len(t))
		for k, x := range t {
			out[k] = localizeValue(x)
		}
		return out
	case []any:
		out := make([]any, len(t))
		for i, x := range t {
			out[i] = localizeValue(x)
		}
		return out
	case string:
		return localizeText(t)
	}
	return v
}

func lookup(m map[string]any, path string) any {
	var cur any = m
	for _, p := range strings.Split(path, ".") {
		mm, ok := cur.(map[string]any)
		if !ok {
			return nil
		}
		cur = mm[p]
	}
	return cur
}

func cell(v any) string {
	switch t := v.(type) {
	case nil:
		return "-"
	case float64:
		return fmt.Sprintf("%v", t)
	case string:
		if t == "" {
			return "-"
		}
		t = localizeText(t)
		if len(t) > 60 {
			return t[:57] + "..."
		}
		return t
	case []any:
		var parts []string
		for _, x := range t {
			if m, ok := x.(map[string]any); ok {
				parts = append(parts, fmt.Sprintf("%v", m["display_name"]))
			} else {
				parts = append(parts, fmt.Sprint(x))
			}
		}
		return strings.Join(parts, ",")
	default:
		return fmt.Sprint(t)
	}
}

func runCtl(parent *CtlCmd, method, path string, in any, columns ...string) error {
	cl, err := parent.client()
	if err != nil {
		return err
	}
	out, err := cl.call(method, path, in)
	if err != nil {
		return err
	}
	cl.show(out, columns...)
	return nil
}

func readJSONFile(path string) (json.RawMessage, error) {
	var data []byte
	var err error
	if path == "-" {
		data, err = readAllStdin()
	} else {
		data, err = os.ReadFile(path)
	}
	if err != nil {
		return nil, NewExitError(ExitFileIO, err.Error())
	}
	if !json.Valid(data) {
		return nil, NewExitErrorf(ExitArgument, "%s is not valid JSON", path)
	}
	return data, nil
}

func readAllStdin() ([]byte, error) {
	return io.ReadAll(stdin)
}

// --- agents ---

type CtlAgentsCmd struct {
	List CtlAgentsListCmd `cmd:"" help:"List agents"`
	Show CtlAgentsShowCmd `cmd:"" help:"Show an agent with its host information"`
}
type CtlAgentsListCmd struct{}
type CtlAgentsShowCmd struct {
	ID int64 `arg:""`
}

func (c *CtlAgentsListCmd) Run(p *CtlCmd) error {
	return runCtl(p, "GET", "/agents", nil, "id", "display_name", "hostname", "token.status", "token.expires_at",
		"last_report_at", "rule.name", "rule.assigned_version", "rule.latest_version", "apply.status", "unacked_alerts", "ca_up_to_date")
}
func (c *CtlAgentsShowCmd) Run(p *CtlCmd) error {
	return runCtl(p, "GET", fmt.Sprintf("/agents/%d", c.ID), nil)
}

// --- tokens ---

type CtlTokensCmd struct {
	Issue   CtlTokensIssueCmd   `cmd:"" help:"Issue a token (the value is shown once)"`
	List    CtlTokensListCmd    `cmd:"" help:"List tokens"`
	Show    CtlTokensShowCmd    `cmd:"" help:"Show a token"`
	Reissue CtlTokensReissueCmd `cmd:"" help:"Reissue a token with the same name (previous values stop working)"`
	Unbind  CtlTokensIDCmd      `cmd:"" help:"Remove the binding to an agent instance"`
	Revoke  CtlTokensRevokeCmd  `cmd:"" help:"Revoke a token immediately"`
}
type CtlTokensIssueCmd struct {
	Name        string `arg:""`
	ValidDays   int    `name:"valid-days" help:"Validity in days (default: central setting)"`
	Rule        int64  `help:"Rule id to apply"`
	RuleVersion int    `name:"rule-version" help:"Rule version (default: latest)"`
}
type CtlTokensListCmd struct{}
type CtlTokensShowCmd struct {
	ID int64 `arg:""`
}
type CtlTokensReissueCmd struct {
	ID        int64 `arg:""`
	ValidDays int   `name:"valid-days"`
}
type CtlTokensIDCmd struct {
	ID int64 `arg:""`
}
type CtlTokensRevokeCmd struct {
	ID int64 `arg:""`
}

func printTokenValue(p *CtlCmd, out any) {
	m, _ := out.(map[string]any)
	if p.JSON {
		fmt.Println(marshalIndent(out))
		return
	}
	tok, _ := m["token"].(map[string]any)
	fmt.Printf("token name: %v\nexpires:    %v\nvalue:      %v\n", tok["name"], cell(tok["expires_at"]), m["value"])
	fmt.Println("\nThe value is shown only now. Initialize the agent on the target host with:")
	fmt.Printf("  printf '%%s\\n' '<value>' | sudo %s agent init --server https://<central>:%d\n", AppName, DefaultAgentPort)
}

func (c *CtlTokensIssueCmd) Run(p *CtlCmd) error {
	cl, err := p.client()
	if err != nil {
		return err
	}
	in := map[string]any{"name": c.Name}
	if c.ValidDays > 0 {
		in["valid_days"] = c.ValidDays
	}
	if c.Rule > 0 {
		in["rule_id"] = c.Rule
		if c.RuleVersion > 0 {
			in["rule_version"] = c.RuleVersion
		}
	}
	out, err := cl.call("POST", "/tokens", in)
	if err != nil {
		return err
	}
	printTokenValue(p, out)
	return nil
}
func (c *CtlTokensListCmd) Run(p *CtlCmd) error {
	return runCtl(p, "GET", "/tokens", nil, "id", "name", "status", "expires_at", "last_seen_at", "instance_id", "pending_renewal", "expiring_unconnected")
}
func (c *CtlTokensShowCmd) Run(p *CtlCmd) error {
	return runCtl(p, "GET", fmt.Sprintf("/tokens/%d", c.ID), nil)
}
func (c *CtlTokensReissueCmd) Run(p *CtlCmd) error {
	cl, err := p.client()
	if err != nil {
		return err
	}
	in := map[string]any{}
	if c.ValidDays > 0 {
		in["valid_days"] = c.ValidDays
	}
	out, err := cl.call("POST", fmt.Sprintf("/tokens/%d/reissue", c.ID), in)
	if err != nil {
		return err
	}
	printTokenValue(p, out)
	return nil
}
func (c *CtlTokensIDCmd) Run(p *CtlCmd) error {
	return runCtl(p, "POST", fmt.Sprintf("/tokens/%d/unbind", c.ID), nil)
}
func (c *CtlTokensRevokeCmd) Run(p *CtlCmd) error {
	return runCtl(p, "POST", fmt.Sprintf("/tokens/%d/revoke", c.ID), nil)
}

// --- rules ---

type CtlRulesCmd struct {
	List       CtlRulesListCmd       `cmd:"" help:"List rules"`
	Show       CtlRulesShowCmd       `cmd:"" help:"Show a rule (latest or given version)"`
	Create     CtlRulesCreateCmd     `cmd:"" help:"Create a rule from templates, from a JSON file or empty"`
	Export     CtlRulesExportCmd     `cmd:"" help:"Print the content of a version as JSON (for editing)"`
	Save       CtlRulesSaveCmd       `cmd:"" help:"Save edited content as a new version"`
	Edit       CtlRulesEditCmd       `cmd:"" help:"Edit the latest version with an editor (--editor) and save it as a new version"`
	History    CtlRulesHistoryCmd    `cmd:"" help:"List the versions of a rule"`
	Diff       CtlRulesDiffCmd       `cmd:"" help:"Show the difference between two versions"`
	Apply      CtlRulesApplyCmd      `cmd:"" help:"Apply a version to agents"`
	ToTemplate CtlRulesToTemplateCmd `cmd:"" name:"to-template" help:"Define an administrator template from a version"`
	Delete     CtlRulesDeleteCmd     `cmd:"" help:"Delete a rule (not possible while applied)"`
}
type CtlRulesListCmd struct{}
type CtlRulesShowCmd struct {
	ID      int64 `arg:""`
	Version int   `help:"Version (default: latest)"`
}
type CtlRulesCreateCmd struct {
	Name        string   `arg:""`
	Description string   `help:"Description"`
	Template    []string `help:"Template to expand (repeatable)"`
	Choice      []string `help:"Conflict choice FIELD=TEMPLATE (repeatable)"`
	File        string   `help:"Content JSON file ('-' for standard input)"`
}
type CtlRulesExportCmd struct {
	ID      int64 `arg:""`
	Version int   `help:"Version (default: latest)"`
}
type CtlRulesSaveCmd struct {
	ID          int64   `arg:""`
	File        string  `help:"Content JSON file ('-' for standard input)" required:""`
	Comment     string  `help:"Comment of the new version"`
	Description *string `help:"New description"`
}
type CtlRulesEditCmd struct {
	ID      int64  `arg:""`
	Comment string `help:"Comment of the new version"`
	Editor  string `help:"Editor command" default:"vi"`
}
type CtlRulesHistoryCmd struct {
	ID int64 `arg:""`
}
type CtlRulesDiffCmd struct {
	ID   int64 `arg:""`
	From int   `arg:""`
	To   int   `arg:""`
}
type CtlRulesApplyCmd struct {
	ID      int64   `arg:""`
	Agent   []int64 `help:"Agent id (repeatable)" required:""`
	Version int     `help:"Version (default: latest)"`
}
type CtlRulesToTemplateCmd struct {
	ID          int64  `arg:""`
	Name        string `arg:""`
	Version     int    `help:"Version (default: latest)"`
	Description string `help:"Description"`
}
type CtlRulesDeleteCmd struct {
	ID int64 `arg:""`
}

func (c *CtlRulesListCmd) Run(p *CtlCmd) error {
	return runCtl(p, "GET", "/rules", nil, "id", "name", "latest_version", "updated_at", "applied_agents", "description")
}

func ruleVersionPath(id int64, v int) string {
	if v > 0 {
		return fmt.Sprintf("/rules/%d/versions/%d", id, v)
	}
	return fmt.Sprintf("/rules/%d", id)
}

func (c *CtlRulesShowCmd) Run(p *CtlCmd) error {
	return runCtl(p, "GET", ruleVersionPath(c.ID, c.Version), nil)
}

func (c *CtlRulesCreateCmd) Run(p *CtlCmd) error {
	in := map[string]any{"name": c.Name, "description": c.Description}
	if len(c.Template) > 0 {
		in["templates"] = c.Template
		choices := map[string]string{}
		for _, ch := range c.Choice {
			k, v, ok := strings.Cut(ch, "=")
			if !ok {
				return NewExitErrorf(ExitArgument, "--choice must be FIELD=TEMPLATE: %q", ch)
			}
			choices[k] = v
		}
		in["choices"] = choices
	} else if c.File != "" {
		raw, err := readJSONFile(c.File)
		if err != nil {
			return err
		}
		in["content"] = raw
	}
	return runCtl(p, "POST", "/rules", in)
}

func (c *CtlRulesExportCmd) Run(p *CtlCmd) error {
	cl, err := p.client()
	if err != nil {
		return err
	}
	out, err := cl.call("GET", ruleVersionPath(c.ID, c.Version), nil)
	if err != nil {
		return err
	}
	m, _ := out.(map[string]any)
	fmt.Println(marshalIndent(m["content"]))
	return nil
}

func (c *CtlRulesSaveCmd) Run(p *CtlCmd) error {
	raw, err := readJSONFile(c.File)
	if err != nil {
		return err
	}
	in := map[string]any{"content": raw, "comment": c.Comment}
	if c.Description != nil {
		in["description"] = *c.Description
	}
	return runCtl(p, "PUT", fmt.Sprintf("/rules/%d", c.ID), in)
}

func (c *CtlRulesEditCmd) Run(p *CtlCmd) error {
	cl, err := p.client()
	if err != nil {
		return err
	}
	out, err := cl.call("GET", fmt.Sprintf("/rules/%d", c.ID), nil)
	if err != nil {
		return err
	}
	m, _ := out.(map[string]any)
	orig := marshalIndent(m["content"])
	// The temporary file is kept in the data directory (next to the
	// executable) and removed afterwards.
	f, err := os.CreateTemp(DataDir(), ".rule-edit-*.json")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	if _, err := f.WriteString(orig + "\n"); err != nil {
		return err
	}
	f.Close()
	path, err := LookupCommand(c.Editor)
	if err != nil {
		return NewExitError(ExitExec, err.Error())
	}
	proc, err := os.StartProcess(path, []string{path, f.Name()}, &os.ProcAttr{Files: []*os.File{os.Stdin, os.Stdout, os.Stderr}})
	if err != nil {
		return NewExitError(ExitExec, err.Error())
	}
	st, err := proc.Wait()
	if err != nil || !st.Success() {
		return NewExitError(ExitExec, "the editor failed; nothing was saved")
	}
	data, err := os.ReadFile(f.Name())
	if err != nil {
		return err
	}
	if strings.TrimSpace(string(data)) == strings.TrimSpace(orig) {
		fmt.Println("No changes; nothing was saved.")
		return nil
	}
	if !json.Valid(data) {
		return NewExitError(ExitArgument, "the edited content is not valid JSON; nothing was saved")
	}
	out, err = cl.call("PUT", fmt.Sprintf("/rules/%d", c.ID), map[string]any{"content": json.RawMessage(data), "comment": c.Comment})
	if err != nil {
		return err
	}
	m, _ = out.(map[string]any)
	fmt.Printf("Saved version %v.\n", m["latest_version"])
	return nil
}

func (c *CtlRulesHistoryCmd) Run(p *CtlCmd) error {
	cl, err := p.client()
	if err != nil {
		return err
	}
	out, err := cl.call("GET", fmt.Sprintf("/rules/%d", c.ID), nil)
	if err != nil {
		return err
	}
	m, _ := out.(map[string]any)
	cl.show(m["versions"], "version", "created_at", "created_by", "comment")
	return nil
}

func (c *CtlRulesDiffCmd) Run(p *CtlCmd) error {
	cl, err := p.client()
	if err != nil {
		return err
	}
	out, err := cl.call("GET", fmt.Sprintf("/rules/%d/diff?from=%d&to=%d", c.ID, c.From, c.To), nil)
	if err != nil {
		return err
	}
	if p.JSON {
		fmt.Println(marshalIndent(out))
		return nil
	}
	m, _ := out.(map[string]any)
	fmt.Printf("--- version %d\n+++ version %d\n", c.From, c.To)
	lines, _ := m["lines"].([]any)
	for _, l := range lines {
		lm, _ := l.(map[string]any)
		fmt.Printf("%v%v\n", lm["op"], lm["text"])
	}
	return nil
}

func (c *CtlRulesApplyCmd) Run(p *CtlCmd) error {
	return runCtl(p, "POST", fmt.Sprintf("/rules/%d/apply", c.ID), map[string]any{"version": c.Version, "agent_ids": c.Agent})
}

func (c *CtlRulesToTemplateCmd) Run(p *CtlCmd) error {
	return runCtl(p, "POST", fmt.Sprintf("/rules/%d/to-template", c.ID), map[string]any{"version": c.Version, "name": c.Name, "description": c.Description})
}

func (c *CtlRulesDeleteCmd) Run(p *CtlCmd) error {
	return runCtl(p, "DELETE", fmt.Sprintf("/rules/%d", c.ID), nil)
}

// --- templates ---

type CtlTemplatesCmd struct {
	List   CtlTemplatesListCmd   `cmd:"" help:"List templates"`
	Show   CtlTemplatesShowCmd   `cmd:"" help:"Show a template"`
	Create CtlTemplatesCreateCmd `cmd:"" help:"Create an administrator template"`
	Update CtlTemplatesUpdateCmd `cmd:"" help:"Change an administrator template"`
	Delete CtlTemplatesDeleteCmd `cmd:"" help:"Delete an administrator template"`
}
type CtlTemplatesListCmd struct{}
type CtlTemplatesShowCmd struct {
	Name string `arg:""`
}
type CtlTemplatesCreateCmd struct {
	Name        string `arg:""`
	Description string `help:"Description"`
	FromBuiltin string `name:"from-builtin" help:"Copy a built-in template"`
	FromRule    int64  `name:"from-rule" help:"Copy a rule version (with --version)"`
	Version     int    `help:"Rule version for --from-rule"`
	File        string `help:"Content JSON file ('-' for standard input)"`
}
type CtlTemplatesUpdateCmd struct {
	Name        string `arg:""`
	File        string `help:"Content JSON file ('-' for standard input)" required:""`
	Description string `help:"Description"`
}
type CtlTemplatesDeleteCmd struct {
	Name string `arg:""`
}

func (c *CtlTemplatesListCmd) Run(p *CtlCmd) error {
	return runCtl(p, "GET", "/templates", nil, "name", "builtin", "updated_at", "description")
}
func (c *CtlTemplatesShowCmd) Run(p *CtlCmd) error {
	return runCtl(p, "GET", "/templates/"+url.PathEscape(c.Name), nil)
}
func (c *CtlTemplatesCreateCmd) Run(p *CtlCmd) error {
	in := map[string]any{"name": c.Name, "description": c.Description}
	switch {
	case c.FromBuiltin != "":
		in["from_builtin"] = c.FromBuiltin
	case c.FromRule > 0:
		in["from_rule"] = map[string]any{"rule_id": c.FromRule, "version": c.Version}
	case c.File != "":
		raw, err := readJSONFile(c.File)
		if err != nil {
			return err
		}
		in["content"] = raw
	}
	return runCtl(p, "POST", "/templates", in)
}
func (c *CtlTemplatesUpdateCmd) Run(p *CtlCmd) error {
	raw, err := readJSONFile(c.File)
	if err != nil {
		return err
	}
	return runCtl(p, "PUT", "/templates/"+url.PathEscape(c.Name), map[string]any{"description": c.Description, "content": raw})
}
func (c *CtlTemplatesDeleteCmd) Run(p *CtlCmd) error {
	return runCtl(p, "DELETE", "/templates/"+url.PathEscape(c.Name), nil)
}

// --- detections ---

type CtlDetectionsCmd struct {
	List CtlDetectionsListCmd `cmd:"" help:"List detections"`
	Show CtlDetectionsShowCmd `cmd:"" help:"Show a detection"`
	Ack  CtlDetectionsAckCmd  `cmd:"" help:"Mark detections as acknowledged (AGENT_ID:ID ...)"`
}
type CtlDetectionsListCmd struct {
	Agent        int64  `help:"Agent id"`
	Category     string `help:"alert or normal" enum:",alert,normal" default:""`
	Type         string `help:"exec, denied, read, network, writable or exec_file"`
	From         string `help:"From (RFC3339 or 2006-01-02T15:04)"`
	To           string `help:"To (RFC3339 or 2006-01-02T15:04)"`
	Acknowledged string `help:"true or false" enum:",true,false" default:""`
	Suppressed   string `help:"true or false" enum:",true,false" default:""`
	Limit        int    `help:"Maximum rows" default:"100"`
	Offset       int    `help:"Rows to skip"`
}
type CtlDetectionsShowCmd struct {
	Agent int64 `arg:""`
	ID    int64 `arg:""`
}
type CtlDetectionsAckCmd struct {
	Items []string `arg:"" help:"AGENT_ID:ID"`
}

func (c *CtlDetectionsListCmd) Run(p *CtlCmd) error {
	q := url.Values{}
	if c.Agent > 0 {
		q.Set("agent_id", fmt.Sprint(c.Agent))
	}
	for k, v := range map[string]string{"category": c.Category, "type": c.Type, "from": c.From, "to": c.To, "acknowledged": c.Acknowledged, "suppressed": c.Suppressed} {
		if v != "" {
			q.Set(k, v)
		}
	}
	q.Set("limit", fmt.Sprint(c.Limit))
	q.Set("offset", fmt.Sprint(c.Offset))
	return runCtl(p, "GET", "/detections?"+q.Encode(), nil, "agent_id", "id", "last_at", "agent_name", "category", "type", "os_user", "exe", "target", "count", "acknowledged_at", "suppressed")
}
func (c *CtlDetectionsShowCmd) Run(p *CtlCmd) error {
	return runCtl(p, "GET", fmt.Sprintf("/detections/%d/%d", c.Agent, c.ID), nil)
}
func (c *CtlDetectionsAckCmd) Run(p *CtlCmd) error {
	var items []map[string]int64
	for _, s := range c.Items {
		var a, id int64
		if _, err := fmt.Sscanf(s, "%d:%d", &a, &id); err != nil {
			return NewExitErrorf(ExitArgument, "expected AGENT_ID:ID, got %q", s)
		}
		items = append(items, map[string]int64{"agent_id": a, "id": id})
	}
	return runCtl(p, "POST", "/detections/ack", map[string]any{"items": items})
}

// --- suppressions ---

type CtlSuppressionsCmd struct {
	List   CtlSuppressionsListCmd   `cmd:"" help:"List suppression conditions"`
	Create CtlSuppressionsCreateCmd `cmd:"" help:"Create a suppression condition (empty fields match anything; exe and target accept *)"`
	Delete CtlSuppressionsDeleteCmd `cmd:"" help:"Delete a suppression condition"`
}
type CtlSuppressionsListCmd struct{}
type CtlSuppressionsCreateCmd struct {
	Agent   int64  `help:"Only this agent id (default: all agents)"`
	Type    string `help:"Detection type"`
	User    string `help:"OS user"`
	Exe     string `help:"Executable (glob with *)"`
	Target  string `help:"Target (glob with *)"`
	Comment string `help:"Comment"`
}
type CtlSuppressionsDeleteCmd struct {
	ID int64 `arg:""`
}

func (c *CtlSuppressionsListCmd) Run(p *CtlCmd) error {
	return runCtl(p, "GET", "/suppressions", nil, "id", "agent_name", "type", "os_user", "exe", "target", "comment", "created_by")
}
func (c *CtlSuppressionsCreateCmd) Run(p *CtlCmd) error {
	in := map[string]any{"type": c.Type, "os_user": c.User, "exe": c.Exe, "target": c.Target, "comment": c.Comment}
	if c.Agent > 0 {
		in["agent_id"] = c.Agent
	}
	return runCtl(p, "POST", "/suppressions", in)
}
func (c *CtlSuppressionsDeleteCmd) Run(p *CtlCmd) error {
	return runCtl(p, "DELETE", fmt.Sprintf("/suppressions/%d", c.ID), nil)
}

// --- accounts ---

type CtlAccountsCmd struct {
	List     CtlAccountsListCmd     `cmd:"" help:"List accounts"`
	Create   CtlAccountsCreateCmd   `cmd:"" help:"Create an account (password from standard input)"`
	Role     CtlAccountsRoleCmd     `cmd:"" help:"Change the role"`
	Password CtlAccountsPasswordCmd `cmd:"" help:"Reset the password (from standard input)"`
	Disable  CtlAccountsIDCmd       `cmd:"" help:"Disable an account"`
	Enable   CtlAccountsEnableCmd   `cmd:"" help:"Enable an account"`
	Delete   CtlAccountsDeleteCmd   `cmd:"" help:"Delete an account"`
}
type CtlAccountsListCmd struct{}
type CtlAccountsCreateCmd struct {
	Name     string `arg:""`
	Role     string `help:"admin or viewer" enum:"admin,viewer" required:""`
	Language string `help:"Display language of the browser screens: auto (follow the browser), en or ja" enum:"auto,en,ja" default:"auto"`
}
type CtlAccountsRoleCmd struct {
	ID   int64  `arg:""`
	Role string `arg:"" enum:"admin,viewer"`
}
type CtlAccountsPasswordCmd struct {
	ID int64 `arg:""`
}
type CtlAccountsIDCmd struct {
	ID int64 `arg:""`
}
type CtlAccountsEnableCmd struct {
	ID int64 `arg:""`
}
type CtlAccountsDeleteCmd struct {
	ID int64 `arg:""`
}

func (c *CtlAccountsListCmd) Run(p *CtlCmd) error {
	return runCtl(p, "GET", "/accounts", nil, "id", "name", "role", "language", "disabled", "locked_until", "created_at")
}
func (c *CtlAccountsCreateCmd) Run(p *CtlCmd) error {
	pw, err := readNewPassword()
	if err != nil {
		return err
	}
	return runCtl(p, "POST", "/accounts", map[string]any{"name": c.Name, "role": c.Role, "language": c.Language, "password": pw})
}
func (c *CtlAccountsRoleCmd) Run(p *CtlCmd) error {
	return runCtl(p, "PUT", fmt.Sprintf("/accounts/%d/role", c.ID), map[string]any{"role": c.Role})
}
func (c *CtlAccountsPasswordCmd) Run(p *CtlCmd) error {
	pw, err := readNewPassword()
	if err != nil {
		return err
	}
	return runCtl(p, "PUT", fmt.Sprintf("/accounts/%d/password", c.ID), map[string]any{"password": pw})
}
func (c *CtlAccountsIDCmd) Run(p *CtlCmd) error {
	return runCtl(p, "POST", fmt.Sprintf("/accounts/%d/disable", c.ID), nil)
}
func (c *CtlAccountsEnableCmd) Run(p *CtlCmd) error {
	return runCtl(p, "POST", fmt.Sprintf("/accounts/%d/enable", c.ID), nil)
}
func (c *CtlAccountsDeleteCmd) Run(p *CtlCmd) error {
	return runCtl(p, "DELETE", fmt.Sprintf("/accounts/%d", c.ID), nil)
}

// --- certificates, history ---

type CtlCertsCmd struct {
	Status CtlCertsStatusCmd `cmd:"" help:"Show the CA and server certificate status"`
}
type CtlCertsStatusCmd struct{}

func (c *CtlCertsStatusCmd) Run(p *CtlCmd) error { return runCtl(p, "GET", "/certificates", nil) }

type CtlHistoryCmd struct {
	Category string `help:"account, rule, token, certificate, rejected, template, suppression or detection"`
	Limit    int    `help:"Maximum rows" default:"50"`
	Offset   int    `help:"Rows to skip"`
}

func (c *CtlHistoryCmd) Run(p *CtlCmd) error {
	q := url.Values{}
	q.Set("limit", fmt.Sprint(c.Limit))
	q.Set("offset", fmt.Sprint(c.Offset))
	if c.Category != "" {
		q.Set("category", c.Category)
	}
	return runCtl(p, "GET", "/history?"+q.Encode(), nil, "at", "account_name", "category", "action", "target", "detail", "remote_addr", "count")
}
