package workers

import (
	"fmt"
	"log"
	"net"
	"os"
	"strconv"
	"sync"
	"syscall"
	"time"

	"mimamori/app/modules"
)

// listenTCP opens a listening socket for "IP:port". The socket is created
// with system calls (instead of net.Listen) so that nothing is read from
// /proc; the address must be an IP address.
func listenTCP(addr string) (*net.TCPListener, error) {
	host, portStr, err := net.SplitHostPort(addr)
	if err != nil {
		return nil, err
	}
	port, err := strconv.Atoi(portStr)
	if err != nil || port < 0 || port > 65535 {
		return nil, fmt.Errorf("%s: invalid port", addr)
	}
	ip := net.ParseIP(host)
	if host == "" {
		ip = net.IPv4zero
	}
	if ip == nil {
		return nil, fmt.Errorf("%s: the listen address must be an IP address", addr)
	}
	var fd int
	if ip4 := ip.To4(); ip4 != nil {
		fd, err = syscall.Socket(syscall.AF_INET, syscall.SOCK_STREAM|syscall.SOCK_CLOEXEC, 0)
		if err != nil {
			return nil, err
		}
		sa := &syscall.SockaddrInet4{Port: port}
		copy(sa.Addr[:], ip4)
		err = bindListen(fd, sa)
	} else {
		fd, err = syscall.Socket(syscall.AF_INET6, syscall.SOCK_STREAM|syscall.SOCK_CLOEXEC, 0)
		if err != nil {
			return nil, err
		}
		sa := &syscall.SockaddrInet6{Port: port}
		copy(sa.Addr[:], ip.To16())
		err = bindListen(fd, sa)
	}
	if err != nil {
		_ = syscall.Close(fd)
		return nil, fmt.Errorf("listen %s: %w", addr, err)
	}
	f := os.NewFile(uintptr(fd), "listener "+addr)
	ln, err := net.FileListener(f)
	_ = f.Close() // FileListener keeps its own copy of the descriptor
	if err != nil {
		return nil, err
	}
	tl, ok := ln.(*net.TCPListener)
	if !ok {
		_ = ln.Close()
		return nil, fmt.Errorf("%s: not a TCP listener", addr)
	}
	return tl, nil
}

func bindListen(fd int, sa syscall.Sockaddr) error {
	if err := syscall.SetsockoptInt(fd, syscall.SOL_SOCKET, syscall.SO_REUSEADDR, 1); err != nil {
		return err
	}
	if err := syscall.Bind(fd, sa); err != nil {
		return err
	}
	return syscall.Listen(fd, listenBacklog)
}

// listenBacklog is the queue length of pending connections.
const listenBacklog = 512

// connLimit bounds the open connections of both listeners together
// (modules.MaxOpenConns, lowered by the limit on open files). A listener
// takes a place before it accepts and the connection gives it back when it
// is closed; while no place is free the listeners do not accept, so new
// connections wait in the kernel's backlog. Nothing is refused and nothing
// is evicted. Each listener holds one place while it waits in accept.
type connLimit struct {
	places chan struct{}
}

func newConnLimit(places int) *connLimit {
	return &connLimit{places: make(chan struct{}, places)}
}

// open returns the number of places in use.
func (l *connLimit) open() int { return len(l.places) }

// openConnPlaces returns the number of places for a soft limit on open
// files: min(MaxOpenConns, limit/4), so that at least half of the
// descriptors stay for the databases and everything else (a connection may
// use a second one for a mail sent inside its request).
func openConnPlaces(limit uint64) int {
	places := uint64(modules.MaxOpenConns)
	if q := limit / 4; q < places {
		places = q
	}
	return int(max(places, 1))
}

// newProcessConnLimit reads the soft limit on open files once and returns
// the bound for the process. It logs one line when the limit lowers the
// bound.
func newProcessConnLimit(logger *log.Logger) *connLimit {
	var rl syscall.Rlimit
	if err := syscall.Getrlimit(syscall.RLIMIT_NOFILE, &rl); err != nil {
		logger.Printf("open file limit: %v; the central allows %d connections at the same time", err, modules.MaxOpenConns)
		return newConnLimit(modules.MaxOpenConns)
	}
	places := openConnPlaces(rl.Cur)
	if places < modules.MaxOpenConns {
		logger.Printf("open file limit %d allows %d connections at the same time, the central is built for %d: raise the limit (LimitNOFILE of the service, or ulimit -n)",
			rl.Cur, places, modules.MaxOpenConns)
	}
	return newConnLimit(places)
}

// limitListener accepts connections of one listening socket within the
// bound and wraps each in a modules.IdleConn with the idle time idle
// (modules.IdleTimeout for the central; tests use less).
type limitListener struct {
	ln        *net.TCPListener
	lim       *connLimit
	idle      time.Duration
	done      chan struct{}
	closeOnce sync.Once
}

// listener wraps ln: its connections count against the bound.
func (l *connLimit) listener(ln *net.TCPListener, idle time.Duration) net.Listener {
	return &limitListener{ln: ln, lim: l, idle: idle, done: make(chan struct{})}
}

// Accept waits for a free place, then accepts. It returns net.ErrClosed
// when the listener is closed while it waits for a place.
func (l *limitListener) Accept() (net.Conn, error) {
	select {
	case l.lim.places <- struct{}{}:
	case <-l.done:
		return nil, net.ErrClosed
	}
	tc, err := l.ln.AcceptTCP()
	if err != nil {
		<-l.lim.places
		return nil, err
	}
	c, err := modules.NewIdleConn(tc, l.idle, func() { <-l.lim.places })
	if err != nil {
		_ = tc.Close()
		<-l.lim.places
		return nil, err
	}
	return c, nil
}

// Close closes the listening socket; an Accept waiting for a place returns.
func (l *limitListener) Close() error {
	l.closeOnce.Do(func() { close(l.done) })
	return l.ln.Close()
}

// Addr returns the listening address.
func (l *limitListener) Addr() net.Addr { return l.ln.Addr() }
