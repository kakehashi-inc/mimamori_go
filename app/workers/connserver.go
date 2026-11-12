package workers

import (
	"bufio"
	"bytes"
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"runtime/debug"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"mimamori/app/modules"
)

// listenerSpec is one listening socket and how its connections are served.
type listenerSpec struct {
	name    string
	ln      *net.TCPListener
	tls     *tls.Config // nil for plain HTTP
	handler http.Handler
}

// job is a unit of work for a worker: a connection or a periodic task.
type job struct {
	conn       net.Conn
	spec       *listenerSpec
	acceptedAt time.Time
	task       func()
	// ready is set when the connection has sent what a worker needs to start
	// on it without waiting (probeConn). Only ready jobs are handed over.
	ready bool
	// probed is set once the connection was probed.
	probed bool
}

// connServer accepts connections on several listeners from ONE flow and
// processes requests with at most `workers` flows. With the default of one
// worker, the accepting flow itself processes every request and periodic
// task in turn, so no goroutine is created at all. With more workers, the
// accepting flow only accepts and dispatches, and workers-many goroutines do
// the processing. A connection is handed to a worker only once it has sent
// data (the accepting flow probes the queued connections without blocking),
// so a client that sends nothing never occupies a worker. Connections that
// cannot be handled within queueTimeout are rejected.
type connServer struct {
	specs        []*listenerSpec
	workers      int
	queueTimeout time.Duration
	logger       *log.Logger

	queue    []job
	jobCh    chan job
	lastPoll time.Time
	wg       sync.WaitGroup
	taskBusy atomic.Bool
	// probeBuf is the buffer of probeConn and lastProbe the time every
	// waiting connection was last probed (used by the accepting flow only).
	probeBuf  []byte
	lastProbe time.Time
}

func newConnServer(specs []*listenerSpec, workers int, queueTimeout time.Duration, logger *log.Logger) *connServer {
	if workers < 1 {
		workers = 1
	}
	return &connServer{specs: specs, workers: workers, queueTimeout: queueTimeout, logger: logger, probeBuf: make([]byte, modules.ProbeHeadBytes)}
}

// run is the main loop of the central flow. tick is called between jobs and
// returns a periodic task to run (or nil); signals are checked via stopping.
func (s *connServer) run(stopping func() bool, tick func() func()) {
	if s.workers > 1 {
		s.jobCh = make(chan job)
		for i := 0; i < s.workers; i++ {
			s.wg.Add(1)
			go s.workerLoop()
		}
	}
	for !stopping() {
		s.acceptAll()
		if t := tick(); t != nil && !s.taskBusy.Load() {
			s.taskBusy.Store(true)
			s.queue = append(s.queue, job{task: t, acceptedAt: time.Now(), ready: true})
		}
		s.expireQueue()
		s.probeQueue()
		if s.workers == 1 {
			if i := s.nextReady(0); i >= 0 {
				s.process(s.take(i))
			}
			continue
		}
		// Hand ready jobs, in queue order, to the workers that are free.
		for i := s.nextReady(0); i >= 0; i = s.nextReady(i) {
			sent := false
			select {
			case s.jobCh <- s.queue[i]:
				sent = true
			default:
			}
			if !sent {
				break
			}
			s.take(i)
		}
	}
	for _, j := range s.queue {
		if j.conn != nil {
			s.reject(j)
		}
	}
	s.queue = nil
	if s.jobCh != nil {
		close(s.jobCh)
		s.wg.Wait()
	}
	for _, sp := range s.specs {
		sp.ln.Close()
	}
}

// nextReady returns the index of the first ready job at or after from, or -1.
func (s *connServer) nextReady(from int) int {
	for i := from; i < len(s.queue); i++ {
		if s.queue[i].ready {
			return i
		}
	}
	return -1
}

// take removes the job at index i from the queue and returns it.
func (s *connServer) take(i int) job {
	j := s.queue[i]
	s.queue = append(s.queue[:i], s.queue[i+1:]...)
	return j
}

