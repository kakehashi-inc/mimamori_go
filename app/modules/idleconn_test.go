package modules

import (
	"context"
	"errors"
	"io"
	"net"
	"strconv"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"
)

// The tests run over real loopback sockets. Times have a generous margin
// above, because other tests may run on the machine at the same time.

// idlePair returns a connected pair: the accepted side wrapped in an
// IdleConn with the given idle time (onClose counts its calls) and the
// dialled side as a plain TCP connection. rcvbuf sets the receive buffer of
// the plain side and sndbuf the send buffer of the wrapped side (0: the
// default).
func idlePair(t *testing.T, idle time.Duration, rcvbuf, sndbuf int) (*IdleConn, *net.TCPConn, *atomic.Int32) {
	t.Helper()
	ln, err := net.ListenTCP("tcp", &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	d := net.Dialer{Timeout: 5 * time.Second}
	if rcvbuf > 0 {
		d.Control = func(_, _ string, rc syscall.RawConn) error {
			return rc.Control(func(fd uintptr) { _ = syscall.SetsockoptInt(int(fd), syscall.SOL_SOCKET, syscall.SO_RCVBUF, rcvbuf) })
		}
	}
	peer, err := d.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { peer.Close() })
	tc, err := ln.AcceptTCP()
	if err != nil {
		t.Fatal(err)
	}
	if sndbuf > 0 {
		_ = tc.SetWriteBuffer(sndbuf)
	}
	var closed atomic.Int32
	ic, err := NewIdleConn(tc, idle, func() { closed.Add(1) })
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ic.Close() })
	return ic, peer.(*net.TCPConn), &closed
}

func within(t *testing.T, what string, got, low, high time.Duration) {
	t.Helper()
	if got < low || got > high {
		t.Fatalf("%s after %v, want between %v and %v", what, got, low, high)
	}
}

func TestIdleConnReadCutAfterSilence(t *testing.T) {
	const idle = 500 * time.Millisecond
	ic, peer, closed := idlePair(t, idle, 0, 0)
	start := time.Now()
	_, err := ic.Read(make([]byte, 10))
	within(t, "read cut", time.Since(start), idle, idle+700*time.Millisecond)
	var ne net.Error
	if !errors.As(err, &ne) || !ne.Timeout() {
		t.Fatalf("want a timeout error, got %v", err)
	}
	if closed.Load() != 1 {
		t.Fatalf("onClose calls: %d", closed.Load())
	}
	if read, cut := ic.SilenceCut(); !read || cut != err {
		t.Fatalf("SilenceCut after the read cut: %v %v, want true and %v", read, cut, err)
	}
	start = time.Now()
	if _, err := ic.Read(make([]byte, 10)); err == nil || time.Since(start) > 100*time.Millisecond {
		t.Fatalf("a second read must fail at once: %v after %v", err, time.Since(start))
	}
	_ = ic.Close()
	if closed.Load() != 1 {
		t.Fatalf("onClose must run once: %d", closed.Load())
	}
	// The peer sees the connection closed.
	_ = peer.SetReadDeadline(time.Now().Add(2 * time.Second))
	if _, err := peer.Read(make([]byte, 1)); err != io.EOF {
		t.Fatalf("the peer must see the close: %v", err)
	}
}

func TestIdleConnReadMovesWithEveryByte(t *testing.T) {
	const idle = 300 * time.Millisecond
	ic, peer, closed := idlePair(t, idle, 0, 0)
	go func() {
		end := time.Now().Add(5 * idle)
		for time.Now().Before(end) {
			time.Sleep(idle / 3)
			if _, err := peer.Write([]byte{'x'}); err != nil {
				return
			}
		}
		_ = peer.Close()
	}()
	start := time.Now()
	total := 0
	buf := make([]byte, 16)
	for {
		n, err := ic.Read(buf)
		total += n
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("cut after %v and %d bytes: %v", time.Since(start), total, err)
		}
	}
	if time.Since(start) < 5*idle || total < 10 || closed.Load() != 0 {
		t.Fatalf("read %d bytes over %v, closed %d", total, time.Since(start), closed.Load())
	}
}

