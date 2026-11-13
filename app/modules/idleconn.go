package modules

import (
	"io"
	"net"
	"os"
	"sync"
	"syscall"
	"time"
)

// IdleTimeout is the longest silence of a peer that is waited for: a
// connection on which the peer sends nothing, or takes nothing of what is
// written, for this long is cut. It is fixed, not a setting, and is the same
// for the central's listeners and for the clients of the agent and the CLI.
const IdleTimeout = 60 * time.Second

// IdleConn is a TCP connection on which no wait for the peer lasts longer
// than idle, and nothing else limits time:
//
//   - R1 Read: a Read waits at most idle for bytes, counted from its start;
//     every byte that arrives ends the wait.
//   - R2 Write: a Write is cut only when the kernel took no byte of it for
//     idle. While it waits it asks the kernel again at least every
//     min(1 s, idle/8), because the kernel reports a socket writable only
//     when about a third of its send buffer is free, which for a slow peer
//     can take longer than idle although bytes leave all the time.
//   - R3 When the idle limit passes, the connection closes itself at that
//     moment and the error (a net.Error whose Timeout is true) is returned;
//     later reads and writes fail at once.
//   - R4 A deadline set from above (net/http, crypto/tls) through
//     SetDeadline, SetReadDeadline or SetWriteDeadline is kept and applied at
//     once together with the idle limit, also to a read or write in
//     progress: the earlier of the two counts, and reaching the deadline from
//     above does not close the connection.
//   - R5 Pause takes the idle limit off reading (a read in progress waits on
//     without it and still ends at once when the peer closes or resets);
//     Resume puts it back, counted from that moment. Writes are never
//     paused.
//   - R6 Close closes the TCP connection once and then calls onClose once.
//
// The type holds the TCP connection in a named field and has only the
// methods of net.Conn plus CloseWrite, Pause and Resume: no ReadFrom or
// WriteTo, so that nothing reads or writes past the rules above.
type IdleConn struct {
	tcp   *net.TCPConn
	raw   syscall.RawConn
	idle  time.Duration
	retry time.Duration // how often a waiting Write asks the kernel again

	mu     sync.Mutex
	paused bool
	rIdle  time.Time // idle limit of the read in progress
	rExt   time.Time // read deadline set from above (zero: none)
	wIdle  time.Time // idle limit of the write in progress (zero: none)
	wExt   time.Time // write deadline set from above (zero: none)

	closeOnce sync.Once
	closeErr  error
	onClose   func()
}

// writeRetryMax is the longest pause between two attempts of a waiting
// Write (R2).
const writeRetryMax = time.Second

// NewIdleConn wraps c. idle must be greater than zero; onClose may be nil.
func NewIdleConn(c *net.TCPConn, idle time.Duration, onClose func()) (*IdleConn, error) {
	raw, err := c.SyscallConn()
	if err != nil {
		return nil, err
	}
	return &IdleConn{tcp: c, raw: raw, idle: idle, retry: min(writeRetryMax, idle/8), onClose: onClose}, nil
}

// DialIdle connects to addr (host:port) and wraps the connection in an
// IdleConn. Connecting, name resolution included, waits at most idle; happy
// eyeballs (parallel attempts) is off.
func DialIdle(addr string, idle time.Duration) (*IdleConn, error) {
	d := net.Dialer{Timeout: idle, FallbackDelay: -1}
	c, err := d.Dial("tcp", addr)
	if err != nil {
		return nil, err
	}
	tc, ok := c.(*net.TCPConn)
	if !ok {
		_ = c.Close()
		return nil, &net.OpError{Op: "dial", Net: "tcp", Err: syscall.EPROTOTYPE}
	}
	ic, err := NewIdleConn(tc, idle, nil)
	if err != nil {
		_ = c.Close()
		return nil, err
	}
	return ic, nil
}

// earlier returns the earlier of two times; a zero time means none.
func earlier(a, b time.Time) time.Time {
	switch {
	case a.IsZero():
		return b
	case b.IsZero():
		return a
	case a.Before(b):
		return a
	}
	return b
}

// applyRead sets the socket's read deadline (mu held): the deadline from
// above and, unless paused, the idle limit of the read in progress.
func (c *IdleConn) applyRead() {
	d := c.rExt
	if !c.paused {
		d = earlier(d, c.rIdle)
	}
	_ = c.tcp.SetReadDeadline(d)
}

// applyWrite sets the socket's write deadline (mu held): the deadline from
// above and, while a write is in progress, its idle limit and the next
// attempt (R2).
func (c *IdleConn) applyWrite(now time.Time) {
	d := c.wExt
	if !c.wIdle.IsZero() {
		d = earlier(earlier(d, c.wIdle), now.Add(c.retry))
	}
	_ = c.tcp.SetWriteDeadline(d)
}

