package modules

import (
	"crypto/tls"
	"encoding/base64"
	"fmt"
	"mime"
	"net"
	"net/smtp"
	"strconv"
	"strings"
	"time"
)

// SendMail delivers one plain-text notification to the configured
// recipients (smtp.to).
func SendMail(cfg SMTPConfig, subject, body string) error {
	if !cfg.Enabled() {
		return fmt.Errorf("SMTP is not configured")
	}
	return SendMailTo(cfg, cfg.To, subject, body)
}

// SendMailTo delivers one plain-text message to the given recipients
// synchronously in the calling flow. The whole exchange is bounded by
// cfg.TimeoutSec through a deadline on the connection.
func SendMailTo(cfg SMTPConfig, to []string, subject, body string) error {
	if !cfg.CanSend() {
		return fmt.Errorf("SMTP is not configured (smtp.host and smtp.from are required)")
	}
	if len(to) == 0 {
		return fmt.Errorf("no recipient")
	}
	timeout := time.Duration(cfg.TimeoutSec) * time.Second
	addr := net.JoinHostPort(cfg.Host, strconv.Itoa(cfg.Port))
	deadline := time.Now().Add(timeout)
	d := net.Dialer{Deadline: deadline, FallbackDelay: -1}
	var conn net.Conn
	var err error
	tlsCfg := &tls.Config{ServerName: cfg.Host, MinVersion: tls.VersionTLS12}
	if cfg.Security == "tls" {
		raw, derr := d.Dial("tcp", addr)
		if derr != nil {
			return derr
		}
		_ = raw.SetDeadline(deadline)
		tc := tls.Client(raw, tlsCfg)
		if err := tc.Handshake(); err != nil {
			raw.Close()
			return err
		}
		conn = tc
	} else {
		conn, err = d.Dial("tcp", addr)
		if err != nil {
			return err
		}
	}
	defer conn.Close()
	_ = conn.SetDeadline(deadline)
	c, err := smtp.NewClient(conn, cfg.Host)
	if err != nil {
		return err
	}
	defer c.Close()
	if cfg.HeloName != "" {
		if err := c.Hello(cfg.HeloName); err != nil {
			return err
		}
	}
	if cfg.Security == "starttls" {
		if err := c.StartTLS(tlsCfg); err != nil {
			return fmt.Errorf("STARTTLS: %w", err)
		}
	}
	if cfg.Auth {
		if err := c.Auth(smtpAuth(cfg)); err != nil {
			return fmt.Errorf("SMTP auth: %w", err)
		}
	}
	if err := c.Mail(cfg.From); err != nil {
		return err
	}
	for _, rcpt := range to {
		if err := c.Rcpt(rcpt); err != nil {
			return err
		}
	}
	w, err := c.Data()
	if err != nil {
		return err
	}
	if _, err := w.Write([]byte(buildMessage(cfg, to, subject, body))); err != nil {
		return err
	}
	if err := w.Close(); err != nil {
		return err
	}
	return c.Quit()
}

func buildMessage(cfg SMTPConfig, to []string, subject, body string) string {
	var b strings.Builder
	b.WriteString("From: " + cfg.From + "\r\n")
	b.WriteString("To: " + strings.Join(to, ", ") + "\r\n")
	b.WriteString("Subject: " + mime.BEncoding.Encode("UTF-8", subject) + "\r\n")
	b.WriteString("Date: " + time.Now().Format(time.RFC1123Z) + "\r\n")
	b.WriteString("MIME-Version: 1.0\r\n")
	b.WriteString("Content-Type: text/plain; charset=UTF-8\r\n")
	b.WriteString("Content-Transfer-Encoding: base64\r\n\r\n")
	enc := base64.StdEncoding.EncodeToString([]byte(body))
	for len(enc) > 76 {
		b.WriteString(enc[:76] + "\r\n")
		enc = enc[76:]
	}
	b.WriteString(enc + "\r\n")
	return b.String()
}

// smtpAuth chooses the authentication mechanism: LOGIN without encryption,
// because net/smtp refuses to send PLAIN over an unencrypted connection, and
// PLAIN with TLS (tls or starttls).
func smtpAuth(cfg SMTPConfig) smtp.Auth {
	if cfg.Security == "none" {
		return &loginAuth{user: cfg.Username, pass: cfg.Password}
	}
	return smtp.PlainAuth("", cfg.Username, cfg.Password, cfg.Host)
}

// loginAuth implements the LOGIN mechanism (see smtpAuth).
type loginAuth struct{ user, pass string }

func (a *loginAuth) Start(*smtp.ServerInfo) (string, []byte, error) { return "LOGIN", nil, nil }
func (a *loginAuth) Next(fromServer []byte, more bool) ([]byte, error) {
	if !more {
		return nil, nil
	}
	// Servers word the two challenges differently ("Username:", "User Name",
	// "Password:"), so only the key word is looked at.
	challenge := strings.ToLower(string(fromServer))
	switch {
	case strings.Contains(challenge, "user"):
		return []byte(a.user), nil
	case strings.Contains(challenge, "pass"):
		return []byte(a.pass), nil
	}
	return nil, fmt.Errorf("unexpected LOGIN challenge %q", fromServer)
}
