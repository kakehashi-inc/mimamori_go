package modules

import (
	"database/sql"
	"errors"
	"fmt"
	"net"
	"sort"
	"strconv"
	"strings"

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
	To         []string
	TimeoutSec int
	HeloName   string
}

// Enabled reports whether mail can be sent.
func (s SMTPConfig) Enabled() bool {
	return s.Host != "" && s.From != "" && len(s.To) > 0
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
	Workers                 int
	QueueTimeoutSec         int
	TokenValidDays          int
	TokenRenewBeforeDays    int
	RetentionDays           int
	DefaultCheckIntervalSec int
	SessionHours            int
	SessionRememberDays     int
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
	// Restart is true for items read only when the central starts (the
	// listeners, the workers and the built-in agent); the others take effect
	// while the central is running.
	Restart bool
	get     func(c *CentralConfig) string
	set     func(c *CentralConfig, v string) error
	secret  bool
}

// restartItems lists the items that take effect when the central restarts.
var restartItems = map[string]bool{
	"web_listen": true, "agent_listen": true, "workers": true, "queue_timeout_sec": true, "builtin_agent": true,
}

func init() {
	for i := range ConfigFields {
		ConfigFields[i].Restart = restartItems[ConfigFields[i].Name]
	}
}

func cfgIntField(name, help string, p func(c *CentralConfig) *int) ConfigField {
	return ConfigField{Name: name, Kind: "int", Help: help,
		get: func(c *CentralConfig) string { return strconv.Itoa(*p(c)) },
		set: func(c *CentralConfig, v string) error {
			n, err := strconv.Atoi(strings.TrimSpace(v))
			if err != nil || n <= 0 {
				return fmt.Errorf("%s must be a positive integer", name)
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

func cfgListField(name, help string, p func(c *CentralConfig) *[]string) ConfigField {
	return ConfigField{Name: name, Kind: "list", Help: help,
		get: func(c *CentralConfig) string { return strings.Join(*p(c), ",") },
		set: func(c *CentralConfig, v string) error {
			var out []string
			for _, s := range strings.Split(v, ",") {
				if s = strings.TrimSpace(s); s != "" {
					out = append(out, s)
				}
			}
			*p(c) = out
			return nil
		}}
}

func parseBool(name, v string) (bool, error) {
	b, err := strconv.ParseBool(strings.TrimSpace(v))
	if err != nil {
		return false, fmt.Errorf("%s must be true or false", name)
	}
	return b, nil
}

// ConfigFields lists every configuration item in display order.
var ConfigFields = []ConfigField{
	cfgStringField("web_listen", "Listen address of the browser screens (IP:port)", func(c *CentralConfig) *string { return &c.WebListen }),
	cfgStringField("agent_listen", "Listen address of the agent and management API (IP:port)", func(c *CentralConfig) *string { return &c.AgentListen }),
	cfgListField("server_names", "Host names and IP addresses in the server certificate", func(c *CentralConfig) *[]string { return &c.ServerNames }),
	cfgIntField("workers", "Requests processed at the same time", func(c *CentralConfig) *int { return &c.Workers }),
	cfgIntField("queue_timeout_sec", "Longest wait of a queued request (seconds)", func(c *CentralConfig) *int { return &c.QueueTimeoutSec }),
	cfgIntField("token_valid_days", "Validity of a new or renewed token (days)", func(c *CentralConfig) *int { return &c.TokenValidDays }),
	cfgIntField("token_renew_before_days", "Renew a token this many days before it expires", func(c *CentralConfig) *int { return &c.TokenRenewBeforeDays }),
	cfgIntField("retention_days", "Retention of detections, reports and history (days)", func(c *CentralConfig) *int { return &c.RetentionDays }),
	cfgIntField("default_check_interval_sec", "Default check interval of the agents (seconds)", func(c *CentralConfig) *int { return &c.DefaultCheckIntervalSec }),
	cfgIntField("session_hours", "Login session lifetime (hours)", func(c *CentralConfig) *int { return &c.SessionHours }),
	cfgIntField("session_remember_days", "Lifetime of a kept login (days)", func(c *CentralConfig) *int { return &c.SessionRememberDays }),
	{Name: "builtin_agent", Kind: "bool", Help: "Run the built-in agent (monitoring of the central host)",
		get: func(c *CentralConfig) string { return strconv.FormatBool(c.BuiltinAgent != nil && *c.BuiltinAgent) },
		set: func(c *CentralConfig, v string) error {
			b, err := parseBool("builtin_agent", v)
			if err == nil {
				c.BuiltinAgent = &b
			}
			return err
		}},
	cfgIntField("notify.min_interval_min", "Shortest interval between notification mails (minutes)", func(c *CentralConfig) *int { return &c.Notify.MinIntervalMin }),
	cfgIntField("notify.dedupe_hours", "Do not notify the same detection again within (hours)", func(c *CentralConfig) *int { return &c.Notify.DedupeHours }),
	cfgIntField("notify.max_lines", "Maximum detection lines in one mail", func(c *CentralConfig) *int { return &c.Notify.MaxLines }),
	cfgStringField("notify.subject_prefix", "Subject prefix of notification mails", func(c *CentralConfig) *string { return &c.Notify.SubjectPrefix }),
	cfgStringField("smtp.host", "SMTP server (notifications are off while empty)", func(c *CentralConfig) *string { return &c.SMTP.Host }),
	cfgIntField("smtp.port", "SMTP port", func(c *CentralConfig) *int { return &c.SMTP.Port }),
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
	cfgListField("smtp.to", "Recipient addresses", func(c *CentralConfig) *[]string { return &c.SMTP.To }),
	cfgIntField("smtp.timeout_sec", "SMTP timeout (seconds)", func(c *CentralConfig) *int { return &c.SMTP.TimeoutSec }),
	cfgStringField("smtp.helo_name", "Name sent in HELO/EHLO (empty: the host name)", func(c *CentralConfig) *string { return &c.SMTP.HeloName }),
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
	return nil, fmt.Errorf("unknown configuration item %q (items: %s)", name, strings.Join(names, ", "))
}

// Secret reports whether the value must not be displayed.
func (f *ConfigField) Secret() bool { return f.secret }

// Value returns the field's value in c as text.
func (f *ConfigField) Value(c *CentralConfig) string { return f.get(c) }

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
	if c.Workers <= 0 {
		c.Workers = DefaultWorkers
	}
	if c.QueueTimeoutSec <= 0 {
		c.QueueTimeoutSec = DefaultQueueTimeoutSec
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
	if c.SessionHours <= 0 {
		c.SessionHours = DefaultSessionHours
	}
	if c.SessionRememberDays <= 0 {
		c.SessionRememberDays = DefaultRememberDays
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
		switch c.SMTP.Security {
		case "tls":
			c.SMTP.Port = 465
		case "none":
			c.SMTP.Port = 25
		default:
			c.SMTP.Port = 587
		}
	}
	if c.SMTP.TimeoutSec <= 0 {
		c.SMTP.TimeoutSec = DefaultSMTPTimeoutSec
	}
}

// Validate checks the configuration values.
func (c *CentralConfig) Validate() error {
	var errs []string
	for _, a := range []struct{ name, addr string }{{"web_listen", c.WebListen}, {"agent_listen", c.AgentListen}} {
		host, _, err := net.SplitHostPort(a.addr)
		if err != nil {
			errs = append(errs, fmt.Sprintf("%s: %v", a.name, err))
		} else if host != "" && net.ParseIP(host) == nil {
			errs = append(errs, fmt.Sprintf("%s: the address must be an IP address", a.name))
		}
	}
	if c.WebListen == c.AgentListen {
		errs = append(errs, "web_listen and agent_listen must differ")
	}
	switch c.SMTP.Security {
	case "starttls", "tls", "none":
	default:
		errs = append(errs, "smtp.security must be starttls, tls or none")
	}
	if c.SessionRememberDays > 365 {
		errs = append(errs, "session_remember_days must be 365 or less")
	}
	if c.Workers > 64 {
		errs = append(errs, "workers must be 64 or less")
	}
	if len(errs) > 0 {
		return errors.New(strings.Join(errs, "; "))
	}
	return nil
}

// storedCentralConfig reads the stored items (without defaults) and returns
// the configuration and the names of the stored items.
func storedCentralConfig(db models.Execer) (*CentralConfig, map[string]bool, error) {
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
func LoadCentralConfig(db models.Execer) (*CentralConfig, error) {
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
func CentralConfigState(db models.Execer) (*CentralConfig, map[string]bool, error) {
	c, stored, err := storedCentralConfig(db)
	if err != nil {
		return nil, nil, err
	}
	c.applyDefaults()
	return c, stored, nil
}

// ApplyCentralConfigChanges sets and unsets items in one transaction after
// checking the resulting configuration as a whole. It returns the names of
// the items whose stored value changed.
func ApplyCentralConfigChanges(db *sql.DB, set map[string]string, unset []string) ([]string, error) {
	cur, stored, err := storedCentralConfig(db)
	if err != nil {
		return nil, err
	}
	removed := map[string]bool{}
	for _, name := range unset {
		if _, err := FindConfigField(name); err != nil {
			return nil, err
		}
		if _, ok := set[name]; ok {
			return nil, fmt.Errorf("%s is both set and unset", name)
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
	if err := next.Validate(); err != nil {
		return nil, err
	}
	var changed []string
	tx, err := db.Begin()
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback() }()
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
	return changed, tx.Commit()
}

// KeepRestartItems copies the items that take effect only at start from
// running into c, so a configuration reloaded while running keeps them.
func (c *CentralConfig) KeepRestartItems(running *CentralConfig) {
	c.WebListen, c.AgentListen = running.WebListen, running.AgentListen
	c.Workers, c.QueueTimeoutSec = running.Workers, running.QueueTimeoutSec
	c.BuiltinAgent = running.BuiltinAgent
}
