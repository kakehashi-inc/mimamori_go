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
	"syscall"
	"time"
	"unsafe"

	"mimamori/app/modules"
)

// listenerSpec is one listening socket and how its connections are served.
type listenerSpec struct {
	name    string
	ln      *net.TCPListener
	tls     *tls.Config // nil for plain HTTP
	handler http.Handler
	// fd is the descriptor of ln (for ppoll) and pausedUntil the end of a
	// short pause after an accept error (used by the accepting flow only).
	fd          int
	pausedUntil time.Time
}

// connServer accepts connections on several listeners from ONE flow (the
// accepting flow) and runs request handlers with at most `workers` flows.
// With the default of one worker, the accepting flow runs every handler and
// periodic task itself, so no goroutine is created at all. With more
// workers, workers-many goroutines run handlers and periodic tasks only.
//
// Every socket operation happens in the accepting flow and never waits: one
// ppoll per pass reports which listeners and connections are ready, and
// requests are read into memory and responses written from memory as the
// sockets allow. A handler gets a request only when it is complete in
// memory, and its response is queued on the connection. The only wait on a
// peer is the TLS handshake (TLSHandshakeTimeout; a source whose handshake
// runs into it is refused for HandshakePenalty). See conn for the states.
type connServer struct {
	specs        []*listenerSpec
	workers      int
	queueTimeout time.Duration
	logger       *log.Logger
	// exempt reports whether a peer is never penalised (isLocalPeer; tests
	// replace it).
	exempt func(remoteAddr string, local net.Addr) bool

	conns []*conn // open connections, in the order they were accepted
	ready []job   // complete requests and periodic tasks waiting for a worker
	// busy is the number of jobs handed to worker goroutines whose result
	// has not been collected yet (always 0 with one worker).
	busy     int
	taskBusy bool
	// bufferedTotal is the number of bytes all connections buffer
	// (recomputed every pass, increased by reads).
	bufferedTotal int
	penalty       map[string]time.Time // source address -> refused until

	// waitingSince is the earliest time a connection accepted in this pass
	// may have been waiting in the kernel; lastReturn is when the previous
	// ppoll returned.
	waitingSince time.Time
	lastReturn   time.Time

	pollFds    []pollFd
	pollOwners []pollOwner
	scratch    []byte

	// workers > 1 only: jobs go out on jobCh, results come back on doneCh
	// and a byte on the wake pipe makes ppoll return.
	jobCh  chan job
	doneCh chan jobResult
	wakeR  int
	wakeW  int
	wg     sync.WaitGroup
}

// pollOwner says what one entry of the poll set belongs to: a connection,
// a listener, or (both nil) the wake pipe.
type pollOwner struct {
	c  *conn
	sp *listenerSpec
}

// job is a unit of work for a worker: a complete request or a periodic task.
type job struct {
	conn    *conn
	req     *http.Request
	handler http.Handler
	task    func()
}

// jobResult is what a worker returns: the serialised response of a request
// (conn is nil for a periodic task).
type jobResult struct {
	conn *conn
	resp []byte
}

func newConnServer(specs []*listenerSpec, workers int, queueTimeout time.Duration, logger *log.Logger) *connServer {
	if workers < 1 {
		workers = 1
	}
	return &connServer{
		specs: specs, workers: workers, queueTimeout: queueTimeout, logger: logger,
		exempt: isLocalPeer, penalty: map[string]time.Time{},
		scratch: make([]byte, 16<<10), wakeR: -1, wakeW: -1,
	}
}

// connState is where a connection is in its life (one request per
// connection).
type connState int

const (
	// stateHello (TLS only): reading the first record (the ClientHello).
	// When it is complete, the handshake runs (blocking, bounded).
	stateHello connState = iota
	// stateRequest: reading the request (the head, then Content-Length
	// bytes of body). For TLS, raw bytes are decrypted as they arrive.
	stateRequest
	// stateQueued: the request is complete and waits for a worker.
	stateQueued
	// stateWorking: a worker runs the handler.
	stateWorking
	// stateWriting: the response is written as the socket accepts it.
	stateWriting
	stateClosed
)

