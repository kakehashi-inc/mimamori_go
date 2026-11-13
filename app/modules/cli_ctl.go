package modules

import (
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"syscall"
	"text/tabwriter"
	"time"
	"unicode/utf8"

	"mimamori/app/models"
)

// CtlCmd groups the management commands. They run on the central host and
// use the management API on the address of agent_listen (see CtlTarget).
type CtlCmd struct {
	JSON bool `help:"Print raw JSON (rules export and command-sets export always print JSON; rules edit prints its messages only)"`

	Agents       CtlAgentsCmd       `cmd:"" help:"Agents"`
	Tokens       CtlTokensCmd       `cmd:"" help:"Tokens"`
	Rules        CtlRulesCmd        `cmd:"" help:"Monitoring rules"`
	Templates    CtlTemplatesCmd    `cmd:"" help:"Templates"`
	Detections   CtlDetectionsCmd   `cmd:"" help:"Detections"`
	Suppressions CtlSuppressionsCmd `cmd:"" help:"Suppression conditions"`
	CommandSets  CtlCommandSetsCmd  `cmd:"" name:"command-sets" help:"Command sets of agents: definitions, run requests and results"`
	Accounts     CtlAccountsCmd     `cmd:"" help:"Accounts"`
	Certs        CtlCertsCmd        `cmd:"" help:"Certificate status"`
	History      CtlHistoryCmd      `cmd:"" help:"History"`
}

// ctlClient is the connected management API client.
type ctlClient struct {
	http *HTTPSClient
	json bool
	// full prints every cell in full (long text is shortened otherwise).
	full bool
	// port is the port of the central's agent_listen.
	port int
}

func (c *CtlCmd) client() (*ctlClient, error) {
	// The management API is the central's own listener: its port comes from
	// the central's configuration (agent_listen), the CA and the secret the
	// central writes at every start from its data directory. Whoever can
	// read that directory can use the CLI; it is not tied to an account.
	dir := DataDir()
	dbPath := filepath.Join(dir, CentralDBFileName)
	if _, err := os.Stat(dbPath); err != nil {
		return nil, NewExitErrorf(ExitConfig, "central database: %v", err)
	}
	db, err := OpenSQLite(dbPath, SchemaCentral)
	if err != nil {
		return nil, NewExitErrorf(ExitConfig, "central database: %v", err)
	}
	cfg, err := LoadCentralConfig(db)
	db.Close()
	if err != nil {
		return nil, NewExitError(ExitConfig, err.Error())
	}
	host, portNum, err := CtlTarget(cfg.AgentListen)
	if err != nil {
		return nil, NewExitErrorf(ExitConfig, "agent_listen: %v", err)
	}
	caFile := filepath.Join(dir, CABundleFileName)
	pemData, err := os.ReadFile(caFile)
	if err != nil {
		return nil, NewExitErrorf(ExitFileIO, "cannot read the CA certificate of the central: %v (the central writes it when it starts)", err)
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(pemData) {
		return nil, NewExitErrorf(ExitFileIO, "no certificate in %s (the central writes it when it starts)", caFile)
	}
	// The server certificate always contains the loopback addresses but not
	// necessarily the listen address, so it is verified under the IPv4
	// loopback address whatever address is connected to.
	hc, err := NewHTTPSClient(host, portNum, &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12, ServerName: ctlServerName}, 60*time.Second)
	if err != nil {
		return nil, NewExitError(ExitConfig, err.Error())
	}
	secret, err := os.ReadFile(filepath.Join(dir, CLISecretFileName))
	if err != nil {
		return nil, NewExitErrorf(ExitFileIO, "cannot read the CLI secret of the central: %v (the central writes it each time it starts)", err)
	}
	hc.Headers["Authorization"] = "Bearer " + strings.TrimSpace(string(secret))
	return &ctlClient{http: hc, json: c.JSON, port: portNum}, nil
}

