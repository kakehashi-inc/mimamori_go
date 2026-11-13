package modules

import (
	"bufio"
	"database/sql"
	"net"
	"net/smtp"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"mimamori/app/models"
)

// TestOpenSQLitePathCharacters checks that characters with a meaning in an
// SQLite URI do not move the database out of its directory.
func TestOpenSQLitePathCharacters(t *testing.T) {
	MigrationsFS = os.DirFS("../..")
	for _, name := range []string{"a#b", "a?b", "a%b", "a b", "100%41"} {
		t.Run(name, func(t *testing.T) {
			parent := t.TempDir()
			path := filepath.Join(parent, name, "central.db")
			db, err := OpenSQLitePool(path, SchemaCentral)
			if err != nil {
				t.Fatal(err)
			}
			db.Close()
			if _, err := os.Stat(path); err != nil {
				t.Fatalf("database not at %s: %v", path, err)
			}
			entries, _ := os.ReadDir(parent)
			if len(entries) != 1 || entries[0].Name() != name {
				t.Fatalf("unexpected files beside the data directory: %v", entries)
			}
		})
	}
}

// TestOpenSQLiteKeepsExistingMode checks that opening an existing database
// does not change the mode of its file.
func TestOpenSQLiteKeepsExistingMode(t *testing.T) {
	MigrationsFS = os.DirFS("../..")
	path := filepath.Join(t.TempDir(), "central.db")
	db, err := OpenSQLitePool(path, SchemaCentral)
	if err != nil {
		t.Fatal(err)
	}
	db.Close()
	if err := os.Chmod(path, 0o640); err != nil {
		t.Fatal(err)
	}
	db, err = OpenSQLitePool(path, SchemaCentral)
	if err != nil {
		t.Fatal(err)
	}
	db.Close()
	if fi, _ := os.Stat(path); fi.Mode().Perm() != 0o640 {
		t.Fatalf("mode changed to %o", fi.Mode().Perm())
	}
}

// TestAcquireLockWaitsForStatusCheck checks that a start does not fail
// while "status" holds the shared lock for a moment.
func TestAcquireLockWaitsForStatusCheck(t *testing.T) {
	path := filepath.Join(t.TempDir(), "x.lock")
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	fd := int(f.Fd())
	if err := syscall.Flock(fd, syscall.LOCK_SH|syscall.LOCK_NB); err != nil {
		f.Close()
		t.Fatal(err)
	}
	// The timer's flow unlocks through the descriptor, so the file is closed
	// only after that flow has ended.
	released := make(chan struct{})
	time.AfterFunc(150*time.Millisecond, func() {
		_ = syscall.Flock(fd, syscall.LOCK_UN)
		close(released)
	})
	defer func() {
		<-released
		f.Close()
	}()
	l, err := AcquireLock(path)
	if err != nil {
		t.Fatalf("lock not taken after the shared lock was released: %v", err)
	}
	l.Release()
}

// applyConfig applies configuration changes in one transaction, as the
// commands and the settings screen do.
func applyConfig(db *sql.DB, set map[string]string, unset []string) (changed []string, err error) {
	err = withLocalTx(db, func(tx models.Writer) error {
		changed, err = ApplyCentralConfigChanges(tx, set, unset)
		return err
	})
	return changed, err
}

// TestCentralConfigValidation checks the checks made when items are saved.
func TestCentralConfigValidation(t *testing.T) {
	MigrationsFS = os.DirFS("../..")
	db, err := OpenSQLitePool(filepath.Join(t.TempDir(), "central.db"), SchemaCentral)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err := DefaultCentralConfig().Validate(); err != nil {
		t.Fatalf("defaults rejected: %v", err)
	}
	bad := map[string][]string{
		"web_listen":              {"127.0.0.1:65536", "127.0.0.1:abc", "127.0.0.1:-1", "127.0.0.1:0", ""},
		"agent_listen":            {"127.0.0.1:0", " "},
		"smtp.security":           {""},
		"notify.subject_prefix":   {""},
		"server_names":            {"bad_name!", "a b", "http://x", "a.example.com:9080", "*.example.com", "a\nb"},
		"smtp.host":               {"http://x", "a b"},
		"smtp.from":               {"not mail", "Name <a@example.com>", "a@example.com,b@example.com"},
		"smtp.helo_name":          {"a\rb"},
		"smtp.username":           {"a\x00b"},
		"token_valid_days":        {"30"},
		"token_renew_before_days": {"365"},
		"retention_days":          {"36501", "200000"},
		"smtp.port":               {"65536"},
	}
	for name, values := range bad {
		for _, v := range values {
			if _, err := applyConfig(db, map[string]string{name: v}, nil); err == nil {
				t.Errorf("%s = %q accepted", name, v)
			}
		}
	}
	good := map[string]string{
		"web_listen":     "127.0.0.1:19100",
		"agent_listen":   "[::1]:19101",
		"retention_days": "36500",
		"smtp.host":      "Mail.Example.com",
		"smtp.from":      "a@example.com",
		"smtp.helo_name": "",
		"server_names":   "2001:DB8::1, ::ffff:192.0.2.10,Central.Example.com",
	}
	for name, v := range good {
		if _, err := applyConfig(db, map[string]string{name: v}, nil); err != nil {
			t.Errorf("%s = %q rejected: %v", name, v, err)
		}
	}
	cfg, err := LoadCentralConfig(db)
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(cfg.ServerNames, ","); got != "2001:db8::1,192.0.2.10,central.example.com" {
		t.Errorf("server_names stored as %q", got)
	}
}