// conn is one accepted connection. Only the accepting flow uses it; a
// worker gets the parsed request (with its body in memory) and gives back
// the response bytes.
type conn struct {
	nc    net.Conn
	raw   syscall.RawConn
	fd    int
	spec  *listenerSpec
	state connState
	// deadline is when the current state runs out: the connection is then
	// rejected (reading, queued) or closed (writing).
	deadline time.Time
	// queueDeadline is when a request that is not complete, or waits for a
	// worker, is rejected (accept time plus the queue timeout, moved by the
	// time a large body took to arrive).
	queueDeadline time.Time
	headDoneAt    time.Time
	total         time.Time // headDoneAt + ConnTotalTimeout

	buf      []byte // request bytes received so far (plaintext for TLS)
	scanned  int    // buf[:scanned] holds no end of the head
	headLen  int    // length of the head with its blank line; 0 until parsed
	bodyLen  int    // body bytes to wait for (Content-Length)
	tooLarge bool   // Content-Length above MaxRequestBody: no body is read
	request  *http.Request

	tlsConn *tls.Conn
	tlsIO   *tlsBuffer // nil for plain HTTP

	out []byte // response bytes not yet written to the socket
}

// buffered returns the number of bytes the connection holds in memory.
func (c *conn) buffered() int {
	n := len(c.buf) + len(c.out)
	if c.tlsIO != nil {
		n += len(c.tlsIO.in) + len(c.tlsIO.out)
	}
	return n
}

// want returns the number of request bytes needed: the head (up to
// MaxHeadBytes, plus one byte to notice a longer head) until it is parsed,
// then the head and the body.
func (c *conn) want() int {
	if c.headLen == 0 {
		return modules.MaxHeadBytes + 1
	}
	return c.headLen + c.bodyLen
}

// wantMore returns how many bytes to read from the socket next: for plain
// HTTP the missing request bytes, for TLS the raw bytes that carry them
// (with room for the record overhead), at most one read chunk.
func (c *conn) wantMore() int {
	n := c.want() - len(c.buf)
	if c.tlsIO != nil {
		n += tlsRecordSlack
	}
	return min(n, readChunk)
}

const (
	// readChunk is the most a connection reads from its socket at once
	// before its request is looked at again.
	readChunk = 64 << 10
	// tlsRecordSlack covers the header and tag of the records that carry
	// the missing request bytes.
	tlsRecordSlack = 1 << 10
	// acceptGuard bounds Accept on a listener that ppoll reported ready, in
	// case the connection vanished in between.
	acceptGuard = 10 * time.Millisecond
	// acceptPause is how long a listener is left out of the poll set after
	// an accept error (for example too many open files), so that the error
	// does not make the loop spin.
	acceptPause = 100 * time.Millisecond
)

// Answers the connection server writes itself.
var (
	busyResponse           = []byte("HTTP/1.1 503 Service Unavailable\r\nConnection: close\r\nContent-Type: application/json\r\nContent-Length: 16\r\n\r\n{\"error\":\"busy\"}")
	badRequestResponse     = []byte("HTTP/1.1 400 Bad Request\r\nConnection: close\r\nContent-Length: 0\r\n\r\n")
	headTooLargeResponse   = errorResponse(http.StatusRequestHeaderFieldsTooLarge, "request header too large")
	lengthRequiredResponse = errorResponse(http.StatusLengthRequired, "length required")
)

func errorResponse(status int, msg string) []byte {
	body := `{"error":"` + msg + `"}`
	return []byte(fmt.Sprintf("HTTP/1.1 %d %s\r\nConnection: close\r\nContent-Type: application/json\r\nX-Content-Type-Options: nosniff\r\nContent-Length: %d\r\n\r\n%s",
		status, http.StatusText(status), len(body), body))
}

// run is the main loop of the accepting flow. tick is called once per pass
// and returns a periodic task to run (or nil); stopping reports the stop
// signal. On stop, connections whose request is not in work are rejected,
// the requests in work are finished and their responses written (at most
// StopFlushTimeout), then everything is closed.
func (s *connServer) run(stopping func() bool, tick func() func()) {
	s.start()
	var stopAt time.Time
	for {
		now := time.Now()
		if stopAt.IsZero() && stopping() {
			stopAt = now.Add(modules.StopFlushTimeout)
			s.beginStop()
		}
		if !stopAt.IsZero() && (s.idle() || now.After(stopAt)) {
			break
		}
		s.pass(tick, !stopAt.IsZero())
	}
	s.shutdown()
}

// start prepares the listeners' descriptors and, with more than one worker,
// the wake pipe and the worker goroutines.
func (s *connServer) start() {
	for _, sp := range s.specs {
		sp.fd = -1
		if raw, err := sp.ln.SyscallConn(); err == nil {
			_ = raw.Control(func(fd uintptr) { sp.fd = int(fd) })
		}
	}
	if s.workers == 1 {
		return
	}
	var p [2]int
	if err := syscall.Pipe2(p[:], syscall.O_NONBLOCK|syscall.O_CLOEXEC); err != nil {
		// Without the pipe, finished jobs are collected at the next pass
		// (pollTimeout keeps the passes short while jobs are in work).
		s.logger.Printf("wake pipe: %v", err)
	} else {
		s.wakeR, s.wakeW = p[0], p[1]
	}
	s.jobCh = make(chan job)
	s.doneCh = make(chan jobResult, s.workers)
	for i := 0; i < s.workers; i++ {
		s.wg.Add(1)
		go s.workerLoop()
	}
}

