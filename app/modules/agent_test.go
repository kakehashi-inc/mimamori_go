package modules

import (
	"crypto/tls"
	"encoding/json"
	"errors"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"mimamori/app/models"
)

// sinceAll returns a since function that gives every root the same time.
func sinceAll(t int64) func(string) int64 {
	return func(string) int64 { return t }
}

func TestExecveatIsExecution(t *testing.T) {
	raw := `type=SYSCALL msg=audit(1700000000.100:10): arch=c000003e syscall=322 success=yes exit=0 euid=48 comm="tool" exe="/usr/bin/tool" key="mimamori"
type=EXECVE msg=audit(1700000000.100:10): argc=2 a0="tool" a1="-v"
`
	c := &RuleContent{Users: []Element{{Value: "apache"}}, Exec: CategoryOnly{Category: "alert"}}
	got := AggregateAuditEvents(ParseAuditRaw(raw), c, map[int]string{48: "apache"})
	if len(got) != 1 || got[0].Type != TypeExec || got[0].Target != "tool -v" {
		t.Fatalf("execveat must be an execution: %+v", got)
	}
}

func TestValidateAbsPathRejectsWhiteSpace(t *testing.T) {
	for _, p := range []string{"/tmp/d/x\v-e\v2", "/tmp/a\fb", "/tmp/a\u00a0b", "/tmp/a\u2028b", "/tmp/a\u0085b", "/tmp/a b", "/tmp/a\"b", "/tmp/a\x7fb"} {
		if ValidateAbsPath(p) == nil {
			t.Fatalf("%q must be rejected", p)
		}
	}
	for _, p := range []string{"/var/www/html", "/opt/app-1/storage_x", "/srv/\u65e5\u672c\u8a9e"} {
		if err := ValidateAbsPath(p); err != nil {
			t.Fatalf("%q: %v", p, err)
		}
	}
}

func TestMissingDirs(t *testing.T) {
	base := t.TempDir()
	got := missingDirs(filepath.Join(base, "a", "b"))
	if len(got) != 2 || got[0] != filepath.Join(base, "a") || got[1] != filepath.Join(base, "a", "b") {
		t.Fatalf("missing dirs: %v", got)
	}
	if err := os.MkdirAll(filepath.Join(base, "a", "b"), 0o700); err != nil {
		t.Fatal(err)
	}
	removeCreated(got)
	if _, err := os.Stat(filepath.Join(base, "a")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("the created parent directories must be removed")
	}
	if _, err := os.Stat(base); err != nil {
		t.Fatal("a directory that existed before must be kept")
	}
	if got := missingDirs(base); len(got) != 0 {
		t.Fatalf("existing directory: %v", got)
	}
}

// fakeCentral starts a TLS server with a certificate signed by signer that
// answers the CA request with offered and the sync request with status.
func fakeCentral(t *testing.T, signer *CertKeyPair, offered []string, status int) (string, int) {
	t.Helper()
	srvCert, err := GenerateServerCert(signer, nil, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	tc, err := srvCert.TLSCertificate()
	if err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/agent/v1/ca", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(CAResponse{CAPEMs: offered})
	})
	mux.HandleFunc("/agent/v1/sync", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(status)
		_ = json.NewEncoder(w).Encode(SyncResponse{AgentName: "a1", TokenExpiresAt: time.Now().Add(time.Hour).Unix()})
	})
	srv := httptest.NewUnstartedServer(mux)
	srv.TLS = &tls.Config{Certificates: []tls.Certificate{*tc}, MinVersion: tls.VersionTLS12}
	srv.Config.ErrorLog = log.New(io.Discard, "", 0)
	srv.StartTLS()
	t.Cleanup(srv.Close)
	host, port, _ := net.SplitHostPort(srv.Listener.Addr().String())
	p, _ := strconv.Atoi(port)
	return host, p
}

func TestInitConnectTrustsOnlyTheFingerprintCA(t *testing.T) {
	genuine, err := GenerateCA(time.Now())
	if err != nil {
		t.Fatal(err)
	}
	attacker, err := GenerateCA(time.Now())
	if err != nil {
		t.Fatal(err)
	}
	offered := []string{genuine.CertPEM, attacker.CertPEM}
	fp := FormatFingerprint(Fingerprint(genuine.Cert))

	// A server holding a certificate of another CA must not be trusted, even
	// when the answer to the CA request also contains the genuine CA.
	host, port := fakeCentral(t, attacker, offered, http.StatusOK)
	if _, _, err := initConnect(AgentInitOptions{CentralHost: host, CentralPort: port, CAFingerprint: fp}, "tok", "id"); err == nil {
		t.Fatal("a certificate signed by a CA other than the given fingerprint must be rejected")
	}

	// The genuine server: only the CA with the fingerprint is kept.
	host, port = fakeCentral(t, genuine, offered, http.StatusOK)
	cfg, _, err := initConnect(AgentInitOptions{CentralHost: host, CentralPort: port, CAFingerprint: fp}, "tok", "id")
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.CAPEMs) != 1 || cfg.CAPEMs[0] != genuine.CertPEM {
		t.Fatalf("trusted CAs: %d", len(cfg.CAPEMs))
	}

	// Without a fingerprint (trust on first use) every offered CA is kept.
	cfg, _, err = initConnect(AgentInitOptions{CentralHost: host, CentralPort: port}, "tok", "id")
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.CAPEMs) != 2 {
		t.Fatalf("trusted CAs without a fingerprint: %d", len(cfg.CAPEMs))
	}
}