func TestIdleConnWriteCutFromLastByteTaken(t *testing.T) {
	const idle = time.Second
	ic, peer, closed := idlePair(t, idle, 16<<10, 32<<10)
	// The peer takes bytes for 2 x idle, then nothing.
	stopped := make(chan time.Time, 1)
	go func() {
		buf := make([]byte, 4<<10)
		end := time.Now().Add(2 * idle)
		for time.Now().Before(end) {
			if _, err := peer.Read(buf); err != nil {
				break
			}
			time.Sleep(50 * time.Millisecond)
		}
		stopped <- time.Now()
	}()
	start := time.Now()
	n, err := ic.Write(make([]byte, 64<<20))
	cut := time.Now()
	var ne net.Error
	if !errors.As(err, &ne) || !ne.Timeout() {
		t.Fatalf("want a timeout error, got %v after %d bytes", err, n)
	}
	s := <-stopped
	if cut.Sub(start) < 2*idle {
		t.Fatalf("cut after %v, while the peer was taking bytes", cut.Sub(start))
	}
	// The kernel takes the last bytes shortly after the peer stopped
	// reading (what the buffers hold); the cut is idle after that.
	within(t, "write cut after the peer stopped reading", cut.Sub(s), idle-200*time.Millisecond, idle+time.Second)
	if closed.Load() != 1 {
		t.Fatalf("the cut must close the connection: %d", closed.Load())
	}
	if read, c := ic.SilenceCut(); read || c != err {
		t.Fatalf("SilenceCut after the write cut: %v %v, want false and %v", read, c, err)
	}
}

func TestIdleConnWriteNotCutWhilePeerTakes(t *testing.T) {
	const idle = 500 * time.Millisecond
	ic, peer, closed := idlePair(t, idle, 16<<10, 32<<10)
	const size = 8 << 20
	got := make(chan int, 1)
	go func() {
		buf := make([]byte, 2<<10)
		total := 0
		end := time.Now().Add(4 * idle)
		for time.Now().Before(end) {
			n, err := peer.Read(buf)
			total += n
			if err != nil {
				got <- total
				return
			}
			time.Sleep(20 * time.Millisecond)
		}
		rest, _ := io.Copy(io.Discard, peer)
		got <- total + int(rest)
	}()
	start := time.Now()
	n, err := ic.Write(make([]byte, size))
	if err != nil || n != size {
		t.Fatalf("write: %d bytes after %v: %v", n, time.Since(start), err)
	}
	if time.Since(start) < 4*idle {
		t.Fatalf("the write took only %v, the test proves nothing", time.Since(start))
	}
	_ = ic.Close()
	if total := <-got; total != size {
		t.Fatalf("the peer received %d bytes", total)
	}
	if closed.Load() != 1 {
		t.Fatalf("onClose calls: %d", closed.Load())
	}
}

func TestIdleConnPauseAndResume(t *testing.T) {
	const idle = 400 * time.Millisecond
	ic, _, closed := idlePair(t, idle, 0, 0)
	resumed := make(chan time.Time, 1)
	go func() {
		time.Sleep(idle / 2)
		ic.Pause()
		time.Sleep(3 * idle)
		resumed <- time.Now()
		ic.Resume()
	}()
	start := time.Now()
	_, err := ic.Read(make([]byte, 1))
	end := time.Now()
	r := <-resumed
	if !isTimeout(err) {
		t.Fatalf("want a timeout after the resume, got %v", err)
	}
	if end.Sub(start) < 3*idle {
		t.Fatalf("the paused read was cut after %v", end.Sub(start))
	}
	within(t, "read cut after the resume", end.Sub(r), idle-50*time.Millisecond, idle+700*time.Millisecond)
	if closed.Load() != 1 {
		t.Fatalf("onClose calls: %d", closed.Load())
	}

	// A paused read still ends at once when the peer closes.
	ic, peer, _ := idlePair(t, idle, 0, 0)
	ic.Pause()
	closedAt := make(chan time.Time, 1)
	go func() {
		time.Sleep(2 * idle)
		closedAt <- time.Now()
		_ = peer.Close()
	}()
	_, err = ic.Read(make([]byte, 1))
	if err != io.EOF {
		t.Fatalf("want EOF, got %v", err)
	}
	if d := time.Since(<-closedAt); d > 300*time.Millisecond {
		t.Fatalf("the read ended %v after the peer closed", d)
	}
}

