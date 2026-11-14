package modules

import (
	"bufio"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"io"
	"net"
	"os"
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
// smtp.timeout_sec = 2 s: the server answers the steps before the silent one
// at once, and a delay of the test's flows on a busy machine must not make
// it silent for the limit. A cut is checked against the moment the product
// counts from (never earlier than one limit after it), with a generous
// margin above.

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
	limit    int           // smtp.timeout_sec of the client (0: 2)
	delay    time.Duration // before every answer
	silentAt string        // the step that is never answered
	slowTake bool          // take the message at about 6 MB per second
	done     chan struct{} // closed when the test ends

	mu       sync.Mutex
	lastSent time.Time // when the server began to send its last answer
	received int       // bytes of the message taken
	got      bool      // the whole message arrived
}

// mailConns holds the connection SendMailTo made to each mail server of the
// tests, by the server's address (each server takes one connection), until
// the server's test ends.
var mailConns sync.Map

func init() {
	mailDial = func(addr string, idle time.Duration) (*IdleConn, error) {
		c, err := DialIdle(addr, idle)
		if err == nil {
			mailConns.Store(addr, c)
		}
		return c, err
	}
}

// conn returns the client's connection to s, nil before SendMailTo made it.
func (s *testMailServer) conn() *IdleConn {
	c, _ := mailConns.Load(s.addr.String())
	ic, _ := c.(*IdleConn)
	return ic
}

// messageTakeFor is how long a server silent at stepMessage takes the
// message before it stops reading: longer than the limit, so that a cut by
// the total time of the write would come while the message is still taken.
// At about 1.6 MB per second the server takes less than a quarter of a
// message of 16 MiB (22 MB in base64) in that time; with what the kernels of
// both sides hold (some MB) the client is still writing when it stops.
const messageTakeFor = 3 * time.Second

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
	// Forgotten before the listener closes (cleanups run last first): a
	// later server can get the same port only after that.
	t.Cleanup(func() { mailConns.Delete(s.addr.String()) })
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
	return SMTPConfig{Host: "127.0.0.1", Port: s.addr.Port, Security: s.security, From: "central@example.com", TimeoutSec: s.limitSec()}
}

// limitSec is the client's smtp.timeout_sec.
func (s *testMailServer) limitSec() int {
	if s.limit > 0 {
		return s.limit
	}
	return 2
}

// limitTime is the client's smtp.timeout_sec as a duration.
func (s *testMailServer) limitTime() time.Duration { return time.Duration(s.limitSec()) * time.Second }

func (s *testMailServer) sent() time.Time {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.lastSent
}

// silent waits until the test ends: the server sends nothing more and
// takes nothing more.
func (s *testMailServer) silent(net.Conn) { <-s.done }

// answer sends one line after the delay, unless step is the silent one. The
// time it records is taken before the write: no byte of the answer reaches
// the client before it.
func (s *testMailServer) answer(c net.Conn, step, line string) bool {
	if step == s.silentAt {
		s.silent(c)
		return false
	}
	time.Sleep(s.delay)
	before := time.Now()
	if _, err := io.WriteString(c, line+"\r\n"); err != nil {
		return false
	}
	s.mu.Lock()
	s.lastSent = before
	s.mu.Unlock()
	return true
}

// throttled takes at most 128 KiB per read and then pauses 10 ms (the
// server reads 64 KiB at a time: about 6 MB per second).
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
				s.takeThenStop(c)
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

