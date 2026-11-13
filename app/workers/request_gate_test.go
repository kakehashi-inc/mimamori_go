package workers

import (
	"bufio"
	"bytes"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"mimamori/app/models"
	"mimamori/app/modules"
)

// withBody returns a request head (with %d for the length) followed by a
// body of n bytes.
func withBody(head string, n int) []byte {
	return append([]byte(fmt.Sprintf(head, n)), bytes.Repeat([]byte("x"), n)...)
}

// The answers the gate writes itself, and what it adds to every answer.
func TestGateBodyRules(t *testing.T) {
	s := startServe(t, serveOpts{})
	s.both(t, func(t *testing.T, dial func() net.Conn) {
		a := roundTrip(t, dial(), "POST /test/echo HTTP/1.1\r\nHost: x\r\nTransfer-Encoding: chunked\r\n\r\n3\r\nabc\r\n0\r\n\r\n")
		if a.status != http.StatusLengthRequired || !strings.Contains(a.body, `"code":"common.length_required"`) ||
			a.header.Get("Content-Type") != "application/json" || !a.close {
			t.Errorf("chunked body: %d %q %v", a.status, a.body, a.header)
		}
		a = roundTrip(t, dial(), fmt.Sprintf("POST /test/echo HTTP/1.1\r\nHost: x\r\nContent-Length: %d\r\n\r\n", modules.MaxRequestBody+1))
		if a.status != http.StatusBadRequest || !strings.Contains(a.body, "too large") || !a.close {
			t.Errorf("body over the limit: %d %q %v", a.status, a.body, a.header)
		}
		// The body ends early: the peer closes its sending side.
		c := dial()
		_, _ = io.WriteString(c, "POST /test/echo HTTP/1.1\r\nHost: x\r\nContent-Length: 10\r\n\r\n01234")
		closeWrite(t, c)
		a = readAnswer(c, bufio.NewReader(c), "POST", 10*time.Second)
		if a.status != http.StatusBadRequest || a.body != "" || !a.close {
			t.Errorf("body ended early: %d %q %v %v", a.status, a.body, a.header, a.err)
		}
		// Content-Length on every answer, also for HEAD (the length GET has).
		get := roundTrip(t, dial(), getReq("/test/slow"))
		head := roundTrip(t, dial(), "HEAD /test/slow HTTP/1.1\r\nHost: x\r\n\r\n")
		if get.header.Get("Content-Length") != "4" || head.status != http.StatusOK || head.header.Get("Content-Length") != "4" || head.body != "" {
			t.Errorf("GET %v; HEAD %d %q %v", get.header, head.status, head.body, head.header)
		}
		if get.close {
			t.Errorf("Connection: close on an ordinary answer")
		}
		// A refused body with Expect: 100-continue: the final answer at
		// once, no 100 Continue.
		c = dial()
		_, _ = fmt.Fprintf(c, "POST /api/v1/rules HTTP/1.1\r\nHost: x\r\nX-Mimamori-Request: 1\r\nExpect: 100-continue\r\nContent-Length: %d\r\n\r\n", 20<<20)
		a = readAnswer(c, bufio.NewReader(c), "POST", 5*time.Second)
		if a.status/100 != 4 || !a.close {
			t.Errorf("refused body with Expect: %d %v", a.status, a.header)
		}
		a = roundTrip(t, dial(), "OPTIONS * HTTP/1.1\r\nHost: x\r\n\r\n")
		if a.status != http.StatusBadRequest || a.header.Get("X-Content-Type-Options") != "nosniff" {
			t.Errorf("OPTIONS *: %d %v", a.status, a.header)
		}
	})
}

// closeWrite closes the sending side of a plain or TLS connection.
func closeWrite(t *testing.T, c net.Conn) {
	t.Helper()
	cw, ok := c.(interface{ CloseWrite() error })
	if !ok {
		t.Fatalf("%T cannot close its sending side", c)
	}
	if err := cw.CloseWrite(); err != nil {
		t.Fatal(err)
	}
}

// paddedAgentReport is an agent report of about n bytes (the agent API
// ignores unknown fields).
func paddedAgentReport(n int) []byte {
	return []byte(`{"created_at":1,"detections":[],"dropped":0,"padding":"` + strings.Repeat("p", n) + `"}`)
}

