package modules

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"mimamori/app/models"
)

// RemoteTransport talks to the central over HTTPS with the agent's token.
type RemoteTransport struct {
	client     *HTTPSClient
	instanceID string
}

// NewRemoteTransport prepares the HTTPS client for an agent configuration.
func NewRemoteTransport(cfg *AgentConfig) (*RemoteTransport, error) {
	tlsCfg, err := AgentTLSConfig(cfg.CAPEMs)
	if err != nil {
		return nil, err
	}
	c, err := NewHTTPSClient(cfg.CentralHost, cfg.CentralPort, tlsCfg)
	if err != nil {
		return nil, err
	}
	t := &RemoteTransport{client: c, instanceID: cfg.AgentID}
	t.SetToken(cfg.Token)
	return t, nil
}

// SetToken changes the token used for authentication.
func (t *RemoteTransport) SetToken(token string) {
	t.client.Headers["Authorization"] = "Bearer " + token
	t.client.Headers[HeaderInstanceID] = t.instanceID
}

// SetCAs replaces the trusted CAs.
func (t *RemoteTransport) SetCAs(pems []string) {
	if cfg, err := AgentTLSConfig(pems); err == nil {
		t.client.TLS = cfg
	}
}

// Sync implements AgentTransport.
func (t *RemoteTransport) Sync(req *SyncRequest) (*SyncResponse, error) {
	var resp SyncResponse
	if err := t.client.JSON("POST", "/agent/v1/sync", req, &resp); err != nil {
		return nil, err
	}
	return &resp, nil
}

// Report implements AgentTransport.
func (t *RemoteTransport) Report(req *ReportRequest) error {
	return t.client.JSON("POST", "/agent/v1/report", req, nil)
}

// AgentInitOptions are the arguments of "agent init".
type AgentInitOptions struct {
	CentralHost   string
	CentralPort   int
	Overwrite     bool
	CAFingerprint string
	DataDir       string
}