func (s *connServer) workerLoop() {
	defer s.wg.Done()
	for j := range s.jobCh {
		s.doneCh <- s.runJob(j)
		if s.wakeW >= 0 {
			_, _ = syscall.Write(s.wakeW, []byte{1})
		}
	}
}

// pass is one round of the accepting flow: wait for readiness, then do the
// I/O that is possible without waiting, expire deadlines and start jobs.
func (s *connServer) pass(tick func() func(), stopping bool) {
	s.bufferedTotal = 0
	for _, c := range s.conns {
		s.bufferedTotal += c.buffered()
	}
	s.buildPollSet(stopping, time.Now())
	s.wait(s.pollTimeout(time.Now()))
	now := time.Now()
	s.collect()
	for i, o := range s.pollOwners {
		if s.pollFds[i].revents == 0 {
			continue
		}
		switch {
		case o.c != nil:
			s.onReady(o.c)
		case o.sp != nil:
			s.accept(o.sp, now)
		}
	}
	s.expire(time.Now())
	if !stopping {
		if t := tick(); t != nil && !s.taskBusy {
			s.taskBusy = true
			s.ready = append(s.ready, job{task: t})
		}
	}
	s.dispatch()
	s.compact()
}

// buildPollSet lists what the next ppoll waits for: the wake pipe, the
// listeners (not while stopping), connections reading (unless the memory
// bound holds them back) and connections writing.
func (s *connServer) buildPollSet(stopping bool, now time.Time) {
	s.pollFds = s.pollFds[:0]
	s.pollOwners = s.pollOwners[:0]
	add := func(fd int, events int16, o pollOwner) {
		s.pollFds = append(s.pollFds, pollFd{fd: int32(fd), events: events})
		s.pollOwners = append(s.pollOwners, o)
	}
	if s.wakeR >= 0 {
		add(s.wakeR, pollIn, pollOwner{})
	}
	if !stopping {
		for _, sp := range s.specs {
			if sp.fd >= 0 && !now.Before(sp.pausedUntil) {
				add(sp.fd, pollIn, pollOwner{sp: sp})
			}
		}
	}
	for _, c := range s.conns {
		switch {
		case c.state == stateHello,
			c.state == stateRequest && s.room(c, c.wantMore()) > 0:
			add(c.fd, pollIn, pollOwner{c: c})
		case c.state == stateWriting:
			add(c.fd, pollOut, pollOwner{c: c})
		}
	}
}

// pollTimeout is 0 when a job can be started now; otherwise the time until
// the earliest deadline, at most LoopWakeInterval (so that the stop signal
// and the periodic tasks are looked at regularly).
func (s *connServer) pollTimeout(now time.Time) time.Duration {
	if len(s.ready) > 0 && s.busy < s.workers {
		return 0
	}
	d := modules.LoopWakeInterval
	if s.busy > 0 && s.wakeR < 0 {
		d = 10 * time.Millisecond
	}
	for _, c := range s.conns {
		if c.state == stateWorking || c.state == stateClosed {
			continue
		}
		if until := c.deadline.Sub(now) + time.Millisecond; until < d {
			d = until
		}
	}
	return max(d, 0)
}

// wait runs ppoll and records since when a connection accepted in this
// pass may have been waiting: if ppoll waited, nothing was pending when it
// began; otherwise the connection may have arrived while the previous pass
// was busy (for example running a handler), so its wait counts from the
// end of the previous ppoll.
func (s *connServer) wait(timeout time.Duration) {
	entered := time.Now()
	if len(s.pollFds) == 0 {
		time.Sleep(timeout)
	} else if err := ppoll(s.pollFds, timeout); err != nil {
		s.logger.Printf("ppoll: %v", err)
		time.Sleep(10 * time.Millisecond)
	}
	returned := time.Now()
	if returned.Sub(entered) > time.Millisecond || s.lastReturn.IsZero() {
		s.waitingSince = entered
	} else {
		s.waitingSince = s.lastReturn
	}
	s.lastReturn = returned
}

// collect empties the wake pipe and takes the results of finished jobs.
func (s *connServer) collect() {
	if s.wakeR >= 0 {
		for {
			n, err := syscall.Read(s.wakeR, s.scratch)
			if n <= 0 || err != nil {
				break
			}
		}
	}
	for s.busy > 0 {
		select {
		case r := <-s.doneCh:
			s.busy--
			s.finish(r)
		default:
			return
		}
	}
}