// A large body is read only from a peer authenticated for its route: an
// unauthenticated one is answered at once without its body being sent, on
// every route kind; an authenticated one works on the agent, management
// and browser routes.
func TestGateLargeBodyNeedsAuthentication(t *testing.T) {
	s := startServe(t, serveOpts{})
	e := s.e
	tok, a, _ := commandAgent(t, e, "big-host")
	adminCk := login(t, e, testAdminEmail, "Admin-pass-123")
	const n = 20 << 20

	headOnly := func(c net.Conn, head string) (answer, time.Duration) {
		start := time.Now()
		if _, err := fmt.Fprintf(c, head, n); err != nil {
			t.Fatal(err)
		}
		return readAnswer(c, bufio.NewReader(c), "POST", 5*time.Second), time.Since(start)
	}
	for _, tc := range []struct {
		what, head string
		want       int
	}{
		{"no session", "POST /api/v1/rules HTTP/1.1\r\nHost: x\r\nX-Mimamori-Request: 1\r\nContent-Length: %d\r\n\r\n", http.StatusUnauthorized},
		{"unknown session", "POST /api/v1/rules HTTP/1.1\r\nHost: x\r\nX-Mimamori-Request: 1\r\nCookie: " + sessionCookie + "=" + modules.SessionPrefix + "nothing\r\nContent-Length: %d\r\n\r\n", http.StatusUnauthorized},
		{"no CSRF header", "POST /api/v1/rules HTTP/1.1\r\nHost: x\r\nCookie: " + sessionCookie + "=" + adminCk + "\r\nContent-Length: %d\r\n\r\n", http.StatusForbidden},
		{"login", "POST /api/v1/auth/login HTTP/1.1\r\nHost: x\r\nX-Mimamori-Request: 1\r\nContent-Length: %d\r\n\r\n", http.StatusBadRequest},
		{"no route", "POST /api/v1/nothing HTTP/1.1\r\nHost: x\r\nX-Mimamori-Request: 1\r\nCookie: " + sessionCookie + "=" + adminCk + "\r\nContent-Length: %d\r\n\r\n", 0},
	} {
		got, d := headOnly(s.dialWeb(t), tc.head)
		if (tc.want != 0 && got.status != tc.want) || got.status/100 != 4 || d > time.Second || !got.close {
			t.Fatalf("web, %s: %d %q %v after %v, want %d at once", tc.what, got.status, got.body, got.err, d, tc.want)
		}
	}
	for _, tc := range []struct{ what, head string }{
		{"bad token", "POST /agent/v1/report HTTP/1.1\r\nHost: x\r\nAuthorization: Bearer mmt_wrong\r\nX-Mimamori-Instance-Id: m\r\nContent-Length: %d\r\n\r\n"},
		{"other instance", "POST /agent/v1/sync HTTP/1.1\r\nHost: x\r\nAuthorization: Bearer " + tok + "\r\nContent-Length: %d\r\n\r\n"},
		{"bad CLI secret", "POST /admin/v1/rules HTTP/1.1\r\nHost: x\r\nAuthorization: Bearer mmc_wrong\r\nContent-Length: %d\r\n\r\n"},
	} {
		got, d := headOnly(s.dialAgent(t), tc.head)
		if got.status != http.StatusUnauthorized || d > time.Second {
			t.Fatalf("agent listener, %s: %d %q %v after %v, want 401 at once", tc.what, got.status, got.body, got.err, d)
		}
	}
	// The checks changed nothing: the token is still unused and unbound.
	if tk, err := models.GetToken(e.c.db.Read(), a.TokenID.Int64); err != nil || tk.UsedAt != 0 || tk.InstanceID != "" {
		t.Fatalf("token after refused requests: %+v %v", tk, err)
	}

	// Authenticated: the body is read and the handler answers on it.
	send := func(c net.Conn, head string, body []byte) answer {
		if _, err := fmt.Fprintf(c, head, len(body)); err != nil {
			t.Fatal(err)
		}
		if _, err := c.Write(body); err != nil {
			t.Fatal(err)
		}
		return readAnswer(c, bufio.NewReader(c), "POST", 30*time.Second)
	}
	got := send(s.dialAgent(t), "POST /agent/v1/report HTTP/1.1\r\nHost: x\r\nAuthorization: Bearer "+tok+"\r\n"+modules.HeaderInstanceID+": m-big\r\nContent-Length: %d\r\n\r\n", paddedAgentReport(n))
	if got.status != http.StatusOK {
		t.Fatalf("authenticated 20 MiB report: %d %q %v", got.status, got.body, got.err)
	}
	big := []byte(`{"name":"r","templates":[],"padding":"` + strings.Repeat("p", n) + `"}`)
	got = send(s.dialAgent(t), "POST /admin/v1/rules HTTP/1.1\r\nHost: x\r\nAuthorization: Bearer "+e.cliKey+"\r\nContent-Length: %d\r\n\r\n", big)
	if got.status != http.StatusBadRequest || !strings.Contains(got.body, "padding") {
		t.Fatalf("authenticated large management request: %d %q %v, want the handler's 400 on the unknown field", got.status, got.body, got.err)
	}
	var hdr strings.Builder
	for k, v := range browserHeaders(adminCk) {
		fmt.Fprintf(&hdr, "%s: %s\r\n", k, v)
	}
	got = send(s.dialWeb(t), "POST /api/v1/rules HTTP/1.1\r\nHost: x\r\n"+hdr.String()+"Content-Length: %d\r\n\r\n", big)
	if got.status != http.StatusBadRequest || !strings.Contains(got.body, "padding") {
		t.Fatalf("authenticated large browser request: %d %q %v, want the handler's 400 on the unknown field", got.status, got.body, got.err)
	}
}

