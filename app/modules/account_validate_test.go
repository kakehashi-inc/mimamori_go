package modules

import (
	"bufio"
	"net"
	"os"
	"strings"
	"testing"
	"time"

	"mimamori/app/models"
)

func TestValidateEmail(t *testing.T) {
	long := strings.Repeat("a", 64) + "@" + strings.Repeat("b", 189) // 254 characters
	for _, tc := range []struct {
		in   string
		want string // "" means rejected
	}{
		{"user@example.com", "user@example.com"},
		{"  User.Name+tag@Example.COM ", "User.Name+tag@Example.COM"},
		{"user@localhost", "user@localhost"},
		{long, long},
		{long + "b", ""},
		{"", ""},
		{"   ", ""},
		{"user", ""},
		{"@example.com", ""},
		{"user@", ""},
		{"User <user@example.com>", ""},
		{"<user@example.com>", ""},
		{"a@example.com,b@example.com", ""},
		{`"a b"@example.com`, ""},
		{"a b@example.com", ""},
		{"user@example.com\r\nBcc: x@example.com", ""},
		{"user\t@example.com", ""},
		{"user@exa\x00mple.com", ""},
	} {
		got, err := ValidateEmail(tc.in)
		if tc.want == "" {
			if err == nil {
				t.Errorf("ValidateEmail(%q) = %q, want an error", tc.in, got)
			}
			continue
		}
		if err != nil || got != tc.want {
			t.Errorf("ValidateEmail(%q) = %q, %v; want %q", tc.in, got, err, tc.want)
		}
	}
}

func TestValidateDisplayName(t *testing.T) {
	for _, tc := range []struct {
		in   string
		want string
	}{
		{"Alice", "Alice"},
		{"  山田 太郎  ", "山田 太郎"},
		{strings.Repeat("あ", 64), strings.Repeat("あ", 64)},
		{strings.Repeat("あ", 65), ""},
		{"", ""},
		{" \t ", ""},
		{"a\nb", ""},
		{"a\x7fb", ""},
		{"\xff", ""},
	} {
		got, err := ValidateDisplayName(tc.in)
		if tc.want == "" {
			if err == nil {
				t.Errorf("ValidateDisplayName(%q) = %q, want an error", tc.in, got)
			}
			continue
		}
		if err != nil || got != tc.want {
			t.Errorf("ValidateDisplayName(%q) = %q, %v; want %q", tc.in, got, err, tc.want)
		}
	}
}

func TestSMTPModes(t *testing.T) {
	for _, tc := range []struct {
		cfg          SMTPConfig
		dev, canSend bool
	}{
		{SMTPConfig{}, true, false},
		{SMTPConfig{From: "a@example.com"}, true, false},
		{SMTPConfig{Host: "mail.example.com"}, false, false},
		{SMTPConfig{Host: "mail.example.com", From: "a@example.com"}, false, true},
	} {
		if tc.cfg.DevelopmentMode() != tc.dev || tc.cfg.CanSend() != tc.canSend {
			t.Errorf("%+v: development %v, can send %v", tc.cfg, tc.cfg.DevelopmentMode(), tc.cfg.CanSend())
		}
	}
	if err := SendMailTo(SMTPConfig{Host: "127.0.0.1", Port: 1}, []string{"b@example.com"}, "s", "b"); err == nil {
		t.Fatal("SendMailTo without smtp.from must fail before connecting")
	}
	if err := SendMailTo(SMTPConfig{Host: "127.0.0.1", Port: 1, From: "a@example.com"}, nil, "s", "b"); err == nil {
		t.Fatal("SendMailTo without recipients must fail")
	}
}

