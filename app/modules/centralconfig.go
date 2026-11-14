package modules

import (
	"encoding/json"
	"fmt"
	"net"
	"net/mail"
	"sort"
	"strconv"
	"strings"
	"unicode"

	"mimamori/app/models"
)

// SMTPConfig configures notification mail delivery.
type SMTPConfig struct {
	Host       string
	Port       int
	Security   string // "starttls", "tls" or "none"
	Auth       bool
	Username   string
	Password   string
	From       string
	TimeoutSec int
	HeloName   string
}

// CanSend reports whether mail can be sent to given recipients (a server and
// a sender are set). Verification code mails need this.
func (s SMTPConfig) CanSend() bool {
	return strings.TrimSpace(s.Host) != "" && strings.TrimSpace(s.From) != ""
}

// DevelopmentMode reports whether no SMTP server is configured. Then no
// notification is sent and no verification code is asked for (at login and
// for an own e-mail address change), because none could be delivered.
func (s SMTPConfig) DevelopmentMode() bool {
	return strings.TrimSpace(s.Host) == ""
}

// NotifyConfig configures notification throttling.
type NotifyConfig struct {
	MinIntervalMin int
	DedupeHours    int
	MaxLines       int
	SubjectPrefix  string
}

// CentralConfig is the central's configuration. It is stored in the settings
// table of the central database (one row per item, key "config.<name>");
// items without a row use their defaults.
type CentralConfig struct {
	WebListen               string
	AgentListen             string
	ServerNames             []string
	TokenValidDays          int
	TokenRenewBeforeDays    int
	RetentionDays           int
	DefaultCheckIntervalSec int
	SessionRememberDays     int
	TwoStepTrustDays        int
	BuiltinAgent            *bool
	Notify                  NotifyConfig
	SMTP                    SMTPConfig
}

// configKeyPrefix marks configuration rows in the settings table (the table
// also holds internal values such as the server key).
const configKeyPrefix = "config."

// ConfigField describes one configuration item.
type ConfigField struct {
	Name string
	Kind string // "string", "int", "bool", "list" (comma separated) or "secret"
	Help string
	// Min and Max are the smallest and the largest value of an item of kind
	// "int", both accepted. The validation and the range the screens show
	// (GET /config) are made from them; other kinds leave them 0.
	Min, Max int
	// Restart is true for items read only when the central starts (the
	// listeners and the built-in agent); the others take effect while the
	// central is running.
	Restart bool
	get     func(c *CentralConfig) string
	set     func(c *CentralConfig, v string) error
	secret  bool
}

// restartItems lists the items that take effect when the central restarts.
var restartItems = map[string]bool{
	"web_listen": true, "agent_listen": true, "builtin_agent": true,
}

func init() {
	for i := range ConfigFields {
		ConfigFields[i].Restart = restartItems[ConfigFields[i].Name]
	}
}

// cfgIntField makes an item of kind "int" accepting min to max. The upper
// bounds keep every time computed from the items far from overflowing.
func cfgIntField(name, help string, min, max int, p func(c *CentralConfig) *int) ConfigField {
	return ConfigField{Name: name, Kind: "int", Help: help, Min: min, Max: max,
		get: func(c *CentralConfig) string { return strconv.Itoa(*p(c)) },
		set: func(c *CentralConfig, v string) error {
			n, err := ParseWholeInt(v)
			if err != nil || n <= 0 {
				return M("config.not_positive_int", "name", name)
			}
			*p(c) = n
			return nil
		}}
}

func cfgStringField(name, help string, p func(c *CentralConfig) *string) ConfigField {
	return ConfigField{Name: name, Kind: "string", Help: help,
		get: func(c *CentralConfig) string { return *p(c) },
		set: func(c *CentralConfig, v string) error { *p(c) = strings.TrimSpace(v); return nil }}
}

func parseBool(name, v string) (bool, error) {
	b, err := strconv.ParseBool(strings.TrimSpace(v))
	if err != nil {
		return false, M("config.not_bool", "name", name)
	}
	return b, nil
}