// sendWholeThenRead writes a whole request before it reads the answer.
func sendWholeThenRead(t *testing.T, c net.Conn, raw []byte) (answer, error) {
	t.Helper()
	_ = c.SetWriteDeadline(time.Now().Add(60 * time.Second))
	_, werr := c.Write(raw)
	return readAnswer(c, bufio.NewReader(c), "POST", 60*time.Second), werr
}

// A client that sends a whole body the central answers without reading it
// (over the size limit, or not authenticated) and only then reads gets the
// answer and no write error, on both listeners.
func TestGateAnswerAfterWholeBody(t *testing.T) {
	s := startServe(t, serveOpts{})
	over := modules.MaxRequestBody + 1
	for _, tc := range []struct {
		what string
		dial func() net.Conn
		raw  []byte
		want int
	}{
		{"web, over the limit", func() net.Conn { return s.dialWeb(t) }, withBody("POST /test/echo HTTP/1.1\r\nHost: x\r\nContent-Length: %d\r\n\r\n", over), http.StatusBadRequest},
		{"web, refused 20 MiB", func() net.Conn { return s.dialWeb(t) }, withBody("POST /api/v1/rules HTTP/1.1\r\nHost: x\r\nX-Mimamori-Request: 1\r\nContent-Length: %d\r\n\r\n", 20<<20), http.StatusUnauthorized},
		{"agent, over the limit", func() net.Conn { return s.dialAgent(t) }, withBody("POST /test/echo HTTP/1.1\r\nHost: x\r\nContent-Length: %d\r\n\r\n", over), http.StatusBadRequest},
		{"agent, refused 20 MiB", func() net.Conn { return s.dialAgent(t) }, withBody("POST /agent/v1/report HTTP/1.1\r\nHost: x\r\nAuthorization: Bearer mmt_wrong\r\nContent-Length: %d\r\n\r\n", 20<<20), http.StatusUnauthorized},
	} {
		a, werr := sendWholeThenRead(t, tc.dial(), tc.raw)
		if werr != nil || a.err != nil || a.status != tc.want {
			t.Fatalf("%s: write error %v; answer %d %q %v, want %d", tc.what, werr, a.status, a.body, a.err, tc.want)
		}
	}
}

