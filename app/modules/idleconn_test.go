package modules

import (
	"errors"
	"io"
	"net"
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
