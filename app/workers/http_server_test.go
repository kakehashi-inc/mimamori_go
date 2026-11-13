package workers

import (
	"bufio"
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"fmt"
	"io"
	"log"
	"math/big"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"mimamori/app/modules"
)

// testIdle is the idle time of the test servers (the central uses
// modules.IdleTimeout). Checks of a cut accept up to testIdle + cutSlack,
// since other tests may run on the machine at the same time.
const (
	testIdle = time.Second
	cutSlack = 2 * time.Second
)

// lockedBuffer is a log destination for several flows.
type lockedBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (l *lockedBuffer) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}

func (l *lockedBuffer) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.String()
}

// serveOpts configures the test servers; zero values mean testIdle, the
// central's bound and budget, and 127.0.0.1.
type serveOpts struct {
	idle   time.Duration
	places int
	budget int64
	listen string
}

// heldResult is what a /test/hold handler saw at its end.
type heldResult struct {
	ctxErr error
	ended  time.Time
}

// serveEnv runs both servers of the central (newHTTPServer) on loopback
// with the central's real handlers and pre-checks plus test routes.
type serveEnv struct {
	t           *testing.T
	e           *testEnv
	idle        time.Duration
	lim         *connLimit
	budget      *bodyBudget
	web, agent  *http.Server
	webAddr     string
	agentAddr   string
	logs        *lockedBuffer
	held        sync.Map // id -> heldResult
	inWork      atomic.Int32
	maxInWork   atomic.Int32
	stopOnce    sync.Once
	baselineCnt int // places held by the two listeners waiting in accept
}

func startServe(t *testing.T, o serveOpts) *serveEnv {
	t.Helper()
	if o.idle == 0 {
		o.idle = testIdle
	}
	if o.places == 0 {
		o.places = modules.MaxOpenConns
	}
	if o.budget == 0 {
		o.budget = modules.MaxBufferedBytes
	}
	if o.listen == "" {
		o.listen = "127.0.0.1:0"
	}
	e := newTestEnv(t)
	s := &serveEnv{t: t, e: e, idle: o.idle, lim: newConnLimit(o.places), budget: newBodyBudget(o.budget), logs: &lockedBuffer{}, baselineCnt: 2}
	logger := log.New(s.logs, "", 0)
	webLn, err := listenTCP(o.listen)
	if err != nil {
		t.Fatal(err)
	}
	agentLn, err := listenTCP(o.listen)
	if err != nil {
		t.Fatal(err)
	}
	s.webAddr, s.agentAddr = webLn.Addr().String(), agentLn.Addr().String()
	s.web = newHTTPServer(s.routes(e.web), s.admit(e.c.admitWebBody), nil, s.budget, logger)
	s.agent = newHTTPServer(s.routes(e.agent), s.admit(e.c.admitAgentBody), e.c.tlsConfig(), s.budget, logger)
	go func() { _ = s.web.Serve(s.lim.listener(webLn, o.idle)) }()
	go func() { _ = s.agent.ServeTLS(s.lim.listener(agentLn, o.idle), "", "") }()
	t.Cleanup(s.stop)
	// Both listeners hold their place while they wait in accept.
	s.waitOpen(t, s.baselineCnt, 5*time.Second)
	return s
}

// stop shuts both servers down (once) and waits.
func (s *serveEnv) stop() {
	s.stopOnce.Do(func() {
		var wg sync.WaitGroup
		for _, srv := range []*http.Server{s.web, s.agent} {
			wg.Add(1)
			go func() { defer wg.Done(); _ = srv.Shutdown(context.Background()) }()
		}
		wg.Wait()
	})
}