func TestIdleConnDeadlineFromAbove(t *testing.T) {
	const idle = time.Second
	ic, peer, closed := idlePair(t, idle, 0, 0)
	// A past deadline set from another flow ends a read in progress.
	go func() {
		time.Sleep(100 * time.Millisecond)
		_ = ic.SetReadDeadline(time.Unix(1, 0))
	}()
	start := time.Now()
	_, err := ic.Read(make([]byte, 1))
	if !isTimeout(err) || time.Since(start) > idle/2 {
		t.Fatalf("the deadline from above must end the read: %v after %v", err, time.Since(start))
	}
	// and a read begun afterwards.
	start = time.Now()
	if _, err := ic.Read(make([]byte, 1)); !isTimeout(err) || time.Since(start) > idle/2 {
		t.Fatalf("a later read: %v after %v", err, time.Since(start))
	}
	if closed.Load() != 0 {
		t.Fatal("a deadline from above must not close the connection")
	}
	if _, cut := ic.SilenceCut(); cut != nil {
		t.Fatalf("a deadline from above is no silence cut: %v", cut)
	}
	// Set back to zero: usable, and the idle rule holds again.
	_ = ic.SetReadDeadline(time.Time{})
	if _, err := peer.Write([]byte{'a'}); err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, 1)
	if n, err := ic.Read(buf); n != 1 || err != nil || buf[0] != 'a' {
		t.Fatalf("read after the reset: %d %v", n, err)
	}
	start = time.Now()
	_, err = ic.Read(buf)
	within(t, "idle cut after the reset", time.Since(start), idle, idle+700*time.Millisecond)
	if !isTimeout(err) || closed.Load() != 1 {
		t.Fatalf("idle cut: %v, closed %d", err, closed.Load())
	}
	if read, cut := ic.SilenceCut(); !read || cut != err {
		t.Fatalf("SilenceCut after the idle cut: %v %v", read, cut)
	}
}

func TestIdleConnWriteDeadlineFromAbove(t *testing.T) {
	const idle = 2 * time.Second
	ic, peer, closed := idlePair(t, idle, 16<<10, 32<<10)
	_ = ic.SetWriteDeadline(time.Now().Add(300 * time.Millisecond))
	start := time.Now()
	n, err := ic.Write(make([]byte, 32<<20))
	if !isTimeout(err) {
		t.Fatalf("want a timeout, got %v", err)
	}
	within(t, "write ended by the deadline from above", time.Since(start), 250*time.Millisecond, idle/2)
	if closed.Load() != 0 {
		t.Fatal("the deadline from above must not close the connection")
	}
	// The connection stays usable: what was written arrives, and a write
	// after the reset completes.
	_ = ic.SetWriteDeadline(time.Time{})
	done := make(chan int64, 1)
	go func() {
		got, _ := io.Copy(io.Discard, peer)
		done <- got
	}()
	m, err := ic.Write([]byte("end"))
	if err != nil || m != 3 {
		t.Fatalf("write after the reset: %d %v", m, err)
	}
	_ = ic.Close()
	if got := <-done; got != int64(n)+3 {
		t.Fatalf("the peer received %d bytes, want %d", got, n+3)
	}
}

