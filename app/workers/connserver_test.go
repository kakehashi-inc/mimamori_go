package workers

import (
	"bufio"
	"bytes"
	"context"
	"crypto/tls"
	"fmt"
	"io"
	"log"
	"math/rand"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"mimamori/app/models"
	"mimamori/app/modules"
)

// The management API is used only by a client of this host: a loopback
// source address, or a source address equal to the local address the
// connection was accepted on. Forwarding headers are never used.
func TestIsLocalPeer(t *testing.T) {
	local := &net.TCPAddr{IP: net.ParseIP("192.0.2.7"), Port: 9081}
	local6 := &net.TCPAddr{IP: net.ParseIP("2001:db8::7"), Port: 9081}
	mapped := &net.TCPAddr{IP: net.ParseIP("::ffff:192.0.2.7"), Port: 9081}
	cases := []struct {
		remote string
		local  net.Addr
		want   bool
	}{
		{"127.0.0.1:5000", nil, true},
		{"127.0.0.2:5000", local, true},
		{"[::1]:5000", nil, true},
		{"[::ffff:127.0.0.1]:5000", nil, true},
		{"192.0.2.7:5000", local, true},
		{"[::ffff:192.0.2.7]:5000", local, true},
		{"192.0.2.7:5000", mapped, true},
		{"[2001:db8::7]:5000", local6, true},
		{"192.0.2.8:5000", local, false},
		{"192.0.2.7:5000", nil, false},
		{"192.0.2.7:5000", local6, false},
		{"0.0.0.0:5000", &net.TCPAddr{IP: net.IPv4zero}, false},
		{"[fe80::7%eth0]:5000", &net.TCPAddr{IP: net.ParseIP("fe80::7"), Zone: "eth0"}, true},
		{"[fe80::7%eth1]:5000", &net.TCPAddr{IP: net.ParseIP("fe80::7"), Zone: "eth0"}, false},
		{"garbage", local, false},
	}
	for _, c := range cases {
		if got := isLocalPeer(c.remote, c.local); got != c.want {
			t.Errorf("isLocalPeer(%q, %v) = %v, want %v", c.remote, c.local, got, c.want)
		}
	}
}

// The management API answers a client that connected from the host's own
// address to that address, and stays hidden for every other peer.
func TestAdminAPIFromOwnAddress(t *testing.T) {
	e := newTestEnv(t)
	call := func(remote, local string) int {
		req, _ := http.NewRequest("GET", "/admin/v1/agents", nil)
		req.RemoteAddr = remote
		req.Header.Set("Authorization", "Bearer "+e.cliKey)
		addr, _ := net.ResolveTCPAddr("tcp", local)
		req = req.WithContext(context.WithValue(req.Context(), http.LocalAddrContextKey, addr))
		rec := httptest.NewRecorder()
		e.agent.ServeHTTP(rec, req)
		return rec.Code
	}
	if code := call("192.0.2.7:4000", "192.0.2.7:9081"); code != http.StatusOK {
		t.Fatalf("own address: status %d, want 200", code)
	}
	if code := call("192.0.2.8:4000", "192.0.2.7:9081"); code != http.StatusNotFound {
		t.Fatalf("other address: status %d, want 404", code)
	}
	if code := call("[::ffff:192.0.2.7]:4000", "192.0.2.7:9081"); code != http.StatusOK {
		t.Fatalf("IPv4-mapped own address: status %d, want 200", code)
	}
}

// foreignSources are loopback addresses the test servers treat like
// sources of other hosts (the penalty never applies to this host).
var foreignSources = map[string]bool{"127.0.0.2": true, "127.0.0.3": true}

// connTestServer runs the central's connection server on loopback
// listeners: the browser handler on plain HTTP and the agent handler on
// HTTPS, both wrapped by testRoutes.
type connTestServer struct {
	e         *testEnv
	srv       *connServer
	web, agnt string
	stop      atomic.Bool
	done      chan struct{}
}

// startConnTestServer starts the test server on 127.0.0.1. Every source
// address is a source of its own for the handshake order (the central
// counts all loopback addresses as one source; see
// TestConnServerLoopbackIsOneSource), so that loopback addresses can play
// several hosts, and foreignSources are not exempt from the refusal.
func startConnTestServer(t *testing.T, workers int, queueTimeout time.Duration) *connTestServer {
	t.Helper()
	return startConnTestServerWith(t, workers, queueTimeout, "127.0.0.1:0", func(s *connServer) {
		s.exempt = func(host string) bool {
			return !foreignSources[host] && s.isOwnSource(host)
		}
		s.source = func(host string) string { return host }
	})
}

// startConnTestServerOn starts the test server with both listeners on the
// IP address of listen and the central's own rules for sources.
func startConnTestServerOn(t *testing.T, workers int, queueTimeout time.Duration, listen string) *connTestServer {
	t.Helper()
	return startConnTestServerWith(t, workers, queueTimeout, listen, nil)
}

// startConnTestServerWith starts the test server; setup (if set) changes
// the server before its accepting flow starts.
func startConnTestServerWith(t *testing.T, workers int, queueTimeout time.Duration, listen string, setup func(*connServer)) *connTestServer {
	t.Helper()
	e := newTestEnv(t)
	webLn, err := listenTCP(listen)
	if err != nil {
		t.Fatal(err)
	}
	agentLn, err := listenTCP(listen)
	if err != nil {
		t.Fatal(err)
	}
	specs := []*listenerSpec{
		{name: "web", ln: webLn, handler: testRoutes(e.web), admit: testAdmit(e.c.admitWebBody)},
		{name: "agent", ln: agentLn, tls: e.c.tlsConfig(), handler: testRoutes(e.agent), admit: testAdmit(e.c.admitAgentBody)},
	}
	ts := &connTestServer{e: e, web: webLn.Addr().String(), agnt: agentLn.Addr().String(), done: make(chan struct{})}
	ts.srv = newConnServer(specs, workers, queueTimeout, log.New(io.Discard, "", 0))
	if setup != nil {
		setup(ts.srv)
	}
	// The test runs the accepting flow on a goroutine of its own; with one
	// worker the server itself starts none. The stop signal is reported
	// once, like the central's (a signal read from a channel).
	go func() {
		defer close(ts.done)
		ts.srv.run(func() bool { return ts.stop.Swap(false) }, func() func() { return nil })
	}()
	t.Cleanup(ts.shutdown)
	return ts
}

func (ts *connTestServer) shutdown() {
	ts.stop.Store(true)
	<-ts.done
}

// testRoutes adds routes for the tests in front of a handler:
// /test/slow?ms=N answers after N ms, /test/big?mib=N answers N MiB,
// /test/echo answers the number of body bytes and their sum.
func testRoutes(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/test/slow":
			ms, _ := strconv.Atoi(r.URL.Query().Get("ms"))
			time.Sleep(time.Duration(ms) * time.Millisecond)
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
			sum := 0
			for _, b := range data {
				sum += int(b)
			}
			fmt.Fprintf(w, "%d %d", len(data), sum)
		default:
			next.ServeHTTP(w, r)
		}
	})
}

// testAdmit admits the large bodies of the test routes and asks the
// central's check for every other route.
func testAdmit(next func(*http.Request) bool) func(*http.Request) bool {
	return func(r *http.Request) bool {
		return strings.HasPrefix(r.URL.Path, "/test/") || next(r)
	}
}

// dial connects to addr, from the loopback address src when it is set.
func dial(t *testing.T, addr, src string) net.Conn {
	t.Helper()
	d := net.Dialer{Timeout: 5 * time.Second}
	if src != "" {
		d.LocalAddr = &net.TCPAddr{IP: net.ParseIP(src)}
	}
	c, err := d.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.Close() })
	return c
}

func dialSilent(t *testing.T, addr string, n int) []net.Conn {
	t.Helper()
	var out []net.Conn
	for i := 0; i < n; i++ {
		out = append(out, dial(t, addr, ""))
	}
	return out
}

// statusLine reads the first line of an answer within wait.
func statusLine(c net.Conn, wait time.Duration) string {
	_ = c.SetReadDeadline(time.Now().Add(wait))
	line, err := bufio.NewReader(c).ReadString('\n')
	if line == "" && err != nil {
		return "error: " + err.Error()
	}
	return strings.TrimSpace(line)
}