// probeQueue marks the queued connections that have sent data as ready and
// closes those whose client has gone. It never blocks. A new connection is
// probed at once; connections already probed are probed again every
// ProbeInterval, so that the loop does not spend its time on them while it
// waits for a free worker.
func (s *connServer) probeQueue() {
	all := time.Since(s.lastProbe) >= modules.ProbeInterval
	if all {
		s.lastProbe = time.Now()
	}
	kept := s.queue[:0]
	for _, j := range s.queue {
		if !j.ready && (all || !j.probed) {
			j.probed = true
			ready, gone := probeConn(j.conn, j.spec.tls == nil, s.probeBuf)
			if gone {
				_ = j.conn.Close()
				continue
			}
			j.ready = ready
		}
		kept = append(kept, j)
	}
	s.queue = kept
}

// probeConn looks at the data a client has sent without consuming it and
// without blocking (recv with MSG_PEEK|MSG_DONTWAIT). A TLS connection is
// ready when its first record (the ClientHello) has arrived completely; a
// plain HTTP connection when the complete request head has arrived. Data
// that fills buf is ready as well. gone is true when the client closed the
// connection without sending anything, or the connection failed.
func probeConn(c net.Conn, plain bool, buf []byte) (ready, gone bool) {
	sc, ok := c.(syscall.Conn)
	if !ok {
		return true, false
	}
	raw, err := sc.SyscallConn()
	if err != nil {
		return true, false
	}
	var n int
	var rerr error
	if err := raw.Read(func(fd uintptr) bool {
		n, _, rerr = syscall.Recvfrom(int(fd), buf, syscall.MSG_PEEK|syscall.MSG_DONTWAIT)
		return true // never wait for the descriptor
	}); err != nil {
		return false, true
	}
	switch {
	case errors.Is(rerr, syscall.EAGAIN) || errors.Is(rerr, syscall.EINTR):
		return false, false
	case rerr != nil, n <= 0:
		return false, true
	case n >= len(buf):
		return true, false
	case !plain:
		return tlsRecordComplete(buf[:n]), false
	}
	head := buf[:n]
	return bytes.Contains(head, []byte("\r\n\r\n")) || bytes.Contains(head, []byte("\n\n")), false
}

// tlsRecordComplete reports whether b holds the first TLS record
// completely (a 5-byte header whose last two bytes are the length of the
// rest). Data that is not a handshake record is complete at once: the
// handshake then fails without waiting.
func tlsRecordComplete(b []byte) bool {
	const recordTypeHandshake = 0x16
	if b[0] != recordTypeHandshake {
		return true
	}
	if len(b) < 5 {
		return false
	}
	return len(b) >= 5+(int(b[3])<<8|int(b[4]))
}

func (s *connServer) workerLoop() {
	defer s.wg.Done()
	for j := range s.jobCh {
		s.process(j)
	}
}

// acceptAll polls every listener once. When a queued job is ready (or a
// connection was just accepted and is still to be probed) the poll does not
// wait; otherwise each listener is waited on for a short interval, which is
// also the interval at which waiting connections are probed again.
//
// A connection accepted now may have been waiting in the kernel since the
// previous poll ended (for example while the single worker was processing a
// request), so its waiting time is counted from that moment.
func (s *connServer) acceptAll() {
	waitingSince := s.lastPoll
	if waitingSince.IsZero() {
		waitingSince = time.Now()
	}
	defer func() { s.lastPoll = time.Now() }()
	busy := s.nextReady(0) >= 0
	for _, sp := range s.specs {
		wait := modules.AcceptPollInterval
		if busy {
			wait = time.Millisecond
		}
		for {
			_ = sp.ln.SetDeadline(time.Now().Add(wait))
			c, err := sp.ln.Accept()
			if err != nil {
				var ne net.Error
				if errors.As(err, &ne) && ne.Timeout() {
					break
				}
				if !errors.Is(err, net.ErrClosed) {
					s.logger.Printf("%s: accept: %v", sp.name, err)
				}
				break
			}
			j := job{conn: c, spec: sp, acceptedAt: waitingSince}
			if len(s.queue) >= modules.MaxPendingConns && !s.evictWaiting() {
				s.reject(j)
				continue
			}
			s.queue = append(s.queue, j)
			busy = true
			wait = time.Millisecond
		}
	}
}