// Several hundred unauthenticated connections that announce and send large
// bodies do not delay an authenticated 20 MiB report: their bodies are
// never held.
func TestGateUnauthenticatedBodiesDoNotDelayReport(t *testing.T) {
	s := startServe(t, serveOpts{})
	tok, _, _ := commandAgent(t, s.e, "busy-host")
	chunk := bytes.Repeat([]byte("x"), 1<<20)
	var wg sync.WaitGroup
	for i := 0; i < 256; i++ {
		c := s.dialWeb(t)
		wg.Add(1)
		go func() {
			defer wg.Done()
			_ = c.SetWriteDeadline(time.Now().Add(20 * time.Second))
			_, _ = fmt.Fprintf(c, "POST /api/v1/rules HTTP/1.1\r\nHost: x\r\nX-Mimamori-Request: 1\r\nContent-Length: %d\r\n\r\n", 30<<20)
			_, _ = c.Write(chunk)
		}()
	}
	time.Sleep(500 * time.Millisecond)
	start := time.Now()
	c := s.dialAgent(t)
	a, werr := sendWholeThenRead(t, c, append([]byte(fmt.Sprintf("POST /agent/v1/report HTTP/1.1\r\nHost: x\r\nAuthorization: Bearer %s\r\n%s: m-busy\r\nContent-Length: %d\r\n\r\n", tok, modules.HeaderInstanceID, len(paddedAgentReport(20<<20)))), paddedAgentReport(20<<20)...))
	if werr != nil || a.status != http.StatusOK || time.Since(start) > 10*time.Second {
		t.Fatalf("authenticated 20 MiB report behind 256 unauthenticated bodies: %d %v %v after %v", a.status, a.err, werr, time.Since(start))
	}
	wg.Wait()
	s.budget.mu.Lock()
	p := s.budget.peak
	s.budget.mu.Unlock()
	if p != int64(len(paddedAgentReport(20<<20))) {
		t.Fatalf("the budget held %d bytes at most, want only the report's", p)
	}
}

// With a small budget, more large bodies than fit are all answered, and
// never more than the budget is held.
func TestGateBodyMemory(t *testing.T) {
	const size = 5 << 20 // room for two bodies of 2 MiB
	s := startServe(t, serveOpts{budget: size})
	body := bytes.Repeat([]byte("x"), 2<<20)
	var wg sync.WaitGroup
	start := time.Now()
	for i := 0; i < 8; i++ {
		c := s.dialWeb(t)
		wg.Add(1)
		go func() {
			defer wg.Done()
			a, werr := sendWholeThenRead(t, c, append([]byte(fmt.Sprintf("POST /test/echo?ms=300 HTTP/1.1\r\nHost: x\r\nContent-Length: %d\r\n\r\n", len(body))), body...))
			if werr != nil || a.status != http.StatusOK || !strings.HasPrefix(a.body, fmt.Sprintf("%d ", len(body))) {
				t.Errorf("large body: %d %q %v %v", a.status, a.body, a.err, werr)
			}
		}()
	}
	wg.Wait()
	s.budget.mu.Lock()
	peak, free := s.budget.peak, s.budget.free
	s.budget.mu.Unlock()
	if peak > size || peak < 2<<20 || free != size {
		t.Fatalf("budget %d: peak %d, free at the end %d", size, peak, free)
	}
	// Two at a time, 300 ms each: four rounds at least.
	if d := time.Since(start); d < 1100*time.Millisecond {
		t.Fatalf("8 bodies answered after %v: the bound did not make them wait", d)
	}
}

