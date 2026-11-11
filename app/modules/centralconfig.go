package modules

import (
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"strings"
)

// SMTPConfig configures notification mail delivery.
type SMTPConfig struct {
	Host       string   `json:"host"`
	Port       int      `json:"port"`
	Security   string   `json:"security"` // "starttls", "tls" or "none"
	Auth       bool     `json:"auth"`
	Username   string   `json:"username"`
	Password   string   `json:"password"`
	From       string   `json:"from"`
	To         []string `json:"to"`
	TimeoutSec int      `json:"timeout_sec"`
	HeloName   string   `json:"helo_name"`
}

// Enabled reports whether mail can be sent.
func (s SMTPConfig) Enabled() bool {
	return s.Host != "" && s.From != "" && len(s.To) > 0
}

// NotifyConfig configures notification throttling.
type NotifyConfig struct {
	MinIntervalMin int    `json:"min_interval_min"`
	DedupeHours    int    `json:"dedupe_hours"`
	MaxLines       int    `json:"max_lines"`
	SubjectPrefix  string `json:"subject_prefix"`
}

// CentralConfig is the central's configuration file (JSON).
type CentralConfig struct {
	WebListen               string       `json:"web_listen"`
	AgentListen             string       `json:"agent_listen"`
	ServerNames             []string     `json:"server_names"`
	Workers                 int          `json:"workers"`
	QueueTimeoutSec         int          `json:"queue_timeout_sec"`
	TokenValidDays          int          `json:"token_valid_days"`
	TokenRenewBeforeDays    int          `json:"token_renew_before_days"`
	RetentionDays           int          `json:"retention_days"`
	DefaultCheckIntervalSec int          `json:"default_check_interval_sec"`
	SessionHours            int          `json:"session_hours"`
	SessionRememberDays     int          `json:"session_remember_days"`
	BuiltinAgent            *bool        `json:"builtin_agent"`
	Notify                  NotifyConfig `json:"notify"`
	SMTP                    SMTPConfig   `json:"smtp"`
}

// DefaultCentralConfig returns the defaults used for missing values.
func DefaultCentralConfig() *CentralConfig {
	c := &CentralConfig{}
	c.applyDefaults()
	return c
}

func (c *CentralConfig) applyDefaults() {
	if c.WebListen == "" {
		c.WebListen = DefaultWebListen
	}
	if c.AgentListen == "" {
		c.AgentListen = DefaultAgentListen
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
	for name, addr := range map[string]string{"web_listen": c.WebListen, "agent_listen": c.AgentListen} {
		host, _, err := net.SplitHostPort(addr)
		if err != nil {
			errs = append(errs, fmt.Sprintf("%s: %v", name, err))
		} else if host != "" && net.ParseIP(host) == nil {
			errs = append(errs, fmt.Sprintf("%s: the address must be an IP address", name))
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

// LoadCentralConfig reads the configuration file. A missing file yields the
// defaults. The file may hold the SMTP password, so a warning is returned when
// group or others can access it.
func LoadCentralConfig(path string) (*CentralConfig, []string, error) {
	var warnings []string
	c := &CentralConfig{}
	data, err := os.ReadFile(path)
	switch {
	case errors.Is(err, os.ErrNotExist):
		warnings = append(warnings, fmt.Sprintf("configuration file %s not found; using defaults", path))
	case err != nil:
		return nil, nil, err
	default:
		dec := json.NewDecoder(strings.NewReader(string(data)))
		dec.DisallowUnknownFields()
		if err := dec.Decode(c); err != nil {
			return nil, nil, fmt.Errorf("%s: %w", path, err)
		}
		if err := CheckSecurePath(path); err != nil {
			warnings = append(warnings, "configuration file should be readable by root only: "+err.Error())
		}
	}
	c.applyDefaults()
	if err := c.Validate(); err != nil {
		return nil, nil, fmt.Errorf("%s: %w", path, err)
	}
	return c, warnings, nil
}

// ExampleCentralConfig returns an example configuration as JSON.
func ExampleCentralConfig() string {
	c := DefaultCentralConfig()
	c.ServerNames = []string{"central.example.com", "192.0.2.10"}
	c.SMTP.Host = "smtp.example.com"
	c.SMTP.From = "mimamori@example.com"
	c.SMTP.To = []string{"security@example.com"}
	b, _ := json.MarshalIndent(c, "", "  ")
	return string(b)
}