// webRaw sends raw bytes to the plain listener and returns the status line
// and how long it took.
func (ts *connTestServer) webRaw(t *testing.T, raw string) (string, time.Duration) {
	t.Helper()
	start := time.Now()
	c, err := net.DialTimeout("tcp", ts.web, 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if _, err := io.WriteString(c, raw); err != nil {
		t.Fatal(err)
	}
	line := statusLine(c, 30*time.Second)
	return line, time.Since(start)
}

// agentDo sends one HTTPS request to the agent listener from src (empty:
// 127.0.0.1) and returns the status and body.
func (ts *connTestServer) agentDo(src, method, path string, body []byte, hdr map[string]string) (int, []byte, time.Duration, error) {
	start := time.Now()
	d := net.Dialer{Timeout: 5 * time.Second}
	if src != "" {
		d.LocalAddr = &net.TCPAddr{IP: net.ParseIP(src)}
	}
	raw, err := d.Dial("tcp", ts.agnt)
	if err != nil {
		return 0, nil, 0, err
	}
	defer raw.Close()
	_ = raw.SetDeadline(time.Now().Add(30 * time.Second))
	pool, _ := modules.CertPoolFromPEMs(ts.e.c.currentCAPEMs(), time.Now())
	c := tls.Client(raw, &tls.Config{RootCAs: pool, ServerName: "127.0.0.1", MinVersion: tls.VersionTLS12})
	req, _ := http.NewRequest(method, "https://"+ts.agnt+path, bytes.NewReader(body))
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	req.Close = true
	bw := bufio.NewWriter(c)
	if err := req.Write(bw); err != nil {
		return 0, nil, 0, err
	}
	if err := bw.Flush(); err != nil {
		return 0, nil, 0, err
	}
	resp, err := http.ReadResponse(bufio.NewReader(c), req)
	if err != nil {
		return 0, nil, 0, err
	}
	data, err := io.ReadAll(resp.Body)
	resp.Body.Close()
	return resp.StatusCode, data, time.Since(start), err
}

func (ts *connTestServer) agentGet(t *testing.T, path string) (int, time.Duration) {
	t.Helper()
	code, _, d, err := ts.agentDo("", "GET", path, nil, nil)
	if err != nil {
		t.Fatalf("GET %s: %v", path, err)
	}
	return code, d
}

// expectClosed checks that the server closes c within wait (bytes of the
// server's handshake before the close are read away).
func expectClosed(t *testing.T, c net.Conn, wait time.Duration, what string) {
	t.Helper()
	_ = c.SetReadDeadline(time.Now().Add(wait))
	if _, err := io.Copy(io.Discard, c); isTimeout(err) {
		t.Fatalf("%s: not closed within %v", what, wait)
	}
}

func isTimeout(err error) bool {
	ne, ok := err.(net.Error)
	return ok && ne.Timeout()
}

const healthRequest = "GET /api/v1/health HTTP/1.1\r\nHost: x\r\n\r\n"

// checkFast checks that plain requests (and HTTPS requests) are answered
// with 200 within limit.
func (ts *connTestServer) checkFast(t *testing.T, limit time.Duration, what string) {
	t.Helper()
	line, d := ts.webRaw(t, healthRequest)
	if !strings.Contains(line, " 200 ") || d > limit {
		t.Fatalf("web request %s: %q after %v (limit %v)", what, line, d, limit)
	}
	// An HTTPS request includes the handshake (crypto, slower under -race).
	code, d := ts.agentGet(t, "/agent/v1/ca")
	if code != http.StatusOK || d > limit+time.Second {
		t.Fatalf("agent request %s: %d after %v", what, code, d)
	}
}

// forWorkers runs a test with one worker (no goroutine) and with two.
func forWorkers(t *testing.T, f func(t *testing.T, workers int)) {
	for _, w := range []int{1, 2} {
		t.Run(fmt.Sprintf("workers=%d", w), func(t *testing.T) { f(t, w) })
	}
}

// Connections that send nothing, or half a request head, or half a
// ClientHello, never reach a worker: normal requests on both listeners are
// answered at once, and the waiting connections are rejected after the
// queue timeout (plain HTTP with 503, TLS by closing).
func TestConnServerSilentConnections(t *testing.T) {
	forWorkers(t, func(t *testing.T, workers int) {
		ts := startConnTestServer(t, workers, 2*time.Second)
		silentWeb := dialSilent(t, ts.web, 50)
		silentAgent := dialSilent(t, ts.agnt, 50)
		half := dial(t, ts.web, "")
		if _, err := io.WriteString(half, "GET /api/v1/health HTTP/1.1\r\nHost: x\r\n"); err != nil {
			t.Fatal(err)
		}
		halfHello := dial(t, ts.agnt, "")
		if _, err := halfHello.Write(clientHello(t)[:100]); err != nil {
			t.Fatal(err)
		}
		time.Sleep(200 * time.Millisecond)
		for i := 0; i < 3; i++ {
			ts.checkFast(t, 100*time.Millisecond, "with silent connections")
			code, _, d, err := ts.agentDo("", "GET", "/admin/v1/agents", nil, map[string]string{"Authorization": "Bearer " + ts.e.cliKey})
			if err != nil || code != http.StatusOK || d > 2*time.Second {
				t.Fatalf("management request with silent connections: %d %v after %v", code, err, d)
			}
		}
		for _, c := range []net.Conn{silentWeb[0], half} {
			if line := statusLine(c, 5*time.Second); !strings.Contains(line, " 503 ") {
				t.Fatalf("waiting plain connection: %q, want 503", line)
			}
		}
		expectClosed(t, silentAgent[0], 5*time.Second, "silent TLS connection")
		expectClosed(t, halfHello, 5*time.Second, "half ClientHello")
	})
}

// A full table makes room by dropping the oldest connection that sent
// nothing (once it has waited EvictGrace), so silent connections cannot
// keep a normal client out.
func TestConnServerFullQueue(t *testing.T) {
	ts := startConnTestServer(t, 1, 30*time.Second)
	dialSilent(t, ts.web, modules.MaxPendingConns+10)
	time.Sleep(200 * time.Millisecond)
	line, d := ts.webRaw(t, healthRequest)
	if !strings.Contains(line, " 200 ") || d > modules.EvictGrace+time.Second {
		t.Fatalf("request with a full table: %q after %v", line, d)
	}
}

// A table full of connections that sent a complete head and wait for their
// peer otherwise (a large body that does not come, or a response that is not
// read) also makes room: the oldest of them is dropped, so a normal client
// is still served.
func TestConnServerFullQueueOfBodiesAndReaders(t *testing.T) {
	forWorkers(t, func(t *testing.T, workers int) {
		ts := startConnTestServer(t, workers, 30*time.Second)
		const readers = 20
		var held []net.Conn
		for i := 0; i < readers; i++ {
			c := dial(t, ts.web, "")
			_ = c.(*net.TCPConn).SetReadBuffer(4096)
			if _, err := io.WriteString(c, "GET /test/big?mib=4 HTTP/1.1\r\nHost: x\r\n\r\n"); err != nil {
				t.Fatal(err)
			}
			held = append(held, c)
		}
		// Let the handlers finish (slower under -race); the responses stay
		// unread (4 MiB is more than the kernel's buffers take).
		time.Sleep(3 * time.Second)
		for i := 0; i < modules.MaxPendingConns-readers+10; i++ {
			c := dial(t, ts.web, "")
			if _, err := io.WriteString(c, "POST /test/echo HTTP/1.1\r\nHost: x\r\nContent-Length: 30000000\r\n\r\n"); err != nil {
				t.Fatal(err)
			}
			held = append(held, c)
		}
		time.Sleep(500 * time.Millisecond)
		// A connection is dropped only after EvictGrace in its state (the
		// readers' responses may have been finished late under -race), so
		// a new client may wait that long in the backlog.
		for i := 0; i < 3; i++ {
			ts.checkFast(t, modules.EvictGrace+time.Second, "with a table full of waiting bodies and readers")
		}
		// Each further connection evicts the oldest: the remaining readers
		// of a large response (closed; the kernel still sends what it
		// holds), then the oldest large-body connection, which gets 503.
		for i := 0; i < readers; i++ {
			c := dial(t, ts.web, "")
			if _, err := io.WriteString(c, "POST /test/echo HTTP/1.1\r\nHost: x\r\nContent-Length: 30000000\r\n\r\n"); err != nil {
				t.Fatal(err)
			}
			time.Sleep(20 * time.Millisecond)
		}
		ts.checkFast(t, modules.EvictGrace+time.Second, "after the readers were dropped")
		if line := statusLine(held[readers], 5*time.Second); !strings.Contains(line, " 503 ") {
			t.Fatalf("oldest large-body connection: %q, want 503", line)
		}
	})
}

const loginHead = "POST /api/v1/auth/login HTTP/1.1\r\nHost: x\r\nX-Mimamori-Request: 1\r\nContent-Type: application/json\r\nContent-Length: 40\r\n\r\n"

// Login requests that send the head and no body (or the body slowly) hold
// no worker: other requests are answered at once, and the stallers are
// rejected at the queue timeout. A normal login request is unchanged.
func TestConnServerStalledLoginBody(t *testing.T) {
	forWorkers(t, func(t *testing.T, workers int) {
		ts := startConnTestServer(t, workers, 2*time.Second)
		line, _ := ts.webRaw(t, loginHead+`{"email":"a@example.com","password":"x"}`)
		if !strings.Contains(line, " 401 ") {
			t.Fatalf("normal login: %q, want 401", line)
		}
		// A connection accepted right after a slow request (the password
		// check) counts its wait from before that request, as it may have
		// waited during it; a short request in between keeps the timing of
		// the stallers below exact.
		ts.webRaw(t, healthRequest)
		var stallers []net.Conn
		for i := 0; i < 3; i++ {
			c := dial(t, ts.web, "")
			if _, err := io.WriteString(c, loginHead+`{"em`); err != nil {
				t.Fatal(err)
			}
			stallers = append(stallers, c)
		}
		start := time.Now()
		time.Sleep(100 * time.Millisecond)
		for i := 0; i < 5; i++ {
			ts.checkFast(t, 100*time.Millisecond, "behind stalled login bodies")
		}
		for _, c := range stallers {
			line := statusLine(c, 5*time.Second)
			d := time.Since(start)
			if !strings.Contains(line, " 503 ") || d < 1500*time.Millisecond || d > 3500*time.Millisecond {
				t.Fatalf("stalled login body: %q after %v, want 503 at the queue timeout", line, d)
			}
		}
	})
}

// A complete ClientHello and then silence delays no plain request (the
// handshake waits in nested passes); the handshake ends at its limit, the
// source is then refused for the penalty period while another source is
// served. This host itself is never refused.
func TestConnServerStalledHandshake(t *testing.T) {
	forWorkers(t, func(t *testing.T, workers int) {
		ts := startConnTestServer(t, workers, 10*time.Second)
		hello := clientHello(t)
		staller := dial(t, ts.agnt, "127.0.0.2")
		if _, err := staller.Write(hello); err != nil {
			t.Fatal(err)
		}
		time.Sleep(100 * time.Millisecond)
		line, d := ts.webRaw(t, healthRequest)
		if !strings.Contains(line, " 200 ") || d > 100*time.Millisecond {
			t.Fatalf("request during a stalled handshake: %q after %v", line, d)
		}
		expectClosed(t, staller, 5*time.Second, "stalled handshake")

		// The same source is refused at accept, at once.
		again := dial(t, ts.agnt, "127.0.0.2")
		start := time.Now()
		if _, err := again.Write(hello); err != nil && !isTimeout(err) {
			t.Logf("write to the refused connection: %v", err)
		}
		expectClosed(t, again, time.Second, "connection of a penalised source")
		if d := time.Since(start); d > 500*time.Millisecond {
			t.Fatalf("penalised source closed after %v, want at once", d)
		}
		line, d = ts.webRaw(t, healthRequest)
		if !strings.Contains(line, " 200 ") || d > 100*time.Millisecond {
			t.Fatalf("request after a refused handshake: %q after %v", line, d)
		}
		// Another source is served.
		code, _, _, err := ts.agentDo("127.0.0.3", "GET", "/agent/v1/ca", nil, nil)
		if err != nil || code != http.StatusOK {
			t.Fatalf("other source during the penalty: %d %v", code, err)
		}

		// This host is never penalised.
		local := dial(t, ts.agnt, "")
		if _, err := local.Write(hello); err != nil {
			t.Fatal(err)
		}
		expectClosed(t, local, 5*time.Second, "stalled handshake of this host")
		if code, _ := ts.agentGet(t, "/agent/v1/ca"); code != http.StatusOK {
			t.Fatalf("this host after its stalled handshake: %d", code)
		}
	})
}

// A client that finishes the handshake and then sends no request holds no
// worker and is disconnected at the queue timeout.
func TestConnServerSilentAfterHandshake(t *testing.T) {
	forWorkers(t, func(t *testing.T, workers int) {
		ts := startConnTestServer(t, workers, 2*time.Second)
		raw := dial(t, ts.agnt, "")
		pool, _ := modules.CertPoolFromPEMs(ts.e.c.currentCAPEMs(), time.Now())
		c := tls.Client(raw, &tls.Config{RootCAs: pool, ServerName: "127.0.0.1", MinVersion: tls.VersionTLS12})
		_ = c.SetDeadline(time.Now().Add(5 * time.Second))
		if err := c.Handshake(); err != nil {
			t.Fatal(err)
		}
		start := time.Now()
		for i := 0; i < 3; i++ {
			ts.checkFast(t, 100*time.Millisecond, "after a silent handshake")
		}
		_ = c.SetDeadline(time.Now().Add(5 * time.Second))
		n, err := c.Read(make([]byte, 1))
		d := time.Since(start)
		if n != 0 || err == nil || isTimeout(err) || d > 3*time.Second {
			t.Fatalf("silent TLS client: read %d, %v after %v; want closed at the queue timeout", n, err, d)
		}
	})
}

// A client that requests a large response and does not read it holds no
// worker; it is not cut while it reads slowly within the total limit.
func TestConnServerUnreadResponse(t *testing.T) {
	forWorkers(t, func(t *testing.T, workers int) {
		ts := startConnTestServer(t, workers, 2*time.Second)
		lazy := dial(t, ts.web, "")
		if _, err := io.WriteString(lazy, "GET /test/big?mib=8 HTTP/1.1\r\nHost: x\r\n\r\n"); err != nil {
			t.Fatal(err)
		}
		lazyTLS, err := net.DialTimeout("tcp", ts.agnt, 5*time.Second)
		if err != nil {
			t.Fatal(err)
		}
		defer lazyTLS.Close()
		pool, _ := modules.CertPoolFromPEMs(ts.e.c.currentCAPEMs(), time.Now())
		tc := tls.Client(lazyTLS, &tls.Config{RootCAs: pool, ServerName: "127.0.0.1", MinVersion: tls.VersionTLS12})
		_ = tc.SetDeadline(time.Now().Add(30 * time.Second))
		if _, err := io.WriteString(tc, "GET /test/big?mib=8 HTTP/1.1\r\nHost: x\r\n\r\n"); err != nil {
			t.Fatal(err)
		}
		// Let the handlers and the encryption of the responses finish.
		time.Sleep(time.Second)
		for i := 0; i < 3; i++ {
			ts.checkFast(t, 100*time.Millisecond, "while large responses are not read")
		}
		// Past the queue timeout, the slow readers still get everything.
		time.Sleep(1500 * time.Millisecond)
		for _, r := range []io.Reader{lazy, tc} {
			_ = lazy.SetReadDeadline(time.Now().Add(20 * time.Second))
			resp, err := http.ReadResponse(bufio.NewReader(r), nil)
			if err != nil {
				t.Fatal(err)
			}
			n, err := io.Copy(io.Discard, resp.Body)
			resp.Body.Close()
			if err != nil || n != 8<<20 {
				t.Fatalf("slow reader got %d bytes, %v", n, err)
			}
		}
	})
}

// Large bodies (a 20 MiB agent report) arrive completely on both listeners,
// and 200 concurrent clients are all answered.
func TestConnServerLargeBodyAndManyClients(t *testing.T) {
	forWorkers(t, func(t *testing.T, workers int) {
		ts := startConnTestServer(t, workers, 10*time.Second)
		body := make([]byte, 20<<20)
		rand.New(rand.NewSource(1)).Read(body)
		sum := 0
		for _, b := range body {
			sum += int(b)
		}
		want := fmt.Sprintf("%d %d", len(body), sum)
		pool, _ := modules.CertPoolFromPEMs(ts.e.c.currentCAPEMs(), time.Now())
		hc, err := modules.NewHTTPSClient("127.0.0.1", ts.srv.specs[1].ln.Addr().(*net.TCPAddr).Port, &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12}, 60*time.Second)
		if err != nil {
			t.Fatal(err)
		}
		status, got, err := hc.Do("POST", "/test/echo", body)
		if err != nil || status != http.StatusOK || string(got) != want {
			t.Fatalf("20 MiB over HTTPS: %d %q %v, want %q", status, got, err, want)
		}
		resp, err := http.Post("http://"+ts.web+"/test/echo", "application/octet-stream", bytes.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		got, _ = io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK || string(got) != want {
			t.Fatalf("20 MiB over HTTP: %d %q, want %q", resp.StatusCode, got, want)
		}

		var wg sync.WaitGroup
		var failed atomic.Int32
		var firstErr atomic.Value
		for i := 0; i < 200; i++ {
			wg.Add(1)
			go func(i int) {
				defer wg.Done()
				if i%4 == 0 {
					code, _, _, err := ts.agentDo("", "GET", "/agent/v1/ca", nil, nil)
					if err != nil || code != http.StatusOK {
						failed.Add(1)
						firstErr.CompareAndSwap(nil, fmt.Sprintf("agent %d %v", code, err))
					}
					return
				}
				c, err := net.DialTimeout("tcp", ts.web, 5*time.Second)
				if err != nil {
					failed.Add(1)
					firstErr.CompareAndSwap(nil, err.Error())
					return
				}
				defer c.Close()
				_, _ = io.WriteString(c, healthRequest)
				if line := statusLine(c, 20*time.Second); !strings.Contains(line, " 200 ") {
					failed.Add(1)
					firstErr.CompareAndSwap(nil, line)
				}
			}(i)
		}
		wg.Wait()
		if n := failed.Load(); n > 0 {
			t.Fatalf("%d of 200 concurrent clients failed, first: %v", n, firstErr.Load())
		}
	})
}

// Random bytes on both listeners cause no panic and no stall; bytes after a
// complete request are ignored; a body over the limit gets the limit's
// error without being read; a chunked body gets 411; a malformed head 400.
func TestConnServerOddInput(t *testing.T) {
	forWorkers(t, func(t *testing.T, workers int) {
		ts := startConnTestServer(t, workers, 2*time.Second)
		rnd := rand.New(rand.NewSource(7))
		var conns []net.Conn
		for i := 0; i < 20; i++ {
			for _, addr := range []string{ts.web, ts.agnt} {
				c := dial(t, addr, "")
				junk := make([]byte, 1+rnd.Intn(4096))
				rnd.Read(junk)
				if addr == ts.agnt && junk[0] == 0x16 {
					junk[0] = 0x17 // a stalled handshake is tested elsewhere
				}
				_, _ = c.Write(junk)
				conns = append(conns, c)
			}
		}
		ts.checkFast(t, 100*time.Millisecond, "after random bytes")
		for _, c := range conns {
			_ = c.SetReadDeadline(time.Now().Add(5 * time.Second))
			if _, err := io.Copy(io.Discard, c); isTimeout(err) {
				t.Fatalf("a connection with random bytes was not closed")
			}
		}

		line, _ := ts.webRaw(t, healthRequest+"GARBAGE AFTER THE REQUEST\r\n\r\nmore")
		if !strings.Contains(line, " 200 ") {
			t.Fatalf("request followed by other bytes: %q", line)
		}
		big := fmt.Sprintf("POST /api/v1/auth/login HTTP/1.1\r\nHost: x\r\nX-Mimamori-Request: 1\r\nContent-Type: application/json\r\nContent-Length: %d\r\n\r\n{", modules.MaxRequestBody+1)
		c := dial(t, ts.web, "")
		_, _ = io.WriteString(c, big)
		_ = c.SetReadDeadline(time.Now().Add(5 * time.Second))
		resp, err := http.ReadResponse(bufio.NewReader(c), nil)
		if err != nil {
			t.Fatal(err)
		}
		msg, _ := io.ReadAll(resp.Body)
		if resp.StatusCode != http.StatusBadRequest || !strings.Contains(string(msg), "too large") {
			t.Fatalf("body over the limit: %d %s", resp.StatusCode, msg)
		}
		line, _ = ts.webRaw(t, "POST /test/echo HTTP/1.1\r\nHost: x\r\nX-Mimamori-Request: 1\r\nTransfer-Encoding: chunked\r\n\r\n3\r\nabc\r\n0\r\n\r\n")
		if !strings.Contains(line, " 411 ") {
			t.Fatalf("chunked body: %q, want 411", line)
		}
		line, _ = ts.webRaw(t, "NOT A REQUEST\r\n\r\n")
		if !strings.Contains(line, " 400 ") {
			t.Fatalf("malformed head: %q, want 400", line)
		}
		line, _ = ts.webRaw(t, "GET / HTTP/1.1\r\nX: "+strings.Repeat("a", modules.MaxHeadBytes)+"\r\n\r\n")
		if !strings.Contains(line, " 431 ") {
			t.Fatalf("head over the limit: %q, want 431", line)
		}
	})
}

// A complete request that waits longer than the queue timeout for a busy
// worker is rejected (503 on plain HTTP, closed on TLS).
func TestConnServerQueueTimeoutBusyWorker(t *testing.T) {
	forWorkers(t, func(t *testing.T, workers int) {
		ts := startConnTestServer(t, workers, time.Second)
		var wg sync.WaitGroup
		for i := 0; i < workers; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				if line, _ := ts.webRaw(t, "GET /test/slow?ms=1800 HTTP/1.1\r\nHost: x\r\n\r\n"); !strings.Contains(line, " 200 ") {
					t.Errorf("slow request: %q", line)
				}
			}()
		}
		time.Sleep(200 * time.Millisecond)
		waiting := dial(t, ts.web, "")
		_, _ = io.WriteString(waiting, healthRequest)
		tlsErr := make(chan error, 1)
		go func() {
			_, _, _, err := ts.agentDo("", "GET", "/agent/v1/ca", nil, nil)
			tlsErr <- err
		}()
		if line := statusLine(waiting, 5*time.Second); !strings.Contains(line, " 503 ") {
			t.Fatalf("request behind a busy worker: %q, want 503", line)
		}
		if err := <-tlsErr; err == nil {
			t.Fatal("TLS request behind a busy worker was answered, want closed")
		}
		wg.Wait()
		ts.checkFast(t, 100*time.Millisecond, "after the slow requests")
	})
}