// ConfigFields lists every configuration item in display order.
var ConfigFields = []ConfigField{
	cfgStringField("web_listen", "Listen address of the browser screens (IP:port)", func(c *CentralConfig) *string { return &c.WebListen }),
	cfgStringField("agent_listen", "Listen address of the agent and management API (IP:port)", func(c *CentralConfig) *string { return &c.AgentListen }),
	{Name: "server_names", Kind: "list", Help: "Host names and IP addresses in the server certificate",
		get: func(c *CentralConfig) string { return strings.Join(c.ServerNames, ",") },
		set: func(c *CentralConfig, v string) error {
			// Each name is kept in its canonical form (NormalizeCertName),
			// as it appears in the certificate.
			c.ServerNames = nil
			for _, s := range strings.Split(v, ",") {
				if s = NormalizeCertName(s); s != "" {
					c.ServerNames = append(c.ServerNames, s)
				}
			}
			return nil
		}},
	cfgIntField("token_valid_days", "Validity of a new or renewed token (days)", 1, 36500, func(c *CentralConfig) *int { return &c.TokenValidDays }),
	cfgIntField("token_renew_before_days", "Renew a token this many days before it expires", 1, 36500, func(c *CentralConfig) *int { return &c.TokenRenewBeforeDays }),
	cfgIntField("retention_days", "Retention of detections, of the history of rejected connections, of command runs and of run requests that ended or whose agent does not respond (days)", 1, 36500, func(c *CentralConfig) *int { return &c.RetentionDays }),
	cfgIntField("default_check_interval_sec", "Default check interval of the agents (seconds)", MinCheckInterval, MaxCheckInterval, func(c *CentralConfig) *int { return &c.DefaultCheckIntervalSec }),
	cfgIntField("session_remember_days", "Lifetime of a kept login (days)", 1, 365, func(c *CentralConfig) *int { return &c.SessionRememberDays }),
	cfgIntField("two_step_trust_days", "Days a browser marked as trusted at the login code step needs no code", 1, MaxTrustDays, func(c *CentralConfig) *int { return &c.TwoStepTrustDays }),
	{Name: "builtin_agent", Kind: "bool", Help: "Run the built-in agent (monitoring of the central host)",
		get: func(c *CentralConfig) string { return strconv.FormatBool(c.BuiltinAgent != nil && *c.BuiltinAgent) },
		set: func(c *CentralConfig, v string) error {
			b, err := parseBool("builtin_agent", v)
			if err == nil {
				c.BuiltinAgent = &b
			}
			return err
		}},
	cfgIntField("notify.min_interval_min", "Shortest interval between notification mails (minutes)", 1, 43200, func(c *CentralConfig) *int { return &c.Notify.MinIntervalMin }),
	cfgIntField("notify.dedupe_hours", "Do not notify the same detection again within (hours)", 1, 8760, func(c *CentralConfig) *int { return &c.Notify.DedupeHours }),
	cfgIntField("notify.max_lines", "Maximum detection lines in one mail", 1, 10000, func(c *CentralConfig) *int { return &c.Notify.MaxLines }),
	cfgStringField("notify.subject_prefix", "Subject prefix of notification mails", func(c *CentralConfig) *string { return &c.Notify.SubjectPrefix }),
	cfgStringField("smtp.host", "SMTP server (while empty, no notifications are sent and no verification code is asked for at login)", func(c *CentralConfig) *string { return &c.SMTP.Host }),
	cfgIntField("smtp.port", "SMTP port", 1, 65535, func(c *CentralConfig) *int { return &c.SMTP.Port }),
	cfgStringField("smtp.security", "starttls, tls or none", func(c *CentralConfig) *string { return &c.SMTP.Security }),
	{Name: "smtp.auth", Kind: "bool", Help: "Use SMTP authentication",
		get: func(c *CentralConfig) string { return strconv.FormatBool(c.SMTP.Auth) },
		set: func(c *CentralConfig, v string) error {
			b, err := parseBool("smtp.auth", v)
			if err == nil {
				c.SMTP.Auth = b
			}
			return err
		}},
	cfgStringField("smtp.username", "SMTP user name", func(c *CentralConfig) *string { return &c.SMTP.Username }),
	{Name: "smtp.password", Kind: "secret", Help: "SMTP password (read from standard input)", secret: true,
		get: func(c *CentralConfig) string { return c.SMTP.Password },
		set: func(c *CentralConfig, v string) error { c.SMTP.Password = v; return nil }},
	cfgStringField("smtp.from", "Sender address", func(c *CentralConfig) *string { return &c.SMTP.From }),
	cfgIntField("smtp.timeout_sec", "Longest silence of the SMTP server that is waited for (seconds)", 1, 3600, func(c *CentralConfig) *int { return &c.SMTP.TimeoutSec }),
	cfgStringField("smtp.helo_name", "Name sent in HELO/EHLO (empty: localhost)", func(c *CentralConfig) *string { return &c.SMTP.HeloName }),
}