// accept takes one connection from a listener that ppoll reported ready.
func (s *connServer) accept(sp *listenerSpec, now time.Time) {
	_ = sp.ln.SetDeadline(now.Add(acceptGuard))
	nc, err := sp.ln.Accept()
	if err != nil {
		var ne net.Error
		if errors.As(err, &ne) && ne.Timeout() {
			return
		}
		if !errors.Is(err, net.ErrClosed) {
			s.logger.Printf("%s: accept: %v", sp.name, err)
		}
		sp.pausedUntil = now.Add(acceptPause)
		return
	}
	if sp.tls != nil && s.penalised(nc, now) {
		_ = nc.Close()
		return
	}
	c := &conn{nc: nc, spec: sp, fd: -1}
	if sc, ok := nc.(syscall.Conn); ok {
		if raw, err := sc.SyscallConn(); err == nil {
			c.raw = raw
			_ = raw.Control(func(fd uintptr) { c.fd = int(fd) })
		}
	}
	if c.fd < 0 {
		_ = nc.Close()
		return
	}
	c.queueDeadline = s.waitingSince.Add(s.queueTimeout)
	c.deadline = c.queueDeadline
	c.state = stateRequest
	if sp.tls != nil {
		c.tlsIO = &tlsBuffer{Conn: nc}
		c.tlsConn = tls.Server(c.tlsIO, sp.tls)
		c.state = stateHello
	}
	if len(s.conns) >= modules.MaxPendingConns {
		s.compact()
	}
	if len(s.conns) >= modules.MaxPendingConns && !s.evictWaiting() {
		s.reject(c)
		return
	}
	s.conns = append(s.conns, c)
	// The request often follows the connection at once.
	s.onReady(c)
}

// evictWaiting makes room in a full table by rejecting the oldest
// connection that has not delivered a complete request head, so that
// connections which send nothing cannot keep new clients out. When there is
// none, the oldest connection that waits for its peer otherwise (a large
// body that has not arrived, or a response the client does not read) is
// dropped, so that such connections cannot keep new clients out either. It
// reports whether one was dropped.
func (s *connServer) evictWaiting() bool {
	for _, c := range s.conns {
		if c.state == stateHello || (c.state == stateRequest && c.headLen == 0) {
			s.reject(c)
			s.compact()
			return true
		}
	}
	for _, c := range s.conns {
		switch c.state {
		case stateRequest:
			s.reject(c)
		case stateWriting:
			s.closeConn(c) // part of the response may be written already
		default:
			continue
		}
		s.compact()
		return true
	}
	return false
}

// onReady does the I/O a ready connection allows in its state.
func (s *connServer) onReady(c *conn) {
	switch c.state {
	case stateHello:
		s.readHello(c)
	case stateRequest:
		s.readRequest(c)
	case stateWriting:
		s.flush(c)
	}
}

// readHello buffers the first TLS record and runs the handshake once it is
// complete.
func (s *connServer) readHello(c *conn) {
	for {
		want := helloLength(c.tlsIO.in)
		if len(c.tlsIO.in) >= want {
			break
		}
		n, eof, err := c.readSocket(&c.tlsIO.in, want, want)
		if err != nil || eof {
			s.closeConn(c)
			return
		}
		if n == 0 {
			return
		}
	}
	if time.Now().After(c.deadline) {
		s.reject(c) // waited too long already (behind a busy flow)
		return
	}
	s.handshake(c)
}

// helloLength returns the length of the first TLS record that b starts:
// the 5-byte header and the length it states. Data that is not a handshake
// record is complete at once (the handshake then fails without waiting).
func helloLength(b []byte) int {
	const recordTypeHandshake = 0x16
	switch {
	case len(b) > 0 && b[0] != recordTypeHandshake:
		return len(b)
	case len(b) < 5:
		return 5
	}
	return 5 + (int(b[3])<<8 | int(b[4]))
}

// handshake runs the TLS handshake. The client's first record is buffered,
// so only its next flight is waited for, at most TLSHandshakeTimeout. A
// source whose handshake runs into the limit is penalised.
func (s *connServer) handshake(c *conn) {
	_ = c.nc.SetDeadline(time.Now().Add(modules.TLSHandshakeTimeout))
	c.tlsIO.blocking = true
	err := c.tlsConn.Handshake()
	c.tlsIO.blocking = false
	_ = c.nc.SetDeadline(time.Time{})
	if err != nil {
		var ne net.Error
		if errors.As(err, &ne) && ne.Timeout() {
			s.penalise(c.nc, time.Now())
		}
		s.closeConn(c)
		return
	}
	c.state = stateRequest
	// The request may have arrived with the client's last handshake record.
	s.readRequest(c)
}

