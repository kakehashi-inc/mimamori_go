package modules

import (
	"bufio"
	"crypto/tls"
	"crypto/x509"
	"io"
	"net"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

// A mail server of the test on loopback. It answers the dialogue of
// SendMailTo step by step; each answer can be delayed, one step can be left
// unanswered (the server stays silent until the client closes), and the
// message can be taken slowly or not at all. The limit of these tests is
// smtp.timeout_sec = 1 s; times have a generous margin above, because other
// tests may run on the machine at the same time.

// The steps of the dialogue, in the order the client meets them.
const (
	stepGreeting  = "greeting"
	stepHandshake = "handshake" // the TLS handshake of security "tls"
	stepEHLO      = "ehlo"
	stepSTARTTLS  = "starttls"
	stepMail      = "mail"
	stepRcpt      = "rcpt"
	stepData      = "data"
	stepMessage   = "message" // taking the message after 354
	stepQuit      = "quit"
)

type testMailServer struct {
	addr     *net.TCPAddr
	security string
	cert     *tls.Certificate
	delay    time.Duration // before every answer
	silentAt string        // the step that is never answered
	slowTake bool          // take the message at about 12 MiB per second
	done     chan struct{} // closed when the test ends

	mu       sync.Mutex
	lastSent time.Time // when the server last sent a byte
	received int       // bytes of the message taken
	got      bool      // the whole message arrived
}

// startMailServer listens on loopback. The receive buffer of accepted
// connections is small, so that a server that does not read stops the
// client after a few hundred KiB.
func startMailServer(t *testing.T, s *testMailServer) *testMailServer {
	t.Helper()
	lc := net.ListenConfig{Control: func(_, _ string, rc syscall.RawConn) error {
		return rc.Control(func(fd uintptr) { _ = syscall.SetsockoptInt(int(fd), syscall.SOL_SOCKET, syscall.SO_RCVBUF, 64<<10) })
	}}
	ln, err := lc.Listen(t.Context(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	s.done = make(chan struct{})
	t.Cleanup(func() { close(s.done) })
	s.addr = ln.Addr().(*net.TCPAddr)
	go func() {
		c, err := ln.Accept()
		if err != nil {
			return
		}
		defer c.Close()
		s.serve(c)
	}()
	return s
}

func (s *testMailServer) cfg() SMTPConfig {
	return SMTPConfig{Host: "127.0.0.1", Port: s.addr.Port, Security: s.security, From: "central@example.com", TimeoutSec: 1}
}

func (s *testMailServer) sent() time.Time {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.lastSent
}

// silent waits until the test ends: the server sends nothing more and
// takes nothing more.
func (s *testMailServer) silent(net.Conn) { <-s.done }

// answer sends one line after the delay, unless step is the silent one.
func (s *testMailServer) answer(c net.Conn, step, line string) bool {
	if step == s.silentAt {
		s.silent(c)
		return false
	}
	time.Sleep(s.delay)
	if _, err := io.WriteString(c, line+"\r\n"); err != nil {
		return false
	}
	s.mu.Lock()
	s.lastSent = time.Now()
	s.mu.Unlock()
	return true
}

// throttled takes at most 128 KiB per read and then pauses 10 ms.
type throttled struct{ c net.Conn }

func (r throttled) Read(p []byte) (int, error) {
	if len(p) > 128<<10 {
		p = p[:128<<10]
	}
	n, err := r.c.Read(p)
	time.Sleep(10 * time.Millisecond)
	return n, err
}

func (s *testMailServer) serve(c net.Conn) {
	if s.security == "tls" {
		if s.silentAt == stepHandshake {
			s.silent(c)
			return
		}
		tc := tls.Server(c, &tls.Config{Certificates: []tls.Certificate{*s.cert}})
		if err := tc.Handshake(); err != nil {
			return
		}
		c = tc
	}
	if !s.answer(c, stepGreeting, "220 test ESMTP") {
		return
	}
	var src io.Reader = c
	if s.slowTake {
		src = throttled{c}
	}
	br := bufio.NewReaderSize(src, 64<<10)
	for {
		line, err := br.ReadString('\n')
		if err != nil {
			return
		}
		cmd := strings.ToUpper(strings.TrimSpace(line))
		switch {
		case strings.HasPrefix(cmd, "EHLO"), strings.HasPrefix(cmd, "HELO"):
			ext := ""
			if s.security == "starttls" {
				if _, ok := c.(*tls.Conn); !ok {
					ext = "\r\n250 STARTTLS"
				}
			}
			resp := "250 test"
			if ext != "" {
				resp = "250-test" + ext
			}
			if !s.answer(c, stepEHLO, resp) {
				return
			}
		case cmd == "STARTTLS":
			if !s.answer(c, stepSTARTTLS, "220 go ahead") {
				return
			}
			tc := tls.Server(c, &tls.Config{Certificates: []tls.Certificate{*s.cert}})
			if err := tc.Handshake(); err != nil {
				return
			}
			c = tc
			br = bufio.NewReaderSize(c, 64<<10)
		case strings.HasPrefix(cmd, "MAIL"):
			if !s.answer(c, stepMail, "250 ok") {
				return
			}
		case strings.HasPrefix(cmd, "RCPT"):
			if !s.answer(c, stepRcpt, "250 ok") {
				return
			}
		case cmd == "DATA":
			if !s.answer(c, stepData, "354 go ahead") {
				return
			}
			if s.silentAt == stepMessage {
				s.silent(c) // the server takes nothing of the message
				return
			}
			for {
				l, err := br.ReadString('\n')
				if err != nil {
					return
				}
				s.mu.Lock()
				s.received += len(l)
				s.mu.Unlock()
				if l == ".\r\n" {
					break
				}
			}
			s.mu.Lock()
			s.got = true
			s.mu.Unlock()
			if !s.answer(c, stepMessage, "250 queued") {
				return
			}
		case cmd == "QUIT":
			s.answer(c, stepQuit, "221 bye")
			return
		default:
			s.answer(c, "", "502 unknown")
		}
	}
}

func testMailCert(t *testing.T) *tls.Certificate {
	t.Helper()
	ca, err := GenerateCA(time.Now())
	if err != nil {
		t.Fatal(err)
	}
	pair, err := GenerateServerCert(ca, []string{"127.0.0.1"}, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	cert, err := pair.TLSCertificate()
	if err != nil {
		t.Fatal(err)
	}
	pool := x509.NewCertPool()
	pool.AddCert(ca.Cert)
	mailRootCAs = pool
	t.Cleanup(func() { mailRootCAs = nil })
	return cert
}

// sendSlowAnswers: every answer comes after 0.7 of the limit, the whole mail
// takes several limits, and it is sent.
func sendSlowAnswers(t *testing.T, security string, cert *tls.Certificate) {
	s := startMailServer(t, &testMailServer{security: security, cert: cert, delay: 700 * time.Millisecond})
	start := time.Now()
	if err := SendMailTo(s.cfg(), []string{"a@example.com"}, "subject", "body"); err != nil {
		t.Fatalf("%s: slow answers: %v after %v", security, err, time.Since(start))
	}
	if d := time.Since(start); d < 3*time.Second {
		t.Fatalf("%s: the mail took only %v, the test proves nothing", security, d)
	}
}

// sendSilentAt: the server stays silent at step; the error comes one limit
// after the server's last byte (or after connecting, when it sent none).
func sendSilentAt(t *testing.T, security string, cert *tls.Certificate, step string, body string) {
	s := startMailServer(t, &testMailServer{security: security, cert: cert, silentAt: step})
	start := time.Now()
	err := SendMailTo(s.cfg(), []string{"a@example.com"}, "subject", body)
	end := time.Now()
	if err == nil {
		t.Fatalf("%s: silent at %s: sent", security, step)
	}
	from := s.sent()
	if from.IsZero() {
		from = start
	}
	if d := end.Sub(from); d < 900*time.Millisecond || d > 2500*time.Millisecond {
		t.Fatalf("%s: silent at %s: error %v after the last byte: %v", security, step, d, err)
	}
}

func TestSendMailSilence(t *testing.T) {
	t.Run("slow answers", func(t *testing.T) {
		t.Parallel()
		sendSlowAnswers(t, "none", nil)
	})
	t.Run("large message taken steadily", func(t *testing.T) {
		t.Parallel()
		s := startMailServer(t, &testMailServer{security: "none", slowTake: true})
		body := strings.Repeat("0123456789abcdef", 3<<19) // 24 MiB, 32 MiB in base64
		start := time.Now()
		if err := SendMailTo(s.cfg(), []string{"a@example.com"}, "subject", body); err != nil {
			t.Fatalf("large message: %v after %v", err, time.Since(start))
		}
		if d := time.Since(start); d < 2*time.Second {
			t.Fatalf("the message was taken in %v, the test proves nothing", d)
		}
		s.mu.Lock()
		defer s.mu.Unlock()
		if !s.got || s.received < 32<<20 {
			t.Fatalf("the server received %d bytes, complete %v", s.received, s.got)
		}
	})
	for _, step := range []string{stepGreeting, stepRcpt, stepData} {
		t.Run("no answer at "+step, func(t *testing.T) {
			t.Parallel()
			sendSilentAt(t, "none", nil, step, "body")
		})
	}
	t.Run("message not taken", func(t *testing.T) {
		t.Parallel()
		sendSilentAt(t, "none", nil, stepMessage, strings.Repeat("x", 8<<20))
	})
}

func TestSendMailSTARTTLSAndTLS(t *testing.T) {
	cert := testMailCert(t)
	for _, security := range []string{"starttls", "tls"} {
		t.Run(security, func(t *testing.T) {
			t.Run("slow answers", func(t *testing.T) {
				t.Parallel()
				sendSlowAnswers(t, security, cert)
			})
			t.Run("no answer at rcpt", func(t *testing.T) {
				t.Parallel()
				sendSilentAt(t, security, cert, stepRcpt, "body")
			})
			t.Run("message not taken", func(t *testing.T) {
				t.Parallel()
				sendSilentAt(t, security, cert, stepMessage, strings.Repeat("x", 8<<20))
			})
		})
	}
	t.Run("tls handshake not answered", func(t *testing.T) {
		sendSilentAt(t, "tls", cert, stepHandshake, "body")
	})
}
