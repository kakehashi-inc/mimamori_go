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
	"testing"
	"time"

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

func startConnTestServer(t *testing.T, workers int, queueTimeout time.Duration) *connTestServer {
	t.Helper()
	e := newTestEnv(t)
	webLn, err := listenTCP("127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	agentLn, err := listenTCP("127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	specs := []*listenerSpec{
		{name: "web", ln: webLn, handler: testRoutes(e.web)},
		{name: "agent", ln: agentLn, tls: e.c.tlsConfig(), handler: testRoutes(e.agent)},
	}
	ts := &connTestServer{e: e, web: webLn.Addr().String(), agnt: agentLn.Addr().String(), done: make(chan struct{})}
	ts.srv = newConnServer(specs, workers, queueTimeout, log.New(io.Discard, "", 0))
	ts.srv.exempt = func(remote string, local net.Addr) bool {
		return !foreignSources[remoteHost(remote)] && isLocalPeer(remote, local)
	}
	// The test runs the accepting flow on a goroutine of its own; with one
	// worker the server itself starts none.
	go func() {
		defer close(ts.done)
		ts.srv.run(ts.stop.Load, func() func() { return nil })
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
// nothing, so silent connections cannot keep a normal client out.
func TestConnServerFullQueue(t *testing.T) {
	ts := startConnTestServer(t, 1, 30*time.Second)
	dialSilent(t, ts.web, modules.MaxPendingConns+10)
	time.Sleep(200 * time.Millisecond)
	line, d := ts.webRaw(t, healthRequest)
	if !strings.Contains(line, " 200 ") || d > time.Second {
		t.Fatalf("request with a full table: %q after %v", line, d)
	}
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

// A complete ClientHello and then silence delays the others once by
// TLSHandshakeTimeout at most; the source is then refused for the penalty
// period while another source is served. This host itself is never refused.
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
		if !strings.Contains(line, " 200 ") || d > modules.TLSHandshakeTimeout+500*time.Millisecond {
			t.Fatalf("request behind a stalled handshake: %q after %v", line, d)
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
	buf := &tlsBuffer{Conn: sc, blocking: true}
	server := tls.Server(buf, e.c.tlsConfig())
	if err := server.Handshake(); err != nil {
		t.Fatal(err)
	}
	client := <-clientCh
	if client == nil {
		t.Fatal("client handshake failed")
	}
	defer client.Close()
	buf.blocking = false

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