// FindConfigField returns the field with the given name.
func FindConfigField(name string) (*ConfigField, error) {
	for i := range ConfigFields {
		if ConfigFields[i].Name == name {
			return &ConfigFields[i], nil
		}
	}
	names := make([]string, 0, len(ConfigFields))
	for _, f := range ConfigFields {
		names = append(names, f.Name)
	}
	sort.Strings(names)
	return nil, M("config.unknown_item", "name", strconv.Quote(name), "items", names)
}

// Secret reports whether the value must not be displayed.
func (f *ConfigField) Secret() bool { return f.secret }

// Value returns the field's value in c as text.
func (f *ConfigField) Value(c *CentralConfig) string { return f.get(c) }

// Default returns the field's value when it is not stored while the other
// items are as in c: the port of the mail server is the one of c's
// smtp.security.
func (f *ConfigField) Default(c *CentralConfig) string {
	d := DefaultCentralConfig()
	d.SMTP.Port = defaultSMTPPort(c.SMTP.Security)
	return f.get(d)
}

// defaultSMTPPort is the port of the mail server for a security (smtp.security)
// when smtp.port is not stored.
func defaultSMTPPort(security string) int {
	switch security {
	case "tls":
		return 465
	case "none":
		return 25
	default:
		return 587
	}
}

// DefaultWebListen is the default listen address of the browser screens.
func DefaultWebListen() string {
	return net.JoinHostPort(DefaultListenAddress, strconv.Itoa(DefaultWebPort))
}

// DefaultAgentListen is the default listen address for the agents.
func DefaultAgentListen() string {
	return net.JoinHostPort(DefaultListenAddress, strconv.Itoa(DefaultAgentPort))
}

// DefaultCentralConfig returns the defaults used for missing values.
func DefaultCentralConfig() *CentralConfig {
	c := &CentralConfig{}
	c.applyDefaults()
	return c
}

func (c *CentralConfig) applyDefaults() {
	if c.WebListen == "" {
		c.WebListen = DefaultWebListen()
	}
	if c.AgentListen == "" {
		c.AgentListen = DefaultAgentListen()
	}
	if c.TokenValidDays <= 0 {
		c.TokenValidDays = DefaultTokenValidDays
	}
	if c.TokenRenewBeforeDays <= 0 {
		c.TokenRenewBeforeDays = DefaultTokenRenewDays
	}
	if c.RetentionDays <= 0 {
		c.RetentionDays = DefaultRetentionDays
	}
	if c.DefaultCheckIntervalSec <= 0 {
		c.DefaultCheckIntervalSec = DefaultCheckIntervalSec
	}
	if c.SessionRememberDays <= 0 {
		c.SessionRememberDays = DefaultRememberDays
	}
	if c.TwoStepTrustDays <= 0 {
		c.TwoStepTrustDays = DefaultTrustDays
	}
	if c.BuiltinAgent == nil {
		c.BuiltinAgent = boolPtr(true)
	}
	if c.Notify.MinIntervalMin <= 0 {
		c.Notify.MinIntervalMin = DefaultNotifyIntervalMin
	}
	if c.Notify.DedupeHours <= 0 {
		c.Notify.DedupeHours = DefaultNotifyDedupeHours
	}
	if c.Notify.MaxLines <= 0 {
		c.Notify.MaxLines = DefaultNotifyMaxLines
	}
	if c.Notify.SubjectPrefix == "" {
		c.Notify.SubjectPrefix = "[" + AppName + "]"
	}
	if c.SMTP.Security == "" {
		c.SMTP.Security = "starttls"
	}
	if c.SMTP.Port == 0 {
		c.SMTP.Port = defaultSMTPPort(c.SMTP.Security)
	}
	if c.SMTP.TimeoutSec <= 0 {
		c.SMTP.TimeoutSec = DefaultSMTPTimeoutSec
	}
}

// emptyAllowed lists the items whose empty value is meaningful (no extra
// certificate names, notifications off, no user name, and so on). The others
// need a value: an empty one would only store their default.
var emptyAllowed = map[string]bool{
	"server_names": true, "smtp.host": true, "smtp.username": true, "smtp.password": true,
	"smtp.from": true, "smtp.helo_name": true,
}

// validHostName reports whether s is an ASCII DNS name: labels of letters,
// digits and hyphens (1 to 63 characters each), at most 253 characters.
func validHostName(s string) bool {
	if s == "" || len(s) > 253 {
		return false
	}
	for _, label := range strings.Split(s, ".") {
		if label == "" || len(label) > 63 {
			return false
		}
		for _, r := range label {
			ok := r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '-'
			if !ok {
				return false
			}
		}
	}
	return true
}