// ctlServerName is the name ctl verifies the server certificate under: a
// name every server certificate of the central contains (ServerCertNames).
const ctlServerName = "127.0.0.1"

// CtlTarget returns the address and port ctl connects to for the central's
// agent_listen: the loopback address when the central listens on every
// address (127.0.0.1 for an empty host or 0.0.0.0, ::1 for ::), otherwise
// the listen IP itself.
func CtlTarget(agentListen string) (string, int, error) {
	host, port, err := net.SplitHostPort(agentListen)
	if err != nil {
		return "", 0, err
	}
	portNum, err := strconv.Atoi(port)
	if err != nil || portNum <= 0 || portNum > 65535 {
		return "", 0, fmt.Errorf("%s: invalid port", agentListen)
	}
	if host == "" {
		return "127.0.0.1", portNum, nil
	}
	ip := net.ParseIP(host)
	if ip == nil {
		return "", 0, fmt.Errorf("%s: the listen address must be an IP address", agentListen)
	}
	if ip.IsUnspecified() {
		// An IPv6 wildcard socket takes IPv4 connections only when the
		// system allows it (net.ipv6.bindv6only), so ::1 is used for it.
		if ip.To4() == nil {
			return "::1", portNum, nil
		}
		return "127.0.0.1", portNum, nil
	}
	return ip.String(), portNum, nil
}

func (cl *ctlClient) call(method, path string, in any) (any, error) {
	var out any
	if err := cl.http.JSON(method, "/admin/v1"+path, in, &out); err != nil {
		return nil, cl.callError(err)
	}
	return out, nil
}

// exists reports whether a GET of path finds the object: false on 404, an
// error (as call returns it) on any other failure.
func (cl *ctlClient) exists(path string) (bool, error) {
	var out any
	err := cl.http.JSON("GET", "/admin/v1"+path, nil, &out)
	var he *HTTPError
	switch {
	case err == nil:
		return true, nil
	case errors.As(err, &he) && he.Status == 404:
		return false, nil
	}
	return false, cl.callError(err)
}

// callError converts an error of a management API request to the error the
// command ends with (exit code and message).
func (cl *ctlClient) callError(err error) error {
	var he *HTTPError
	if errors.As(err, &he) {
		code := ExitGeneral
		msg := he.Error()
		switch he.Status {
		case 400, 404, 409:
			code = ExitArgument
		case 403:
			// The management API returns 403 only when a built-in
			// template is to be changed or deleted (the CLI has full
			// authority), so it is an invalid argument like 409.
			code = ExitArgument
		case 401:
			code = ExitAuth
			msg += " (" + CLISecretFileName + " does not match the running central; the central writes it each time it starts)"
		}
		return NewExitError(code, msg)
	}
	if errors.Is(err, syscall.ECONNREFUSED) {
		return NewExitError(ExitExec, "the central is not running or not listening on "+cl.http.Address+" (a changed agent_listen takes effect when the central is restarted)")
	}
	if IsCertificateError(err) {
		return NewExitErrorf(ExitAuth, "the certificate of the central cannot be verified with %s (it does not match the central's CA): %v", CABundleFileName, err)
	}
	return NewExitError(ExitExec, err.Error())
}

// show prints a value: a table when columns are given and --json is off.
// The human output leaves out the coded texts for the screens (the keys
// ending in "_i18n"); --json prints the answer as it was received.
func (cl *ctlClient) show(v any, columns ...string) {
	if cl.json {
		fmt.Println(marshalIndent(v))
		return
	}
	v = withoutI18n(v)
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
			defer fmt.Printf("(%s total)\n", cell("total", t["total"], true))
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
			name := col
			if col == "detail" && keepsUserDetail(m) {
				name = "" // printed as it is
			}
			cells = append(cells, cell(name, lookup(m, col), cl.full))
		}
		fmt.Fprintln(tw, strings.Join(cells, "\t"))
	}
	tw.Flush()
}