func TestIdleConnOnlyConnMethods(t *testing.T) {
	var c any = &IdleConn{}
	if _, ok := c.(io.ReaderFrom); ok {
		t.Fatal("IdleConn must not be an io.ReaderFrom")
	}
	if _, ok := c.(io.WriterTo); ok {
		t.Fatal("IdleConn must not be an io.WriterTo")
	}
}

func TestDialIdle(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	c, err := DialIdle(addr, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if c.RemoteAddr().String() != addr {
		t.Fatalf("remote address %v", c.RemoteAddr())
	}
	_ = c.Close()
	_ = ln.Close()
	if _, err := DialIdle(addr, time.Second); !errors.Is(err, syscall.ECONNREFUSED) {
		t.Fatalf("a refused address must fail: %v", err)
	}
}

// fullListener listens on ip (IPv4) with an accept queue of one connection,
// fills the queue and never accepts: the kernel then drops every further
// SYN, so a connect to it waits without any answer. It returns the port.
func fullListener(t *testing.T, ip net.IP) int {
	t.Helper()
	fd, err := syscall.Socket(syscall.AF_INET, syscall.SOCK_STREAM|syscall.SOCK_CLOEXEC, 0)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = syscall.Close(fd) })
	sa := &syscall.SockaddrInet4{}
	copy(sa.Addr[:], ip.To4())
	if err := syscall.Bind(fd, sa); err != nil {
		t.Fatal(err)
	}
	if err := syscall.Listen(fd, 0); err != nil {
		t.Fatal(err)
	}
	bound, err := syscall.Getsockname(fd)
	if err != nil {
		t.Fatal(err)
	}
	port := bound.(*syscall.SockaddrInet4).Port
	addr := net.JoinHostPort(ip.String(), strconv.Itoa(port))
	for range 8 {
		c, err := net.DialTimeout("tcp", addr, 300*time.Millisecond)
		if err != nil {
			return port // not answered: the queue is full
		}
		t.Cleanup(func() { _ = c.Close() })
	}
	t.Fatalf("the accept queue of %s did not fill", addr)
	return 0
}

// fakeDNS serves the A records addrs for the name host on a UDP port of
// loopback (no data for other types of host, "no such host" for other
// names) and returns a resolver of Go's own that asks only this server.
// Every answer waits for delay; with never no query is answered.
func fakeDNS(t *testing.T, host string, delay time.Duration, never bool, addrs ...string) *net.Resolver {
	t.Helper()
	pc, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = pc.Close() })
	go func() {
		buf := make([]byte, 1500)
		for {
			n, from, err := pc.ReadFrom(buf)
			if err != nil {
				return
			}
			if never {
				continue
			}
			answer := dnsAnswer(buf[:n], host, addrs)
			go func() {
				time.Sleep(delay)
				_, _ = pc.WriteTo(answer, from)
			}()
		}
	}()
	server := pc.LocalAddr().String()
	return &net.Resolver{PreferGo: true, Dial: func(ctx context.Context, _, _ string) (net.Conn, error) {
		var d net.Dialer
		return d.DialContext(ctx, "udp", server)
	}}
}

// dnsAnswer answers the DNS query q: the A records addrs when it asks for
// the A records of host, no data for another type of host, "no such host"
// for another name.
func dnsAnswer(q []byte, host string, addrs []string) []byte {
	if len(q) < 12 {
		return nil
	}
	var labels []string
	i := 12
	for i < len(q) && q[i] != 0 {
		l := int(q[i])
		if i+1+l > len(q) {
			return nil
		}
		labels = append(labels, string(q[i+1:i+1+l]))
		i += l + 1
	}
	i++ // the root label
	if i+4 > len(q) {
		return nil
	}
	qtype := int(q[i])<<8 | int(q[i+1])
	resp := append([]byte{q[0], q[1], 0x81, 0x80, 0, 1, 0, 0, 0, 0, 0, 0}, q[12:i+4]...)
	switch {
	case !strings.EqualFold(strings.Join(labels, "."), host):
		resp[3] |= 3 // NXDOMAIN
	case qtype == 1:
		resp[7] = byte(len(addrs))
		for _, a := range addrs {
			resp = append(resp, 0xc0, 12, 0, 1, 0, 1, 0, 0, 0, 60, 0, 4)
			resp = append(resp, net.ParseIP(a).To4()...)
		}
	}
	return resp
}

