package workers

import (
	"bufio"
	"bytes"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"runtime/debug"
	"sync"
	"sync/atomic"
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
}

// connServer accepts connections on several listeners from ONE flow and
// processes requests with at most `workers` flows. With the default of one
// worker, the accepting flow itself processes every request and periodic
// task in turn, so no goroutine is created at all. With more workers, the
// accepting flow only accepts and dispatches, and workers-many goroutines do
// the processing. Connections that cannot be handled within queueTimeout
// are rejected.
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
}

func newConnServer(specs []*listenerSpec, workers int, queueTimeout time.Duration, logger *log.Logger) *connServer {
	if workers < 1 {
		workers = 1
	}
	return &connServer{specs: specs, workers: workers, queueTimeout: queueTimeout, logger: logger}
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
			s.queue = append(s.queue, job{task: t, acceptedAt: time.Now()})
		}
		s.expireQueue()
		if len(s.queue) == 0 {
			continue
		}
		if s.workers == 1 {
			j := s.queue[0]
			s.queue = s.queue[1:]
			s.process(j)
			continue
		}
		for len(s.queue) > 0 {
			select {
			case s.jobCh <- s.queue[0]:
				s.queue = s.queue[1:]
				continue
			default:
			}
			break
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

func (s *connServer) workerLoop() {
	defer s.wg.Done()
	for j := range s.jobCh {
		s.process(j)
	}
}

// acceptAll polls every listener once. When work is already queued the poll
// does not wait; otherwise each listener is waited on for a short interval.
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
	for _, sp := range s.specs {
		wait := modules.AcceptPollInterval
		if len(s.queue) > 0 {
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
			if len(s.queue) >= modules.MaxPendingConns {
				s.reject(j)
				continue
			}
			s.queue = append(s.queue, j)
			wait = time.Millisecond
		}
	}
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
		_, _ = io.WriteString(j.conn, "HTTP/1.1 503 Service Unavailable\r\nConnection: close\r\nContent-Type: application/json\r\nContent-Length: 21\r\n\r\n{\"error\":\"busy\"}\r\n\r\n")
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
	_ = conn.SetDeadline(time.Now().Add(modules.ConnTotalTimeout))
	req.RemoteAddr = conn.RemoteAddr().String()
	req.TLS = state
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
	bw := bufio.NewWriter(c)
	if err := resp.Write(bw); err == nil {
		_ = bw.Flush()
	}
}

// listenTCP opens a TCP listener.
func listenTCP(addr string) (*net.TCPListener, error) {
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return nil, err
	}
	tl, ok := ln.(*net.TCPListener)
	if !ok {
		ln.Close()
		return nil, fmt.Errorf("%s: not a TCP listener", addr)
	}
	return tl, nil
}

// isLoopback reports whether the connection's source address (never a
// forwarding header) is a loopback address.
func isLoopback(remoteAddr string) bool {
	host, _, err := net.SplitHostPort(remoteAddr)
	if err != nil {
		return false
	}
	ip := net.ParseIP(host)
	return ip != nil && (ip.Equal(net.IPv4(127, 0, 0, 1)) || ip.Equal(net.IPv6loopback))
}