// withoutI18n returns a decoded JSON value without the keys ending in
// "_i18n" at any depth: the codes and parameters the central sends next to
// its English texts for the screens of the browser.
func withoutI18n(v any) any {
	switch t := v.(type) {
	case map[string]any:
		out := make(map[string]any, len(t))
		for k, x := range t {
			if !strings.HasSuffix(k, "_i18n") {
				out[k] = withoutI18n(x)
			}
		}
		return out
	case []any:
		out := make([]any, len(t))
		for i, x := range t {
			out[i] = withoutI18n(x)
		}
		return out
	}
	return v
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

// isTimeKey reports whether a field of the API holds a timestamp.
func isTimeKey(key string) bool {
	switch key {
	case "at", "not_before", "not_after", "locked_until", "switch_after", "scheduled_for":
		return true
	}
	return strings.HasSuffix(key, "_at") || strings.HasSuffix(key, "ctime")
}

// localizeField renders a string field for display: timestamps in local
// time, the history detail (which embeds timestamps) with its timestamps in
// local time, and every other value unchanged.
func localizeField(key, s string) string {
	if i := strings.LastIndex(key, "."); i >= 0 {
		key = key[i+1:]
	}
	switch {
	case isTimeKey(key):
		return localTime(s)
	case key == "detail":
		return localizeText(s)
	}
	return s
}

// localizeValue converts the timestamps of a decoded JSON value to local
// time for display (the --json output keeps the API's UTC values).
func localizeValue(v any) any {
	return localizeKeyed("", v)
}

func localizeKeyed(key string, v any) any {
	switch t := v.(type) {
	case map[string]any:
		keepDetail := keepsUserDetail(t)
		out := make(map[string]any, len(t))
		for k, x := range t {
			if k == "detail" && keepDetail {
				out[k] = x
				continue
			}
			out[k] = localizeKeyed(k, x)
		}
		return out
	case []any:
		out := make([]any, len(t))
		for i, x := range t {
			out[i] = localizeKeyed(key, x)
		}
		return out
	case string:
		return localizeField(key, t)
	}
	return v
}

// keepsUserDetail reports whether the "detail" of the row m is printed as it
// is. The detail of a history row is converted to local time only where the
// system writes it (token and certificate history, "expires <time>"); in the
// other categories it holds text typed by users, which is shown unchanged.
func keepsUserDetail(m map[string]any) bool {
	cat, isHistory := m["category"].(string)
	return isHistory && cat != models.HistToken && cat != models.HistCertificate
}

// formatNumber prints a JSON number; whole numbers never use the exponent
// form, so identifiers can be passed to other commands as printed.
func formatNumber(f float64) string {
	if f == math.Trunc(f) && math.Abs(f) < 1e18 {
		return strconv.FormatInt(int64(f), 10)
	}
	return strconv.FormatFloat(f, 'f', -1, 64)
}

// shorten cuts a text to at most n characters (never inside a character),
// marking the cut with "...".
func shorten(s string, n int) string {
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	r := []rune(s)
	return string(r[:n-3]) + "..."
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

// cell renders the value of the column col for a table.
func cell(col string, v any, full bool) string {
	switch t := v.(type) {
	case nil:
		return "-"
	case float64:
		return formatNumber(t)
	case string:
		if t == "" {
			return "-"
		}
		t = localizeField(col, t)
		if !full {
			return shorten(t, 60)
		}
		return t
	case []any:
		var parts []string
		for _, x := range t {
			if m, ok := x.(map[string]any); ok {
				parts = append(parts, fmt.Sprintf("%v", m["display_name"]))
			} else if f, ok := x.(float64); ok {
				parts = append(parts, formatNumber(f))
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
	return runCtlTable(parent, false, method, path, in, columns...)
}

// runCtlTable is runCtl with the choice to print every cell in full.
func runCtlTable(parent *CtlCmd, full bool, method, path string, in any, columns ...string) error {
	cl, err := parent.client()
	if err != nil {
		return err
	}
	cl.full = full
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
	if !IsJSONObject(data) {
		return nil, NewExitErrorf(ExitArgument, "%s must contain a JSON object", path)
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
	Issue   CtlTokensIssueCmd   `cmd:"" help:"Issue a token (the value is shown in full)"`
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
	ValidDays int   `name:"valid-days" help:"Validity in days (default: central setting)"`
}
type CtlTokensIDCmd struct {
	ID int64 `arg:""`
}
type CtlTokensRevokeCmd struct {
	ID int64 `arg:""`
}

func printTokenValue(p *CtlCmd, cl *ctlClient, out any, reissued bool) {
	m, _ := out.(map[string]any)
	if p.JSON {
		fmt.Println(marshalIndent(out))
		return
	}
	tok, _ := m["token"].(map[string]any)
	fmt.Printf("token name: %v\nexpires:    %v\nvalue:      %v\n", tok["name"], cell("expires_at", tok["expires_at"], true), m["value"])
	fmt.Println("\nInitialize the agent on the monitored host with the following command and enter the value when asked:")
	fmt.Println("  " + agentInitCommand(cl.port, reissued))
}

// agentInitCommand is the "agent init" command shown after a token is
// issued or reissued: the port is given when agent_listen does not use the
// default port, and a reissued value goes to an initialized agent.
func agentInitCommand(port int, reissued bool) string {
	s := fmt.Sprintf("./%s agent init --central-host <central host>", AppName)
	if port != DefaultAgentPort {
		s += fmt.Sprintf(" --central-port %d", port)
	}
	if reissued {
		s += " --overwrite"
	}
	return s
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
	printTokenValue(p, cl, out, false)
	return nil
}
func (c *CtlTokensListCmd) Run(p *CtlCmd) error {
	// The token list is printed without shortening any value.
	return runCtlTable(p, true, "GET", "/tokens", nil, "id", "name", "value", "status", "expires_at", "last_seen_at", "instance_id", "pending_renewal", "expiring_unconnected")
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
	printTokenValue(p, cl, out, true)
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
	Unassign   CtlRulesUnassignCmd   `cmd:"" help:"Take the rule away from agents (they remove their audit rules)"`
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
	Choice      []string `help:"Conflict choice FIELD=TEMPLATE (repeatable; FIELD may contain commas)" sep:"none"`
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
type CtlRulesUnassignCmd struct {
	Agent []int64 `help:"Agent id (repeatable)" required:""`
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

func (c *CtlRulesUnassignCmd) Run(p *CtlCmd) error {
	return runCtl(p, "POST", "/rules/unassign", map[string]any{"agent_ids": c.Agent})
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
	FromRule    int64  `name:"from-rule" help:"Copy a rule version (latest unless --version is given)"`
	Version     int    `help:"Rule version for --from-rule (default: latest)"`
	File        string `help:"Content JSON file ('-' for standard input)"`
}
type CtlTemplatesUpdateCmd struct {
	Name        string  `arg:""`
	File        string  `help:"Content JSON file ('-' for standard input)" required:""`
	Description *string `help:"New description (default: unchanged)"`
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
	in := map[string]any{"content": raw}
	if c.Description != nil {
		in["description"] = *c.Description
	}
	return runCtl(p, "PUT", "/templates/"+url.PathEscape(c.Name), in)
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
	From         string `help:"From, in local time: YYYY-MM-DD HH:MM:SS, YYYY-MM-DD HH:MM (T instead of the space is accepted), YYYY-MM-DD or RFC3339"`
	To           string `help:"To, in local time: YYYY-MM-DD HH:MM:SS, YYYY-MM-DD HH:MM (T instead of the space is accepted), YYYY-MM-DD or RFC3339"`
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

// ctlTimeLayouts are the forms --from and --to accept besides RFC3339; they
// are read in the local time zone of the CLI.
var ctlTimeLayouts = []string{"2006-01-02 15:04:05", "2006-01-02 15:04", "2006-01-02T15:04:05", "2006-01-02T15:04", "2006-01-02"}

// parseCtlTime reads a --from / --to value and returns it as RFC3339 in UTC.
func parseCtlTime(flag, s string) (string, error) {
	if s == "" {
		return "", nil
	}
	if t, err := time.Parse(time.RFC3339, s); err == nil {
		return t.UTC().Format(time.RFC3339), nil
	}
	for _, layout := range ctlTimeLayouts {
		if t, err := time.ParseInLocation(layout, s, time.Local); err == nil {
			return t.UTC().Format(time.RFC3339), nil
		}
	}
	return "", NewExitErrorf(ExitArgument, "invalid --%s %q: use \"YYYY-MM-DD HH:MM:SS\", \"YYYY-MM-DD HH:MM\", YYYY-MM-DD (local time) or RFC3339", flag, s)
}

func (c *CtlDetectionsListCmd) Run(p *CtlCmd) error {
	from, err := parseCtlTime("from", c.From)
	if err != nil {
		return err
	}
	to, err := parseCtlTime("to", c.To)
	if err != nil {
		return err
	}
	q := url.Values{}
	if c.Agent > 0 {
		q.Set("agent_id", fmt.Sprint(c.Agent))
	}
	for k, v := range map[string]string{"category": c.Category, "type": c.Type, "from": from, "to": to, "acknowledged": c.Acknowledged, "suppressed": c.Suppressed} {
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

// Run acknowledges the given detections. When any of them does not exist,
// it acknowledges nothing, names the missing ones and ends with the exit
// code of a missing object (as every ctl command on a 404).
func (c *CtlDetectionsAckCmd) Run(p *CtlCmd) error {
	var items []map[string]int64
	for _, s := range c.Items {
		a, id, ok := parseDetectionRef(s)
		if !ok {
			return NewExitErrorf(ExitArgument, "expected AGENT_ID:ID (two positive integers), got %q", s)
		}
		items = append(items, map[string]int64{"agent_id": a, "id": id})
	}
	cl, err := p.client()
	if err != nil {
		return err
	}
	return cl.ackDetections(items)
}

// ackDetections checks that every item exists and then acknowledges them
// all; see CtlDetectionsAckCmd.Run.
func (cl *ctlClient) ackDetections(items []map[string]int64) error {
	var missing []string
	for _, it := range items {
		found, err := cl.exists(fmt.Sprintf("/detections/%d/%d", it["agent_id"], it["id"]))
		if err != nil {
			return err
		}
		if !found {
			missing = append(missing, fmt.Sprintf("%d:%d", it["agent_id"], it["id"]))
		}
	}
	if len(missing) > 0 {
		return NewExitErrorf(ExitArgument, "detection not found: %s (nothing was acknowledged)", strings.Join(missing, ", "))
	}
	out, err := cl.call("POST", "/detections/ack", map[string]any{"items": items})
	if err != nil {
		return err
	}
	cl.show(out)
	return nil
}

// parseDetectionRef reads AGENT_ID:ID; both parts must be positive decimal
// integers and nothing else may follow.
func parseDetectionRef(s string) (agent, id int64, ok bool) {
	as, is, found := strings.Cut(s, ":")
	if !found {
		return 0, 0, false
	}
	agent, ok1 := parsePositiveID(as)
	id, ok2 := parsePositiveID(is)
	if !ok1 || !ok2 {
		return 0, 0, false
	}
	return agent, id, true
}

// parsePositiveID reads a decimal integer of digits only (no sign, no
// spaces) that is greater than zero.
func parsePositiveID(s string) (int64, bool) {
	if s == "" || strings.Trim(s, "0123456789") != "" {
		return 0, false
	}
	n, err := strconv.ParseInt(s, 10, 64)
	return n, err == nil && n > 0
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
	List        CtlAccountsListCmd        `cmd:"" help:"List accounts"`
	Create      CtlAccountsCreateCmd      `cmd:"" help:"Create an account (password from standard input)"`
	Email       CtlAccountsEmailCmd       `cmd:"" help:"Change the e-mail address (login ID) at once, without a verification code; ends the account's sessions"`
	DisplayName CtlAccountsDisplayNameCmd `cmd:"" name:"display-name" help:"Change the display name"`
	Role        CtlAccountsRoleCmd        `cmd:"" help:"Change the role"`
	Notify      CtlAccountsNotifyCmd      `cmd:"" help:"Choose whether the account receives notification mails"`
	Password    CtlAccountsPasswordCmd    `cmd:"" help:"Reset the password (from standard input)"`
	Disable     CtlAccountsIDCmd          `cmd:"" help:"Disable an account"`
	Enable      CtlAccountsEnableCmd      `cmd:"" help:"Enable an account"`
	Delete      CtlAccountsDeleteCmd      `cmd:"" help:"Delete an account"`
}
type CtlAccountsListCmd struct{}
type CtlAccountsCreateCmd struct {
	Email       string `arg:"" help:"E-mail address (the login ID)"`
	DisplayName string `name:"display-name" help:"Display name shown on the screens" required:""`
	Role        string `help:"admin or viewer" enum:"admin,viewer" required:""`
	Language    string `help:"Display language of the browser screens: auto (follow the browser), en or ja" enum:"auto,en,ja" default:"auto"`
}
type CtlAccountsEmailCmd struct {
	ID    int64  `arg:""`
	Email string `arg:"" help:"New e-mail address"`
}
type CtlAccountsDisplayNameCmd struct {
	ID   int64  `arg:""`
	Name string `arg:"" help:"New display name"`
}
type CtlAccountsRoleCmd struct {
	ID   int64  `arg:""`
	Role string `arg:"" enum:"admin,viewer"`
}
type CtlAccountsNotifyCmd struct {
	ID    int64  `arg:""`
	State string `arg:"" enum:"on,off" help:"on: the account receives notification mails (while enabled); off: it does not"`
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
	return runCtl(p, "GET", "/accounts", nil, "id", "email", "display_name", "role", "language", "disabled", "notify", "locked_until", "created_at")
}
func (c *CtlAccountsCreateCmd) Run(p *CtlCmd) error {
	email, err := ValidateEmail(c.Email)
	if err != nil {
		return NewExitError(ExitArgument, err.Error())
	}
	display, err := ValidateDisplayName(c.DisplayName)
	if err != nil {
		return NewExitError(ExitArgument, err.Error())
	}
	pw, err := readNewPassword()
	if err != nil {
		return err
	}
	return runCtl(p, "POST", "/accounts", map[string]any{"email": email, "display_name": display, "role": c.Role, "language": c.Language, "password": pw})
}
func (c *CtlAccountsEmailCmd) Run(p *CtlCmd) error {
	email, err := ValidateEmail(c.Email)
	if err != nil {
		return NewExitError(ExitArgument, err.Error())
	}
	return runCtl(p, "PUT", fmt.Sprintf("/accounts/%d/email", c.ID), map[string]any{"email": email})
}
func (c *CtlAccountsDisplayNameCmd) Run(p *CtlCmd) error {
	display, err := ValidateDisplayName(c.Name)
	if err != nil {
		return NewExitError(ExitArgument, err.Error())
	}
	return runCtl(p, "PUT", fmt.Sprintf("/accounts/%d/display-name", c.ID), map[string]any{"display_name": display})
}
func (c *CtlAccountsRoleCmd) Run(p *CtlCmd) error {
	return runCtl(p, "PUT", fmt.Sprintf("/accounts/%d/role", c.ID), map[string]any{"role": c.Role})
}
func (c *CtlAccountsNotifyCmd) Run(p *CtlCmd) error {
	return runCtl(p, "PUT", fmt.Sprintf("/accounts/%d/notify", c.ID), map[string]any{"notify": c.State == "on"})
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
	Category string `help:"account, rule, token, certificate, rejected, template, suppression, detection, config or command"`
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