// On stop, the request in work is finished and its response written, and
// waiting connections are rejected.
func TestConnServerStop(t *testing.T) {
	forWorkers(t, func(t *testing.T, workers int) {
		ts := startConnTestServer(t, workers, 10*time.Second)
		silent := dial(t, ts.web, "")
		time.Sleep(100 * time.Millisecond)
		inWork := dial(t, ts.web, "")
		_, _ = io.WriteString(inWork, "GET /test/slow?ms=600 HTTP/1.1\r\nHost: x\r\n\r\n")
		time.Sleep(200 * time.Millisecond)
		ts.stop.Store(true)
		if line := statusLine(inWork, 5*time.Second); !strings.Contains(line, " 200 ") {
			t.Fatalf("request in work at stop: %q, want 200", line)
		}
		if line := statusLine(silent, 5*time.Second); !strings.Contains(line, " 503 ") {
			t.Fatalf("waiting connection at stop: %q, want 503", line)
		}
		select {
		case <-ts.done:
		case <-time.After(5 * time.Second):
			t.Fatal("run did not return")
		}
	})
}

// After the handshake crypto/tls keeps its state over the temporary error
// of tlsBuffer (a record that arrives in parts is decrypted once complete),
// and its writes go to the outbound buffer only.
func TestTLSBufferWouldBlock(t *testing.T) {
	e := newTestEnv(t)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	pool, _ := modules.CertPoolFromPEMs(e.c.currentCAPEMs(), time.Now())
	clientCh := make(chan *tls.Conn, 1)
	go func() {
		raw, err := net.Dial("tcp", ln.Addr().String())
		if err != nil {
			clientCh <- nil
			return
		}
		c := tls.Client(raw, &tls.Config{RootCAs: pool, ServerName: "127.0.0.1", MinVersion: tls.VersionTLS12})
		_ = c.SetDeadline(time.Now().Add(10 * time.Second))
		if c.Handshake() != nil {
			clientCh <- nil
			return
		}
		clientCh <- c
	}()
	sc, err := ln.Accept()
	if err != nil {
		t.Fatal(err)
	}
	defer sc.Close()
	_ = sc.SetDeadline(time.Now().Add(10 * time.Second))
	// During the handshake the buffer reads and writes the socket without
	// waiting and calls await when it would have to wait; here await simply
	// waits for the socket.
	rc, err := sc.(syscall.Conn).SyscallConn()
	if err != nil {
		t.Fatal(err)
	}
	fd := -1
	_ = rc.Control(func(f uintptr) { fd = int(f) })
	awaits := 0
	buf := &tlsBuffer{Conn: sc, raw: rc, await: func(events int16) error {
		awaits++
		fds := []pollFd{{fd: int32(fd), events: events}}
		if err := ppoll(fds, 10*time.Second); err != nil || fds[0].revents == 0 {
			return errHandshakeLimit
		}
		return nil
	}}
	server := tls.Server(buf, e.c.tlsConfig())
	if err := server.Handshake(); err != nil {
		t.Fatal(err)
	}
	client := <-clientCh
	if client == nil {
		t.Fatal("client handshake failed")
	}
	defer client.Close()
	if awaits == 0 {
		t.Fatal("the handshake never waited for its peer through await")
	}
	buf.await = nil

	p := make([]byte, 64)
	if n, err := server.Read(p); n != 0 || err != errWouldBlock {
		t.Fatalf("read with nothing buffered: %d %v", n, err)
	}
	go func() { _, _ = client.Write([]byte("hello over tls")) }()
	// Read the raw record from the socket and hand it over in two parts.
	head := make([]byte, 5)
	if _, err := io.ReadFull(sc, head); err != nil {
		t.Fatal(err)
	}
	rest := make([]byte, int(head[3])<<8|int(head[4]))
	if _, err := io.ReadFull(sc, rest); err != nil {
		t.Fatal(err)
	}
	record := append(head, rest...)
	buf.in = append(buf.in, record[:3]...)
	if n, err := server.Read(p); n != 0 || err != errWouldBlock {
		t.Fatalf("read with a partial header: %d %v", n, err)
	}
	buf.in = append(buf.in, record[3:10]...)
	if n, err := server.Read(p); n != 0 || err != errWouldBlock {
		t.Fatalf("read with a partial record: %d %v", n, err)
	}
	buf.in = append(buf.in, record[10:]...)
	n, err := server.Read(p)
	if err != nil || string(p[:n]) != "hello over tls" {
		t.Fatalf("read of the completed record: %q %v", p[:n], err)
	}

	if _, err := server.Write([]byte("pong")); err != nil {
		t.Fatal(err)
	}
	if len(buf.out) == 0 {
		t.Fatal("write did not go to the outbound buffer")
	}
	if _, err := sc.Write(buf.out); err != nil {
		t.Fatal(err)
	}
	got := make([]byte, 4)
	if _, err := io.ReadFull(client, got); err != nil || string(got) != "pong" {
		t.Fatalf("client read %q %v", got, err)
	}
}