// routes adds test routes in front of a listener's handler:
// /test/slow?ms=N answers after N ms; /test/big?mib=N answers N MiB;
// /test/echo?ms=N answers the number of body bytes and their sum (after N
// ms); /test/panic writes and panics; /test/ctx describes the request's
// connection; /test/hold?id=X&ms=N works N ms and records its context.
func (s *serveEnv) routes(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ms, _ := strconv.Atoi(r.URL.Query().Get("ms"))
		switch r.URL.Path {
		case "/test/slow":
			n := s.inWork.Add(1)
			for {
				m := s.maxInWork.Load()
				if n <= m || s.maxInWork.CompareAndSwap(m, n) {
					break
				}
			}
			time.Sleep(time.Duration(ms) * time.Millisecond)
			s.inWork.Add(-1)
			_, _ = io.WriteString(w, "slow")
		case "/test/big":
			mib, _ := strconv.Atoi(r.URL.Query().Get("mib"))
			_, _ = w.Write(bytes.Repeat([]byte("b"), mib<<20))
		case "/test/echo":
			data, err := io.ReadAll(r.Body)
			if err != nil {
				http.Error(w, err.Error(), http.StatusBadRequest)
				return
			}
			time.Sleep(time.Duration(ms) * time.Millisecond)
			sum := 0
			for _, b := range data {
				sum += int(b)
			}
			fmt.Fprintf(w, "%d %d", len(data), sum)
		case "/test/panic":
			_, _ = io.WriteString(w, "partial")
			panic("boom")
		case "/test/ctx":
			local := r.Context().Value(http.LocalAddrContextKey)
			ta, isTCP := local.(*net.TCPAddr)
			fmt.Fprintf(w, "remote=%s local=%v tcp=%v tls=%v done=%v", r.RemoteAddr, ta, isTCP, r.TLS != nil, r.Context().Done() != nil)
		case "/test/hold":
			time.Sleep(time.Duration(ms) * time.Millisecond)
			s.held.Store(r.URL.Query().Get("id"), heldResult{ctxErr: r.Context().Err(), ended: time.Now()})
			_, _ = io.WriteString(w, "held")
		default:
			next.ServeHTTP(w, r)
		}
	})
}

// admit admits the large bodies of the test routes and asks the central's
// pre-check for every other route; X-Test-Admit-Panic makes it panic.
func (s *serveEnv) admit(next func(*http.Request) bool) func(*http.Request) bool {
	return func(r *http.Request) bool {
		if r.Header.Get("X-Test-Admit-Panic") != "" {
			panic("admit boom")
		}
		return strings.HasPrefix(r.URL.Path, "/test/") || next(r)
	}
}

// waitOpen waits until at most want places are in use.
func (s *serveEnv) waitOpen(t *testing.T, want int, wait time.Duration) time.Duration {
	t.Helper()
	start := time.Now()
	for s.lim.open() > want {
		if time.Since(start) > wait {
			t.Fatalf("%d places in use after %v, want %d", s.lim.open(), wait, want)
		}
		time.Sleep(time.Millisecond)
	}
	return time.Since(start)
}

// dialWeb connects to the plain listener.
func (s *serveEnv) dialWeb(t *testing.T) net.Conn {
	t.Helper()
	return dialTCP(t, s.webAddr, nil)
}

// dialAgentRaw connects to the TLS listener without a handshake.
func (s *serveEnv) dialAgentRaw(t *testing.T) net.Conn {
	t.Helper()
	return dialTCP(t, s.agentAddr, nil)
}

// dialAgent connects to the TLS listener and completes the handshake.
func (s *serveEnv) dialAgent(t *testing.T) *tls.Conn {
	t.Helper()
	c := tls.Client(s.dialAgentRaw(t), testClientTLS())
	_ = c.SetDeadline(time.Now().Add(10 * time.Second))
	if err := c.Handshake(); err != nil {
		t.Fatal(err)
	}
	_ = c.SetDeadline(time.Time{})
	return c
}

// both runs f for the plain listener and for the TLS listener.
func (s *serveEnv) both(t *testing.T, f func(t *testing.T, dial func() net.Conn)) {
	t.Run("web", func(t *testing.T) { f(t, func() net.Conn { return s.dialWeb(t) }) })
	t.Run("agent", func(t *testing.T) { f(t, func() net.Conn { return s.dialAgent(t) }) })
}

func testClientTLS(protos ...string) *tls.Config {
	return &tls.Config{InsecureSkipVerify: true, MinVersion: tls.VersionTLS12, NextProtos: protos}
}

// dialTCP connects to addr; control (if set) adjusts the socket before it
// connects.
func dialTCP(t *testing.T, addr string, control func(fd uintptr)) net.Conn {
	t.Helper()
	d := net.Dialer{Timeout: 10 * time.Second}
	if control != nil {
		d.Control = func(_, _ string, rc syscall.RawConn) error {
			return rc.Control(control)
		}
	}
	c, err := d.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })
	return c
}

// answer is one response read from a connection.
type answer struct {
	status int
	header http.Header
	body   string
	close  bool // Connection: close
	err    error
}