// penalise refuses the source of a connection whose handshake ran into the
// limit for HandshakePenalty (never this host itself). Each entry costs
// the accepting flow TLSHandshakeTimeout, so the list stays short; expired
// entries are dropped here and when they are looked up.
func (s *connServer) penalise(nc net.Conn, now time.Time) {
	if s.exempt(nc.RemoteAddr().String(), nc.LocalAddr()) {
		return
	}
	for k, until := range s.penalty {
		if !now.Before(until) {
			delete(s.penalty, k)
		}
	}
	host := remoteHost(nc.RemoteAddr().String())
	s.penalty[host] = now.Add(modules.HandshakePenalty)
	s.logger.Printf("TLS handshake of %s did not finish within %v; its connections are refused for %v",
		host, modules.TLSHandshakeTimeout, modules.HandshakePenalty)
}

// penalised reports whether the source of a new TLS connection is refused.
func (s *connServer) penalised(nc net.Conn, now time.Time) bool {
	host := remoteHost(nc.RemoteAddr().String())
	until, ok := s.penalty[host]
	if !ok {
		return false
	}
	if now.Before(until) {
		return true
	}
	delete(s.penalty, host)
	return false
}

// readRequest moves the request bytes the socket has into memory and
// queues the request once it is complete.
func (s *connServer) readRequest(c *conn) {
	for {
		var n int
		var eof bool
		var err error
		if c.tlsIO == nil {
			n, eof, err = c.readSocket(&c.buf, len(c.buf)+s.room(c, c.wantMore()), c.want())
			s.bufferedTotal += n
		} else {
			n, eof, err = s.receiveTLS(c)
		}
		if err != nil {
			if c.tlsIO != nil {
				// crypto/tls queued an alert (for example unexpected
				// message on a renegotiation attempt): send it if the
				// socket takes it now.
				c.out, c.tlsIO.out = c.tlsIO.out, nil
				_, _ = c.writeOut()
			}
			s.closeConn(c)
			return
		}
		complete, resp := c.progress(time.Now())
		switch {
		case resp != nil:
			s.fail(c, resp)
			return
		case complete:
			s.enqueue(c)
			return
		case eof && c.tlsIO == nil && len(c.buf) > 0:
			s.fail(c, badRequestResponse) // the client stopped in the middle
			return
		case eof:
			s.closeConn(c)
			return
		case n == 0:
			return // nothing more now, or held back by the memory bound
		}
	}
}

// receiveTLS decrypts what is buffered, reads raw bytes when more are
// needed (within the memory bound) and decrypts them. n is the number of
// plaintext bytes added; eof is set when the client closed the connection.
func (s *connServer) receiveTLS(c *conn) (n int, eof bool, err error) {
	before := len(c.buf)
	eof, err = c.decrypt()
	if eof || err != nil || len(c.buf) >= c.want() {
		return len(c.buf) - before, eof, err
	}
	limit := len(c.tlsIO.in) + s.room(c, c.wantMore())
	raw, rawEOF, err := c.readSocket(&c.tlsIO.in, limit, limit)
	s.bufferedTotal += raw
	if err != nil {
		return len(c.buf) - before, false, err
	}
	if raw > 0 {
		eof, err = c.decrypt()
	}
	return len(c.buf) - before, eof || rawEOF, err
}

// decrypt moves the plaintext crypto/tls can produce from the buffered raw
// bytes into the request buffer, up to the bytes the request needs.
func (c *conn) decrypt() (eof bool, err error) {
	for len(c.buf) < c.want() {
		c.buf = grow(c.buf, min(c.want()-len(c.buf), readChunk), c.want())
		n, err := c.tlsConn.Read(c.buf[len(c.buf):cap(c.buf)])
		c.buf = c.buf[:len(c.buf)+n]
		switch {
		case errors.Is(err, errWouldBlock):
			return false, nil
		case errors.Is(err, io.EOF):
			return true, nil
		case err != nil:
			return false, err
		}
	}
	return false, nil
}

// room returns how many more bytes a connection may buffer now, up to
// want: every connection may hold MaxHeadBytes; beyond that it may read
// only while all connections together buffer less than MaxBufferedBytes.
func (s *connServer) room(c *conn, want int) int {
	have := c.buffered()
	free := max(modules.MaxBufferedBytes-s.bufferedTotal, modules.MaxHeadBytes-have)
	return max(0, min(want, free))
}