// TestSendMailToRecipients checks that SendMailTo delivers to the given
// recipients, not to the notification recipients.
func TestSendMailToRecipients(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	result := make(chan string, 1)
	go func() {
		conn, err := ln.Accept()
		if err != nil {
			result <- ""
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
				} else if strings.HasPrefix(line, "To: ") {
					log = append(log, line)
				}
				continue
			}
			log = append(log, line)
			switch cmd := strings.ToUpper(line); {
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
	cfg := SMTPConfig{Host: "127.0.0.1", Port: ln.Addr().(*net.TCPAddr).Port, Security: "none", From: "a@example.com",
		TimeoutSec: 10}
	if err := SendMailTo(cfg, []string{"user@example.com"}, "subject", "body"); err != nil {
		t.Fatalf("SendMailTo: %v", err)
	}
	got := <-result
	if !strings.Contains(got, "RCPT TO:<user@example.com>") || !strings.Contains(got, "To: user@example.com") || strings.Count(got, "RCPT TO:") != 1 {
		t.Fatalf("recipients: %s", got)
	}
}

func TestTwoStepTrustDaysConfig(t *testing.T) {
	c := DefaultCentralConfig()
	if c.TwoStepTrustDays != DefaultTrustDays || DefaultTrustDays != 30 {
		t.Fatalf("default two_step_trust_days %d", c.TwoStepTrustDays)
	}
	f, err := FindConfigField("two_step_trust_days")
	if err != nil || f.Restart {
		t.Fatalf("two_step_trust_days: %v, restart %v", err, f != nil && f.Restart)
	}
	for v, ok := range map[string]bool{"1": true, "365": true, "366": false, "0": false, "-1": false, "x": false} {
		c := DefaultCentralConfig()
		err := f.set(c, v)
		if err == nil {
			err = c.Validate()
		}
		if (err == nil) != ok {
			t.Errorf("two_step_trust_days=%s: %v", v, err)
		}
	}
	if _, err := FindConfigField("session_hours"); err == nil {
		t.Fatal("session_hours must be gone")
	}
	if strings.Contains(f.Help, "3") || strings.Contains(f.Help, "hour") {
		t.Fatalf("the help must not mention the fixed periods: %q", f.Help)
	}
}

func TestRevokeAccountAuth(t *testing.T) {
	MigrationsFS = os.DirFS("../..")
	db, err := OpenSQLite(t.TempDir()+"/central.db", SchemaCentral)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	now := time.Now().Unix()
	ids := make([]int64, 2)
	err = db.Write(func(tx models.Writer) error {
		for i, email := range []string{"a@example.com", "b@example.com"} {
			a := &models.Account{Email: email, DisplayName: "x", PasswordHash: "h", Role: models.RoleViewer}
			if err := models.InsertAccount(tx, a); err != nil {
				return err
			}
			ids[i] = a.ID
			for _, s := range []string{"s1", "s2"} {
				if err := models.InsertSession(tx, &models.Session{ID: email + s, AccountID: a.ID, CreatedAt: now, ExpiresAt: now + 60, LastSeenAt: now}); err != nil {
					return err
				}
			}
			if err := models.InsertBrowserVerification(tx, &models.BrowserVerification{ID: email + "v", AccountID: a.ID, CreatedAt: now, ExpiresAt: now + 60}); err != nil {
				return err
			}
			if err := models.InsertAuthCode(tx, &models.AuthCode{Purpose: models.CodePurposeLogin, AccountID: a.ID, ChallengeHash: email, CodeHash: "c", CreatedAt: now, ExpiresAt: now + 60}); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Write(func(tx models.Writer) error {
		return models.InsertAccount(tx, &models.Account{Email: "A@EXAMPLE.COM", DisplayName: "dup", PasswordHash: "h", Role: models.RoleViewer})
	}); err == nil {
		t.Fatal("e-mail addresses must be unique without regard to case")
	}
	if a, err := models.GetAccountByEmail(db.Read(), "B@Example.com"); err != nil || a.ID != ids[1] {
		t.Fatalf("lookup without regard to case: %v %v", a, err)
	}
	if err := db.Write(func(tx models.Writer) error { return RevokeAccountAuth(tx, ids[0], "a@example.coms1") }); err != nil {
		t.Fatal(err)
	}
	count := func(q string, id int64) int {
		var n int
		_ = db.Read().QueryRow(q, id).Scan(&n)
		return n
	}
	for i, want := range [][3]int{{1, 0, 0}, {2, 1, 1}} {
		got := [3]int{
			count(`SELECT COUNT(*) FROM sessions WHERE account_id = ?`, ids[i]),
			count(`SELECT COUNT(*) FROM browser_verifications WHERE account_id = ?`, ids[i]),
			count(`SELECT COUNT(*) FROM auth_codes WHERE account_id = ?`, ids[i]),
		}
		if got != want {
			t.Fatalf("account %d: sessions, verifications, codes = %v, want %v", i, got, want)
		}
	}
	// Code bookkeeping used by the login and e-mail address change steps.
	c, err := models.GetLoginCodeByChallenge(db.Read(), "b@example.com")
	if err != nil || c.AccountID != ids[1] {
		t.Fatalf("code by challenge: %v %v", c, err)
	}
	for want := 1; want <= 2; want++ {
		var n int
		err := db.Write(func(tx models.Writer) (err error) {
			n, err = models.RecordAuthCodeAttempt(tx, c.ID)
			return err
		})
		if err != nil || n != want {
			t.Fatalf("attempt %d: %d %v", want, n, err)
		}
	}
	// A right code is used up once, and not after the attempt limit or
	// its expiry.
	for _, tc := range []struct {
		max  int
		now  int64
		want bool
	}{{2, now, false}, {3, now + 60, false}, {3, now, true}, {3, now, false}} {
		var ok bool
		err := db.Write(func(tx models.Writer) (err error) {
			ok, err = models.ConsumeAuthCode(tx, c.ID, tc.max, tc.now)
			return err
		})
		if err != nil || ok != tc.want {
			t.Fatalf("consume (max %d, now %d): %v %v, want %v", tc.max, tc.now-now, ok, err, tc.want)
		}
	}
	if err := db.Write(func(tx models.Writer) error {
		if err := models.DeleteExpiredAuthCodes(tx, now+61); err != nil {
			return err
		}
		return models.DeleteExpiredBrowserVerifications(tx, now+61)
	}); err != nil {
		t.Fatal(err)
	}
	if count(`SELECT COUNT(*) FROM auth_codes WHERE account_id = ?`, ids[1])+count(`SELECT COUNT(*) FROM browser_verifications WHERE account_id = ?`, ids[1]) != 0 {
		t.Fatal("expired codes and verifications must be removed")
	}
}

func TestVerificationCodeHelpers(t *testing.T) {
	seen := map[string]bool{}
	for i := 0; i < 200; i++ {
		c, err := GenerateNumericCode(AuthCodeDigits)
		if err != nil {
			t.Fatal(err)
		}
		if len(c) != 6 || strings.Trim(c, "0123456789") != "" {
			t.Fatalf("code %q", c)
		}
		seen[c] = true
	}
	if len(seen) < 150 {
		t.Fatalf("codes repeat too often: %d distinct of 200", len(seen))
	}
	k1, k2 := []byte("key-1"), []byte("key-2")
	h := HashAuthCode(k1, "login:1:abc", "123456")
	if h == HashAuthCode(k2, "login:1:abc", "123456") || h == HashAuthCode(k1, "login:2:abc", "123456") ||
		h == HashAuthCode(k1, "login:1:abc", "123457") || h != HashAuthCode(k1, "login:1:abc", "123456") || strings.Contains(h, "123456") {
		t.Fatal("the code hash must depend on the key, the binding and the code")
	}
}