// readAnswer reads one response from br within wait.
func readAnswer(c net.Conn, br *bufio.Reader, method string, wait time.Duration) answer {
	_ = c.SetReadDeadline(time.Now().Add(wait))
	defer func() { _ = c.SetReadDeadline(time.Time{}) }()
	resp, err := http.ReadResponse(br, &http.Request{Method: method})
	if err != nil {
		return answer{err: err}
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	return answer{status: resp.StatusCode, header: resp.Header, body: string(b), close: resp.Close, err: err}
}

// roundTrip sends raw on c and reads one answer.
func roundTrip(t *testing.T, c net.Conn, raw string) answer {
	t.Helper()
	if _, err := io.WriteString(c, raw); err != nil {
		t.Fatal(err)
	}
	method, _, _ := strings.Cut(raw, " ")
	return readAnswer(c, bufio.NewReader(c), method, 10*time.Second)
}

// untilClosed reads away what comes on c and returns how long it took the
// server to close it (EOF or reset); ok is false when it did not within
// wait.
func untilClosed(c net.Conn, wait time.Duration) (time.Duration, bool) {
	start := time.Now()
	_ = c.SetReadDeadline(start.Add(wait))
	_, err := io.Copy(io.Discard, c)
	if ne, isNet := err.(net.Error); isNet && ne.Timeout() {
		return time.Since(start), false
	}
	return time.Since(start), true
}

func getReq(path string) string {
	return "GET " + path + " HTTP/1.1\r\nHost: x\r\n\r\n"
}

// dribble writes data one byte at a time with gap between the bytes.
func dribble(c net.Conn, data string, gap time.Duration) error {
	for i := 0; i < len(data); i++ {
		if _, err := c.Write([]byte{data[i]}); err != nil {
			return err
		}
		time.Sleep(gap)
	}
	return nil
}

// slowWriter writes every byte on its own with a pause (a slow TLS client).
type slowWriter struct {
	net.Conn
	gap time.Duration
}

func (s *slowWriter) Write(p []byte) (int, error) {
	for i := range p {
		if _, err := s.Conn.Write(p[i : i+1]); err != nil {
			return i, err
		}
		time.Sleep(s.gap)
	}
	return len(p), nil
}

// newTestServerCert returns a self-signed server certificate for 127.0.0.1
// with the given serial number.
func newTestServerCert(t *testing.T, serial int64) *tls.Certificate {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(serial), Subject: pkix.Name{CommonName: "test"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour),
		KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		IPAddresses: []net.IP{net.ParseIP("127.0.0.1")},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	leaf, _ := x509.ParseCertificate(der)
	return &tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key, Leaf: leaf}
}

// Many requests at once are all served at once: no worker limit, no
// "busy".
func TestServeManyAtOnce(t *testing.T) {
	s := startServe(t, serveOpts{})
	run := func(n int, path string, limit time.Duration) {
		var wg sync.WaitGroup
		var failed atomic.Int32
		var first atomic.Value
		start := time.Now()
		for i := 0; i < n; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				c, err := net.DialTimeout("tcp", s.webAddr, 10*time.Second)
				if err != nil {
					failed.Add(1)
					first.CompareAndSwap(nil, err.Error())
					return
				}
				defer c.Close()
				_, _ = io.WriteString(c, getReq(path))
				a := readAnswer(c, bufio.NewReader(c), "GET", 30*time.Second)
				if a.status != http.StatusOK {
					failed.Add(1)
					first.CompareAndSwap(nil, fmt.Sprintf("%d %v", a.status, a.err))
				}
			}()
		}
		wg.Wait()
		d := time.Since(start)
		if failed.Load() > 0 || d > limit {
			t.Fatalf("%d requests of %s: %d failed (first: %v), all done after %v (limit %v)", n, path, failed.Load(), first.Load(), d, limit)
		}
	}
	// 300 handlers of 300 ms each: served one after another they would take
	// 90 s; at once about a second.
	run(300, "/test/slow?ms=300", 8*time.Second)
	if m := s.maxInWork.Load(); m < 100 {
		t.Fatalf("at most %d handlers worked at the same time, want most of 300", m)
	}
	run(500, "/api/v1/health", 20*time.Second)
}