// progress parses the head once it is complete and reports whether the
// whole request has arrived. A request that cannot be served gets resp.
func (c *conn) progress(now time.Time) (complete bool, resp []byte) {
	if c.headLen == 0 {
		end := headEnd(c.buf, c.scanned)
		if end < 0 || end > modules.MaxHeadBytes {
			if len(c.buf) > modules.MaxHeadBytes {
				return false, headTooLargeResponse
			}
			c.scanned = max(0, len(c.buf)-2)
			return false, nil
		}
		req, err := http.ReadRequest(bufio.NewReader(bytes.NewReader(c.buf[:end])))
		if err != nil {
			return false, badRequestResponse
		}
		if len(req.TransferEncoding) > 0 {
			return false, lengthRequiredResponse
		}
		c.request, c.headLen, c.headDoneAt = req, end, now
		c.total = now.Add(modules.ConnTotalTimeout)
		switch {
		case req.ContentLength > modules.MaxRequestBody:
			c.tooLarge = true
		case req.ContentLength > 0:
			c.bodyLen = int(req.ContentLength)
		}
		if c.bodyLen > modules.MaxHeadBytes {
			// A large body (an agent report) may take until the total limit.
			c.deadline = c.total
		}
	}
	return len(c.buf) >= c.headLen+c.bodyLen, nil
}

// headEnd returns the length of the request head in b, up to and including
// the blank line that ends it ("\r\n\r\n" or "\n\n"), or -1. b[:from] is
// known to hold no end.
func headEnd(b []byte, from int) int {
	for i := from; i < len(b); i++ {
		if b[i] != '\n' {
			continue
		}
		if i+1 < len(b) && b[i+1] == '\n' {
			return i + 2
		}
		if i+2 < len(b) && b[i+1] == '\r' && b[i+2] == '\n' {
			return i + 3
		}
	}
	return -1
}

// enqueue puts a complete request in line for a worker. The time a large
// body took to arrive does not count against the queue timeout.
func (s *connServer) enqueue(c *conn) {
	if c.bodyLen > modules.MaxHeadBytes {
		c.queueDeadline = c.queueDeadline.Add(time.Since(c.headDoneAt))
	}
	c.state = stateQueued
	c.deadline = c.queueDeadline
	s.ready = append(s.ready, job{conn: c})
}

// dispatch starts queued jobs in order: with one worker, one job in this
// flow (the next pass polls without waiting, so the I/O of the other
// connections goes on between jobs); with more, one per free worker.
func (s *connServer) dispatch() {
	for len(s.ready) > 0 && s.busy < s.workers {
		j := s.ready[0]
		s.ready = s.ready[1:]
		if j.task == nil {
			c := j.conn
			if c.state != stateQueued {
				continue // rejected while it waited
			}
			j = s.jobFor(c)
			c.state = stateWorking
		}
		if s.workers == 1 {
			s.finish(s.runJob(j))
			return
		}
		s.busy++
		s.jobCh <- j // a worker is free, so it takes the job at once
	}
	if len(s.ready) == 0 {
		s.ready = nil
	}
}

// jobFor builds the request a worker gets: the parsed head, the body from
// memory and what the handlers learn about the connection.
func (s *connServer) jobFor(c *conn) job {
	req := c.request
	switch {
	case c.tooLarge:
		req.Body = io.NopCloser(errReader{&http.MaxBytesError{Limit: modules.MaxRequestBody}})
	case c.bodyLen == 0:
		req.Body = http.NoBody
	default:
		req.Body = io.NopCloser(bytes.NewReader(c.buf[c.headLen : c.headLen+c.bodyLen]))
	}
	req.RemoteAddr = c.nc.RemoteAddr().String()
	if c.tlsConn != nil {
		st := c.tlsConn.ConnectionState()
		req.TLS = &st
	}
	// The local address of the connection (never a header) tells the
	// management API whether a client of this host connected to one of the
	// host's own addresses.
	req = req.WithContext(context.WithValue(req.Context(), http.LocalAddrContextKey, c.nc.LocalAddr()))
	return job{conn: c, req: req, handler: c.spec.handler}
}

// runJob runs a periodic task or a handler. It touches no connection, so
// worker goroutines can run it.
func (s *connServer) runJob(j job) jobResult {
	if j.task == nil {
		return jobResult{conn: j.conn, resp: s.serve(j.handler, j.req)}
	}
	defer func() {
		if r := recover(); r != nil {
			s.logger.Printf("periodic task panic: %v\n%s", r, debug.Stack())
		}
	}()
	j.task()
	return jobResult{}
}