// validMailHost reports whether s can be the name of a mail server: ASCII
// letters, digits, hyphens, underscores and dots (a trailing dot is allowed),
// at most 253 characters.
func validMailHost(s string) bool {
	if s == "" || len(s) > 253 {
		return false
	}
	for _, r := range s {
		ok := r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '-' || r == '_' || r == '.'
		if !ok {
			return false
		}
	}
	return true
}

// validMailAddress reports whether s is exactly one mail address (without a
// display name).
func validMailAddress(s string) bool {
	a, err := mail.ParseAddress(s)
	return err == nil && a.Address == s
}

// Validate checks the configuration values. The error is a Problems value
// (its text: the problems joined with "; ").
func (c *CentralConfig) Validate() error {
	if ps := c.validationProblems(); len(ps) > 0 {
		return Problems(ps)
	}
	return nil
}

// validationProblems returns one problem per problem of the configuration.
// Each message names its items in parameters (name, field, other), so that
// the screens can name them by their labels.
func (c *CentralConfig) validationProblems() []Problem {
	var errs []Problem
	add := func(m Msg) { errs = append(errs, Problem{Msg: m}) }
	for _, a := range []struct{ name, addr string }{{"web_listen", c.WebListen}, {"agent_listen", c.AgentListen}} {
		host, port, err := net.SplitHostPort(a.addr)
		if err != nil {
			add(M("config.listen_invalid", "field", a.name, "detail", err.Error()))
			continue
		}
		if host != "" && net.ParseIP(host) == nil {
			add(M("config.listen_ip", "field", a.name))
		}
		if p, err := ParseWholeInt(port); err != nil || p < 1 || p > 65535 {
			add(M("config.listen_port", "field", a.name))
		}
	}
	if c.WebListen == c.AgentListen {
		add(M("config.listen_same", "name", "web_listen", "other", "agent_listen"))
	}
	for _, n := range c.ServerNames {
		if net.ParseIP(n) == nil && !validHostName(n) {
			add(M("config.server_name_invalid", "name", "server_names", "value", strconv.Quote(n)))
		}
	}
	switch c.SMTP.Security {
	case "starttls", "tls", "none":
	default:
		add(M("config.smtp_security", "name", "smtp.security"))
	}
	if h := c.SMTP.Host; h != "" && net.ParseIP(h) == nil && !validMailHost(h) {
		add(M("config.smtp_host", "name", "smtp.host"))
	}
	if c.SMTP.From != "" && !validMailAddress(c.SMTP.From) {
		add(M("config.smtp_from", "name", "smtp.from"))
	}
	for i := range ConfigFields {
		f := &ConfigFields[i]
		switch f.Kind {
		case "int":
			n, _ := strconv.Atoi(f.get(c))
			switch {
			case n < f.Min:
				add(M("config.int_min", "name", f.Name, "min", f.Min))
			case n > f.Max:
				add(M("config.int_max", "name", f.Name, "max", f.Max))
			}
		case "string", "list", "secret":
			if strings.IndexFunc(f.get(c), unicode.IsControl) >= 0 {
				add(M("config.control_chars", "name", f.Name))
			}
		}
	}
	if c.TokenRenewBeforeDays >= c.TokenValidDays {
		add(M("config.renew_before", "name", "token_renew_before_days", "other", "token_valid_days"))
	}
	return errs
}

// storedCentralConfig reads the stored items (without defaults) and returns
// the configuration and the names of the stored items.
func storedCentralConfig(db models.Reader) (*CentralConfig, map[string]bool, error) {
	rows, err := models.ListSettings(db)
	if err != nil {
		return nil, nil, err
	}
	c := &CentralConfig{}
	stored := map[string]bool{}
	for _, r := range rows {
		name, ok := strings.CutPrefix(r.Key, configKeyPrefix)
		if !ok {
			continue
		}
		f, err := FindConfigField(name)
		if err != nil {
			return nil, nil, err
		}
		if err := f.set(c, r.Value); err != nil {
			return nil, nil, err
		}
		stored[name] = true
	}
	return c, stored, nil
}

// LoadCentralConfig reads the configuration from the central database.
func LoadCentralConfig(db models.Reader) (*CentralConfig, error) {
	c, _, err := storedCentralConfig(db)
	if err != nil {
		return nil, fmt.Errorf("configuration: %w", err)
	}
	c.applyDefaults()
	if err := c.Validate(); err != nil {
		return nil, fmt.Errorf("configuration: %w", err)
	}
	return c, nil
}

// CentralConfigState returns the effective configuration and which items are
// stored (the others use their defaults).
func CentralConfigState(db models.Reader) (*CentralConfig, map[string]bool, error) {
	c, stored, err := storedCentralConfig(db)
	if err != nil {
		return nil, nil, err
	}
	c.applyDefaults()
	return c, stored, nil
}