// AgentInit initializes the agent: it checks the prerequisites in order and
// creates the local database (holding the configuration) only when every
// check passed.
func AgentInit(o AgentInitOptions) error {
	if err := ValidateCentral(o.CentralHost, o.CentralPort); err != nil {
		return NewExitError(ExitArgument, err.Error())
	}
	// 1. auditd (the tools exist and auditctl can query the kernel, which
	// also tells whether this user is permitted to control auditing)
	if err := CheckAuditd(); err != nil {
		return NewExitError(ExitConfig, err.Error())
	}
	// 2. existing initialization (the agent ID is kept on re-initialization;
	// the agent must be stopped, because a running agent writes back its own
	// copy of the configuration)
	dbPath := filepath.Join(o.DataDir, AgentLocalDBFileName)
	lockPath := filepath.Join(o.DataDir, AgentLockFileName)
	agentID := ""
	var created []string
	if _, err := os.Stat(dbPath); err == nil {
		db, err := OpenSQLite(dbPath, SchemaAgentLocal)
		if err != nil {
			return DBOpenFailed(err, ExitFileIO, "local database")
		}
		agentID = models.GetLocalState(db.Read(), agentCfgID)
		_, cfgErr := LoadAgentConfig(db.Read())
		db.Close()
		if !errors.Is(cfgErr, ErrAgentNotInitialized) && !o.Overwrite {
			return NewExitError(ExitArgument, "the agent is already initialized; use --overwrite to replace it")
		}
		if _, err := os.Stat(lockPath); errors.Is(err, os.ErrNotExist) {
			created = append(created, lockPath)
		}
		lock, err := AcquireLock(lockPath)
		if err != nil {
			removeCreated(created)
			return NewExitError(ExitConfig, "the agent is running; stop it before re-initializing")
		}
		defer lock.Release()
	}
	token, err := ReadSecret("Token: ")
	if err != nil {
		removeCreated(created)
		return NewExitError(ExitArgument, err.Error())
	}
	if agentID == "" {
		if agentID, err = RandomHex(16); err != nil {
			removeCreated(created)
			return err
		}
	}
	// 3. storage: the data directory and the local database (prepared before
	// the token is bound to this agent instance, so a storage failure leaves
	// the token unbound)
	created = append(created, missingDirs(o.DataDir)...)
	if err := EnsureDataDir(o.DataDir); err != nil {
		removeCreated(created)
		return NewExitErrorf(ExitFileIO, "data directory %s: %v", o.DataDir, err)
	}
	if _, err := os.Stat(dbPath); errors.Is(err, os.ErrNotExist) {
		for _, p := range []string{dbPath, dbPath + "-wal", dbPath + "-shm"} {
			if _, err := os.Lstat(p); errors.Is(err, os.ErrNotExist) {
				created = append(created, p)
			}
		}
	}
	db, err := OpenSQLite(dbPath, SchemaAgentLocal)
	if err != nil {
		removeCreated(created)
		return DBOpenFailed(err, ExitFileIO, "local database")
	}
	// 4. connect, receive the CA, authenticate (binds the token to this agent)
	cfg, resp, err := initConnect(o, token, agentID)
	if err != nil {
		db.Close()
		removeCreated(created)
		var ee *ExitError
		if errors.As(err, &ee) {
			return ee
		}
		return NewExitError(ExitExec, err.Error())
	}
	// 5. save the configuration; a rule stored by the previous
	// initialization is cleared (the next sync applies the assigned rule or
	// removes the audit rules). Both are one transaction.
	err = db.Write(func(tx models.Writer) error {
		if err := cfg.Save(tx); err != nil {
			return err
		}
		return resetStoredRule(tx)
	})
	db.Close()
	if err != nil {
		removeCreated(created)
		return NewExitErrorf(ExitFileIO, "local database: %v", err)
	}

	for _, h := range HaltActions() {
		fmt.Fprintf(os.Stderr, "Warning: %s in %s halts or single-users the host on disk or queue problems; change it (see the setup guide)\n", h, AuditdConfFile)
	}
	fmt.Printf("Initialized.\n")
	fmt.Printf("  token name:  %s\n", resp.AgentName)
	fmt.Printf("  expires:     %s\n", time.Unix(cfg.TokenExpiresAt, 0).Local().Format("2006-01-02 15:04:05"))
	if resp.Rule != nil {
		fmt.Printf("  rule:        %s (version %d)\n", resp.Rule.Name, resp.Rule.Version)
	} else {
		fmt.Printf("  rule:        none (the agent only reports host information until a rule is applied)\n")
	}
	fmt.Printf("  CA:          %s\n", FormatFingerprint(CAFingerprintsOf(cfg.CAPEMs)[0]))
	fmt.Printf("Start the agent with: ./%s agent start\n", AppName)
	return nil
}

// missingDirs returns dir and those of its parent directories that do not
// exist, outermost first, so that removeCreated removes the innermost first.
func missingDirs(dir string) []string {
	var out []string
	for d := filepath.Clean(dir); ; {
		if _, err := os.Lstat(d); !errors.Is(err, os.ErrNotExist) {
			break
		}
		out = append([]string{d}, out...)
		parent := filepath.Dir(d)
		if parent == d {
			break
		}
		d = parent
	}
	return out
}

// removeCreated removes what this run created, newest first (a directory
// only when it is empty).
func removeCreated(paths []string) {
	for i := len(paths) - 1; i >= 0; i-- {
		_ = os.Remove(paths[i])
	}
}