func TestInitConnectErrors(t *testing.T) {
	ca, err := GenerateCA(time.Now())
	if err != nil {
		t.Fatal(err)
	}
	// An authentication failure has the documented exit code.
	host, port := fakeCentral(t, ca, []string{ca.CertPEM}, http.StatusUnauthorized)
	_, _, err = initConnect(AgentInitOptions{CentralHost: host, CentralPort: port}, "tok", "id")
	var ee *ExitError
	if !errors.As(err, &ee) || ee.Code != ExitAuth {
		t.Fatalf("authentication failure: %v", err)
	}

	// A host name that is not in the certificate is reported as such.
	host, port = fakeCentral(t, ca, []string{ca.CertPEM}, http.StatusOK)
	tr, err := NewRemoteTransport(&AgentConfig{CentralHost: host, CentralPort: port, CAPEMs: []string{ca.CertPEM}})
	if err != nil {
		t.Fatal(err)
	}
	tr.client.TLS.ServerName = "not-in-the-certificate.example"
	_, err = syncCheck(tr, &SyncRequest{})
	if err == nil || !strings.HasPrefix(err.Error(), "the central host is not one of the certificate names") {
		t.Fatalf("host name mismatch: %v", err)
	}
}

func openAgentDB(t *testing.T) (*AgentRuntime, func()) {
	t.Helper()
	MigrationsFS = os.DirFS("../..")
	db, err := OpenSQLite(filepath.Join(t.TempDir(), "agent.db"), SchemaAgentLocal)
	if err != nil {
		t.Fatal(err)
	}
	return NewAgentRuntime(db, &fakeTransport{fail: true}, nil), func() { db.Close() }
}

// setLocalState writes a state key of the agent database in a write
// transaction of its own.
func setLocalState(t testing.TB, db *DB, key, value string) {
	t.Helper()
	if err := db.Write(func(tx models.Writer) error { return models.SetLocalState(tx, key, value) }); err != nil {
		t.Fatal(err)
	}
}

// queueReport queues req in the outbox the way a report does, in a write
// transaction of its own.
func queueReport(rt *AgentRuntime, req *ReportRequest) error {
	return rt.DB.Write(func(tx models.Writer) error { return enqueue(tx, req) })
}

func TestEnqueueKeepsOneHostInfo(t *testing.T) {
	rt, done := openAgentDB(t)
	defer done()
	apply := &ApplyResult{RuleID: 1, Version: 1, Status: "ok"}
	hi := &HostInfo{Hostname: "h"}
	for i := 0; i < 3; i++ {
		if err := queueReport(rt, &ReportRequest{HostInfo: hi, Apply: apply}); err != nil {
			t.Fatal(err)
		}
	}
	if n := models.CountOutbox(rt.DB.Read()); n != 1 {
		t.Fatalf("reports without anything else to deliver must be replaced: %d rows", n)
	}
	v2 := &ApplyResult{RuleID: 1, Version: 2, Status: "ok"}
	_ = queueReport(rt, &ReportRequest{HostInfo: hi, Apply: v2})
	if n := models.CountOutbox(rt.DB.Read()); n != 2 {
		t.Fatalf("a report with another application result must be kept: %d rows", n)
	}
	_ = queueReport(rt, &ReportRequest{HostInfo: hi, Apply: v2, Detections: []DetectionReport{{Type: TypeExec, Category: CategoryAlert}}})
	_ = queueReport(rt, &ReportRequest{HostInfo: hi, Apply: v2, Dropped: 3})
	items, _ := models.ListOutbox(rt.DB.Read())
	var withHost, withDetections, withDropped int
	for _, it := range items {
		var r ReportRequest
		_ = json.Unmarshal([]byte(it.Payload), &r)
		if r.HostInfo != nil {
			withHost++
		}
		withDetections += len(r.Detections)
		withDropped += r.Dropped
	}
	if len(items) != 3 || withHost != 1 || withDetections != 1 || withDropped != 3 {
		t.Fatalf("rows %d, host info %d, detections %d, dropped %d", len(items), withHost, withDetections, withDropped)
	}
}

func ctimeOf(t *testing.T, p string) int64 {
	t.Helper()
	fi, err := os.Stat(p)
	if err != nil {
		t.Fatal(err)
	}
	return fi.Sys().(*syscall.Stat_t).Ctim.Sec
}