// finish takes the result of a job: a periodic task is done; a response is
// queued on its connection and written as far as the socket takes it.
func (s *connServer) finish(r jobResult) {
	if r.conn == nil {
		s.taskBusy = false
		return
	}
	c := r.conn
	if c.state != stateWorking {
		return // closed while the handler ran (stop)
	}
	c.out = s.encode(c, r.resp)
	c.state = stateWriting
	c.deadline = c.total
	s.flush(c)
}

// encode returns the bytes to send for resp: resp itself, or for TLS its
// encryption (crypto/tls writes into the connection's outbound buffer).
func (s *connServer) encode(c *conn, resp []byte) []byte {
	if c.tlsConn == nil {
		return resp
	}
	if _, err := c.tlsConn.Write(resp); err != nil {
		return nil
	}
	out := c.tlsIO.out
	c.tlsIO.out = nil
	return out
}

// flush writes the pending response bytes as far as the socket takes them
// and closes the connection when all are written or the socket failed.
func (s *connServer) flush(c *conn) {
	if done, err := c.writeOut(); done || err != nil {
		s.closeConn(c)
	}
}

// expire rejects connections whose request did not arrive, or did not get
// a worker, in time, and closes those whose response could not be written
// within the total limit.
func (s *connServer) expire(now time.Time) {
	for _, c := range s.conns {
		if !now.After(c.deadline) {
			continue
		}
		switch c.state {
		case stateHello, stateRequest, stateQueued:
			s.reject(c)
		case stateWriting:
			s.closeConn(c)
		}
	}
}

// compact drops closed connections from the table.
func (s *connServer) compact() {
	kept := s.conns[:0]
	for _, c := range s.conns {
		if c.state != stateClosed {
			kept = append(kept, c)
		}
	}
	clear(s.conns[len(kept):])
	s.conns = kept
}

// reject closes a connection that could not be served. A plain HTTP client
// gets a 503 (written without waiting; it fits the socket's buffer); a TLS
// client is simply disconnected.
func (s *connServer) reject(c *conn) {
	if c.tlsIO == nil {
		c.out = busyResponse
		_, _ = c.writeOut()
	}
	s.closeConn(c)
}

// fail answers a request that cannot be served (malformed or too large
// head) and closes the connection. The answer is small, so it is written
// without waiting.
func (s *connServer) fail(c *conn, resp []byte) {
	c.out = s.encode(c, resp)
	_, _ = c.writeOut()
	s.closeConn(c)
}

// closeConn closes a connection. Unread input is read away first, so that
// the close does not reset a response the client has not read yet.
func (s *connServer) closeConn(c *conn) {
	if c.state == stateClosed {
		return
	}
	for i := 0; i < 4; i++ {
		var n int
		_ = c.raw.Read(func(fd uintptr) bool {
			n, _ = syscall.Read(int(fd), s.scratch)
			return true
		})
		if n < len(s.scratch) {
			break
		}
	}
	_ = c.nc.Close()
	c.state = stateClosed
	c.buf, c.out, c.request = nil, nil, nil
	if c.tlsIO != nil {
		c.tlsIO.in, c.tlsIO.out = nil, nil
	}
}

// beginStop rejects every connection whose request is not in work and
// drops the jobs that have not started.
func (s *connServer) beginStop() {
	for _, c := range s.conns {
		switch c.state {
		case stateHello, stateRequest, stateQueued:
			s.reject(c)
		}
	}
	s.ready = nil
	s.compact()
}

// idle reports whether no request is in work and no response is pending.
func (s *connServer) idle() bool {
	return s.busy == 0 && len(s.conns) == 0
}

// shutdown closes every connection and listener and ends the workers.
func (s *connServer) shutdown() {
	for _, c := range s.conns {
		s.closeConn(c)
	}
	s.conns, s.ready = nil, nil
	if s.jobCh != nil {
		close(s.jobCh)
		s.wg.Wait()
	}
	if s.wakeR >= 0 {
		_ = syscall.Close(s.wakeR)
		_ = syscall.Close(s.wakeW)
	}
	for _, sp := range s.specs {
		sp.ln.Close()
	}
}

// readSocket appends to *dst what the socket has, without waiting, until
// *dst holds limit bytes or the socket has nothing more. *dst never grows
// beyond ceiling bytes of capacity (the size the buffer will need). n is
// the number of bytes added; eof is set when the peer closed its side.
func (c *conn) readSocket(dst *[]byte, limit, ceiling int) (n int, eof bool, err error) {
	for len(*dst) < limit {
		b := grow(*dst, min(limit-len(*dst), readChunk), ceiling)
		room := b[len(b):cap(b)][:min(cap(b)-len(b), limit-len(b))]
		var got int
		var rerr error
		if err := c.raw.Read(func(fd uintptr) bool {
			got, rerr = syscall.Read(int(fd), room)
			return true // never wait for the descriptor
		}); err != nil {
			return n, false, err
		}
		*dst = b
		switch {
		case errors.Is(rerr, syscall.EAGAIN):
			return n, false, nil
		case errors.Is(rerr, syscall.EINTR):
			continue
		case rerr != nil:
			return n, false, rerr
		case got == 0:
			return n, true, nil
		}
		*dst = b[:len(b)+got]
		n += got
	}
	return n, false, nil
}