// evictWaiting makes room in a full queue by rejecting the oldest connection
// that has not sent anything usable yet, so that connections which send
// nothing cannot keep new clients out. It reports whether one was rejected.
func (s *connServer) evictWaiting() bool {
	for i, j := range s.queue {
		if j.conn != nil && !j.ready {
			s.reject(s.take(i))
			return true
		}
	}
	return false
}

// expireQueue rejects connections that waited longer than queueTimeout.
func (s *connServer) expireQueue() {
	if len(s.queue) == 0 {
		return
	}
	kept := s.queue[:0]
	for _, j := range s.queue {
		if j.conn != nil && time.Since(j.acceptedAt) > s.queueTimeout {
			s.reject(j)
			continue
		}
		kept = append(kept, j)
	}
	s.queue = kept
}

// reject closes a connection that could not be served. A plain HTTP client
// gets a 503; a TLS client is simply disconnected.
func (s *connServer) reject(j job) {
	if j.spec.tls == nil {
		_ = j.conn.SetDeadline(time.Now().Add(time.Second))
		_, _ = io.WriteString(j.conn, "HTTP/1.1 503 Service Unavailable\r\nConnection: close\r\nContent-Type: application/json\r\nContent-Length: 16\r\n\r\n{\"error\":\"busy\"}")
	}
	_ = j.conn.Close()
}

func (s *connServer) process(j job) {
	if j.task != nil {
		defer s.taskBusy.Store(false)
		defer func() {
			if r := recover(); r != nil {
				s.logger.Printf("periodic task panic: %v\n%s", r, debug.Stack())
			}
		}()
		j.task()
		return
	}
	s.serveConn(j.conn, j.spec)
}

// respWriter buffers a response so it can be written with a Content-Length.
type respWriter struct {
	header http.Header
	status int
	buf    bytes.Buffer
}

func (w *respWriter) Header() http.Header { return w.header }
func (w *respWriter) WriteHeader(code int) {
	if w.status == 0 {
		w.status = code
	}
}
func (w *respWriter) Write(b []byte) (int, error) {
	if w.status == 0 {
		w.status = http.StatusOK
	}
	return w.buf.Write(b)
}

// serveConn handles exactly one request on a connection, in the calling flow.
// The connection has already sent data (probeConn), so the TLS handshake and
// the request head have ConnHeaderTimeout only. The routes that read a body
// before the caller is authenticated (the login routes of the browser API)
// read it within ConnBodyTimeout (readBodyBeforeAuth); every other route
// that reads a body authenticates the request first. The response is
// written within the bound described at ConnWriteTimeout.
func (s *connServer) serveConn(conn net.Conn, sp *listenerSpec) {
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(modules.ConnHeaderTimeout))
	c := conn
	var state *tls.ConnectionState
	if sp.tls != nil {
		tc := tls.Server(conn, sp.tls)
		if err := tc.Handshake(); err != nil {
			return
		}
		st := tc.ConnectionState()
		state = &st
		c = tc
	}
	br := bufio.NewReader(c)
	req, err := http.ReadRequest(br)
	if err != nil {
		if !errors.Is(err, io.EOF) {
			_, _ = io.WriteString(c, "HTTP/1.1 400 Bad Request\r\nConnection: close\r\nContent-Length: 0\r\n\r\n")
		}
		return
	}
	total := time.Now().Add(modules.ConnTotalTimeout)
	_ = conn.SetDeadline(total)
	req.RemoteAddr = conn.RemoteAddr().String()
	req.TLS = state
	// The local address of the connection (never a header) tells the
	// management API whether a client of this host connected to one of the
	// host's own addresses.
	ctx := context.WithValue(req.Context(), http.LocalAddrContextKey, conn.LocalAddr())
	ctx = context.WithValue(ctx, connInfoKey{}, &connInfo{conn: conn, total: total})
	req = req.WithContext(ctx)
	w := &respWriter{header: http.Header{}}
	req.Body = http.MaxBytesReader(w, req.Body, modules.MaxRequestBody)
	func() {
		defer func() {
			if r := recover(); r != nil {
				s.logger.Printf("%s %s: panic: %v\n%s", req.Method, req.URL.Path, r, debug.Stack())
				w.header = http.Header{"Content-Type": {"application/json"}}
				w.status = http.StatusInternalServerError
				w.buf.Reset()
				w.buf.WriteString(`{"error":"internal error"}`)
			}
		}()
		sp.handler.ServeHTTP(w, req)
	}()
	if w.status == 0 {
		w.status = http.StatusOK
	}
	w.header.Set("Connection", "close")
	w.header.Set("X-Content-Type-Options", "nosniff")
	body := w.buf.Bytes()
	if req.Method == http.MethodHead {
		body = nil
	}
	resp := &http.Response{
		StatusCode:    w.status,
		ProtoMajor:    1,
		ProtoMinor:    1,
		Header:        w.header,
		Body:          io.NopCloser(bytes.NewReader(body)),
		ContentLength: int64(len(body)),
		Close:         true,
		Request:       req,
	}
	// A response is written within ConnWriteTimeout plus its size at
	// MinWriteRate (never longer than the whole request may take), so a
	// client that does not read cannot hold the worker much longer than a
	// client reading at that rate needs.
	writeDeadline := time.Now().Add(modules.ConnWriteTimeout + time.Duration(len(body))*time.Second/modules.MinWriteRate)
	if writeDeadline.After(total) {
		writeDeadline = total
	}
	_ = conn.SetDeadline(writeDeadline)
	bw := bufio.NewWriter(c)
	if err := resp.Write(bw); err == nil {
		_ = bw.Flush()
	}
}