// A peer that sends its TLS handshake, its head or its body one byte at a
// time, for several idle times, is never cut and gets its answer.
func TestServeSlowPeerNeverCut(t *testing.T) {
	s := startServe(t, serveOpts{})
	gap := s.idle / 10
	var wg sync.WaitGroup
	check := func(what string, f func() (answer, error)) {
		wg.Add(1)
		go func() {
			defer wg.Done()
			start := time.Now()
			a, err := f()
			if err != nil || a.status != http.StatusOK {
				t.Errorf("%s: %d %q %v %v", what, a.status, a.body, a.err, err)
				return
			}
			if d := time.Since(start); d < 3*s.idle {
				t.Errorf("%s took only %v: not several idle times", what, d)
			}
		}()
	}
	check("head one byte at a time", func() (answer, error) {
		c := s.dialWeb(t)
		err := dribble(c, "GET /api/v1/health HTTP/1.1\r\nHost: x\r\nX-Slow: 1\r\n\r\n", gap)
		return readAnswer(c, bufio.NewReader(c), "GET", 10*time.Second), err
	})
	check("body one byte at a time", func() (answer, error) {
		c := s.dialWeb(t)
		body := strings.Repeat("b", 40)
		_, _ = fmt.Fprintf(c, "POST /test/echo HTTP/1.1\r\nHost: x\r\nContent-Length: %d\r\n\r\n", len(body))
		err := dribble(c, body, gap)
		a := readAnswer(c, bufio.NewReader(c), "POST", 10*time.Second)
		if !strings.HasPrefix(a.body, "40 ") {
			err = fmt.Errorf("echo %q", a.body)
		}
		return a, err
	})
	check("TLS handshake one byte at a time", func() (answer, error) {
		raw := s.dialAgentRaw(t)
		// One small key share keeps the client's flights short.
		cfg := testClientTLS()
		cfg.CurvePreferences = []tls.CurveID{tls.X25519}
		c := tls.Client(&slowWriter{Conn: raw, gap: gap / 5}, cfg)
		if err := c.Handshake(); err != nil {
			return answer{}, err
		}
		if _, err := io.WriteString(c, getReq("/agent/v1/ca")); err != nil {
			return answer{}, err
		}
		return readAnswer(c, bufio.NewReader(c), "GET", 10*time.Second), nil
	})
	wg.Wait()
}

// A silent peer is closed after the idle time in every state, on both
// listeners; meanwhile other requests are answered at once.
func TestServeSilentPeerCut(t *testing.T) {
	s := startServe(t, serveOpts{})
	type state struct {
		what  string
		setup func() net.Conn
	}
	partialHello := "\x16\x03\x01\x02\x00\x01\x00\x01\xfc\x03\x03" + strings.Repeat("r", 20)
	states := []state{
		{"web, nothing sent", func() net.Conn { return s.dialWeb(t) }},
		{"web, half a head", func() net.Conn {
			c := s.dialWeb(t)
			_, _ = io.WriteString(c, "GET /api/v1/health HTTP/1.1\r\nHo")
			return c
		}},
		{"web, half a body", func() net.Conn {
			c := s.dialWeb(t)
			_, _ = io.WriteString(c, "POST /test/echo HTTP/1.1\r\nHost: x\r\nContent-Length: 10\r\n\r\n01234")
			return c
		}},
		{"web, between requests", func() net.Conn {
			c := s.dialWeb(t)
			if a := roundTrip(t, c, getReq("/api/v1/health")); a.status != http.StatusOK {
				t.Errorf("between requests: first request %d %v", a.status, a.err)
			}
			return c
		}},
		{"agent, nothing sent", func() net.Conn { return s.dialAgentRaw(t) }},
		{"agent, half a ClientHello", func() net.Conn {
			c := s.dialAgentRaw(t)
			_, _ = io.WriteString(c, partialHello)
			return c
		}},
		{"agent, handshake done", func() net.Conn { return s.dialAgent(t) }},
		{"agent, half a head", func() net.Conn {
			c := s.dialAgent(t)
			_, _ = io.WriteString(c, "GET /agent/v1/ca HTTP/1.1\r\nHo")
			return c
		}},
		{"agent, half a body", func() net.Conn {
			c := s.dialAgent(t)
			_, _ = io.WriteString(c, "POST /test/echo HTTP/1.1\r\nHost: x\r\nContent-Length: 10\r\n\r\n01234")
			return c
		}},
		{"agent, between requests", func() net.Conn {
			c := s.dialAgent(t)
			if a := roundTrip(t, c, getReq("/agent/v1/ca")); a.status != http.StatusOK {
				t.Errorf("agent between requests: first request %d %v", a.status, a.err)
			}
			return c
		}},
	}
	var wg sync.WaitGroup
	for _, st := range states {
		c := st.setup()
		wg.Add(1)
		go func() {
			defer wg.Done()
			d, ok := untilClosed(c, s.idle+cutSlack+3*time.Second)
			if !ok || d < s.idle-100*time.Millisecond || d > s.idle+cutSlack {
				t.Errorf("%s: closed=%v after %v, want closed after %v", st.what, ok, d, s.idle)
			}
		}()
	}
	// Meanwhile requests are answered at once.
	for i := 0; i < 5; i++ {
		start := time.Now()
		if a := roundTrip(t, s.dialWeb(t), getReq("/api/v1/health")); a.status != http.StatusOK || time.Since(start) > s.idle/2 {
			t.Errorf("request next to silent peers: %d %v after %v", a.status, a.err, time.Since(start))
		}
		time.Sleep(s.idle / 5)
	}
	wg.Wait()
	s.waitOpen(t, s.baselineCnt, 5*time.Second)
	if !strings.Contains(s.logs.String(), "TLS handshake error") {
		t.Errorf("no handshake error logged for the silent TLS peers: %q", s.logs.String())
	}
}