// clientHello returns the first TLS record a client sends.
func clientHello(t *testing.T) []byte {
	t.Helper()
	a, b := net.Pipe()
	go func() {
		_ = tls.Client(a, &tls.Config{ServerName: "127.0.0.1", MinVersion: tls.VersionTLS12}).Handshake()
	}()
	defer a.Close()
	defer b.Close()
	head := make([]byte, 5)
	if _, err := io.ReadFull(b, head); err != nil {
		t.Fatal(err)
	}
	rest := make([]byte, int(head[3])<<8|int(head[4]))
	if _, err := io.ReadFull(b, rest); err != nil {
		t.Fatal(err)
	}
	return append(head, rest...)
}

func TestHelloLength(t *testing.T) {
	for _, tc := range []struct {
		in   []byte
		want int
	}{
		{nil, 5},
		{[]byte{0x16}, 5},
		{[]byte{0x16, 3, 1, 0}, 5},
		{[]byte{0x16, 3, 1, 0, 3}, 8},
		{[]byte{0x16, 3, 1, 1, 0, 1}, 261},
		{[]byte("GET / HTTP/1.1"), 14},
	} {
		if got := helloLength(tc.in); got != tc.want {
			t.Errorf("%v: %d, want %d", tc.in, got, tc.want)
		}
	}
}