// connInfo is what serveConn passes to the handlers about the connection.
type connInfo struct {
	conn  net.Conn
	total time.Time // the deadline of the whole request
}

type connInfoKey struct{}

// errReader returns its error on every read.
type errReader struct{ err error }

func (e errReader) Read([]byte) (int, error) { return 0, e.err }

// readBodyBeforeAuth reads the whole body of a request that is served
// before its caller is authenticated (the login routes) within
// ConnBodyTimeout, so that a client sending its body slowly cannot hold a
// worker for longer. It reports false when the time ran out (the caller
// answers 408). Any other read error is passed on to the handler with the
// data read before it, as if the body had been read directly.
func readBodyBeforeAuth(r *http.Request) bool {
	ci, _ := r.Context().Value(connInfoKey{}).(*connInfo)
	if ci == nil {
		return true
	}
	limit := time.Now().Add(modules.ConnBodyTimeout)
	if limit.After(ci.total) {
		limit = ci.total
	}
	_ = ci.conn.SetReadDeadline(limit)
	data, err := io.ReadAll(r.Body)
	_ = ci.conn.SetReadDeadline(ci.total)
	if err != nil {
		var ne net.Error
		if errors.As(err, &ne) && ne.Timeout() {
			return false
		}
		r.Body = io.NopCloser(io.MultiReader(bytes.NewReader(data), errReader{err}))
		return true
	}
	r.Body = io.NopCloser(bytes.NewReader(data))
	return true
}

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

// isLocalPeer reports whether a connection comes from this host: its source
// address (never a forwarding header) is a loopback address, or equals the
// local address the connection was accepted on (a connection from this host
// to one of its own addresses). local is the local address of the accepted
// connection (nil when unknown: then only loopback counts).
func isLocalPeer(remoteAddr string, local net.Addr) bool {
	host, _, err := net.SplitHostPort(remoteAddr)
	if err != nil {
		return false
	}
	zone := ""
	if i := strings.IndexByte(host, '%'); i >= 0 {
		host, zone = host[:i], host[i+1:] // the zone of a link-local address
	}
	ip := net.ParseIP(host)
	if ip == nil {
		return false
	}
	if ip.IsLoopback() {
		return true
	}
	ta, ok := local.(*net.TCPAddr)
	return ok && ta != nil && !ta.IP.IsUnspecified() && ta.IP.Equal(ip) && (zone == "" || ta.Zone == "" || zone == ta.Zone)
}