func initConnect(o AgentInitOptions, token, agentID string) (*AgentConfig, *SyncResponse, error) {
	// Receive the CA without verification first (trust on first use, or the
	// fingerprint given by the administrator), then verify the server
	// certificate against it.
	probe, err := NewHTTPSClient(o.CentralHost, o.CentralPort, &tls.Config{InsecureSkipVerify: true, MinVersion: tls.VersionTLS12}) //nolint:gosec // the CA is verified below
	if err != nil {
		return nil, nil, err
	}
	var ca CAResponse
	if err := probe.JSON("GET", "/agent/v1/ca", nil, &ca); err != nil {
		return nil, nil, fmt.Errorf("cannot receive the CA certificate: %w", err)
	}
	if len(ca.CAPEMs) == 0 {
		return nil, nil, errors.New("the central returned no CA certificate")
	}
	fps := CAFingerprintsOf(ca.CAPEMs)
	trusted := ca.CAPEMs
	if want := NormalizeFingerprint(o.CAFingerprint); want != "" {
		// Trust only the CA with the given fingerprint: the unverified answer
		// may hold other certificates beside it.
		trusted = nil
		for _, p := range ca.CAPEMs {
			if cert, err := ParseCertPEM(p); err == nil && Fingerprint(cert) == want {
				trusted = []string{p}
				break
			}
		}
		if trusted == nil {
			return nil, nil, fmt.Errorf("CA fingerprint mismatch: the central offered %s", FormatFingerprint(fps[0]))
		}
	}
	cfg := &AgentConfig{AgentID: agentID, CentralHost: o.CentralHost, CentralPort: o.CentralPort, Token: token, InitializedAt: time.Now().Unix()}
	cfg.MergeCAs(trusted, time.Now())
	tr, err := NewRemoteTransport(cfg)
	if err != nil {
		return nil, nil, err
	}
	resp, err := syncCheck(tr, &SyncRequest{AgentVersion: AppVersion, CAFingerprints: CAFingerprintsOf(cfg.CAPEMs)})
	if err != nil {
		return nil, nil, err
	}
	cfg.applySyncResponse(resp)
	return cfg, resp, nil
}

// syncCheck performs one sync and turns the usual failures into messages.
func syncCheck(tr *RemoteTransport, req *SyncRequest) (*SyncResponse, error) {
	resp, err := tr.Sync(req)
	if err != nil {
		var he *HTTPError
		if errors.As(err, &he) && he.Status == 401 {
			return nil, NewExitError(ExitAuth, "authentication failed: the token is invalid, expired, revoked or bound to another agent instance")
		}
		var hn x509.HostnameError
		if errors.As(err, &hn) {
			return nil, fmt.Errorf("the central host is not one of the certificate names (server_names): %w", err)
		}
		var ue x509.UnknownAuthorityError
		if errors.As(err, &ue) || IsCertificateError(err) {
			return nil, fmt.Errorf("the central certificate does not match its CA (check that the central host is one of the certificate names, server_names): %w", err)
		}
		return nil, err
	}
	return resp, nil
}

// applySyncResponse takes the token name, expiry, renewed token and CAs of a
// sync response.
func (c *AgentConfig) applySyncResponse(resp *SyncResponse) {
	c.TokenName = resp.AgentName
	c.TokenExpiresAt = resp.TokenExpiresAt
	if resp.TokenRenewal != nil {
		c.Token = resp.TokenRenewal.Token
		c.TokenExpiresAt = resp.TokenRenewal.ExpiresAt
	}
	c.MergeCAs(resp.CAPEMs, time.Now())
}