// smallReceiveBuffer gives a client socket a small receive buffer, so that
// what the server's kernel takes follows what the client reads.
func smallReceiveBuffer(fd uintptr) {
	_ = syscall.SetsockoptInt(int(fd), syscall.SOL_SOCKET, syscall.SO_RCVBUF, 4096)
}

// A peer that reads nothing of a large answer is cut one idle time after
// the last byte the central's kernel took; a peer that reads slowly gets
// everything.
func TestServeUnreadResponse(t *testing.T) {
	s := startServe(t, serveOpts{})
	c := dialTCP(t, s.webAddr, smallReceiveBuffer)
	start := time.Now()
	_, _ = io.WriteString(c, getReq("/test/big?mib=16"))
	for s.lim.open() <= s.baselineCnt {
		if time.Since(start) > 5*time.Second {
			t.Fatal("the connection was not accepted")
		}
		time.Sleep(time.Millisecond)
	}
	// The buffers of both sides fill within a moment; then nothing leaves
	// the central for the idle time.
	d := s.waitOpen(t, s.baselineCnt, s.idle+cutSlack+3*time.Second)
	if since := time.Since(start); since < s.idle || d > s.idle+cutSlack {
		t.Fatalf("unread answer: released after %v, want one idle time (%v) after the buffers filled", since, s.idle)
	}

	slow := dialTCP(t, s.webAddr, smallReceiveBuffer)
	_, _ = io.WriteString(slow, getReq("/test/big?mib=2"))
	start = time.Now()
	br := bufio.NewReader(slow)
	resp, err := http.ReadResponse(br, nil)
	if err != nil {
		t.Fatal(err)
	}
	n := 0
	buf := make([]byte, 4096)
	for {
		m, err := resp.Body.Read(buf)
		n += m
		if err != nil {
			if err != io.EOF {
				t.Fatalf("slow reader cut after %d bytes and %v: %v", n, time.Since(start), err)
			}
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if n != 2<<20 || time.Since(start) < 2*s.idle {
		t.Fatalf("slow reader: %d bytes after %v, want all of 2 MiB over several idle times", n, time.Since(start))
	}
}

// A handler that works several idle times answers, with and without a
// body, and the connection serves a further request.
func TestServeLongHandler(t *testing.T) {
	s := startServe(t, serveOpts{})
	work := 3 * s.idle
	s.both(t, func(t *testing.T, dial func() net.Conn) {
		c := dial()
		br := bufio.NewReader(c)
		_, _ = fmt.Fprintf(c, "GET /test/slow?ms=%d HTTP/1.1\r\nHost: x\r\n\r\n", work.Milliseconds())
		if a := readAnswer(c, br, "GET", work+10*time.Second); a.status != http.StatusOK || a.body != "slow" {
			t.Fatalf("long handler without a body: %d %q %v", a.status, a.body, a.err)
		}
		_, _ = fmt.Fprintf(c, "POST /test/echo?ms=%d HTTP/1.1\r\nHost: x\r\nContent-Length: 3\r\n\r\nabc", work.Milliseconds())
		if a := readAnswer(c, br, "POST", work+10*time.Second); a.status != http.StatusOK || a.body != "3 294" {
			t.Fatalf("long handler with a body: %d %q %v", a.status, a.body, a.err)
		}
		_, _ = io.WriteString(c, getReq("/test/slow?ms=1"))
		if a := readAnswer(c, br, "GET", 10*time.Second); a.status != http.StatusOK {
			t.Fatalf("further request: %d %v", a.status, a.err)
		}
	})
}

// A peer that closes is released at once in every waiting state; a
// running handler finishes with its context not cancelled, and its place
// is free when it has returned.
func TestServePeerCloses(t *testing.T) {
	s := startServe(t, serveOpts{idle: 30 * time.Second})
	for _, st := range []struct {
		what  string
		setup func() net.Conn
	}{
		{"web, nothing sent", func() net.Conn { return s.dialWeb(t) }},
		{"web, half a head", func() net.Conn {
			c := s.dialWeb(t)
			_, _ = io.WriteString(c, "GET / HTTP/1.1\r\nHo")
			return c
		}},
		{"web, half a body", func() net.Conn {
			c := s.dialWeb(t)
			_, _ = io.WriteString(c, "POST /test/echo HTTP/1.1\r\nHost: x\r\nContent-Length: 10\r\n\r\n01234")
			return c
		}},
		{"web, between requests", func() net.Conn {
			c := s.dialWeb(t)
			roundTrip(t, c, getReq("/api/v1/health"))
			return c
		}},
		{"agent, half a ClientHello", func() net.Conn {
			c := s.dialAgentRaw(t)
			_, _ = io.WriteString(c, "\x16\x03\x01\x02\x00\x01")
			return c
		}},
		{"agent, handshake done", func() net.Conn { return s.dialAgent(t) }},
		{"agent, between requests", func() net.Conn {
			c := s.dialAgent(t)
			roundTrip(t, c, getReq("/agent/v1/ca"))
			return c
		}},
	} {
		c := st.setup()
		time.Sleep(100 * time.Millisecond)
		if s.lim.open() <= s.baselineCnt {
			t.Fatalf("%s: the connection holds no place", st.what)
		}
		_ = c.Close()
		if d := s.waitOpen(t, s.baselineCnt, 5*time.Second); d > time.Second {
			t.Fatalf("%s: released %v after the close", st.what, d)
		}
	}

	// A running handler: the peer closes while it works.
	for i, dial := range []func() net.Conn{func() net.Conn { return s.dialWeb(t) }, func() net.Conn { return s.dialAgent(t) }} {
		id := strconv.Itoa(i)
		c := dial()
		_, _ = io.WriteString(c, getReq("/test/hold?ms=1500&id="+id))
		time.Sleep(300 * time.Millisecond)
		_ = c.Close()
		s.waitOpen(t, s.baselineCnt, 10*time.Second)
		released := time.Now()
		v, ok := s.held.Load(id)
		if !ok {
			t.Fatalf("handler %s: place released before the handler ended", id)
		}
		h := v.(heldResult)
		if h.ctxErr != nil {
			t.Fatalf("handler %s: context ended: %v", id, h.ctxErr)
		}
		if d := released.Sub(h.ended); d > time.Second {
			t.Fatalf("handler %s: place released %v after the handler ended", id, d)
		}
	}
}

// The handler's context carries the peer and the local address, the TLS
// state on the agent listener, and is never done.
func TestServeHandlerContext(t *testing.T) {
	s := startServe(t, serveOpts{})
	for _, tc := range []struct {
		what string
		c    net.Conn
		tls  bool
	}{{"web", s.dialWeb(t), false}, {"agent", s.dialAgent(t), true}} {
		a := roundTrip(t, tc.c, getReq("/test/ctx"))
		want := fmt.Sprintf("remote=%s local=%s tcp=true tls=%v done=false", tc.c.LocalAddr(), tc.c.RemoteAddr(), tc.tls)
		if a.status != http.StatusOK || a.body != want {
			t.Fatalf("%s: %d %q, want %q", tc.what, a.status, a.body, want)
		}
	}
}

// Many requests on persistent connections, with and without bodies, on
// many connections at once are all answered.
func TestServePersistentConnections(t *testing.T) {
	s := startServe(t, serveOpts{})
	var wg sync.WaitGroup
	for i := 0; i < 24; i++ {
		var c net.Conn
		if i%2 == 0 {
			c = s.dialWeb(t)
		} else {
			c = s.dialAgent(t)
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			br := bufio.NewReader(c)
			for j := 0; j < 100; j++ {
				var send, method string
				switch j % 3 {
				case 0:
					send, method = getReq("/test/slow?ms=0"), "GET"
				case 1:
					send, method = "POST /test/echo HTTP/1.1\r\nHost: x\r\nContent-Length: 5\r\n\r\nhello", "POST"
				default:
					send, method = getReq("/test/slow?ms=2"), "GET"
				}
				if _, err := io.WriteString(c, send); err != nil {
					t.Errorf("connection %d request %d: %v", i, j, err)
					return
				}
				if a := readAnswer(c, br, method, 10*time.Second); a.status != http.StatusOK {
					t.Errorf("connection %d request %d: %d %v", i, j, a.status, a.err)
					return
				}
			}
		}()
	}
	wg.Wait()
}

// The TLS listener offers only HTTP/1.1, serves a certificate replaced
// while running on new connections, and sends the closing alert.
func TestServeTLS(t *testing.T) {
	s := startServe(t, serveOpts{})
	c := tls.Client(s.dialAgentRaw(t), testClientTLS("h2", "http/1.1"))
	if err := c.Handshake(); err != nil {
		t.Fatal(err)
	}
	if p := c.ConnectionState().NegotiatedProtocol; p != "http/1.1" {
		t.Fatalf("negotiated %q, want http/1.1", p)
	}
	s.e.c.serverCert.Store(newTestServerCert(t, 4242))
	c2 := s.dialAgent(t)
	if sn := c2.ConnectionState().PeerCertificates[0].SerialNumber.Int64(); sn != 4242 {
		t.Fatalf("certificate serial %d after the replacement, want 4242", sn)
	}
	// The closing alert: after an answer with Connection: close the client
	// reads a clean end, not an unexpected one.
	_, _ = io.WriteString(c2, "GET /agent/v1/ca HTTP/1.1\r\nHost: x\r\nConnection: close\r\n\r\n")
	br := bufio.NewReader(c2)
	if a := readAnswer(c2, br, "GET", 10*time.Second); a.status != http.StatusOK {
		t.Fatalf("request: %d %v", a.status, a.err)
	}
	_ = c2.SetReadDeadline(time.Now().Add(10 * time.Second))
	if _, err := br.ReadByte(); err != io.EOF {
		t.Fatalf("after the answer: %v, want io.EOF (close_notify)", err)
	}
}

// The answers net/http writes itself before any handler runs.
func TestServeLibraryAnswers(t *testing.T) {
	s := startServe(t, serveOpts{})
	s.both(t, func(t *testing.T, dial func() net.Conn) {
		for _, tc := range []struct {
			what, send string
			want       int
		}{
			{"head of 64 KiB + 5 KiB", "GET /test/slow HTTP/1.1\r\nHost: x\r\nX: " + strings.Repeat("a", modules.MaxHeadBytes+5000) + "\r\n\r\n", http.StatusRequestHeaderFieldsTooLarge},
			{"head of 63 KiB", "GET /test/slow HTTP/1.1\r\nHost: x\r\nX: " + strings.Repeat("a", 63<<10) + "\r\n\r\n", http.StatusOK},
			{"not a request", "NOT A REQUEST\r\n\r\n", http.StatusBadRequest},
			{"HTTP/1.1 without Host", "GET /test/slow HTTP/1.1\r\n\r\n", http.StatusBadRequest},
			{"unknown Expect", "GET /test/slow HTTP/1.1\r\nHost: x\r\nExpect: something\r\n\r\n", http.StatusExpectationFailed},
			{"unknown transfer coding", "POST /test/echo HTTP/1.1\r\nHost: x\r\nTransfer-Encoding: gzip\r\n\r\n", http.StatusNotImplemented},
			{"HTTP/2.0 request line", "GET /test/slow HTTP/2.0\r\nHost: x\r\n\r\n", http.StatusHTTPVersionNotSupported},
		} {
			if a := roundTrip(t, dial(), tc.send); a.status != tc.want {
				t.Errorf("%s: %d %q %v, want %d", tc.what, a.status, a.body, a.err, tc.want)
			}
		}
		var b strings.Builder
		b.WriteString("GET /test/slow HTTP/1.1\r\nHost: x\r\n")
		for i := 0; i < 5000; i++ {
			fmt.Fprintf(&b, "X-%d: v\r\n", i)
		}
		b.WriteString("\r\n")
		if b.Len() > modules.MaxHeadBytes {
			t.Fatalf("test head of %d bytes", b.Len())
		}
		if a := roundTrip(t, dial(), b.String()); a.status != http.StatusOK {
			t.Errorf("5000 header lines within 64 KiB: %d %v", a.status, a.err)
		}
	})
	if a := roundTrip(t, s.dialAgentRaw(t), getReq("/agent/v1/ca")); a.status != http.StatusBadRequest || !strings.Contains(a.body, "HTTPS server") {
		t.Errorf("plain HTTP to the TLS listener: %d %q %v", a.status, a.body, a.err)
	}
}

// Over a real connection the management API answers a client of this host
// (loopback, or the host's own address to that address) with the local
// address the library provides.
func TestManagementPeerRealConnection(t *testing.T) {
	call := func(t *testing.T, s *serveEnv, src string) int {
		d := net.Dialer{Timeout: 10 * time.Second, LocalAddr: &net.TCPAddr{IP: net.ParseIP(src)}}
		raw, err := d.Dial("tcp", s.agentAddr)
		if err != nil {
			t.Fatal(err)
		}
		defer raw.Close()
		c := tls.Client(raw, testClientTLS())
		a := roundTrip(t, c, "GET /admin/v1/agents HTTP/1.1\r\nHost: x\r\nAuthorization: Bearer "+s.e.cliKey+"\r\n\r\n")
		return a.status
	}
	t.Run("loopback", func(t *testing.T) {
		s := startServe(t, serveOpts{})
		if code := call(t, s, "127.0.0.1"); code != http.StatusOK {
			t.Fatalf("loopback: %d, want 200", code)
		}
	})
	t.Run("own address", func(t *testing.T) {
		own := hostIPv4(t)
		s := startServe(t, serveOpts{listen: net.JoinHostPort(own, "0")})
		if code := call(t, s, own); code != http.StatusOK {
			t.Fatalf("own address %s: %d, want 200", own, code)
		}
	})
}

// hostIPv4 returns a non-loopback IPv4 address of this host (skips the
// test when there is none).
func hostIPv4(t *testing.T) string {
	t.Helper()
	addrs, err := net.InterfaceAddrs()
	if err != nil {
		t.Skip(err)
	}
	for _, a := range addrs {
		if ipn, ok := a.(*net.IPNet); ok && !ipn.IP.IsLoopback() && ipn.IP.To4() != nil {
			return ipn.IP.String()
		}
	}
	t.Skip("this host has no IPv4 address besides loopback")
	return ""
}

// At stop a request in work and a request whose body is arriving are
// answered; a connection between requests and one without a head are
// closed; new connections are refused; Shutdown needs no time limit.
func TestServeStop(t *testing.T) {
	s := startServe(t, serveOpts{idle: 30 * time.Second})
	between := s.dialWeb(t)
	roundTrip(t, between, getReq("/api/v1/health"))
	fresh := s.dialWeb(t)
	partial := s.dialWeb(t)
	_, _ = io.WriteString(partial, "GET / HTTP/1.1\r\nHo")
	work := s.dialWeb(t)
	_, _ = io.WriteString(work, getReq("/test/slow?ms=1500"))
	tlsWork := s.dialAgent(t)
	_, _ = io.WriteString(tlsWork, getReq("/test/slow?ms=1500"))
	arriving := s.dialWeb(t)
	_, _ = io.WriteString(arriving, "POST /test/echo HTTP/1.1\r\nHost: x\r\nContent-Length: 10\r\n\r\n01234")
	time.Sleep(300 * time.Millisecond)

	stopped := make(chan struct{})
	go func() { s.stop(); close(stopped) }()
	var wg sync.WaitGroup
	for what, c := range map[string]net.Conn{"between requests": between, "nothing sent": fresh, "half a head": partial} {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, ok := untilClosed(c, 20*time.Second); !ok {
				t.Errorf("%s: not closed at stop", what)
			}
		}()
	}
	time.Sleep(500 * time.Millisecond)
	_, _ = io.WriteString(arriving, "56789")
	if a := readAnswer(arriving, bufio.NewReader(arriving), "POST", 10*time.Second); a.status != http.StatusOK || a.body != "10 525" {
		t.Errorf("request whose body arrived during the stop: %d %q %v", a.status, a.body, a.err)
	}
	for what, c := range map[string]net.Conn{"web request in work": work, "agent request in work": tlsWork} {
		if a := readAnswer(c, bufio.NewReader(c), "GET", 10*time.Second); a.status != http.StatusOK || a.body != "slow" {
			t.Errorf("%s: %d %q %v", what, a.status, a.body, a.err)
		}
	}
	wg.Wait()
	select {
	case <-stopped:
	case <-time.After(20 * time.Second):
		t.Fatal("Shutdown did not return")
	}
	if c, err := net.DialTimeout("tcp", s.webAddr, time.Second); err == nil {
		_ = c.Close()
		t.Fatal("a new connection was accepted after the stop")
	}
}