func TestHeadEnd(t *testing.T) {
	for _, tc := range []struct {
		in   string
		from int
		want int
	}{
		{"GET / HTTP/1.1\r\nHost: x\r\n\r\nbody", 0, 27},
		{"GET / HTTP/1.1\nHost: x\n\nbody", 0, 24},
		{"GET / HTTP/1.1\r\nHost: x\r\n", 0, -1},
		{"GET / HTTP/1.1\r\nHost: x\r\n\r", 0, -1},
		{"GET / HTTP/1.1\r\nHost: x\r\n\r\n", 24, 27},
	} {
		if got := headEnd([]byte(tc.in), tc.from); got != tc.want {
			t.Errorf("%q from %d: %d, want %d", tc.in, tc.from, got, tc.want)
		}
	}
}

// A source is exempt from the handshake penalty when it is loopback or one
// of this host's addresses, whatever address it connected to; any other
// source is not.
func TestExemptSource(t *testing.T) {
	own := []net.IP{net.ParseIP("192.0.2.7"), net.ParseIP("2001:db8::7"), net.ParseIP("fe80::7")}
	for _, c := range []struct {
		host string
		want bool
	}{
		{"127.0.0.1", true},        // loopback (to loopback or to an own address)
		{"127.0.0.9", true},        // loopback
		{"::1", true},              // loopback
		{"::ffff:127.0.0.1", true}, // loopback, IPv4-mapped
		{"192.0.2.7", true},        // own address (to loopback or to itself)
		{"::ffff:192.0.2.7", true}, // own address, IPv4-mapped
		{"2001:db8::7", true},      // own IPv6 address
		{"fe80::7%eth0", true},     // own link-local address with its zone
		{"192.0.2.8", false},       // foreign (to anything)
		{"2001:db8::8", false},     // foreign
		{"0.0.0.0", false},
		{"garbage", false},
	} {
		if got := exemptSource(c.host, own); got != c.want {
			t.Errorf("exemptSource(%q) = %v, want %v", c.host, got, c.want)
		}
	}
	if exemptSource("192.0.2.7", nil) {
		t.Error("an address is exempt without being one of the host's")
	}
}

// ownIPv4 returns a non-loopback IPv4 address of this host, or skips.
func ownIPv4(t *testing.T) string {
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
	t.Skip("this host has no non-loopback IPv4 address")
	return ""
}

// A stalled handshake from one of this host's own addresses to loopback
// does not get that address refused: its connections to loopback and to
// itself are served. The same holds for loopback to loopback.
func TestConnServerOwnAddressNotRefused(t *testing.T) {
	own := ownIPv4(t)
	if probe := newConnServer(nil, 1, time.Second, nil); !probe.isOwnSource(own) || !probe.isOwnSource("127.0.0.1") || probe.isOwnSource("198.51.100.1") {
		t.Fatalf("isOwnSource: own %s %v, loopback %v, foreign %v", own, probe.isOwnSource(own), probe.isOwnSource("127.0.0.1"), probe.isOwnSource("198.51.100.1"))
	}
	ts := startConnTestServerOn(t, 1, 10*time.Second, "0.0.0.0:0")
	_, port, _ := net.SplitHostPort(ts.agnt)
	toLoopback, toOwn := net.JoinHostPort("127.0.0.1", port), net.JoinHostPort(own, port)
	get := func(src, dst string) error {
		code, _, _, err := ts.agentDoTo(dst, src, "GET", "/agent/v1/ca", nil, nil)
		if err == nil && code != http.StatusOK {
			err = fmt.Errorf("status %d", code)
		}
		return err
	}
	hello := clientHello(t)
	for _, pair := range [][2]string{{own, toLoopback}, {own, toOwn}, {"127.0.0.1", toLoopback}} {
		staller := dial(t, pair[1], pair[0])
		start := time.Now()
		if _, err := staller.Write(hello); err != nil {
			t.Fatal(err)
		}
		expectClosed(t, staller, 5*time.Second, "stalled handshake")
		if d := time.Since(start); d < modules.TLSHandshakeTimeout-100*time.Millisecond {
			t.Fatalf("%s -> %s: closed after %v, before the handshake limit", pair[0], pair[1], d)
		}
		for _, dst := range []string{toLoopback, toOwn} {
			if err := get(pair[0], dst); err != nil {
				t.Fatalf("after a stall %s -> %s: request %s -> %s: %v", pair[0], pair[1], pair[0], dst, err)
			}
		}
	}
}

// agentDoTo is agentDo to another address of the agent listener.
func (ts *connTestServer) agentDoTo(addr, src, method, path string, body []byte, hdr map[string]string) (int, []byte, time.Duration, error) {
	saved := ts.agnt
	defer func() { ts.agnt = saved }()
	ts.agnt = addr
	return ts.agentDo(src, method, path, body, hdr)
}

// Twenty stalled ClientHellos from one (exempt) source delay a TLS request
// of another source by HandshakeGrace at most (the first staller's grace);
// a request of the same source is answered or rejected at its queue
// timeout, never later.
func TestConnServerStalledHellosOfOneSource(t *testing.T) {
	forWorkers(t, func(t *testing.T, workers int) {
		const queueTimeout = 4 * time.Second
		const margin = 700 * time.Millisecond
		ts := startConnTestServer(t, workers, queueTimeout)
		hello := clientHello(t)
		for i := 0; i < 20; i++ {
			c := dial(t, ts.agnt, "127.0.0.5")
			if _, err := c.Write(hello); err != nil {
				t.Fatal(err)
			}
		}
		type outcome struct {
			what string
			d    time.Duration
			err  error
		}
		results := make(chan outcome, 4)
		request := func(what, src string, after time.Duration) {
			time.Sleep(after)
			code, _, d, err := ts.agentDo(src, "GET", "/agent/v1/ca", nil, nil)
			if err == nil && code != http.StatusOK {
				err = fmt.Errorf("status %d", code)
			}
			results <- outcome{what, d, err}
		}
		go request("other source", "127.0.0.1", 300*time.Millisecond)
		go request("other source later", "127.0.0.1", 2500*time.Millisecond)
		go request("other source last", "127.0.0.6", 5*time.Second)
		start := time.Now()
		go request("same source", "127.0.0.5", 300*time.Millisecond)
		for i := 0; i < 4; i++ {
			r := <-results
			if r.what == "same source" {
				if d := time.Since(start) - 300*time.Millisecond; d > queueTimeout+margin {
					t.Fatalf("request of the stalling source: %v after %v, want at most the queue timeout", r.err, d)
				}
				continue
			}
			if r.err != nil || r.d > modules.HandshakeGrace+margin {
				t.Fatalf("%s: %v after %v, want an answer within the grace", r.what, r.err, r.d)
			}
		}
		ts.checkFast(t, 100*time.Millisecond, "after the stalled ClientHellos")
	})
}

// stallHellos opens n connections to the agent listener that send a
// complete ClientHello and then nothing; src(i) is the source address of
// the i-th.
func (ts *connTestServer) stallHellos(t *testing.T, n int, src func(i int) string) {
	t.Helper()
	hello := clientHello(t)
	for i := 0; i < n; i++ {
		c := dial(t, ts.agnt, src(i))
		if _, err := c.Write(hello); err != nil {
			t.Fatal(err)
		}
	}
}

// Stalled ClientHellos of five and of twenty different sources, each
// stalling for the first time: a plain request is answered at once, and a
// TLS request of another source waits at most HandshakeGrace per staller
// ahead of it and is never rejected.
func TestConnServerFirstTimeStallers(t *testing.T) {
	forWorkers(t, func(t *testing.T, workers int) {
		for _, n := range []int{5, 20} {
			ts := startConnTestServer(t, workers, 10*time.Second)
			ts.stallHellos(t, n, func(i int) string { return fmt.Sprintf("127.0.%d.%d", 1+workers, i+2) })
			time.Sleep(50 * time.Millisecond)
			type result struct {
				code int
				d    time.Duration
				err  error
			}
			tlsDone := make(chan result, 1)
			go func() {
				code, _, d, err := ts.agentDo("127.0.0.1", "GET", "/agent/v1/ca", nil, nil)
				tlsDone <- result{code, d, err}
			}()
			line, d := ts.webRaw(t, healthRequest)
			if !strings.Contains(line, " 200 ") || d > 100*time.Millisecond {
				t.Fatalf("%d stallers: plain request %q after %v, want 200 within 100 ms", n, line, d)
			}
			r := <-tlsDone
			t.Logf("%d stallers: plain request after %v, TLS request of another source after %v", n, d, r.d)
			limit := time.Duration(n)*modules.HandshakeGrace + 700*time.Millisecond
			if r.err != nil || r.code != http.StatusOK || r.d > limit {
				t.Fatalf("%d stallers: TLS request of another source: %d %v after %v, want 200 within %v", n, r.code, r.err, r.d, limit)
			}
			ts.shutdown()
		}
	})
}