// The budget grants first come, first served: a large waiter is not
// overtaken by a smaller one that would fit.
func TestBodyBudgetOrder(t *testing.T) {
	b := newBodyBudget(10)
	b.acquire(6)
	granted := make(chan int64, 3)
	queue := func(n int64) {
		before := waiting(b)
		go func() {
			b.acquire(n)
			granted <- n
		}()
		for waiting(b) == before {
			time.Sleep(time.Millisecond)
		}
	}
	queue(8) // does not fit while 6 are held
	queue(1) // would fit, but waits behind 8
	select {
	case n := <-granted:
		t.Fatalf("%d granted before the large waiter", n)
	case <-time.After(100 * time.Millisecond):
	}
	b.release(6) // 8 fits, then 1
	got := map[int64]bool{}
	for i := 0; i < 2; i++ {
		select {
		case n := <-granted:
			got[n] = true
		case <-time.After(5 * time.Second):
			t.Fatalf("granted %v, then nothing", got)
		}
	}
	if !got[8] || !got[1] {
		t.Fatalf("granted %v, want 8 and 1", got)
	}
	queue(2) // 1 byte free
	b.release(8)
	select {
	case n := <-granted:
		if n != 2 {
			t.Fatalf("granted %d", n)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("2 not granted after a release")
	}
	b.release(1)
	b.release(2)
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.free != 10 || b.peak != 9 || len(b.waiters) != 0 {
		t.Fatalf("free %d peak %d waiters %d", b.free, b.peak, len(b.waiters))
	}
}

// waiting returns the number of waiters of b.
func waiting(b *bodyBudget) int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return len(b.waiters)
}

// A panicking handler is answered 500 with the JSON and logged, the
// connection stays usable; a panicking pre-check counts as admitted.
func TestGatePanic(t *testing.T) {
	s := startServe(t, serveOpts{})
	s.both(t, func(t *testing.T, dial func() net.Conn) {
		c := dial()
		br := bufio.NewReader(c)
		_, _ = io.WriteString(c, getReq("/test/panic"))
		a := readAnswer(c, br, "GET", 10*time.Second)
		if a.status != http.StatusInternalServerError || a.body != string(internalErrorBody) ||
			a.header.Get("Content-Type") != "application/json" || a.header.Get("X-Content-Type-Options") != "nosniff" {
			t.Fatalf("panic: %d %q %v", a.status, a.body, a.header)
		}
		_, _ = io.WriteString(c, getReq("/test/slow"))
		if a := readAnswer(c, br, "GET", 10*time.Second); a.status != http.StatusOK {
			t.Fatalf("same connection after a panic: %d %v", a.status, a.err)
		}
		body := strings.Repeat("y", modules.MaxHeadBytes+1)
		a = roundTrip(t, dial(), fmt.Sprintf("POST /test/echo HTTP/1.1\r\nHost: x\r\nX-Test-Admit-Panic: 1\r\nContent-Length: %d\r\n\r\n%s", len(body), body))
		if a.status != http.StatusOK || !strings.HasPrefix(a.body, fmt.Sprintf("%d ", len(body))) {
			t.Fatalf("panicking pre-check: %d %q %v, want the body read", a.status, a.body, a.err)
		}
	})
	logs := s.logs.String()
	if !strings.Contains(logs, "GET /test/panic: panic: boom\n") || !strings.Contains(logs, "POST /test/echo: body check panic: admit boom\n") {
		t.Fatalf("log: %q", logs)
	}
}

// nosniff is on every answer of both listeners, the security headers on
// every answer of the browser listener.
func TestGateHeaders(t *testing.T) {
	s := startServe(t, serveOpts{})
	for _, path := range []string{"/api/v1/health", "/", "/api/v1/rules", "/test/panic"} {
		a := roundTrip(t, s.dialWeb(t), getReq(path))
		if a.header.Get("X-Content-Type-Options") != "nosniff" {
			t.Errorf("web %s: no nosniff: %v", path, a.header)
		}
		if path != "/test/panic" && (a.header.Get("X-Frame-Options") != "DENY" || a.header.Get("Referrer-Policy") != "no-referrer" || a.header.Get("Content-Security-Policy") == "") {
			t.Errorf("web %s: security headers missing: %v", path, a.header)
		}
	}
	for _, path := range []string{"/agent/v1/ca", "/agent/v1/nothing", "/test/panic"} {
		if a := roundTrip(t, s.dialAgent(t), getReq(path)); a.header.Get("X-Content-Type-Options") != "nosniff" {
			t.Errorf("agent %s: no nosniff: %v", path, a.header)
		}
	}
	a := roundTrip(t, s.dialWeb(t), "POST /test/echo HTTP/1.1\r\nHost: x\r\nTransfer-Encoding: chunked\r\n\r\n0\r\n\r\n")
	if a.header.Get("X-Content-Type-Options") != "nosniff" {
		t.Errorf("411: no nosniff: %v", a.header)
	}
}
