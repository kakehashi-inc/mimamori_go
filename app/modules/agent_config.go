package modules

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"regexp"
	"strconv"
	"time"

	"mimamori/app/models"
)

// AgentConfig is the agent's configuration. It is stored in the state table
// of the agent's local database (keys "config.<name>"), which is closed to
// other users like the rest of the data directory.
type AgentConfig struct {
	// AgentID identifies this installation; the central binds the token to
	// it, so another installation cannot use the same token.
	AgentID string
	// CentralHost is the host name or IP address of the central and
	// CentralPort the port of its agent listener.
	CentralHost    string
	CentralPort    int
	Token          string
	TokenName      string
	TokenExpiresAt int64
	CAPEMs         []string
	InitializedAt  int64
}

const (
	agentCfgID          = "config.agent_id"
	agentCfgCentralHost = "config.central_host"
	agentCfgCentralPort = "config.central_port"
	agentCfgToken       = "config.token"
	agentCfgTokenName   = "config.token_name"
	agentCfgTokenExpiry = "config.token_expires_at"
	agentCfgCAs         = "config.ca_pems"
	agentCfgInitialized = "config.initialized_at"
)

// ErrAgentNotInitialized is returned when the local database holds no
// configuration.
var ErrAgentNotInitialized = errors.New("the agent is not initialized; run agent init")

// LoadAgentConfig reads the configuration from the agent's local database.
func LoadAgentConfig(db models.Reader) (*AgentConfig, error) {
	c := &AgentConfig{
		AgentID:     models.GetLocalState(db, agentCfgID),
		CentralHost: models.GetLocalState(db, agentCfgCentralHost),
		Token:       models.GetLocalState(db, agentCfgToken),
		TokenName:   models.GetLocalState(db, agentCfgTokenName),
	}
	c.CentralPort, _ = strconv.Atoi(models.GetLocalState(db, agentCfgCentralPort))
	c.TokenExpiresAt, _ = strconv.ParseInt(models.GetLocalState(db, agentCfgTokenExpiry), 10, 64)
	c.InitializedAt, _ = strconv.ParseInt(models.GetLocalState(db, agentCfgInitialized), 10, 64)
	if v := models.GetLocalState(db, agentCfgCAs); v != "" {
		if err := json.Unmarshal([]byte(v), &c.CAPEMs); err != nil {
			return nil, fmt.Errorf("trusted CAs: %w", err)
		}
	}
	if c.CentralHost == "" && c.Token == "" {
		return nil, ErrAgentNotInitialized
	}
	if c.AgentID == "" || c.CentralHost == "" || c.CentralPort == 0 || c.Token == "" || len(c.CAPEMs) == 0 {
		return nil, errors.New("incomplete configuration; run agent init --overwrite")
	}
	return c, nil
}

// Save writes the configuration in one transaction, so a crash never leaves
// a partly updated configuration.
func (c *AgentConfig) Save(db *sql.DB) error {
	cas, err := json.Marshal(c.CAPEMs)
	if err != nil {
		return err
	}
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	for _, kv := range [][2]string{
		{agentCfgID, c.AgentID},
		{agentCfgCentralHost, c.CentralHost},
		{agentCfgCentralPort, strconv.Itoa(c.CentralPort)},
		{agentCfgToken, c.Token},
		{agentCfgTokenName, c.TokenName},
		{agentCfgTokenExpiry, strconv.FormatInt(c.TokenExpiresAt, 10)},
		{agentCfgCAs, string(cas)},
		{agentCfgInitialized, strconv.FormatInt(c.InitializedAt, 10)},
	} {
		if err := models.SetLocalState(tx, kv[0], kv[1]); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// MergeCAs adds new CA certificates and drops expired ones. It returns true
// when the list changed.
func (c *AgentConfig) MergeCAs(pems []string, now time.Time) bool {
	seen := map[string]bool{}
	var out []string
	changed := false
	add := func(p string) {
		cert, err := ParseCertPEM(p)
		if err != nil {
			return
		}
		fp := Fingerprint(cert)
		if seen[fp] {
			return
		}
		if now.After(cert.NotAfter) {
			changed = true
			return
		}
		seen[fp] = true
		out = append(out, p)
	}
	for _, p := range c.CAPEMs {
		add(p)
	}
	n := len(out)
	for _, p := range pems {
		add(p)
	}
	if len(out) != n {
		changed = true
	}
	c.CAPEMs = out
	return changed
}

// CAFingerprints returns the fingerprints of the trusted CAs.
func CAFingerprintsOf(pems []string) []string {
	var out []string
	for _, p := range pems {
		if cert, err := ParseCertPEM(p); err == nil {
			out = append(out, Fingerprint(cert))
		}
	}
	return out
}

// CentralAddress returns host:port of the central for messages.
func (c *AgentConfig) CentralAddress() string {
	return net.JoinHostPort(c.CentralHost, strconv.Itoa(c.CentralPort))
}

// centralHostRe matches a host name (letters, digits, hyphens and dots).
var centralHostRe = regexp.MustCompile(`^[A-Za-z0-9]([A-Za-z0-9-]*[A-Za-z0-9])?(\.[A-Za-z0-9]([A-Za-z0-9-]*[A-Za-z0-9])?)*$`)

// ValidateCentral checks the host name or IP address of the central (no
// scheme, port or path) and the port.
func ValidateCentral(host string, port int) error {
	if net.ParseIP(host) == nil && (len(host) > 253 || !centralHostRe.MatchString(host)) {
		return fmt.Errorf("central host %q must be a host name or an IP address (without scheme, port or path)", host)
	}
	if port < 1 || port > 65535 {
		return fmt.Errorf("central port %d must be 1-65535", port)
	}
	return nil
}