// A source that keeps twenty stalled ClientHellos open delays no request
// of another source: once its first handshake gave way, a handshake of it
// is taken only when nobody else waits and gives way at once.
func TestConnServerStallingSourceKeptOpen(t *testing.T) {
	forWorkers(t, func(t *testing.T, workers int) {
		ts := startConnTestServer(t, workers, 10*time.Second)
		ts.stallHellos(t, 20, func(int) string { return "127.0.0.5" })
		// The first of them is a first-time staller: it keeps its grace.
		time.Sleep(modules.HandshakeGrace + 100*time.Millisecond)
		const limit = 200 * time.Millisecond
		for end := time.Now().Add(4 * time.Second); time.Now().Before(end); {
			line, d := ts.webRaw(t, healthRequest)
			if !strings.Contains(line, " 200 ") || d > limit {
				t.Fatalf("plain request: %q after %v, want 200 within %v", line, d, limit)
			}
			for _, src := range []string{"127.0.0.1", "127.0.0.6"} {
				code, _, d, err := ts.agentDo(src, "GET", "/agent/v1/ca", nil, nil)
				if err != nil || code != http.StatusOK || d > limit {
					t.Fatalf("TLS request from %s: %d %v after %v, want 200 within %v", src, code, err, d, limit)
				}
			}
			time.Sleep(100 * time.Millisecond)
		}
	})
}

// slowFlight is a client connection whose second write (the client's
// second handshake flight) is sent only after delay.
type slowFlight struct {
	net.Conn
	writes int
	delay  time.Duration
}

func (c *slowFlight) Write(p []byte) (int, error) {
	c.writes++
	if c.writes == 2 {
		time.Sleep(c.delay)
	}
	return c.Conn.Write(p)
}

// slowTLSGet sends GET /agent/v1/ca over TLS from src with the client's
// second flight delayed by flight and the request sent pause after the
// handshake, and returns the status.
func (ts *connTestServer) slowTLSGet(t *testing.T, src string, flight, pause time.Duration) (int, error) {
	t.Helper()
	raw := dial(t, ts.agnt, src)
	_ = raw.SetDeadline(time.Now().Add(30 * time.Second))
	pool, _ := modules.CertPoolFromPEMs(ts.e.c.currentCAPEMs(), time.Now())
	c := tls.Client(&slowFlight{Conn: raw, delay: flight}, &tls.Config{RootCAs: pool, ServerName: "127.0.0.1", MinVersion: tls.VersionTLS12})
	if err := c.Handshake(); err != nil {
		return 0, err
	}
	time.Sleep(pause)
	if _, err := io.WriteString(c, "GET /agent/v1/ca HTTP/1.1\r\nHost: x\r\nConnection: close\r\n\r\n"); err != nil {
		return 0, err
	}
	resp, err := http.ReadResponse(bufio.NewReader(c), nil)
	if err != nil {
		return 0, err
	}
	resp.Body.Close()
	return resp.StatusCode, nil
}

// A slow but honest handshake (the client's second flight after 1 s)
// succeeds when nobody else waits, and the time a connection waited for its
// handshake to begin never cuts it: its handshake limit counts from the
// start of the handshake, and its queue timeout is moved by that wait.
func TestConnServerSlowHonestHandshake(t *testing.T) {
	forWorkers(t, func(t *testing.T, workers int) {
		const queueTimeout = 2 * time.Second
		ts := startConnTestServer(t, workers, queueTimeout)
		if code, err := ts.slowTLSGet(t, "127.0.0.1", time.Second, 0); err != nil || code != http.StatusOK {
			t.Fatalf("slow handshake, nobody waiting: %d %v", code, err)
		}
		// Three stallers of the same source hold the turn for their grace
		// each (1.2 s); the honest connection behind them then needs 1 s for
		// its handshake (nobody waits behind it) and sends its request
		// 500 ms later: 2.7 s after it arrived, past its queue timeout of
		// 2 s counted from arrival.
		ts.stallHellos(t, 3, func(int) string { return "127.0.0.9" })
		time.Sleep(100 * time.Millisecond)
		start := time.Now()
		code, err := ts.slowTLSGet(t, "127.0.0.9", time.Second, 500*time.Millisecond)
		if err != nil || code != http.StatusOK {
			t.Fatalf("honest connection behind stallers of its source: %d %v after %v", code, err, time.Since(start))
		}
		if d := time.Since(start); d < queueTimeout {
			t.Fatalf("honest connection behind stallers of its source answered after %v; it should have waited for them", d)
		}
	})
}

// A handler that runs inside a nested pass (with one worker, in the
// accepting flow itself) while a handshake waits ends cleanly, the
// handshake still ends at its limit, and a stop during a waiting handshake
// ends the handshake and the central at once.
func TestConnServerNestedPass(t *testing.T) {
	forWorkers(t, func(t *testing.T, workers int) {
		ts := startConnTestServer(t, workers, 10*time.Second)
		hello := clientHello(t)
		staller := dial(t, ts.agnt, "127.0.0.7")
		start := time.Now()
		if _, err := staller.Write(hello); err != nil {
			t.Fatal(err)
		}
		time.Sleep(100 * time.Millisecond)
		line, d := ts.webRaw(t, "GET /test/slow?ms=300 HTTP/1.1\r\nHost: x\r\n\r\n")
		if !strings.Contains(line, " 200 ") || d > 500*time.Millisecond {
			t.Fatalf("handler during a handshake: %q after %v", line, d)
		}
		// (Plain requests only: a TLS request of another source would make
		// the staller give way.)
		if line, d := ts.webRaw(t, healthRequest); !strings.Contains(line, " 200 ") || d > 100*time.Millisecond {
			t.Fatalf("plain request after the handler: %q after %v", line, d)
		}
		expectClosed(t, staller, 5*time.Second, "stalled handshake")
		if d := time.Since(start); d < modules.TLSHandshakeTimeout-100*time.Millisecond || d > modules.TLSHandshakeTimeout+500*time.Millisecond {
			t.Fatalf("stalled handshake closed after %v, want at its limit", d)
		}
		ts.checkFast(t, 100*time.Millisecond, "after the stalled handshake")

		staller = dial(t, ts.agnt, "127.0.0.8")
		if _, err := staller.Write(hello); err != nil {
			t.Fatal(err)
		}
		time.Sleep(200 * time.Millisecond)
		stopAt := time.Now()
		ts.stop.Store(true)
		select {
		case <-ts.done:
		case <-time.After(2 * time.Second):
			t.Fatal("run did not return after a stop during a handshake")
		}
		if d := time.Since(stopAt); d > modules.LoopWakeInterval+200*time.Millisecond {
			t.Fatalf("stop during a handshake took %v", d)
		}
		expectClosed(t, staller, time.Second, "handshake at stop")
	})
}

// The time the accepting flow spends on other work inside a nested pass (a
// handler that runs in it with one worker) does not count against the
// handshake: an honest client of a foreign source whose second flight
// arrived while a 2.5 s handler ran completes its handshake and is not
// refused afterwards.
func TestConnServerBusyFlowKeepsHandshake(t *testing.T) {
	forWorkers(t, func(t *testing.T, workers int) {
		ts := startConnTestServer(t, workers, 10*time.Second)
		type result struct {
			code int
			err  error
		}
		done := make(chan result, 1)
		go func() {
			code, err := ts.slowTLSGet(t, "127.0.0.2", 200*time.Millisecond, 0)
			done <- result{code, err}
		}()
		time.Sleep(50 * time.Millisecond)
		if line, _ := ts.webRaw(t, "GET /test/slow?ms=2500 HTTP/1.1\r\nHost: x\r\n\r\n"); !strings.Contains(line, " 200 ") {
			t.Fatalf("slow handler: %q", line)
		}
		if r := <-done; r.err != nil || r.code != http.StatusOK {
			t.Fatalf("honest handshake while the flow ran a handler: %d %v", r.code, r.err)
		}
		if code, _, _, err := ts.agentDo("127.0.0.2", "GET", "/agent/v1/ca", nil, nil); err != nil || code != http.StatusOK {
			t.Fatalf("the honest source afterwards: %d %v", code, err)
		}
	})
}

// recordingConn keeps the raw bytes a client reads.
type recordingConn struct {
	net.Conn
	got []byte
}

func (c *recordingConn) Read(p []byte) (int, error) {
	n, err := c.Conn.Read(p)
	c.got = append(c.got, p[:n]...)
	return n, err
}

