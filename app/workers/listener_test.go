package workers

import (
	"bufio"
	"io"
	"log"
	"math"
	"net"
	"net/http"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"mimamori/app/modules"
)

// The bound is min(MaxOpenConns, L/4) for the soft limit L on open files.
func TestOpenConnPlaces(t *testing.T) {
	for _, tc := range []struct {
		limit uint64
		want  int
	}{
		{1024, 256},
		{4096, 1024},
		{16383, 4095},
		{16384, modules.MaxOpenConns},
		{20000, modules.MaxOpenConns},
		{524287, modules.MaxOpenConns},
		{math.MaxUint64, modules.MaxOpenConns},
		{3, 1},
	} {
		if got := openConnPlaces(tc.limit); got != tc.want {
			t.Errorf("openConnPlaces(%d) = %d, want %d", tc.limit, got, tc.want)
		}
	}
	// The process's bound follows its real limit, and a lower bound is
	// logged.
	var rl syscall.Rlimit
	if err := syscall.Getrlimit(syscall.RLIMIT_NOFILE, &rl); err != nil {
		t.Fatal(err)
	}
	var logs lockedBuffer
	l := newProcessConnLimit(log.New(&logs, "", 0))
	want := openConnPlaces(rl.Cur)
	if cap(l.places) != want {
		t.Fatalf("bound %d for the open file limit %d, want %d", cap(l.places), rl.Cur, want)
	}
	if logged := strings.Contains(logs.String(), "raise the limit"); logged != (want < modules.MaxOpenConns) {
		t.Fatalf("limit %d, bound %d: log %q", rl.Cur, want, logs.String())
	}
}

// With every place in use a further connection is established by the
// kernel, waits, and is answered when a place is free: nothing is refused.
// A place comes back on every way a connection ends.
func TestConnLimit(t *testing.T) {
	// 6 places: 2 held by the listeners waiting in accept.
	s := startServe(t, serveOpts{places: 6})
	var wg sync.WaitGroup
	for i := 0; i < 5; i++ {
		c := s.dialWeb(t)
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, _ = io.WriteString(c, "GET /test/slow?ms=1500 HTTP/1.1\r\nHost: x\r\nConnection: close\r\n\r\n")
			if a := readAnswer(c, bufio.NewReader(c), "GET", 20*time.Second); a.status != http.StatusOK {
				t.Errorf("request at the bound: %d %v", a.status, a.err)
			}
		}()
	}
	time.Sleep(300 * time.Millisecond)
	if n := s.lim.open(); n != 6 {
		t.Fatalf("%d places in use, want all 6", n)
	}
	start := time.Now()
	c := s.dialWeb(t) // the kernel completes the connection
	if d := time.Since(start); d > time.Second {
		t.Fatalf("connection beyond the bound established after %v", d)
	}
	_, _ = io.WriteString(c, getReq("/test/slow?ms=0"))
	a := readAnswer(c, bufio.NewReader(c), "GET", 20*time.Second)
	if a.status != http.StatusOK || time.Since(start) < 500*time.Millisecond {
		t.Fatalf("connection beyond the bound: %d %v after %v, want 200 after a place was free", a.status, a.err, time.Since(start))
	}
	wg.Wait()
	_ = c.Close()
	s.waitOpen(t, s.baselineCnt, 5*time.Second)

	// Every way a connection ends gives its place back.
	ends := map[string]func(){
		"the peer closes": func() {
			c := s.dialWeb(t)
			roundTrip(t, c, getReq("/api/v1/health"))
			_ = c.Close()
		},
		"the server closes after Connection: close": func() {
			c := s.dialAgent(t)
			roundTrip(t, c, "GET /agent/v1/ca HTTP/1.1\r\nHost: x\r\nConnection: close\r\n\r\n")
		},
		"the idle time passes": func() {
			c := s.dialWeb(t)
			if _, ok := untilClosed(c, s.idle+cutSlack+3*time.Second); !ok {
				t.Errorf("silent connection not closed")
			}
		},
		"the library refuses the head": func() {
			roundTrip(t, s.dialWeb(t), "NOT A REQUEST\r\n\r\n")
		},
		"the request was not read": func() {
			roundTrip(t, s.dialWeb(t), "POST /test/echo HTTP/1.1\r\nHost: x\r\nContent-Length: 40000000\r\nExpect: 100-continue\r\n\r\n")
		},
		"the TLS handshake fails": func() {
			c := s.dialAgentRaw(t)
			_, _ = io.WriteString(c, "\x16\x03\x01\x00\x05garbage")
			untilClosed(c, 10*time.Second)
		},
	}
	for what, end := range ends {
		end()
		if d := s.waitOpen(t, s.baselineCnt, s.idle+cutSlack+3*time.Second); d > cutSlack {
			t.Errorf("%s: place back after %v", what, d)
		}
	}

	// A listener closed while it waits for a place returns.
	full := newConnLimit(1)
	full.places <- struct{}{}
	ln, err := listenTCP("127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	l := full.listener(ln, testIdle)
	done := make(chan error, 1)
	go func() { _, err := l.Accept(); done <- err }()
	time.Sleep(50 * time.Millisecond)
	_ = l.Close()
	select {
	case err := <-done:
		if err != net.ErrClosed {
			t.Fatalf("Accept after Close: %v, want net.ErrClosed", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Accept waiting for a place did not return after Close")
	}
}
