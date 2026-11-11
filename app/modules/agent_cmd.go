package modules

import (
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"
)

// RemoteTransport talks to the central over HTTPS with the agent's token.
type RemoteTransport struct {
	client    *HTTPSClient
	machineID string
}

// NewRemoteTransport prepares the HTTPS client for an agent configuration.
func NewRemoteTransport(cfg *AgentConfig, machineID string) (*RemoteTransport, error) {
	tlsCfg, err := AgentTLSConfig(cfg.CAPEMs)
	if err != nil {
		return nil, err
	}
	c, err := NewHTTPSClient(cfg.ServerURL, tlsCfg, DefaultHTTPTimeoutSec*time.Second)
	if err != nil {
		return nil, err
	}
	t := &RemoteTransport{client: c, machineID: machineID}
	t.SetToken(cfg.Token)
	return t, nil
}

// SetToken changes the token used for authentication.
func (t *RemoteTransport) SetToken(token string) {
	t.client.Headers["Authorization"] = "Bearer " + token
	t.client.Headers[HeaderMachineID] = t.machineID
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
	ServerURL     string
	Overwrite     bool
	CAFingerprint string
	ConfigPath    string
	DataDir       string
}

// AgentInit initializes the agent: it checks the prerequisites in order and
// saves the configuration only when every check passed.
func AgentInit(o AgentInitOptions) error {
	// 1. root
	if os.Geteuid() != 0 {
		return NewExitError(ExitAuth, "agent init must be run as root")
	}
	// 2. auditd
	if err := CheckAuditd(); err != nil {
		return NewExitError(ExitConfig, err.Error())
	}
	// 3. existing configuration
	if _, err := os.Stat(o.ConfigPath); err == nil && !o.Overwrite {
		return NewExitErrorf(ExitArgument, "%s already exists; use --overwrite to replace it", o.ConfigPath)
	}
	token, err := ReadSecret("Token: ")
	if err != nil {
		return NewExitError(ExitArgument, err.Error())
	}
	// 4. storage locations
	var created []string
	for _, d := range []string{filepath.Dir(o.ConfigPath), o.DataDir} {
		if _, err := os.Stat(d); errors.Is(err, os.ErrNotExist) {
			created = append(created, d)
		}
		if err := EnsureSecureDir(d); err != nil {
			removeCreated(created)
			return NewExitErrorf(ExitFileIO, "cannot secure %s: %v", d, err)
		}
	}
	// 5. connect, receive the CA, authenticate (binds the token to this machine)
	cfg, resp, err := initConnect(o, token)
	if err != nil {
		removeCreated(created)
		return NewExitError(ExitExec, err.Error())
	}
	if err := cfg.Save(o.ConfigPath); err != nil {
		removeCreated(created)
		return NewExitErrorf(ExitFileIO, "saving %s: %v", o.ConfigPath, err)
	}

	for _, h := range HaltActions() {
		fmt.Fprintf(os.Stderr, "Warning: %s in %s halts or single-users the host on disk problems; change it (see the setup guide)\n", h, AuditdConfFile)
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
	fmt.Printf("Start the agent with: %s agent start\n", AppName)
	return nil
}

func removeCreated(dirs []string) {
	for i := len(dirs) - 1; i >= 0; i-- {
		_ = os.Remove(dirs[i]) // only removes empty directories
	}
}

func initConnect(o AgentInitOptions, token string) (*AgentConfig, *SyncResponse, error) {
	// Receive the CA without verification first (trust on first use, or the
	// fingerprint given by the administrator), then verify the server
	// certificate against it.
	probe, err := NewHTTPSClient(o.ServerURL, &tls.Config{InsecureSkipVerify: true, MinVersion: tls.VersionTLS12}, DefaultHTTPTimeoutSec*time.Second) //nolint:gosec // the CA is verified below
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
	if want := NormalizeFingerprint(o.CAFingerprint); want != "" {
		found := false
		for _, fp := range fps {
			if fp == want {
				found = true
			}
		}
		if !found {
			return nil, nil, fmt.Errorf("CA fingerprint mismatch: the central offered %s", FormatFingerprint(fps[0]))
		}
	}
	cfg := &AgentConfig{ServerURL: o.ServerURL, Token: token, InitializedAt: time.Now().Unix()}
	cfg.MergeCAs(ca.CAPEMs, time.Now())
	machineID, err := ReadMachineID()
	if err != nil {
		return nil, nil, err
	}
	tr, err := NewRemoteTransport(cfg, machineID)
	if err != nil {
		return nil, nil, err
	}
	resp, err := tr.Sync(&SyncRequest{AgentVersion: AppVersion, CAFingerprints: CAFingerprintsOf(cfg.CAPEMs)})
	if err != nil {
		var he *HTTPError
		if errors.As(err, &he) && he.Status == 401 {
			return nil, nil, errors.New("authentication failed: the token is invalid, expired, revoked or bound to another machine")
		}
		var ue x509.UnknownAuthorityError
		if errors.As(err, &ue) || IsCertificateError(err) {
			return nil, nil, fmt.Errorf("the central certificate does not match its CA (check the server URL is one of the certificate names): %w", err)
		}
		return nil, nil, err
	}
	cfg.TokenName = resp.AgentName
	cfg.TokenExpiresAt = resp.TokenExpiresAt
	if resp.TokenRenewal != nil {
		cfg.Token = resp.TokenRenewal.Token
		cfg.TokenExpiresAt = resp.TokenRenewal.ExpiresAt
	}
	cfg.MergeCAs(resp.CAPEMs, time.Now())
	return cfg, resp, nil
}

// AgentStart runs the agent in the calling flow until SIGINT or SIGTERM.
func AgentStart(configPath, dataDir string) error {
	logger := log.New(os.Stderr, "", log.LstdFlags)
	if os.Geteuid() != 0 {
		return NewExitError(ExitAuth, "the agent must run as root")
	}
	if err := CheckAuditd(); err != nil {
		return NewExitError(ExitConfig, err.Error())
	}
	cfg, err := LoadAgentConfig(configPath)
	if err != nil {
		return NewExitErrorf(ExitConfig, "configuration: %v", err)
	}
	if err := CheckSecurePath(dataDir); err != nil {
		return NewExitErrorf(ExitConfig, "data directory: %v", err)
	}
	lock, err := AcquireLock(filepath.Join(dataDir, AgentLockFileName))
	if err != nil {
		return NewExitError(ExitConfig, err.Error())
	}
	defer lock.Release()
	db, err := OpenSQLite(filepath.Join(dataDir, AgentLocalDBFileName), SchemaAgentLocal)
	if err != nil {
		return NewExitErrorf(ExitConfig, "local database: %v", err)
	}
	defer db.Close()
	machineID, err := ReadMachineID()
	if err != nil {
		return NewExitError(ExitConfig, err.Error())
	}
	tr, err := NewRemoteTransport(cfg, machineID)
	if err != nil {
		return NewExitError(ExitConfig, err.Error())
	}
	rt := NewAgentRuntime(db, tr, dataDir, logger)
	rt.Config, rt.ConfigPath = cfg, configPath
	rt.OnTokenRenewed = tr.SetToken
	prevCAs := len(cfg.CAPEMs)

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	defer signal.Stop(sigCh)
	stop := false
	stopping := func() bool {
		if stop {
			return true
		}
		select {
		case <-sigCh:
			stop = true
		default:
		}
		if len(cfg.CAPEMs) != prevCAs {
			tr.SetCAs(cfg.CAPEMs)
			prevCAs = len(cfg.CAPEMs)
		}
		return stop
	}
	wait := func(d time.Duration) bool {
		t := time.NewTimer(d)
		defer t.Stop()
		select {
		case <-sigCh:
			stop = true
			return false
		case <-t.C:
			return true
		}
	}
	logger.Printf("%s agent %s started (central %s, token %s)", AppName, AppVersion, cfg.ServerURL, cfg.TokenName)
	rt.Run(wait, stopping)
	logger.Printf("agent stopped")
	return nil
}

// marshalIndent is a helper for command output.
func marshalIndent(v any) string {
	b, _ := json.MarshalIndent(v, "", "  ")
	return string(b)
}