// Read reads under R1.
func (c *IdleConn) Read(p []byte) (int, error) {
	c.mu.Lock()
	c.rIdle = time.Now().Add(c.idle)
	c.applyRead()
	c.mu.Unlock()
	n, err := c.tcp.Read(p)
	if err != nil && isTimeout(err) && c.readIdlePassed() {
		_ = c.Close() // silent for the idle time: closed at this moment (R3)
	}
	return n, err
}

// readIdlePassed reports whether a timeout of a read was the idle limit and
// not a deadline set from above.
func (c *IdleConn) readIdlePassed() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	now := time.Now()
	if c.paused || now.Before(c.rIdle) {
		return false
	}
	return c.rExt.IsZero() || c.rExt.After(now)
}

// Write writes all of p under R2.
func (c *IdleConn) Write(p []byte) (int, error) {
	done := 0
	last := time.Now() // when the kernel last took a byte (or the write began)
	defer func() {
		c.mu.Lock()
		c.wIdle = time.Time{}
		c.applyWrite(time.Now())
		c.mu.Unlock()
	}()
	for done < len(p) {
		c.mu.Lock()
		c.wIdle = last.Add(c.idle)
		c.applyWrite(time.Now())
		c.mu.Unlock()
		var n int
		var werr error
		// One call returns as soon as the kernel took at least one byte; it
		// waits for the socket to become writable only when it took nothing.
		err := c.raw.Write(func(fd uintptr) bool {
			for {
				n, werr = syscall.Write(int(fd), p[done:])
				if werr != syscall.EINTR {
					break
				}
			}
			return werr != syscall.EAGAIN
		})
		if err != nil {
			if !isTimeout(err) {
				return done, err // closed or another failure of the socket
			}
			c.mu.Lock()
			ext := c.wExt
			c.mu.Unlock()
			now := time.Now()
			switch {
			case !ext.IsZero() && !ext.After(now):
				return done, err // the deadline set from above
			case now.Before(last.Add(c.idle)):
				continue // ask the kernel again
			}
			_ = c.Close() // took nothing for the idle time: closed at this moment (R3)
			return done, err
		}
		if werr != nil {
			return done, &net.OpError{Op: "write", Net: "tcp", Source: c.tcp.LocalAddr(), Addr: c.tcp.RemoteAddr(), Err: os.NewSyscallError("write", werr)}
		}
		if n <= 0 {
			return done, io.ErrUnexpectedEOF
		}
		done += n
		last = time.Now()
	}
	return done, nil
}

// isTimeout reports whether err is a timeout of a deadline.
func isTimeout(err error) bool {
	ne, ok := err.(net.Error)
	return ok && ne.Timeout()
}

// Pause takes the idle limit off reading (R5): the owner works on a complete
// request and expects nothing from the peer.
func (c *IdleConn) Pause() {
	c.mu.Lock()
	c.paused = true
	c.applyRead()
	c.mu.Unlock()
}

// Resume puts the idle limit for reading back, counted from now (R5).
func (c *IdleConn) Resume() {
	c.mu.Lock()
	c.paused = false
	c.rIdle = time.Now().Add(c.idle)
	c.applyRead()
	c.mu.Unlock()
}

// SetReadDeadline keeps t as the read deadline from above and applies it at
// once (R4).
func (c *IdleConn) SetReadDeadline(t time.Time) error {
	c.mu.Lock()
	c.rExt = t
	c.applyRead()
	c.mu.Unlock()
	return nil
}

// SetWriteDeadline keeps t as the write deadline from above and applies it
// at once (R4).
func (c *IdleConn) SetWriteDeadline(t time.Time) error {
	c.mu.Lock()
	c.wExt = t
	c.applyWrite(time.Now())
	c.mu.Unlock()
	return nil
}

// SetDeadline sets both deadlines from above.
func (c *IdleConn) SetDeadline(t time.Time) error {
	_ = c.SetReadDeadline(t)
	return c.SetWriteDeadline(t)
}

// Close closes the TCP connection once and then calls onClose once (R6).
func (c *IdleConn) Close() error {
	c.closeOnce.Do(func() {
		c.closeErr = c.tcp.Close()
		if c.onClose != nil {
			c.onClose()
		}
	})
	return c.closeErr
}

// CloseWrite closes the sending side (net/http uses it before it closes a
// connection whose request it did not read completely).
func (c *IdleConn) CloseWrite() error { return c.tcp.CloseWrite() }

// LocalAddr returns the local address.
func (c *IdleConn) LocalAddr() net.Addr { return c.tcp.LocalAddr() }

// RemoteAddr returns the peer's address.
func (c *IdleConn) RemoteAddr() net.Addr { return c.tcp.RemoteAddr() }

var _ net.Conn = (*IdleConn)(nil)