// ApplyCentralConfigChanges sets and unsets items inside the write
// transaction tx after checking the resulting configuration as a whole. The
// transaction holds the database's write lock from its begin, so no other
// change is saved between the read and the write: the check is made against
// the values that are really stored. It returns the names of the items
// whose stored value changed. A refused change is answered with a Msg or
// Problems error; any other error is a failure of the database.
//
// The caller begins the transaction and ends it: the changes are saved when
// it commits, and it rolls back when an error is returned. A history row
// the caller writes in the same transaction is saved with the change or not
// at all.
func ApplyCentralConfigChanges(tx models.Writer, set map[string]string, unset []string) ([]string, error) {
	cur, stored, err := storedCentralConfig(tx)
	if err != nil {
		return nil, err
	}
	removed := map[string]bool{}
	for _, name := range unset {
		if _, err := FindConfigField(name); err != nil {
			return nil, err
		}
		if _, ok := set[name]; ok {
			return nil, M("config.set_and_unset", "name", name)
		}
		removed[name] = true
	}
	next := &CentralConfig{}
	values := map[string]string{}
	for _, f := range ConfigFields {
		if stored[f.Name] && !removed[f.Name] {
			values[f.Name] = f.get(cur)
		}
	}
	for name, v := range set {
		f, err := FindConfigField(name)
		if err != nil {
			return nil, err
		}
		if strings.TrimSpace(v) == "" && !emptyAllowed[name] {
			return nil, M("config.value_required", "name", name)
		}
		probe := &CentralConfig{}
		if err := f.set(probe, v); err != nil {
			return nil, err
		}
		values[name] = f.get(probe)
	}
	for name, v := range values {
		f, _ := FindConfigField(name)
		if err := f.set(next, v); err != nil {
			return nil, err
		}
	}
	next.applyDefaults()
	if errs := next.validationProblems(); len(errs) > 0 {
		// Problems the stored configuration already has (for example a value
		// saved by an older version) do not block a change, so that it can
		// be corrected one item at a time; a change must not add a problem.
		before := *cur
		before.applyDefaults()
		known := map[string]bool{}
		for _, e := range before.validationProblems() {
			known[e.Text()] = true
		}
		var added Problems
		for _, e := range errs {
			if !known[e.Text()] {
				added = append(added, e)
			}
		}
		if len(added) > 0 {
			return nil, added
		}
	}
	var changed []string
	for _, f := range ConfigFields {
		v, keep := values[f.Name]
		switch {
		case keep && (!stored[f.Name] || f.get(cur) != v):
			if err := models.SetSetting(tx, configKeyPrefix+f.Name, v); err != nil {
				return nil, err
			}
			changed = append(changed, f.Name)
		case !keep && stored[f.Name]:
			if err := models.DeleteSetting(tx, configKeyPrefix+f.Name); err != nil {
				return nil, err
			}
			changed = append(changed, f.Name)
		}
	}
	return changed, nil
}

// KeepRestartItems copies the items that take effect only at start from
// running into c, so a configuration reloaded while running keeps them.
func (c *CentralConfig) KeepRestartItems(running *CentralConfig) {
	c.WebListen, c.AgentListen = running.WebListen, running.AgentListen
	c.BuiltinAgent = running.BuiltinAgent
}

// runningConfigKey is the settings row holding the values of the items that
// take effect at start (restartItems) as the running central uses them. The
// central writes it at its start, right after it took the lock, opened the
// database and read the configuration (before it opens its listeners), and
// removes it when it stops, so that central status can show what runs while
// a saved change waits for a restart.
const runningConfigKey = "running_config"

// SaveRunningConfig records the start values of the running central.
func SaveRunningConfig(db models.Writer, c *CentralConfig) error {
	values := map[string]string{}
	for _, f := range ConfigFields {
		if f.Restart {
			values[f.Name] = f.get(c)
		}
	}
	b, err := json.Marshal(values)
	if err != nil {
		return err
	}
	return models.SetSetting(db, runningConfigKey, string(b))
}

// ClearRunningConfig removes the record of SaveRunningConfig.
func ClearRunningConfig(db models.Writer) error {
	return models.DeleteSetting(db, runningConfigKey)
}

// LoadRunningConfig returns the start values recorded by the running central
// by item name, or nil when there is no (readable) record.
func LoadRunningConfig(db models.Reader) map[string]string {
	v, found, err := models.GetSettingStrict(db, runningConfigKey)
	if err != nil || !found {
		return nil
	}
	values := map[string]string{}
	if json.Unmarshal([]byte(v), &values) != nil {
		return nil
	}
	return values
}