// AgentSetCentral changes the central host (and port) of an initialized
// agent. The agent must be stopped. port 0 keeps the stored port. The new
// address is saved only after a sync with it succeeded (the token and the
// trusted CAs are kept).
func AgentSetCentral(dataDir, host string, port int) error {
	// The stored port is valid already, so a kept port is checked as 1.
	if err := ValidateCentral(host, max(port, 1)); err != nil {
		return NewExitError(ExitArgument, err.Error())
	}
	dbPath := filepath.Join(dataDir, AgentLocalDBFileName)
	if _, err := os.Stat(dbPath); err != nil {
		return NewExitError(ExitConfig, ErrAgentNotInitialized.Error())
	}
	// An agent.db of another build is refused before agent.lock is
	// written, so that the refused data directory is left as it is.
	if err := CheckSQLiteBuild(dbPath, SchemaAgentLocal); IsOtherBuildDB(err) {
		return DBOpenFailed(err, ExitConfig, "local database")
	}
	lock, err := AcquireLock(filepath.Join(dataDir, AgentLockFileName))
	if err != nil {
		return NewExitError(ExitConfig, "the agent is running; stop it before changing the central host")
	}
	defer lock.Release()
	db, err := OpenSQLite(dbPath, SchemaAgentLocal)
	if err != nil {
		return DBOpenFailed(err, ExitConfig, "local database")
	}
	defer db.Close()
	cfg, err := LoadAgentConfig(db.Read())
	if err != nil {
		return NewExitErrorf(ExitConfig, "configuration: %v", err)
	}
	prev := cfg.CentralAddress()
	cfg.CentralHost = host
	if port != 0 {
		cfg.CentralPort = port
	}
	tr, err := NewRemoteTransport(cfg)
	if err != nil {
		return NewExitError(ExitConfig, err.Error())
	}
	// Report the rule in effect so the central's view does not change.
	req := &SyncRequest{AgentVersion: AppVersion, CAFingerprints: CAFingerprintsOf(cfg.CAPEMs)}
	var rule RuleAssignment
	if json.Unmarshal([]byte(models.GetLocalState(db.Read(), stateRule)), &rule) == nil && rule.ID > 0 {
		req.RuleID, req.RuleVersion = rule.ID, rule.Version
	}
	var ap ApplyResult
	if json.Unmarshal([]byte(models.GetLocalState(db.Read(), stateApply)), &ap) == nil && ap.Status != "" {
		req.LastApply = &ap
	}
	resp, err := syncCheck(tr, req)
	if err != nil {
		code := ExitExec
		var ee *ExitError
		if errors.As(err, &ee) {
			code = ee.Code
		}
		return NewExitErrorf(code, "cannot use %s: %v (nothing was changed)", cfg.CentralAddress(), err)
	}
	cfg.applySyncResponse(resp)
	if err := db.Write(cfg.Save); err != nil {
		return NewExitErrorf(ExitFileIO, "local database: %v", err)
	}
	fmt.Printf("Central changed from %s to %s.\n", prev, cfg.CentralAddress())
	return nil
}

// AgentStart runs the agent with its saved worker count (the calling flow
// is its first worker) until SIGINT or SIGTERM closes its stop channel.
func AgentStart(dataDir string) error {
	logger := log.New(os.Stderr, "", log.LstdFlags)
	if err := CheckAuditd(); err != nil {
		return NewExitError(ExitConfig, err.Error())
	}
	dbPath := filepath.Join(dataDir, AgentLocalDBFileName)
	if _, err := os.Stat(dbPath); err != nil {
		return NewExitError(ExitConfig, ErrAgentNotInitialized.Error())
	}
	// An agent.db of another build is refused before agent.lock is
	// written, so that the refused data directory is left as it is.
	if err := CheckSQLiteBuild(dbPath, SchemaAgentLocal); IsOtherBuildDB(err) {
		return DBOpenFailed(err, ExitConfig, "local database")
	}
	lock, err := AcquireLock(filepath.Join(dataDir, AgentLockFileName))
	if err != nil {
		return NewExitError(ExitConfig, err.Error())
	}
	defer lock.Release()
	db, err := OpenSQLite(dbPath, SchemaAgentLocal)
	if err != nil {
		return DBOpenFailed(err, ExitConfig, "local database")
	}
	defer db.Close()
	cfg, err := LoadAgentConfig(db.Read())
	if err != nil {
		return NewExitErrorf(ExitConfig, "configuration: %v", err)
	}
	workers, err := LoadAgentWorkers(db.Read())
	if err != nil {
		return NewExitError(ExitConfig, err.Error())
	}
	tr, err := NewRemoteTransport(cfg)
	if err != nil {
		return NewExitError(ExitConfig, err.Error())
	}
	rt := NewAgentRuntime(db, tr, logger)
	rt.SetWorkers(workers)
	rt.mon.config = cfg
	// The transport uses the token and the CAs the runtime saved last.
	rt.mon.onConfigSaved = func(c *AgentConfig) {
		tr.SetToken(c.Token)
		tr.SetCAs(c.CAPEMs)
	}
	stopCtx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()
	logger.Printf("%s agent %s started (central %s, token %s, agent workers %d)", AppName, AppVersion, cfg.CentralAddress(), cfg.TokenName, workers)
	err = rt.Run(stopCtx.Done())
	logger.Printf("agent stopped")
	if err != nil {
		return NewExitError(ExitGeneral, err.Error())
	}
	return nil
}

// marshalIndent is a helper for command output. Values are printed as they
// are (& < > are not escaped).
func marshalIndent(v any) string {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	_ = enc.Encode(v)
	return strings.TrimSuffix(buf.String(), "\n")
}
