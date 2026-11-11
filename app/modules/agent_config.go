package modules

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// AgentConfig is the agent's configuration file (root-only, mode 0600).
type AgentConfig struct {
	ServerURL      string   `json:"server_url"`
	Token          string   `json:"token"`
	TokenName      string   `json:"token_name"`
	TokenExpiresAt int64    `json:"token_expires_at"`
	CAPEMs         []string `json:"ca_pems"`
	InitializedAt  int64    `json:"initialized_at"`
}

// LoadAgentConfig reads and checks the configuration file.
func LoadAgentConfig(path string) (*AgentConfig, error) {
	if err := CheckSecurePath(path); err != nil {
		return nil, err
	}
	if err := CheckSecurePath(filepath.Dir(path)); err != nil {
		return nil, err
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var c AgentConfig
	if err := json.Unmarshal(data, &c); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	if c.ServerURL == "" || c.Token == "" || len(c.CAPEMs) == 0 {
		return nil, fmt.Errorf("%s: incomplete configuration; run agent init", path)
	}
	return &c, nil
}

// Save writes the configuration atomically with mode 0600 so a crash never
// leaves a broken file.
func (c *AgentConfig) Save(path string) error {
	data, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return err
	}
	return WriteFileAtomic(path, data, 0o600)
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