func TestScanFilesNewRootAndFirstScan(t *testing.T) {
	rt, done := openAgentDB(t)
	defer done()
	base := t.TempDir()
	mk := func(p string) string {
		full := filepath.Join(base, p)
		_ = os.MkdirAll(filepath.Dir(full), 0o755)
		if err := os.WriteFile(full, []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
		return full
	}
	c := &RuleContent{
		Writable: WritableSpec{Base: base, AllowedPaths: []string{"storage"}},
		ExecFiles: ExecFilesSpec{Extensions: []string{".php"}, OtherCategory: CategoryAlert,
			Legit: []LegitLocation{{Path: "storage/views", Extensions: []string{".php"}, Category: CategoryNormal}}},
	}
	res := &ResolvedRule{Content: c}
	legit := mk("a/storage/views/pre.php")
	now := ctimeOf(t, legit)

	// First scan in the same second as the legitimate file: not reported,
	// and not reported by the next scan either.
	if out := rt.scanFiles(c, res, time.Unix(now, 0)); len(out) != 0 {
		t.Fatalf("first scan: %+v", out)
	}
	if out := rt.scanFiles(c, res, time.Unix(now+10, 0)); len(out) != 0 {
		t.Fatalf("second scan reported a file of the first scan: %+v", out)
	}

	// A new allowed directory with a file created after the last full scan,
	// found by the next full scan after the watermark moved past the file's
	// status change time.
	rt2, done2 := openAgentDB(t)
	defer done2()
	if out := rt2.scanFiles(c, res, time.Unix(now-100, 0)); len(out) != 0 {
		t.Fatalf("first scan: %+v", out)
	}
	evil := mk("b/storage/evil.php")
	ctime := ctimeOf(t, evil)
	if out := rt2.scanFiles(c, res, time.Unix(ctime+20, 0)); len(out) != 0 {
		t.Fatalf("the directory is not an allowed root yet: %+v", out)
	}
	out := rt2.scanFiles(c, res, time.Unix(now-100+int64(c.ScanInterval()), 0))
	if len(out) != 1 || out[0].Target != evil || out[0].Category != CategoryAlert {
		t.Fatalf("file in a new allowed root: %+v", out)
	}
}

// agent set-central without a port keeps the stored port; with a port it
// uses that port.
func TestAgentSetCentralKeepsPort(t *testing.T) {
	ca, err := GenerateCA(time.Now())
	if err != nil {
		t.Fatal(err)
	}
	host, port := fakeCentral(t, ca, []string{ca.CertPEM}, http.StatusOK)
	MigrationsFS = os.DirFS("../..")
	dir := t.TempDir()
	db, err := OpenSQLite(filepath.Join(dir, AgentLocalDBFileName), SchemaAgentLocal)
	if err != nil {
		t.Fatal(err)
	}
	// The stored host does not answer; the stored port is the fake central's.
	cfg := &AgentConfig{AgentID: "id", CentralHost: "192.0.2.1", CentralPort: port, Token: "tok", CAPEMs: []string{ca.CertPEM}}
	if err := db.Write(cfg.Save); err != nil {
		t.Fatal(err)
	}
	db.Close()
	load := func() *AgentConfig {
		t.Helper()
		db, err := OpenSQLite(filepath.Join(dir, AgentLocalDBFileName), SchemaAgentLocal)
		if err != nil {
			t.Fatal(err)
		}
		defer db.Close()
		c, err := LoadAgentConfig(db.Read())
		if err != nil {
			t.Fatal(err)
		}
		return c
	}

	if err := AgentSetCentral(dir, host, 0); err != nil {
		t.Fatalf("set-central without a port: %v", err)
	}
	if c := load(); c.CentralHost != host || c.CentralPort != port {
		t.Fatalf("after set-central without a port: %s:%d, want %s:%d", c.CentralHost, c.CentralPort, host, port)
	}

	// A given port is used: a port nothing listens on fails and changes
	// nothing.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	closed := ln.Addr().(*net.TCPAddr).Port
	ln.Close()
	if err := AgentSetCentral(dir, host, closed); err == nil || !strings.Contains(err.Error(), "nothing was changed") {
		t.Fatalf("set-central to a closed port: %v", err)
	}
	if c := load(); c.CentralPort != port {
		t.Fatalf("port changed by a failed set-central: %d", c.CentralPort)
	}
	if err := AgentSetCentral(dir, host, port); err != nil {
		t.Fatalf("set-central with the port: %v", err)
	}

	// The command line: --port omitted keeps the port, --port 0 is refused.
	zero := 0
	var ee *ExitError
	if err := (&AgentSetCentralCmd{Host: host, Port: &zero}).Run(); !errors.As(err, &ee) || ee.Code != ExitArgument {
		t.Fatalf("--port 0: %v", err)
	}
}