// A TLS response ends with the close_notify alert before the connection is
// closed (TLS 1.2, where the record type of an alert is visible), and the
// client sees a clean end.
func TestConnServerTLSCloseNotify(t *testing.T) {
	ts := startConnTestServer(t, 1, 10*time.Second)
	raw := &recordingConn{Conn: dial(t, ts.agnt, "")}
	_ = raw.SetDeadline(time.Now().Add(10 * time.Second))
	pool, _ := modules.CertPoolFromPEMs(ts.e.c.currentCAPEMs(), time.Now())
	c := tls.Client(raw, &tls.Config{RootCAs: pool, ServerName: "127.0.0.1", MinVersion: tls.VersionTLS12, MaxVersion: tls.VersionTLS12})
	if _, err := io.WriteString(c, "GET /agent/v1/ca HTTP/1.1\r\nHost: x\r\n\r\n"); err != nil {
		t.Fatal(err)
	}
	data, err := io.ReadAll(c)
	if err != nil || !bytes.HasPrefix(data, []byte("HTTP/1.1 200 ")) {
		t.Fatalf("response %q, end %v", data[:min(len(data), 40)], err)
	}
	// Walk the records the server sent: the last one is an alert.
	last := byte(0)
	for b := raw.got; len(b) >= 5; {
		n := 5 + (int(b[3])<<8 | int(b[4]))
		if n > len(b) {
			break
		}
		last, b = b[0], b[n:]
	}
	if last != 0x15 {
		t.Fatalf("last record type %#x, want an alert (close_notify)", last)
	}
}

// All loopback addresses are one source for the handshake order and the
// stall table: after a staller of 127.0.1.2 used its full limit, a staller
// of 127.0.1.3 counts as stalled, so it gives way at once to a TLS request
// of another source (a separate fresh source would keep its grace), and
// the stall is remembered once, for loopback.
func TestConnServerLoopbackIsOneSource(t *testing.T) {
	for _, tc := range []struct{ host, want string }{
		{"127.0.0.1", loopbackSource}, {"127.1.2.3", loopbackSource}, {"::1", loopbackSource},
		{"::ffff:127.0.0.9", loopbackSource}, {"192.0.2.7", "192.0.2.7"}, {"fe80::7%eth0", "fe80::7%eth0"},
	} {
		if got := sourceOf(tc.host); got != tc.want {
			t.Errorf("sourceOf(%q) = %q, want %q", tc.host, got, tc.want)
		}
	}
	own := ownIPv4(t)
	ts := startConnTestServerOn(t, 1, 10*time.Second, "0.0.0.0:0")
	_, port, _ := net.SplitHostPort(ts.agnt)
	ts.agnt = net.JoinHostPort("127.0.0.1", port)
	hello := clientHello(t)
	first := dial(t, ts.agnt, "127.0.1.2")
	start := time.Now()
	if _, err := first.Write(hello); err != nil {
		t.Fatal(err)
	}
	expectClosed(t, first, 5*time.Second, "first loopback staller")
	if d := time.Since(start); d < modules.TLSHandshakeTimeout-100*time.Millisecond {
		t.Fatalf("first loopback staller closed after %v, want at its full limit", d)
	}

	second := dial(t, ts.agnt, "127.0.1.3")
	if _, err := second.Write(hello); err != nil {
		t.Fatal(err)
	}
	time.Sleep(50 * time.Millisecond)
	code, _, d, err := ts.agentDoTo(net.JoinHostPort(own, port), own, "GET", "/agent/v1/ca", nil, nil)
	if err != nil || code != http.StatusOK || d > 250*time.Millisecond {
		t.Fatalf("request of %s behind a staller of the stalled loopback source: %d %v after %v, want 200 at once", own, code, err, d)
	}
	expectClosed(t, second, time.Second, "second loopback staller")
	if code, _ := ts.agentGet(t, "/agent/v1/ca"); code != http.StatusOK {
		t.Fatalf("loopback request after the stalls: %d", code)
	}
	ts.shutdown()
	if st, ok := ts.srv.stalled[loopbackSource]; len(ts.srv.stalled) != 1 || !ok || !st.limit {
		t.Fatalf("stall table %v, want one entry for loopback after a full limit", ts.srv.stalled)
	}
}

// A burst of 500 simultaneous requests is answered completely: when the
// table is full of requests that wait for a worker, new connections wait
// in the kernel's backlog instead of being refused.
func TestConnServerBurst(t *testing.T) {
	forWorkers(t, func(t *testing.T, workers int) {
		ts := startConnTestServer(t, workers, 10*time.Second)
		var wg sync.WaitGroup
		var failed atomic.Int32
		var firstErr atomic.Value
		for i := 0; i < 500; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				c, err := net.DialTimeout("tcp", ts.web, 10*time.Second)
				if err != nil {
					failed.Add(1)
					firstErr.CompareAndSwap(nil, err.Error())
					return
				}
				defer c.Close()
				_, _ = io.WriteString(c, "GET /test/slow?ms=2 HTTP/1.1\r\nHost: x\r\n\r\n")
				if line := statusLine(c, 20*time.Second); !strings.Contains(line, " 200 ") {
					failed.Add(1)
					firstErr.CompareAndSwap(nil, line)
				}
			}()
		}
		wg.Wait()
		if n := failed.Load(); n > 0 {
			t.Fatalf("%d of 500 simultaneous requests failed, first: %v", n, firstErr.Load())
		}
	})
}

// sendAllThenRead writes raw (a head and its whole body) completely before
// it reads the answer, like a client that does not look at the answer
// while it sends, and returns the answer's status line and body.
func sendAllThenRead(t *testing.T, c net.Conn, raw []byte) (int, string) {
	t.Helper()
	_ = c.SetDeadline(time.Now().Add(30 * time.Second))
	if _, err := c.Write(raw); err != nil {
		t.Fatalf("sending the whole request: %v", err)
	}
	resp, err := http.ReadResponse(bufio.NewReader(c), nil)
	if err != nil {
		t.Fatalf("reading the answer: %v", err)
	}
	defer resp.Body.Close()
	msg, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(msg)
}

// tlsDial connects to the agent listener with TLS.
func (ts *connTestServer) tlsDial(t *testing.T) net.Conn {
	t.Helper()
	raw := dial(t, ts.agnt, "")
	pool, _ := modules.CertPoolFromPEMs(ts.e.c.currentCAPEMs(), time.Now())
	c := tls.Client(raw, &tls.Config{RootCAs: pool, ServerName: "127.0.0.1", MinVersion: tls.VersionTLS12})
	_ = c.SetDeadline(time.Now().Add(10 * time.Second))
	if err := c.Handshake(); err != nil {
		t.Fatal(err)
	}
	return c
}

// A client that sends a whole body the central answers without reading it
// (over the size limit, or not authenticated) and only then reads gets the
// answer, not a reset.
func TestConnServerEarlyAnswerAfterWholeBody(t *testing.T) {
	forWorkers(t, func(t *testing.T, workers int) {
		ts := startConnTestServer(t, workers, 10*time.Second)
		withBody := func(head string, n int) []byte {
			return append([]byte(fmt.Sprintf(head, n)), bytes.Repeat([]byte("x"), n)...)
		}
		over := modules.MaxRequestBody + 1
		code, msg := sendAllThenRead(t, dial(t, ts.web, ""),
			withBody("POST /test/echo HTTP/1.1\r\nHost: x\r\nContent-Length: %d\r\n\r\n", over))
		if code != http.StatusBadRequest || !strings.Contains(msg, "too large") {
			t.Fatalf("web, body over the limit: %d %s", code, msg)
		}
		code, msg = sendAllThenRead(t, dial(t, ts.web, ""),
			withBody("POST /api/v1/rules HTTP/1.1\r\nHost: x\r\nX-Mimamori-Request: 1\r\nContent-Length: %d\r\n\r\n", 20<<20))
		if code != http.StatusUnauthorized {
			t.Fatalf("web, unauthenticated large body: %d %s", code, msg)
		}
		code, msg = sendAllThenRead(t, ts.tlsDial(t),
			withBody("POST /agent/v1/report HTTP/1.1\r\nHost: x\r\nAuthorization: Bearer mmt_wrong\r\nContent-Length: %d\r\n\r\n", over))
		if code != http.StatusUnauthorized {
			t.Fatalf("agent, body over the limit without a token: %d %s", code, msg)
		}
		code, msg = sendAllThenRead(t, ts.tlsDial(t),
			withBody("POST /agent/v1/report HTTP/1.1\r\nHost: x\r\nAuthorization: Bearer mmt_wrong\r\nContent-Length: %d\r\n\r\n", 20<<20))
		if code != http.StatusUnauthorized {
			t.Fatalf("agent, unauthenticated large body: %d %s", code, msg)
		}
		ts.checkFast(t, 100*time.Millisecond, "after the early answers")
	})
}

// paddedReport is an agent report of about n bytes (the agent API ignores
// unknown fields).
func paddedReport(n int) []byte {
	return []byte(`{"created_at":1,"detections":[],"dropped":0,"padding":"` + strings.Repeat("p", n) + `"}`)
}

