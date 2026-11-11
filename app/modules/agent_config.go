package modules

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
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
	AgentID        string
	ServerURL      string
	Token          string
	TokenName      string
	TokenExpiresAt int64
	CAPEMs         []string
	InitializedAt  int64
}

const (
	agentCfgID          = "config.agent_id"
	agentCfgServerURL   = "config.server_url"
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
func LoadAgentConfig(db models.Execer) (*AgentConfig, error) {
	c := &AgentConfig{
		AgentID:   models.GetLocalState(db, agentCfgID),
		ServerURL: models.GetLocalState(db, agentCfgServerURL),
		Token:     models.GetLocalState(db, agentCfgToken),
		TokenName: models.GetLocalState(db, agentCfgTokenName),
	}
	c.TokenExpiresAt, _ = strconv.ParseInt(models.GetLocalState(db, agentCfgTokenExpiry), 10, 64)
	c.InitializedAt, _ = strconv.ParseInt(models.GetLocalState(db, agentCfgInitialized), 10, 64)
	if v := models.GetLocalState(db, agentCfgCAs); v != "" {
		if err := json.Unmarshal([]byte(v), &c.CAPEMs); err != nil {
			return nil, fmt.Errorf("trusted CAs: %w", err)
		}
	}
	if c.ServerURL == "" && c.Token == "" {
		return nil, ErrAgentNotInitialized
	}
	if c.AgentID == "" || c.ServerURL == "" || c.Token == "" || len(c.CAPEMs) == 0 {
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
		{agentCfgServerURL, c.ServerURL},
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
