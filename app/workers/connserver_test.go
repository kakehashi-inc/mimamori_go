package workers

import (
	"bufio"
	"context"
	"crypto/tls"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
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

// connTestServer runs the central's connection server with one worker on
// loopback listeners for the browser handler (plain HTTP) and the agent
// handler (HTTPS).
type connTestServer struct {
	e         *testEnv
	web, agnt string
	stop      atomic.Bool
	done      chan struct{}
}

func startConnTestServer(t *testing.T, queueTimeout time.Duration) *connTestServer {
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
		{name: "web", ln: webLn, handler: e.web},
		{name: "agent", ln: agentLn, tls: e.c.tlsConfig(), handler: e.agent},
	}
	ts := &connTestServer{e: e, web: webLn.Addr().String(), agnt: agentLn.Addr().String(), done: make(chan struct{})}
	srv := newConnServer(specs, 1, queueTimeout, log.New(io.Discard, "", 0))
	// The test runs the central flow on a goroutine of its own; with one
	// worker the server itself starts none.
	go func() {
		defer close(ts.done)
		srv.run(ts.stop.Load, func() func() { return nil })
	}()
	t.Cleanup(func() {
		ts.stop.Store(true)
		<-ts.done
	})
	return ts
}

// webRaw sends one plain HTTP request and returns the status line.
func (ts *connTestServer) webRaw(t *testing.T, raw string) (string, time.Duration) {
	t.Helper()
	start := time.Now()
	c, err := net.DialTimeout("tcp", ts.web, 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	_ = c.SetDeadline(time.Now().Add(30 * time.Second))
	if _, err := io.WriteString(c, raw); err != nil {
		t.Fatal(err)
	}
	line, _ := bufio.NewReader(c).ReadString('\n')
	return strings.TrimSpace(line), time.Since(start)
}

// agentGet sends one HTTPS request to the agent listener.
func (ts *connTestServer) agentGet(t *testing.T, path string, hdr map[string]string) (int, time.Duration) {
	t.Helper()
	start := time.Now()
	raw, err := net.DialTimeout("tcp", ts.agnt, 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer raw.Close()
	_ = raw.SetDeadline(time.Now().Add(30 * time.Second))
	pool, _ := modules.CertPoolFromPEMs(ts.e.c.currentCAPEMs(), time.Now())
	c := tls.Client(raw, &tls.Config{RootCAs: pool, ServerName: "127.0.0.1", MinVersion: tls.VersionTLS12})
	req, _ := http.NewRequest("GET", "https://"+ts.agnt+path, nil)
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	req.Close = true
	if err := req.Write(c); err != nil {
		t.Fatal(err)
	}
	resp, err := http.ReadResponse(bufio.NewReader(c), req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	return resp.StatusCode, time.Since(start)
}

func dialSilent(t *testing.T, addr string, n int) []net.Conn {
	t.Helper()
	var out []net.Conn
	for i := 0; i < n; i++ {
		c, err := net.DialTimeout("tcp", addr, 5*time.Second)
		if err != nil {
			t.Fatal(err)
		}
		out = append(out, c)
	}
	t.Cleanup(func() {
		for _, c := range out {
			c.Close()
		}
	})
	return out
}

const healthRequest = "GET /api/v1/health HTTP/1.1\r\nHost: x\r\n\r\n"

// With one worker, connections that send nothing (or half a request head)
// never reach the worker: normal requests on both listeners are answered at
// once, and the waiting connections are rejected after the queue timeout.
func TestConnServerSilentConnections(t *testing.T) {
	ts := startConnTestServer(t, 2*time.Second)
	silentWeb := dialSilent(t, ts.web, 50)
	silentAgent := dialSilent(t, ts.agnt, 50)
	half := dialSilent(t, ts.web, 1)[0]
	if _, err := io.WriteString(half, "GET /api/v1/health HTTP/1.1\r\nHost: x\r\n"); err != nil {
		t.Fatal(err)
	}
	time.Sleep(200 * time.Millisecond)

	for i := 0; i < 3; i++ {
		line, d := ts.webRaw(t, healthRequest)
		if !strings.Contains(line, " 200 ") || d > time.Second {
			t.Fatalf("web request with silent connections: %q after %v", line, d)
		}
		code, d := ts.agentGet(t, "/agent/v1/ca", nil)
		if code != http.StatusOK || d > time.Second {
			t.Fatalf("agent request with silent connections: %d after %v", code, d)
		}
		code, d = ts.agentGet(t, "/admin/v1/agents", map[string]string{"Authorization": "Bearer " + ts.e.cliKey})
		if code != http.StatusOK || d > time.Second {
			t.Fatalf("management request with silent connections: %d after %v", code, d)
		}
	}

	// The queue timeout rejects them: plain HTTP with 503, TLS by closing.
	for _, c := range []net.Conn{silentWeb[0], half} {
		_ = c.SetReadDeadline(time.Now().Add(5 * time.Second))
		line, _ := bufio.NewReader(c).ReadString('\n')
		if !strings.Contains(line, " 503 ") {
			t.Fatalf("waiting plain connection: %q, want 503", line)
		}
	}
	_ = silentAgent[0].SetReadDeadline(time.Now().Add(5 * time.Second))
	if n, err := silentAgent[0].Read(make([]byte, 1)); n != 0 || err == nil || isTimeout(err) {
		t.Fatalf("waiting TLS connection: read %d, %v; want closed", n, err)
	}
}

func isTimeout(err error) bool {
	ne, ok := err.(net.Error)
	return ok && ne.Timeout()
}

// A full queue makes room by dropping the oldest connection that sent
// nothing, so silent connections cannot keep a normal client out.
func TestConnServerFullQueue(t *testing.T) {
	ts := startConnTestServer(t, 30*time.Second)
	dialSilent(t, ts.web, modules.MaxPendingConns+10)
	time.Sleep(200 * time.Millisecond)
	line, d := ts.webRaw(t, healthRequest)
	if !strings.Contains(line, " 200 ") || d > time.Second {
		t.Fatalf("request with a full queue: %q after %v", line, d)
	}
}

// A body sent slowly to a route that reads it before the caller is
// authenticated (login) is cut after ConnBodyTimeout with 408; a normal
// login request is unchanged.
func TestConnServerSlowLoginBody(t *testing.T) {
	ts := startConnTestServer(t, 30*time.Second)
	line, _ := ts.webRaw(t, "POST /api/v1/auth/login HTTP/1.1\r\nHost: x\r\nX-Mimamori-Request: 1\r\nContent-Type: application/json\r\nContent-Length: 40\r\n\r\n{\"email\":\"a@example.com\",\"password\":\"x\"}")
	if !strings.Contains(line, " 401 ") {
		t.Fatalf("normal login: %q, want 401", line)
	}

	c := dialSilent(t, ts.web, 1)[0]
	if _, err := io.WriteString(c, "POST /api/v1/auth/login HTTP/1.1\r\nHost: x\r\nX-Mimamori-Request: 1\r\nContent-Type: application/json\r\nContent-Length: 100\r\n\r\n{"); err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	stopWriting := make(chan struct{})
	defer close(stopWriting)
	go func() {
		for {
			select {
			case <-stopWriting:
				return
			case <-time.After(500 * time.Millisecond):
				if _, err := io.WriteString(c, " "); err != nil {
					return
				}
			}
		}
	}()
	_ = c.SetReadDeadline(time.Now().Add(modules.ConnBodyTimeout + 5*time.Second))
	got, _ := bufio.NewReader(c).ReadString('\n')
	d := time.Since(start)
	if !strings.Contains(got, " 408 ") || d < modules.ConnBodyTimeout-time.Second || d > modules.ConnBodyTimeout+2*time.Second {
		t.Fatalf("slow body: %q after %v, want 408 after about %v", got, d, modules.ConnBodyTimeout)
	}
	line, d = ts.webRaw(t, healthRequest)
	if !strings.Contains(line, " 200 ") || d > time.Second {
		t.Fatalf("request after the slow body: %q after %v", line, d)
	}
}

// A TLS client that stops in the middle of the handshake holds the worker
// for ConnHeaderTimeout at most.
func TestConnServerStalledHandshake(t *testing.T) {
	ts := startConnTestServer(t, 30*time.Second)
	c := dialSilent(t, ts.agnt, 1)[0]
	// The start of a ClientHello record, never completed.
	if _, err := c.Write([]byte{0x16, 0x03, 0x01, 0x02, 0x00, 0x01, 0x00, 0x01, 0xfc}); err != nil {
		t.Fatal(err)
	}
	time.Sleep(200 * time.Millisecond)
	line, d := ts.webRaw(t, healthRequest)
	if !strings.Contains(line, " 200 ") || d > modules.ConnHeaderTimeout+time.Second {
		t.Fatalf("request behind a stalled handshake: %q after %v", line, d)
	}
}