// writeOut writes c.out as far as the socket takes it without waiting. It
// reports whether everything was written.
func (c *conn) writeOut() (bool, error) {
	for len(c.out) > 0 {
		var n int
		var werr error
		if err := c.raw.Write(func(fd uintptr) bool {
			n, werr = syscall.SendmsgN(int(fd), c.out, nil, nil, syscall.MSG_DONTWAIT|syscall.MSG_NOSIGNAL)
			return true // never wait for the descriptor
		}); err != nil {
			return false, err
		}
		switch {
		case errors.Is(werr, syscall.EAGAIN):
			return false, nil
		case errors.Is(werr, syscall.EINTR):
			continue
		case werr != nil:
			return false, werr
		}
		c.out = c.out[n:]
	}
	c.out = nil
	return true, nil
}

// grow returns b with room for at least n more bytes. The capacity
// doubles (so a large body is copied a few times only), but not beyond
// ceiling, the size the buffer will need.
func grow(b []byte, n, ceiling int) []byte {
	if cap(b)-len(b) >= n {
		return b
	}
	nb := make([]byte, len(b), max(len(b)+n, min(len(b)+max(n, len(b)), ceiling)))
	copy(nb, b)
	return nb
}

// tlsBuffer is the net.Conn under a tls.Conn. During the handshake
// (blocking) it returns the bytes the accepting flow buffered and then
// reads the socket, and it writes the socket, both within the socket's
// deadline. After the handshake it never touches the socket: Read returns
// the buffered bytes or errWouldBlock, and Write appends to out, which the
// accepting flow writes as the socket accepts it. crypto/tls keeps its
// state over errWouldBlock, because it is a temporary net.Error (see
// readRecordOrCCS in crypto/tls/conn.go).
type tlsBuffer struct {
	net.Conn
	in       []byte
	out      []byte
	blocking bool
}

func (b *tlsBuffer) Read(p []byte) (int, error) {
	if len(b.in) > 0 {
		n := copy(p, b.in)
		b.in = b.in[n:]
		if len(b.in) == 0 {
			b.in = nil
		}
		return n, nil
	}
	if b.blocking {
		return b.Conn.Read(p)
	}
	return 0, errWouldBlock
}

func (b *tlsBuffer) Write(p []byte) (int, error) {
	if b.blocking {
		return b.Conn.Write(p)
	}
	b.out = append(b.out, p...)
	return len(p), nil
}

// wouldBlockError is what tlsBuffer.Read returns when nothing is buffered.
type wouldBlockError struct{}

func (wouldBlockError) Error() string   { return "no buffered data" }
func (wouldBlockError) Timeout() bool   { return true }
func (wouldBlockError) Temporary() bool { return true }

var errWouldBlock net.Error = wouldBlockError{}

// pollFd is struct pollfd of poll(2) (the same layout on amd64 and arm64).
type pollFd struct {
	fd      int32
	events  int16
	revents int16
}

const (
	pollIn  = 0x1
	pollOut = 0x4
)

// ppoll waits until one of fds is ready or timeout passes. It is ppoll(2)
// because arm64 has no poll(2). The flow waits in the system call like in
// any blocking read; no other flow is created for it.
func ppoll(fds []pollFd, timeout time.Duration) error {
	ts := syscall.NsecToTimespec(int64(timeout))
	_, _, errno := syscall.Syscall6(syscall.SYS_PPOLL, uintptr(unsafe.Pointer(&fds[0])), uintptr(len(fds)), uintptr(unsafe.Pointer(&ts)), 0, 0, 0)
	if errno != 0 && errno != syscall.EINTR {
		return errno
	}
	return nil
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

// serve runs a handler on a complete request and returns the serialised
// response (Connection: close). A panic of the handler is answered with
// 500.
func (s *connServer) serve(h http.Handler, req *http.Request) []byte {
	w := &respWriter{header: http.Header{}}
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
		h.ServeHTTP(w, req)
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
	var out bytes.Buffer
	_ = resp.Write(&out)
	return out.Bytes()
}

// errReader returns its error on every read.
type errReader struct{ err error }

func (e errReader) Read([]byte) (int, error) { return 0, e.err }

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