// A name with several addresses: each address gets the whole idle time on
// its own. The first one never answers (a full accept queue drops the SYN);
// the second is tried after the whole idle time and connects.
func TestDialIdleEveryAddressGetsTheWholeTime(t *testing.T) {
	t.Parallel()
	const idle = 4 * time.Second
	port := fullListener(t, net.IPv4(127, 0, 0, 2))
	ln, err := net.Listen("tcp", net.JoinHostPort("127.0.0.3", strconv.Itoa(port)))
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go func() {
		if c, err := ln.Accept(); err == nil {
			defer c.Close()
			_, _ = io.Copy(io.Discard, c)
		}
	}()
	start := time.Now()
	c, err := dialIdle(net.JoinHostPort("two.test", strconv.Itoa(port)), idle, fakeDNS(t, "two.test", 0, false, "127.0.0.2", "127.0.0.3"))
	took := time.Since(start)
	if err != nil {
		t.Fatalf("dial after %v: %v", took, err)
	}
	defer func() { _ = c.Close() }()
	if got := c.RemoteAddr().String(); got != ln.Addr().String() {
		t.Fatalf("connected to %s, want the second address %s", got, ln.Addr())
	}
	within(t, "connected to the second address", took, idle-50*time.Millisecond, idle+3*time.Second)
}

// When no address connects, the error of the first attempt is returned.
func TestDialIdleNoAddressConnects(t *testing.T) {
	t.Parallel()
	ln, err := net.Listen("tcp", "127.0.0.2:0")
	if err != nil {
		t.Fatal(err)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	_ = ln.Close()
	dns := fakeDNS(t, "two.test", 0, false, "127.0.0.2", "127.0.0.3")
	_, err = dialIdle(net.JoinHostPort("two.test", strconv.Itoa(port)), time.Second, dns)
	var oe *net.OpError
	if !errors.Is(err, syscall.ECONNREFUSED) || !errors.As(err, &oe) || oe.Addr == nil || oe.Addr.String() != net.JoinHostPort("127.0.0.2", strconv.Itoa(port)) {
		t.Fatalf("both addresses refuse: %v, want the refusal of the first one", err)
	}
	_, err = dialIdle(net.JoinHostPort("other.test", "1"), time.Second, dns)
	var de *net.DNSError
	if !errors.As(err, &de) || !de.IsNotFound || !strings.HasPrefix(err.Error(), "dial tcp: lookup other.test") {
		t.Fatalf("unknown name: %v", err)
	}
}

// Name resolution waits at most the idle time: a DNS server that never
// answers ends the dial with an error after it, one that answers within it
// is waited for.
func TestDialIdleResolutionBounded(t *testing.T) {
	t.Parallel()
	const idle = 700 * time.Millisecond
	start := time.Now()
	_, err := dialIdle("slow.test:25", idle, fakeDNS(t, "slow.test", 0, true, "127.0.0.1"))
	took := time.Since(start)
	var de *net.DNSError
	if !errors.As(err, &de) || !de.IsTimeout || !strings.Contains(err.Error(), "slow.test") {
		t.Fatalf("resolution that is never answered: %v", err)
	}
	within(t, "resolution given up", took, idle-50*time.Millisecond, idle+2*time.Second)

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	_, port, _ := net.SplitHostPort(ln.Addr().String())
	c, err := dialIdle(net.JoinHostPort("late.test", port), idle, fakeDNS(t, "late.test", 300*time.Millisecond, false, "127.0.0.1"))
	if err != nil {
		t.Fatalf("an answer within the idle time: %v", err)
	}
	_ = c.Close()
}