// takeThenStop takes the message for messageTakeFor, about 1.6 MB per second
// in bursts of 64 KiB (each burst frees enough of the receive buffer for the
// client's kernel to take more at once), and then reads nothing more until
// the test ends. The client sends the message only after the 354, so
// nothing of it is buffered above c.
func (s *testMailServer) takeThenStop(c net.Conn) {
	buf := make([]byte, 64<<10)
	stopAt := time.Now().Add(messageTakeFor)
	burst := 0
	for time.Now().Before(stopAt) {
		n, err := c.Read(buf)
		s.mu.Lock()
		s.received += n
		s.mu.Unlock()
		if err != nil {
			return
		}
		if burst += n; burst >= len(buf) {
			burst = 0
			time.Sleep(40 * time.Millisecond)
		}
	}
	<-s.done
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

// sendSlowAnswers: every answer comes after half the limit, so that a delay
// of the server on a busy machine does not make it silent for the limit;
// the whole mail takes several limits, and it is sent.
func sendSlowAnswers(t *testing.T, security string, cert *tls.Certificate) {
	s := startMailServer(t, &testMailServer{security: security, cert: cert, delay: time.Second})
	start := time.Now()
	if err := SendMailTo(s.cfg(), []string{"a@example.com"}, "subject", "body"); err != nil {
		t.Fatalf("%s: slow answers: %v after %v", security, err, time.Since(start))
	}
	if d := time.Since(start); d < 3*s.limitTime() {
		t.Fatalf("%s: the mail took only %v, the test proves nothing", security, d)
	}
}

// sendSilentAt: the server stays silent at step; the error comes one limit
// after the client's last progress, never earlier. When the server answers
// the step before, that progress is the arrival of that answer, and the
// server records the moment before it wrote it (the start of the mail when
// the server sent nothing). When the server does not take the message, it
// is the moment the client's kernel took the last byte of it, which comes in
// bursts as the server reads; the test reads it from the client's socket.
// The upper bound has a generous margin for a loaded machine.
func sendSilentAt(t *testing.T, security string, cert *tls.Certificate, step string, body string) {
	s := startMailServer(t, &testMailServer{security: security, cert: cert, silentAt: step})
	conn := func() *IdleConn { return nil }
	if step == stepMessage {
		conn = s.conn
	}
	stopWatch := make(chan struct{})
	watched := watchTaken(conn, stopWatch)
	start := time.Now()
	err := SendMailTo(s.cfg(), []string{"a@example.com"}, "subject", body)
	end := time.Now()
	close(stopWatch)
	samples := <-watched
	if err == nil {
		t.Fatalf("%s: silent at %s: sent", security, step)
	}
	from := s.sent()
	if from.IsZero() {
		from = start
	}
	latest := from // the latest moment the last progress may have been
	if step == stepMessage {
		final := uint64(0)
		for _, smp := range samples {
			final = max(final, smp.taken)
		}
		notYet, taken, ok := lastTaken(samples, final)
		if !ok {
			t.Fatalf("%s: silent at %s: the %d readings of the client's socket do not show when it took its last byte", security, step, len(samples))
		}
		t.Logf("%s: %d bytes; last byte taken between %v and %v after the 354; error after %v", security, final, notYet.Sub(from), taken.Sub(from), end.Sub(from))
		// The client's kernel took bytes of the message for longer than the
		// limit after the 354: a cut by the total time of the write would
		// have come while it did.
		if notYet.Sub(from) < s.limitTime() {
			t.Fatalf("%s: silent at %s: the client's kernel took the last byte %v after the 354, the test proves nothing",
				security, step, notYet.Sub(from))
		}
		from, latest = notYet, taken
	}
	if d := end.Sub(from); d < s.limitTime() {
		t.Fatalf("%s: silent at %s: error %v after a moment before the last progress, want at least %v: %v", security, step, d, s.limitTime(), err)
	}
	if d := end.Sub(latest); d > s.limitTime()+3*time.Second {
		t.Fatalf("%s: silent at %s: error %v after the last progress, want about %v: %v", security, step, d, s.limitTime(), err)
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
		if d := time.Since(start); d < 2*s.limitTime() {
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
		sendSilentAt(t, "none", nil, stepMessage, strings.Repeat("x", 16<<20))
	})
}

func TestSendMailSTARTTLSAndTLS(t *testing.T) {
	cert := testMailCert(t)
	for _, security := range []string{"starttls", "tls"} {
		t.Run(security, func(t *testing.T) {
			t.Parallel()
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
				sendSilentAt(t, security, cert, stepMessage, strings.Repeat("x", 16<<20))
			})
		})
	}
	t.Run("tls handshake not answered", func(t *testing.T) {
		sendSilentAt(t, "tls", cert, stepHandshake, "body")
	})
}

// When the mail server is silent for smtp.timeout_sec and the connection is
// cut, the error says so and gives the error of the read or write that was
// cut, at every step: also at EHLO, where net/smtp tries HELO on the closed
// connection and its own error would hide the cut. The wording of one
// second is checked on its own (TestMailSilenceErrorWording).
func TestSendMailSilenceErrorText(t *testing.T) {
	cert := testMailCert(t)
	for _, tc := range []struct{ security, step, want string }{
		{"none", stepGreeting, "the mail server sent nothing for 2 seconds: "},
		{"none", stepEHLO, "the mail server sent nothing for 2 seconds: "},
		{"starttls", stepEHLO, "the mail server sent nothing for 2 seconds: "},
		{"starttls", stepSTARTTLS, "the mail server sent nothing for 2 seconds: "},
		{"tls", stepHandshake, "the mail server sent nothing for 2 seconds: "},
		{"none", stepMail, "the mail server sent nothing for 2 seconds: "},
		{"none", stepRcpt, "the mail server sent nothing for 2 seconds: "},
		{"none", stepData, "the mail server sent nothing for 2 seconds: "},
		{"none", stepMessage, "the mail server took nothing for 2 seconds: "},
	} {
		t.Run(tc.security+" "+tc.step, func(t *testing.T) {
			t.Parallel()
			body := "body"
			if tc.step == stepMessage {
				body = strings.Repeat("x", 16<<20)
			}
			s := startMailServer(t, &testMailServer{security: tc.security, cert: cert, silentAt: tc.step})
			err := SendMailTo(s.cfg(), []string{"a@example.com"}, "subject", body)
			if err == nil || !strings.HasPrefix(err.Error(), tc.want) || !strings.HasSuffix(err.Error(), "i/o timeout") {
				t.Fatalf("error %v, want %q followed by the timeout of the cut", err, tc.want)
			}
		})
	}
	// Another failure is reported as it is.
	s := startMailServer(t, &testMailServer{security: "none"})
	cfg := s.cfg()
	cfg.From = "bad\r\nfrom@example.com"
	if err := SendMailTo(cfg, []string{"a@example.com"}, "subject", "body"); err == nil || strings.Contains(err.Error(), "the mail server") {
		t.Fatalf("a refused sender: %v", err)
	}
}

// The silence error names what the server did not do and the limit, in the
// singular for one second.
func TestMailSilenceErrorWording(t *testing.T) {
	cut := &net.OpError{Op: "read", Net: "tcp", Err: os.ErrDeadlineExceeded}
	for _, tc := range []struct {
		sec  int
		read bool
		want string
	}{
		{1, true, "the mail server sent nothing for 1 second: read tcp: i/o timeout"},
		{1, false, "the mail server took nothing for 1 second: read tcp: i/o timeout"},
		{30, true, "the mail server sent nothing for 30 seconds: read tcp: i/o timeout"},
		{2, false, "the mail server took nothing for 2 seconds: read tcp: i/o timeout"},
	} {
		err := mailSilenceError(tc.sec, tc.read, cut)
		if err.Error() != tc.want || !errors.Is(err, os.ErrDeadlineExceeded) {
			t.Errorf("%d s, read %v: %q, want %q wrapping the cut", tc.sec, tc.read, err, tc.want)
		}
	}
}