// TestCentralConfigRepairable checks that values saved before the current
// checks existed stop the central from loading the configuration, but can
// still be corrected one item at a time.
func TestCentralConfigRepairable(t *testing.T) {
	MigrationsFS = os.DirFS("../..")
	db, err := OpenSQLitePool(filepath.Join(t.TempDir(), "central.db"), SchemaCentral)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	for k, v := range map[string]string{"retention_days": "200000", "server_names": "a b", "web_listen": "127.0.0.1:0"} {
		if err := models.SetSetting(db, configKeyPrefix+k, v); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := LoadCentralConfig(db); err == nil {
		t.Fatal("invalid stored values loaded")
	}
	if _, err := applyConfig(db, map[string]string{"workers": "65"}, nil); err == nil {
		t.Fatal("a change adding a problem was accepted")
	}
	if _, err := applyConfig(db, map[string]string{"retention_days": "365"}, nil); err != nil {
		t.Fatalf("correction rejected: %v", err)
	}
	if _, err := applyConfig(db, nil, []string{"server_names"}); err != nil {
		t.Fatalf("unset rejected: %v", err)
	}
	if _, err := applyConfig(db, map[string]string{"web_listen": "127.0.0.1:19102"}, nil); err != nil {
		t.Fatalf("correction rejected: %v", err)
	}
	if _, err := LoadCentralConfig(db); err != nil {
		t.Fatalf("corrected configuration not loaded: %v", err)
	}
}

// TestServerCertNamesNormalized checks that configured names compare equal
// to the names read back from the certificate made for them.
func TestServerCertNamesNormalized(t *testing.T) {
	now := time.Now()
	ca, err := GenerateCA(now)
	if err != nil {
		t.Fatal(err)
	}
	names := []string{"2001:DB8::1", "::ffff:192.0.2.10", "Central.Example.com"}
	pair, err := GenerateServerCert(ca, names, now)
	if err != nil {
		t.Fatal(err)
	}
	got := strings.Join(ServerCertNames(CertNames(pair.Cert)), ",")
	want := strings.Join(ServerCertNames(names), ",")
	if got != want {
		t.Fatalf("certificate names %q, configured %q", got, want)
	}
}

func TestSMTPAuthChoice(t *testing.T) {
	cases := []struct {
		security, want string
	}{
		{"none", "LOGIN"},
		{"starttls", "PLAIN"},
		{"tls", "PLAIN"},
	}
	for _, c := range cases {
		a := smtpAuth(SMTPConfig{Security: c.security, Host: "mail.example.com"})
		got := "PLAIN"
		if _, ok := a.(*loginAuth); ok {
			got = "LOGIN"
		}
		if got != c.want {
			t.Errorf("security %s: %s, want %s", c.security, got, c.want)
		}
	}
	var _ smtp.Auth = &loginAuth{}
}

// TestSendMailLoginWithoutEncryption sends a mail without encryption and
// with authentication to a fake server that offers PLAIN and LOGIN.
func TestSendMailLoginWithoutEncryption(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	result := make(chan string, 1)
	go func() {
		conn, err := ln.Accept()
		if err != nil {
			result <- "accept: " + err.Error()
			return
		}
		defer conn.Close()
		_ = conn.SetDeadline(time.Now().Add(10 * time.Second))
		r := bufio.NewReader(conn)
		send := func(s string) { _, _ = conn.Write([]byte(s + "\r\n")) }
		var log []string
		send("220 test")
		inData := false
		for {
			line, err := r.ReadString('\n')
			if err != nil {
				result <- strings.Join(log, "|")
				return
			}
			line = strings.TrimRight(line, "\r\n")
			if inData {
				if line == "." {
					inData = false
					send("250 queued")
				}
				continue
			}
			log = append(log, line)
			cmd := strings.ToUpper(line)
			switch {
			case strings.HasPrefix(cmd, "EHLO"):
				send("250-test")
				send("250 AUTH PLAIN LOGIN")
			case cmd == "AUTH LOGIN":
				send("334 VXNlcm5hbWU6")
			case line == "dXNlcg==": // "user"
				send("334 UGFzc3dvcmQ6")
			case line == "cGFzcw==": // "pass"
				send("235 ok")
			case strings.HasPrefix(cmd, "AUTH"):
				send("504 unexpected")
			case strings.HasPrefix(cmd, "DATA"):
				inData = true
				send("354 go")
			case cmd == "QUIT":
				send("221 bye")
				result <- strings.Join(log, "|")
				return
			default:
				send("250 ok")
			}
		}
	}()
	port := ln.Addr().(*net.TCPAddr).Port
	cfg := SMTPConfig{Host: "127.0.0.1", Port: port, Security: "none", Auth: true, Username: "user", Password: "pass",
		From: "a@example.com", TimeoutSec: 10}
	if err := SendMailTo(cfg, []string{"b@example.com"}, "subject", "body"); err != nil {
		t.Fatalf("SendMailTo: %v", err)
	}
	if got := <-result; !strings.Contains(got, "AUTH LOGIN") {
		t.Fatalf("LOGIN not used: %s", got)
	}
}

// TestRemovedConfigItemIgnored checks that a value of smtp.to saved by an
// earlier version is ignored without error and that the name is refused like
// any unknown name.
func TestRemovedConfigItemIgnored(t *testing.T) {
	MigrationsFS = os.DirFS("../..")
	db, err := OpenSQLitePool(filepath.Join(t.TempDir(), "central.db"), SchemaCentral)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err := models.SetSetting(db, configKeyPrefix+"smtp.to", "not a mail address"); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadCentralConfig(db); err != nil {
		t.Fatalf("stored smtp.to not ignored: %v", err)
	}
	if _, stored, err := CentralConfigState(db); err != nil || stored["smtp.to"] {
		t.Fatalf("state: %v %v", stored, err)
	}
	if _, err := applyConfig(db, map[string]string{"retention_days": "30"}, nil); err != nil {
		t.Fatalf("change with a stored smtp.to: %v", err)
	}
	if _, err := applyConfig(db, map[string]string{"smtp.to": "a@example.com"}, nil); err == nil {
		t.Fatal("smtp.to accepted")
	}
	if _, err := FindConfigField("smtp.to"); err == nil {
		t.Fatal("smtp.to still defined")
	}
}

// TestRunningConfigStatus checks the record of the start values and the
// text central status prints from it.
func TestRunningConfigStatus(t *testing.T) {
	MigrationsFS = os.DirFS("../..")
	db, err := OpenSQLitePool(filepath.Join(t.TempDir(), "central.db"), SchemaCentral)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if LoadRunningConfig(db) != nil {
		t.Fatal("record without a start")
	}
	if _, err := applyConfig(db, map[string]string{"web_listen": "127.0.0.1:19610", "agent_listen": "127.0.0.1:19611", "workers": "2"}, nil); err != nil {
		t.Fatal(err)
	}
	started, err := LoadCentralConfig(db)
	if err != nil {
		t.Fatal(err)
	}
	if err := SaveRunningConfig(db, started); err != nil {
		t.Fatal(err)
	}
	// A change saved while running waits for a restart.
	if _, err := applyConfig(db, map[string]string{"web_listen": "127.0.0.1:19612", "workers": "3"}, nil); err != nil {
		t.Fatal(err)
	}
	saved, err := LoadCentralConfig(db)
	if err != nil {
		t.Fatal(err)
	}
	run := LoadRunningConfig(db)
	if run["web_listen"] != "127.0.0.1:19610" || run["workers"] != "2" || run["builtin_agent"] == "" || len(run) != len(restartItems) {
		t.Fatalf("record: %v", run)
	}
	for _, tc := range []struct {
		item, prefix string
		saved        *CentralConfig
		run          map[string]string
		want         string
	}{
		{"web_listen", "http://", saved, run, "http://127.0.0.1:19610 (saved: 127.0.0.1:19612, takes effect after restart)"},
		{"agent_listen", "https://", saved, run, "https://127.0.0.1:19611"},
		{"workers", "", saved, run, "2 (saved: 3, takes effect after restart)"},
		{"workers", "", saved, nil, "3"},
		{"workers", "", nil, run, "2"},
	} {
		if got, ok := statusValue(tc.item, tc.prefix, tc.saved, tc.run); !ok || got != tc.want {
			t.Errorf("%s: %q, want %q", tc.item, got, tc.want)
		}
	}
	if _, ok := statusValue("workers", "", nil, nil); ok {
		t.Error("value without saved or running configuration")
	}
	if err := ClearRunningConfig(db); err != nil || LoadRunningConfig(db) != nil {
		t.Fatalf("clear: %v", err)
	}
}