// A large body is read only from a peer authenticated for its route: an
// unauthenticated one is answered at once without its body being sent, on
// every listener; an authenticated one works on every route that takes one.
func TestConnServerLargeBodyNeedsAuthentication(t *testing.T) {
	forWorkers(t, func(t *testing.T, workers int) {
		ts := startConnTestServer(t, workers, 10*time.Second)
		e := ts.e
		tok, a, _ := commandAgent(t, e, "big-host")
		instance := "m-big"
		adminCk := login(t, e, testAdminEmail, "Admin-pass-123")
		const n = 20 << 20

		// Unauthenticated: only the head is sent; the answer comes at once.
		headOnly := func(c net.Conn, head string) (string, time.Duration) {
			start := time.Now()
			if _, err := io.WriteString(c, fmt.Sprintf(head, n)); err != nil {
				t.Fatal(err)
			}
			line := statusLine(c, 5*time.Second)
			return line, time.Since(start)
		}
		for _, tc := range []struct{ what, head, want string }{
			{"no session", "POST /api/v1/rules HTTP/1.1\r\nHost: x\r\nX-Mimamori-Request: 1\r\nContent-Length: %d\r\n\r\n", " 401 "},
			{"unknown session", "POST /api/v1/rules HTTP/1.1\r\nHost: x\r\nX-Mimamori-Request: 1\r\nCookie: " + sessionCookie + "=" + modules.SessionPrefix + "nothing\r\nContent-Length: %d\r\n\r\n", " 401 "},
			{"no CSRF header", "POST /api/v1/rules HTTP/1.1\r\nHost: x\r\nCookie: " + sessionCookie + "=" + adminCk + "\r\nContent-Length: %d\r\n\r\n", " 403 "},
			{"login", "POST /api/v1/auth/login HTTP/1.1\r\nHost: x\r\nX-Mimamori-Request: 1\r\nContent-Length: %d\r\n\r\n", " 400 "},
			{"no route", "POST /api/v1/nothing HTTP/1.1\r\nHost: x\r\nX-Mimamori-Request: 1\r\nCookie: " + sessionCookie + "=" + adminCk + "\r\nContent-Length: %d\r\n\r\n", " 40"},
		} {
			line, d := headOnly(dial(t, ts.web, ""), tc.head)
			if !strings.Contains(line, tc.want) || d > time.Second {
				t.Fatalf("web, %s: %q after %v, want%s at once", tc.what, line, d, tc.want)
			}
		}
		for _, tc := range []struct{ what, head string }{
			{"bad token", "POST /agent/v1/report HTTP/1.1\r\nHost: x\r\nAuthorization: Bearer mmt_wrong\r\nX-Mimamori-Instance-Id: m\r\nContent-Length: %d\r\n\r\n"},
			{"other instance", "POST /agent/v1/sync HTTP/1.1\r\nHost: x\r\nAuthorization: Bearer " + tok + "\r\nContent-Length: %d\r\n\r\n"},
			{"bad CLI secret", "POST /admin/v1/rules HTTP/1.1\r\nHost: x\r\nAuthorization: Bearer mmc_wrong\r\nContent-Length: %d\r\n\r\n"},
		} {
			line, d := headOnly(ts.tlsDial(t), tc.head)
			if !strings.Contains(line, " 401 ") || d > time.Second {
				t.Fatalf("agent listener, %s: %q after %v, want 401 at once", tc.what, line, d)
			}
		}
		// The checks changed nothing: the token is still unused and unbound.
		if tk, err := models.GetToken(e.c.db.Read(), a.TokenID.Int64); err != nil || tk.UsedAt != 0 || tk.InstanceID != "" {
			t.Fatalf("token after refused requests: %+v %v", tk, err)
		}

		// Authenticated: the body is read and the handler answers on it.
		code, _, _, err := ts.agentDo("", "POST", "/agent/v1/report", paddedReport(n),
			map[string]string{"Authorization": "Bearer " + tok, modules.HeaderInstanceID: instance})
		if err != nil || code != http.StatusOK {
			t.Fatalf("authenticated 20 MiB report: %d %v", code, err)
		}
		big := []byte(`{"name":"r","templates":[],"padding":"` + strings.Repeat("p", n) + `"}`)
		code, body, _, err := ts.agentDo("", "POST", "/admin/v1/rules", big, map[string]string{"Authorization": "Bearer " + e.cliKey})
		if err != nil || code != http.StatusBadRequest || !strings.Contains(string(body), "padding") {
			t.Fatalf("authenticated large management request: %d %s %v, want the handler's 400 on the unknown field", code, body, err)
		}
		req, _ := http.NewRequest("POST", "http://"+ts.web+"/api/v1/rules", bytes.NewReader(big))
		for k, v := range browserHeaders(adminCk) {
			req.Header.Set(k, v)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		msg, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode != http.StatusBadRequest || !strings.Contains(string(msg), "padding") {
			t.Fatalf("authenticated large browser request: %d %s, want the handler's 400 on the unknown field", resp.StatusCode, msg)
		}
	})
}

// The large-body checks change nothing: no token use, no binding, no
// session extension.
func TestAdmitBodyHasNoSideEffects(t *testing.T) {
	e := newTestEnv(t)
	tok, a, _ := commandAgent(t, e, "quiet-host")
	ck := login(t, e, testAdminEmail, "Admin-pass-123")
	sess, err := models.GetSession(e.c.db.Read(), modules.HashSecret(ck))
	if err != nil {
		t.Fatal(err)
	}
	if err := e.c.db.Write(func(tx models.Writer) error {
		return models.TouchSession(tx, sess.ID, sess.LastSeenAt-3600, sess.ExpiresAt)
	}); err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest("POST", "/agent/v1/report", nil)
	req.Header.Set("Authorization", "Bearer "+tok)
	req.Header.Set(modules.HeaderInstanceID, "m-quiet")
	if !e.c.admitAgentBody(req) {
		t.Fatal("valid token refused")
	}
	if tk, _ := models.GetToken(e.c.db.Read(), a.TokenID.Int64); tk.UsedAt != 0 || tk.InstanceID != "" || tk.LastSeenAt != 0 {
		t.Fatalf("the check recorded a token use: %+v", tk)
	}
	web := httptest.NewRequest("POST", "/api/v1/rules", nil)
	for k, v := range browserHeaders(ck) {
		web.Header.Set(k, v)
	}
	if !e.c.admitWebBody(web) {
		t.Fatal("valid session refused")
	}
	if after, _ := models.GetSession(e.c.db.Read(), sess.ID); after.LastSeenAt != sess.LastSeenAt-3600 || after.ExpiresAt != sess.ExpiresAt {
		t.Fatalf("the check extended the session: %+v", after)
	}
	cli := httptest.NewRequest("POST", "/admin/v1/rules", nil)
	cli.RemoteAddr = "192.0.2.9:4000"
	cli.Header.Set("Authorization", "Bearer "+e.cliKey)
	if e.c.admitAgentBody(cli) {
		t.Fatal("management request of another host admitted")
	}
}

// 256 unauthenticated connections that announce and send large bodies do
// not delay an authenticated 20 MiB report: their bodies are never
// buffered.
func TestConnServerUnauthenticatedBodiesDoNotDelayReport(t *testing.T) {
	forWorkers(t, func(t *testing.T, workers int) {
		ts := startConnTestServer(t, workers, 10*time.Second)
		tok, _, _ := commandAgent(t, ts.e, "busy-host")
		chunk := bytes.Repeat([]byte("x"), 1<<20)
		var wg sync.WaitGroup
		for i := 0; i < modules.MaxPendingConns; i++ {
			c := dial(t, ts.web, "")
			wg.Add(1)
			go func() {
				defer wg.Done()
				_ = c.SetWriteDeadline(time.Now().Add(20 * time.Second))
				_, _ = fmt.Fprintf(c, "POST /api/v1/rules HTTP/1.1\r\nHost: x\r\nX-Mimamori-Request: 1\r\nContent-Length: %d\r\n\r\n", 30<<20)
				_, _ = c.Write(chunk)
			}()
		}
		time.Sleep(500 * time.Millisecond)
		code, _, d, err := ts.agentDo("", "POST", "/agent/v1/report", paddedReport(20<<20),
			map[string]string{"Authorization": "Bearer " + tok, modules.HeaderInstanceID: "m-busy"})
		if err != nil || code != http.StatusOK || d > 10*time.Second {
			t.Fatalf("authenticated 20 MiB report behind 256 unauthenticated bodies: %d %v after %v", code, err, d)
		}
		wg.Wait()
	})
}

// A table full of connections that may already be evicted does not make
// the accepting flow spin: the listeners stay in the poll set, so ppoll
// waits for a new connection (or the next deadline) instead of returning
// at once. Only a future eviction time shortens the wait.
func TestPollTimeoutFullTableDoesNotSpin(t *testing.T) {
	s := newConnServer(nil, 2, 10*time.Second, log.New(io.Discard, "", 0))
	now := time.Now()
	for i := 0; i < modules.MaxPendingConns; i++ {
		s.conns = append(s.conns, &conn{state: stateRequest, stateAt: now.Add(-2 * modules.EvictGrace), deadline: now.Add(5 * time.Second)})
	}
	if !s.hasRoom(now) {
		t.Fatal("a full table of evictable connections has no room")
	}
	if d := s.pollTimeout(now); d < modules.LoopWakeInterval {
		t.Fatalf("pollTimeout = %v with every connection evictable already, want %v", d, modules.LoopWakeInterval)
	}
	s.conns[0].stateAt = now.Add(-modules.EvictGrace + 100*time.Millisecond)
	if d := s.pollTimeout(now); d <= 0 || d > 150*time.Millisecond {
		t.Fatalf("pollTimeout = %v with one connection evictable in 100 ms", d)
	}
	s.conns[1].state, s.conns[1].stateAt = stateDraining, now.Add(-time.Second)
	if d := s.pollTimeout(now); d <= 0 {
		t.Fatalf("pollTimeout = %v with a draining connection in a full table", d)
	}
}
