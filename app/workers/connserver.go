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
	// admit reports, before a body larger than MaxHeadBytes is read, whether
	// the request is authenticated for its route (nil: always). A request it
	// refuses goes to the handler without its body, so the handler gives its
	// usual answer to an unauthenticated request. It runs in the accepting
	// flow, next to the workers, and must change nothing.
	admit func(*http.Request) bool
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
// memory, and its response is queued on the connection. The TLS handshake
// (crypto/tls, which cannot be suspended) runs one at a time, started by run
// between two passes; while it waits for its peer, the accepting flow runs
// its normal passes from inside the handshake's reads and writes (see
// handshake and awaitPeer), so nothing but other handshakes waits for it.
// See conn for the states.
type connServer struct {
	specs        []*listenerSpec
	workers      int
	queueTimeout time.Duration
	logger       *log.Logger
	// exempt reports whether a source address (remoteHost form) is never
	// refused after a stalled handshake: loopback and this host's addresses
	// (isOwnSource; tests replace it).
	exempt func(host string) bool
	// source returns the source a host counts as for the handshake order
	// and the stall table (sourceOf; tests replace it).
	source func(host string) string
	own    hostAddrs

	conns []*conn // open connections, in the order they were accepted
	ready []job   // complete requests and periodic tasks waiting for a worker
	// busy is the number of jobs handed to worker goroutines whose result
	// has not been collected yet (always 0 with one worker).
	busy     int
	taskBusy bool
	// bufferedTotal is the number of bytes all connections buffer
	// (recomputed every pass, increased by reads).
	bufferedTotal int
	// stalled is what is remembered about sources whose TLS handshake gave
	// up (see markStalled).
	stalled  map[string]stall
	stopping bool

	// stopSignal and tick are run's arguments (a nested pass needs them);
	// stopSeen latches the stop signal, which may be reported only once
	// (see stopRequested).
	stopSignal func() bool
	tick       func() func()
	stopSeen   bool
	// shake is the handshake in progress (nil when none): at most one.
	shake *handshakeState

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
	s := &connServer{
		specs: specs, workers: workers, queueTimeout: queueTimeout, logger: logger,
		stalled: map[string]stall{},
		scratch: make([]byte, readChunk), wakeR: -1, wakeW: -1,
	}
	s.exempt = s.isOwnSource
	s.source = sourceOf
	return s
}

// connState is where a connection is in its life (one request per
// connection).
type connState int

const (
	// stateHello (TLS only): reading the first record (the ClientHello).
	stateHello connState = iota
	// stateHandshakeDue (TLS only): the ClientHello is complete; the
	// connection waits for its turn (see nextHandshake).
	stateHandshakeDue
	// stateHandshaking (TLS only, one connection at most): its handshake is
	// in progress. The handshake owns it: no step of a pass reads, writes,
	// expires, evicts or closes it.
	stateHandshaking
	// stateRequest: reading the request (the head, then Content-Length
	// bytes of body). For TLS, raw bytes are decrypted as they arrive.
	stateRequest
	// stateQueued: the request is complete and waits for a worker.
	stateQueued
	// stateWorking: a worker runs the handler.
	stateWorking
	// stateWriting: the response is written as the socket accepts it.
	stateWriting
	// stateDraining: the response is written, but the request was not read
	// completely; what still arrives is read and discarded for a short time
	// so that closing does not reset the connection before the client has
	// read the response (see startDrain).
	stateDraining
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
	host  string // source address (remoteHost form)
	src   string // the source it counts as for handshakes (connServer.source)
	state connState
	// stateAt is when the connection entered its state.
	stateAt time.Time
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
	// admitted is set once spec.admit was asked about a large body;
	// refused when it refused it (the body is not read).
	admitted bool
	refused  bool
	// unread is set when the request was not read completely: after the
	// answer, the input is drained (drainLeft bytes at most).
	unread    bool
	drainLeft int
	request   *http.Request

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
	// drainPerPass is the most a draining connection reads in one pass.
	drainPerPass = 1 << 20
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
//
// This is the top level of the loop and the only place a TLS handshake is
// started: after a pass, when no iteration over the table, the queue or the
// poll set is in progress. While the handshake waits for its peer, passes
// run nested inside it (awaitPeer); they change the table, the queue and
// the poll set, and run starts its next pass from the beginning afterwards.
func (s *connServer) run(stopping func() bool, tick func() func()) {
	s.stopSignal, s.tick = stopping, tick
	s.start()
	var stopAt time.Time
	for {
		now := time.Now()
		if stopAt.IsZero() && s.stopRequested() {
			stopAt = now.Add(modules.StopFlushTimeout)
			s.beginStop()
		}
		if !stopAt.IsZero() && (s.idle() || now.After(stopAt)) {
			break
		}
		s.pass()
		if c := s.nextHandshake(time.Now()); c != nil {
			s.handshake(c)
		}
	}
	s.shutdown()
}

// stopRequested reports whether the stop signal was given. The signal is
// asked at the top level and inside a handshake (awaitPeer); it may report
// a stop only once (the central's reads a channel), so it is latched.
func (s *connServer) stopRequested() bool {
	if !s.stopSeen && s.stopSignal() {
		s.stopSeen = true
	}
	return s.stopSeen
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
// I/O that is possible without waiting, expire deadlines and start jobs. It
// runs at the top level (run) and nested inside a handshake (awaitPeer),
// the same in both, and never starts a handshake. For the handshake in
// progress it only records that its socket is ready.
func (s *connServer) pass() {
	s.bufferedTotal = 0
	for _, c := range s.conns {
		s.bufferedTotal += c.buffered()
	}
	s.buildPollSet(time.Now())
	s.wait(s.pollTimeout(time.Now()))
	now := time.Now()
	s.collect()
	for i, o := range s.pollOwners {
		if s.pollFds[i].revents == 0 {
			continue
		}
		switch {
		case o.c != nil && s.shake != nil && o.c == s.shake.c:
			s.shake.ready = true
		case o.c != nil:
			s.onReady(o.c)
		case o.sp != nil:
			s.acceptAll(o.sp, now)
		}
	}
	s.expire(time.Now())
	if !s.stopping {
		if t := s.tick(); t != nil && !s.taskBusy {
			s.taskBusy = true
			s.ready = append(s.ready, job{task: t})
		}
	}
	s.dispatch()
	s.compact()
}

// buildPollSet lists what the next ppoll waits for: the wake pipe, the
// listeners (not while stopping, and not while the table is full and no
// connection may be evicted: new connections then wait in the kernel's
// backlog), connections reading (unless the memory bound holds them back),
// connections writing or draining, and the handshake in progress with what
// crypto/tls waits for.
func (s *connServer) buildPollSet(now time.Time) {
	s.pollFds = s.pollFds[:0]
	s.pollOwners = s.pollOwners[:0]
	add := func(fd int, events int16, o pollOwner) {
		s.pollFds = append(s.pollFds, pollFd{fd: int32(fd), events: events})
		s.pollOwners = append(s.pollOwners, o)
	}
	if s.wakeR >= 0 {
		add(s.wakeR, pollIn, pollOwner{})
	}
	if !s.stopping && s.hasRoom(now) {
		for _, sp := range s.specs {
			if sp.fd >= 0 && !now.Before(sp.pausedUntil) {
				add(sp.fd, pollIn, pollOwner{sp: sp})
			}
		}
	}
	for _, c := range s.conns {
		switch {
		case c.state == stateHello, c.state == stateDraining,
			c.state == stateRequest && s.room(c, c.wantMore()) > 0:
			add(c.fd, pollIn, pollOwner{c: c})
		case c.state == stateWriting:
			add(c.fd, pollOut, pollOwner{c: c})
		case c.state == stateHandshaking:
			add(c.fd, s.shake.events, pollOwner{c: c})
		}
	}
}

// pollTimeout is 0 when a job can be started or (at the top level) a
// handshake started now; otherwise the time until the earliest deadline, the
// end of the handshake in progress, or until a connection of a full table
// may be evicted, at most LoopWakeInterval (so that the stop signal and the
// periodic tasks are looked at regularly).
func (s *connServer) pollTimeout(now time.Time) time.Duration {
	if len(s.ready) > 0 && s.busy < s.workers || s.shake == nil && s.handshakeWaiting() {
		return 0
	}
	d := modules.LoopWakeInterval
	if s.busy > 0 && s.wakeR < 0 {
		d = 10 * time.Millisecond
	}
	if s.shake != nil {
		at, _ := s.handshakeEndAt(s.shake, now)
		d = min(d, at.Sub(now)+time.Millisecond)
	}
	full := len(s.conns) >= modules.MaxPendingConns
	for _, c := range s.conns {
		if c.state == stateWorking || c.state == stateHandshaking || c.state == stateClosed {
			continue
		}
		if until := c.deadline.Sub(now) + time.Millisecond; until < d {
			d = until
		}
		// A connection that may be evicted already keeps the listeners in
		// the poll set (hasRoom), so only a future eviction time is a
		// reason to wake (otherwise the loop would spin).
		if at, ok := c.evictableAt(); full && ok && at.After(now) {
			d = min(d, at.Sub(now)+time.Millisecond)
		}
	}
	return max(d, 0)
}

// wait runs ppoll and records since when a connection accepted in this
// pass may have been waiting: if ppoll waited, nothing was pending when it
// began; otherwise the connection may have arrived while the previous pass
// was busy (for example running a handler), so its wait counts from the end
// of the previous ppoll.
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

// acceptAll takes every connection waiting on a listener that ppoll
// reported ready, so that the time a connection waits is counted from its
// arrival. It stops when the table is full and no connection may be
// evicted; the rest wait in the kernel's backlog until there is room. It
// also stops after listenBacklog connections, so that a source whose
// connections are closed at once (refused after a stalled handshake) and
// that keeps connecting cannot hold the accepting flow here.
func (s *connServer) acceptAll(sp *listenerSpec, now time.Time) {
	for n := 0; n < listenBacklog && (n == 0 || pollReady(sp.fd)); n++ {
		if !s.makeRoom(now) || !s.accept(sp, now) {
			return
		}
	}
}

// accept takes one connection from a listener that has one waiting. It
// reports whether one was taken.
func (s *connServer) accept(sp *listenerSpec, now time.Time) bool {
	_ = sp.ln.SetDeadline(now.Add(acceptGuard))
	nc, err := sp.ln.Accept()
	if err != nil {
		var ne net.Error
		if errors.As(err, &ne) && ne.Timeout() {
			return false
		}
		if !errors.Is(err, net.ErrClosed) {
			s.logger.Printf("%s: accept: %v", sp.name, err)
		}
		sp.pausedUntil = now.Add(acceptPause)
		return false
	}
	c := &conn{nc: nc, spec: sp, fd: -1, host: remoteHost(nc.RemoteAddr().String())}
	c.src = s.source(c.host)
	if sp.tls != nil && s.refused(c, now) {
		_ = nc.Close()
		return true
	}
	if sc, ok := nc.(syscall.Conn); ok {
		if raw, err := sc.SyscallConn(); err == nil {
			c.raw = raw
			_ = raw.Control(func(fd uintptr) { c.fd = int(fd) })
		}
	}
	if c.fd < 0 {
		_ = nc.Close()
		return true
	}
	c.queueDeadline = s.waitingSince.Add(s.queueTimeout)
	c.deadline = c.queueDeadline
	c.enter(stateRequest)
	if sp.tls != nil {
		c.tlsIO = &tlsBuffer{Conn: nc, raw: c.raw}
		c.tlsConn = tls.Server(c.tlsIO, sp.tls)
		c.enter(stateHello)
	}
	s.conns = append(s.conns, c)
	// The request often follows the connection at once.
	s.onReady(c)
	return true
}

// hasRoom reports whether a new connection can be taken now: the table has
// room, or a connection in it may be evicted.
func (s *connServer) hasRoom(now time.Time) bool {
	if len(s.conns) < modules.MaxPendingConns {
		return true
	}
	return s.victim(now) != nil
}

// makeRoom makes room for a new connection, evicting one if the table is
// full. It reports false when the table is full and no connection may be
// evicted (every one is a complete request waiting for a worker or in work,
// or waits on its peer for less than EvictGrace).
func (s *connServer) makeRoom(now time.Time) bool {
	if len(s.conns) >= modules.MaxPendingConns {
		s.compact()
	}
	if len(s.conns) < modules.MaxPendingConns {
		return true
	}
	c := s.victim(now)
	if c == nil {
		return false
	}
	switch c.state {
	case stateRequest:
		s.reject(c)
	default:
		s.closeConn(c) // a response may be written in part already
	}
	s.compact()
	return true
}

// victim chooses the connection a full table drops for a new one, so that
// connections which wait on their peer cannot keep new clients out: a
// connection that has its answer and is drained, else the oldest that has
// not delivered a complete request head, else the oldest that waits for its
// peer otherwise (a large body that has not arrived, or a response the
// client does not read). Only a connection that has waited in its state for
// EvictGrace is chosen, so that a burst of new clients does not evict
// clients whose request is still on its way. nil when there is none.
func (s *connServer) victim(now time.Time) *conn {
	var head, other *conn
	for _, c := range s.conns {
		at, ok := c.evictableAt()
		if !ok || now.Before(at) {
			continue
		}
		switch {
		case c.state == stateDraining:
			return c
		case c.state == stateHello || c.state == stateHandshakeDue || (c.state == stateRequest && c.headLen == 0):
			if head == nil {
				head = c
			}
		case other == nil:
			other = c
		}
	}
	if head != nil {
		return head
	}
	return other
}

// evictableAt returns when a connection may be evicted from a full table
// (see victim); ok is false when it never may (its request waits for a
// worker or is in work, or its handshake is in progress).
func (c *conn) evictableAt() (at time.Time, ok bool) {
	switch c.state {
	case stateDraining:
		return c.stateAt, true
	case stateHello, stateHandshakeDue, stateRequest, stateWriting:
		return c.stateAt.Add(modules.EvictGrace), true
	}
	return time.Time{}, false
}

// enter puts the connection in a state.
func (c *conn) enter(st connState) {
	c.state = st
	c.stateAt = time.Now()
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
	case stateDraining:
		s.drain(c)
	}
}

// readHello buffers the first TLS record. Once it is complete, the
// connection waits for its handshake (see nextHandshake).
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
	c.enter(stateHandshakeDue)
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

// nextHandshake returns the connection whose handshake run starts next:
// the oldest waiting connection of a source that is not stalled, else the
// oldest of a stalled source (it gives way as soon as another source waits;
// see handshakeEndAt). Connections met on the way whose queue deadline has
// passed, or whose source is refused now, are closed.
func (s *connServer) nextHandshake(now time.Time) *conn {
	if s.stopping {
		return nil
	}
	var stalled *conn
	for _, c := range s.conns {
		if c.state != stateHandshakeDue {
			continue
		}
		switch {
		case !now.Before(c.deadline), s.refused(c, now):
			s.closeConn(c)
		case !s.isStalled(c.src, now):
			return c
		case stalled == nil:
			stalled = c
		}
	}
	return stalled
}

// handshakeWaiting reports whether a connection waits for its handshake.
func (s *connServer) handshakeWaiting() bool {
	for _, c := range s.conns {
		if c.state == stateHandshakeDue {
			return true
		}
	}
	return false
}

// handshakeState is the handshake in progress (connServer.shake).
type handshakeState struct {
	c     *conn
	start time.Time
	// stalled is set when the source counted as stalled at the start.
	stalled bool
	// events is what crypto/tls waits for (pollIn or pollOut); ready is set
	// by a pass whose ppoll reported the socket ready for it.
	events int16
	ready  bool
	// looked is set once the socket was looked at again when the handshake
	// was about to give up for lack of time (see awaitPeer).
	looked bool
	// end is why the handshake ends before crypto/tls is done (nil while it
	// goes on). Once set, every further wait returns it at once.
	end error
}

// Why a handshake ends while it waits for its peer. They are not net.Error
// values, so crypto/tls treats them as final.
var (
	errHandshakeLimit   = errors.New("TLS handshake: the client did not answer within the limit")
	errHandshakeGrace   = errors.New("TLS handshake: gave way to another connection after the grace")
	errHandshakeGaveWay = errors.New("TLS handshake: a stalled source gave way to another source")
	errHandshakeStop    = errors.New("TLS handshake: the central stops")
)

// handshake runs the TLS handshake of c. It is called only by run, between
// two passes, and only one handshake is ever in progress. crypto/tls runs on
// this flow's stack; when it needs bytes the peer has not sent yet (or the
// socket takes no more), tlsBuffer calls awaitPeer, which runs the normal
// passes until the socket is ready or the handshake is to end. The client's
// first record is buffered already, so only its next flight is waited for.
// A handshake that runs into its limit marks its source as stalled.
//
// After Handshake returns, the table, the queue and the poll set may have
// changed (nested passes); this function touches only c and returns to run,
// which starts its next pass from the beginning.
func (s *connServer) handshake(c *conn) {
	if s.shake != nil {
		panic("connServer: a handshake started inside another")
	}
	start := time.Now()
	waited := start.Sub(c.stateAt) // time spent waiting for its turn
	sh := &handshakeState{c: c, start: start, stalled: s.isStalled(c.src, start)}
	s.shake = sh
	c.enter(stateHandshaking)
	c.tlsIO.await = s.awaitPeer
	err := c.tlsConn.Handshake()
	c.tlsIO.await = nil
	s.shake = nil
	if err != nil {
		switch sh.end {
		case errHandshakeLimit:
			s.markStalled(c, true, time.Now())
		case errHandshakeGrace:
			s.markStalled(c, false, time.Now())
		}
		s.closeConn(c)
		return
	}
	// The time the connection waited for its handshake to begin does not
	// count against its queue timeout.
	c.queueDeadline = c.queueDeadline.Add(waited)
	c.deadline = c.queueDeadline
	c.enter(stateRequest)
	// The request may have arrived with the client's last handshake record.
	s.readRequest(c)
}

// awaitPeer is tlsBuffer.await of the handshake in progress: it runs nested
// passes (with the handshake's socket in the poll set for events) until the
// socket is ready, then returns nil and crypto/tls reads or writes again;
// or until the handshake is to end, then it returns why. The end is checked
// before every pass, so a peer that sends a byte at a time ends at the
// limit too. A nested pass starts no handshake, so awaitPeer is never
// entered twice.
//
// The time runs out while the flow is busy too (a handler it runs with one
// worker, a periodic task), and meanwhile it does not look at the socket.
// So before the handshake gives up for lack of time, the socket is looked
// at once more: an answer that arrived in time but was not seen is taken.
// This happens once per handshake (the whole flight of an honest client is
// then in the socket), so a peer that sends a byte at a time still ends.
func (s *connServer) awaitPeer(events int16) error {
	sh := s.shake
	for {
		if sh.end == nil {
			sh.end = s.handshakeEnd(sh, time.Now())
			if sh.end != nil && sh.end != errHandshakeStop && !sh.looked {
				sh.looked = true
				if socketReady(sh.c.fd, events) {
					sh.end = nil
					return nil
				}
			}
		}
		if sh.end != nil {
			return sh.end
		}
		sh.events, sh.ready = events, false
		s.pass()
		if sh.ready {
			return nil
		}
	}
}

// handshakeEnd returns why the handshake in progress ends now, or nil.
func (s *connServer) handshakeEnd(sh *handshakeState, now time.Time) error {
	if s.stopRequested() {
		return errHandshakeStop
	}
	if at, why := s.handshakeEndAt(sh, now); !now.Before(at) {
		return why
	}
	return nil
}

// handshakeEndAt returns when the handshake in progress gives up and why,
// given who waits for a handshake now (only other handshakes wait for it):
//
//	in progress          waiting                       gives up at
//	source not stalled   nobody                        the limit
//	source not stalled   anyone                        start + HandshakeGrace
//	source stalled       nobody                        the limit
//	source stalled       stalled sources only          start + HandshakeGrace
//	                     (its own included)
//	source stalled       another source not stalled    at once
//
// A stalled source was taken only because nobody else waited, so it gives
// way at once to a source that did not stall. Otherwise the grace is enough
// for an honest client, and keeps a staller from holding the turn for the
// full limit while others wait, also those of its own source (which wait
// behind it in any case).
func (s *connServer) handshakeEndAt(sh *handshakeState, now time.Time) (time.Time, error) {
	waiting, fresh := s.handshakesWaiting(sh.c, now)
	switch {
	case sh.stalled && fresh:
		return sh.start, errHandshakeGaveWay
	case waiting:
		return sh.start.Add(modules.HandshakeGrace), errHandshakeGrace
	}
	return sh.start.Add(modules.TLSHandshakeTimeout), errHandshakeLimit
}

// handshakesWaiting reports, for the handshake of c in progress, whether
// another connection waits for its handshake (not refused, before its queue
// deadline), and whether one of them is of another source that is not
// stalled.
func (s *connServer) handshakesWaiting(c *conn, now time.Time) (waiting, fresh bool) {
	for _, o := range s.conns {
		if o.state != stateHandshakeDue || !now.Before(o.deadline) || s.refused(o, now) {
			continue
		}
		waiting = true
		if o.src != c.src && !s.isStalled(o.src, now) {
			return true, true
		}
	}
	return waiting, false
}

// stall is what is remembered about a source whose handshake gave up.
type stall struct {
	// until is the end of the time its connections are taken after those
	// of other sources (HandshakePenalty after its last stall).
	until time.Time
	// limit is set when a handshake of it used the full limit within that
	// time: it is then refused unless it is exempt.
	limit bool
}

// markStalled records that the handshake of c gave up: at the full limit
// (full), or after HandshakeGrace because another connection waited. For
// HandshakePenalty the connections of its source are taken after those of
// other sources; after a full limit they are refused at once unless they
// come from an exempt address. Expired entries are dropped here and when
// they are looked up.
func (s *connServer) markStalled(c *conn, full bool, now time.Time) {
	for k, st := range s.stalled {
		if !now.Before(st.until) {
			delete(s.stalled, k)
		}
	}
	st := s.stalled[c.src]
	st.until = now.Add(modules.HandshakePenalty)
	st.limit = st.limit || full
	s.stalled[c.src] = st
	host := c.host
	switch {
	case !full:
		s.logger.Printf("TLS handshake of %s did not finish within %v while other connections waited; its connections are taken after those of other sources for %v",
			host, modules.HandshakeGrace, modules.HandshakePenalty)
	case s.exempt(host):
		s.logger.Printf("TLS handshake of %s did not finish within %v; its connections are taken after those of other sources for %v",
			host, modules.TLSHandshakeTimeout, modules.HandshakePenalty)
	default:
		s.logger.Printf("TLS handshake of %s did not finish within %v; its connections are refused for %v",
			host, modules.TLSHandshakeTimeout, modules.HandshakePenalty)
	}
}

// isStalled reports whether a handshake of the source src gave up within
// the penalty period.
func (s *connServer) isStalled(src string, now time.Time) bool {
	st, ok := s.stalled[src]
	if !ok {
		return false
	}
	if now.Before(st.until) {
		return true
	}
	delete(s.stalled, src)
	return false
}

// refused reports whether the TLS connection c is refused: a handshake of
// its source ran into the full limit within the penalty period and its
// address is not exempt (loopback and this host's own addresses never are).
func (s *connServer) refused(c *conn, now time.Time) bool {
	return s.isStalled(c.src, now) && s.stalled[c.src].limit && !s.exempt(c.host)
}

// loopbackSource is the one source every loopback address counts as.
const loopbackSource = "loopback"

// sourceOf returns the source host (remoteHost form) counts as for the
// handshake order and the stall table: its address, except that every
// loopback address is one source, because a process of this host can use
// any address of 127.0.0.0/8 (and ::1).
func sourceOf(host string) string {
	h := host
	if i := strings.IndexByte(h, '%'); i >= 0 {
		h = h[:i]
	}
	if ip := net.ParseIP(h); ip != nil && ip.IsLoopback() {
		return loopbackSource
	}
	return host
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
		if resp == nil && c.needsAdmission() && !s.admitBody(c) {
			// Not authenticated for a large body: the handler answers
			// without it, and the body is never read.
			c.refused, c.unread, complete = true, true, true
		}
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
			c.tooLarge, c.unread = true, true
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

// needsAdmission reports whether the request head announces a body larger
// than MaxHeadBytes that the listener's admit check has not seen yet.
func (c *conn) needsAdmission() bool {
	return c.headLen > 0 && c.bodyLen > modules.MaxHeadBytes && !c.admitted
}

// admitBody asks the listener's admit check whether the body of a request
// may be read (see listenerSpec.admit). A panic of the check counts as
// admitted: the handler then authenticates as usual.
func (s *connServer) admitBody(c *conn) (ok bool) {
	c.admitted = true
	if c.spec.admit == nil {
		return true
	}
	defer func() {
		if r := recover(); r != nil {
			s.logger.Printf("%s %s: body check panic: %v\n%s", c.request.Method, c.request.URL.Path, r, debug.Stack())
			ok = true
		}
	}()
	return c.spec.admit(s.requestFor(c))
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
	c.enter(stateQueued)
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
			c.enter(stateWorking)
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
	req := s.requestFor(c)
	switch {
	case c.tooLarge:
		req.Body = io.NopCloser(errReader{&http.MaxBytesError{Limit: modules.MaxRequestBody}})
	case c.refused:
		req.Body = io.NopCloser(errReader{errBodyRefused})
	case c.bodyLen == 0:
		req.Body = http.NoBody
	default:
		req.Body = io.NopCloser(bytes.NewReader(c.buf[c.headLen : c.headLen+c.bodyLen]))
	}
	return job{conn: c, req: req, handler: c.spec.handler}
}

// errBodyRefused is what the body of a request reads whose large body was
// not read because the request is not authenticated for its route.
var errBodyRefused = errors.New("request body not read: a body this large needs an authenticated request")

// requestFor returns the parsed request head with what the handlers learn
// about the connection (no body).
func (s *connServer) requestFor(c *conn) *http.Request {
	req := c.request
	req.RemoteAddr = c.nc.RemoteAddr().String()
	if c.tlsConn != nil {
		st := c.tlsConn.ConnectionState()
		req.TLS = &st
	}
	// The local address of the connection (never a header) tells the
	// management API whether a client of this host connected to one of the
	// host's own addresses.
	return req.WithContext(context.WithValue(req.Context(), http.LocalAddrContextKey, c.nc.LocalAddr()))
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
	c.enter(stateWriting)
	c.deadline = c.total
	s.flush(c)
}

// encode returns the bytes to send for resp: resp itself, or for TLS its
// encryption followed by the close_notify alert (the connection is closed
// after the response; without the alert, a strict client such as openssl
// reports an unexpected end). crypto/tls writes both into the connection's
// outbound buffer.
func (s *connServer) encode(c *conn, resp []byte) []byte {
	if c.tlsConn == nil {
		return resp
	}
	if _, err := c.tlsConn.Write(resp); err != nil {
		return nil
	}
	_ = c.tlsConn.CloseWrite()
	out := c.tlsIO.out
	c.tlsIO.out = nil
	return out
}

// flush writes the pending response bytes as far as the socket takes them.
// When all are written, the connection is closed, or drained first when its
// request was not read completely; it is closed when the socket failed.
func (s *connServer) flush(c *conn) {
	done, err := c.writeOut()
	switch {
	case err != nil:
		s.closeConn(c)
	case done && c.unread && !s.stopping:
		s.startDrain(c)
	case done:
		s.closeConn(c)
	}
}

// startDrain begins draining a connection whose response is written but
// whose request was not read completely (an early error, a body that was
// not read, a rejection while the request arrived). Closing it with unread
// input would reset the connection, and the client, still sending, could
// lose the response. So the sending side is shut down (the client sees the
// end of the response), and what still arrives is read and discarded until
// the client closes, at most DrainMaxBytes and DrainTimeout.
func (s *connServer) startDrain(c *conn) {
	_ = c.raw.Control(func(fd uintptr) { _ = syscall.Shutdown(int(fd), syscall.SHUT_WR) })
	c.enter(stateDraining)
	c.deadline = c.stateAt.Add(modules.DrainTimeout)
	c.drainLeft = modules.DrainMaxBytes
	c.buf, c.request = nil, nil
	if c.tlsIO != nil {
		c.tlsIO.in = nil
	}
	s.drain(c)
}

// drain reads and discards what the socket has, at most drainPerPass bytes
// per pass, and closes the connection when the client closed its side or
// the limit is used up.
func (s *connServer) drain(c *conn) {
	for read := 0; read < drainPerPass; {
		n, rerr := readNow(c.raw, s.scratch)
		switch {
		case errors.Is(rerr, syscall.EAGAIN):
			return
		case errors.Is(rerr, syscall.EINTR):
			continue
		case rerr != nil, n == 0:
			s.closeConn(c)
			return
		}
		read += n
		c.drainLeft -= n
		if c.drainLeft <= 0 {
			s.closeConn(c)
			return
		}
	}
}

// expire rejects connections whose request did not arrive, or did not get
// a worker, in time, and closes those whose response could not be written
// within the total limit and those drained long enough. A plain HTTP
// request that was arriving gets its 503 and is drained.
func (s *connServer) expire(now time.Time) {
	for _, c := range s.conns {
		if !now.After(c.deadline) {
			continue
		}
		switch {
		case c.state == stateRequest && c.tlsIO == nil:
			s.answer(c, busyResponse)
		case c.state == stateHello, c.state == stateHandshakeDue, c.state == stateRequest, c.state == stateQueued:
			s.reject(c)
		case c.state == stateWriting, c.state == stateDraining:
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

// reject closes a connection that could not be served, at once. A plain
// HTTP client gets a 503 (written without waiting; it fits the socket's
// buffer); a TLS client is simply disconnected.
func (s *connServer) reject(c *conn) {
	if c.tlsIO == nil {
		c.out = busyResponse
		_, _ = c.writeOut()
	}
	s.closeConn(c)
}

// fail answers a request that cannot be served (malformed or too large
// head, a body without Content-Length).
func (s *connServer) fail(c *conn, resp []byte) {
	s.answer(c, resp)
}

// answer writes resp on a connection whose request was not read completely
// and drains the connection afterwards (see startDrain). The answer is
// small; it must be written within DrainTimeout.
func (s *connServer) answer(c *conn, resp []byte) {
	c.unread = true
	c.out = s.encode(c, resp)
	c.enter(stateWriting)
	c.deadline = c.stateAt.Add(modules.DrainTimeout)
	s.flush(c)
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
	s.stopping = true
	for _, c := range s.conns {
		switch c.state {
		case stateHello, stateHandshakeDue, stateRequest, stateQueued:
			s.reject(c)
		case stateDraining:
			s.closeConn(c)
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
		got, rerr := readNow(c.raw, room)
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
		n, werr := sendNow(c.raw, c.out)
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

// tlsBuffer is the net.Conn under a tls.Conn. During the handshake (await
// set) it returns the bytes the accepting flow buffered and then reads the
// socket without waiting, and it writes the socket without waiting; when
// the socket has nothing (or takes nothing), it calls await, which runs the
// accepting flow's other work until the socket is ready and returns an error
// when the handshake is to end (see connServer.awaitPeer). After the
// handshake it never touches the socket: Read returns the buffered bytes or
// errWouldBlock, and Write appends to out, which the accepting flow writes
// as the socket accepts it. crypto/tls keeps its state over errWouldBlock,
// because it is a temporary net.Error (see readRecordOrCCS in
// crypto/tls/conn.go).
type tlsBuffer struct {
	net.Conn
	raw   syscall.RawConn
	in    []byte
	out   []byte
	await func(events int16) error
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
	if b.await == nil {
		return 0, errWouldBlock
	}
	if len(p) == 0 {
		return 0, nil
	}
	for {
		n, err := readNow(b.raw, p)
		switch {
		case errors.Is(err, syscall.EAGAIN):
			if err := b.await(pollIn); err != nil {
				return 0, err
			}
		case errors.Is(err, syscall.EINTR):
		case err != nil:
			return 0, err
		case n == 0:
			return 0, io.EOF
		default:
			return n, nil
		}
	}
}

// SetWriteDeadline does nothing: the socket is never waited on, and a
// deadline on it would make the accepting flow's later writes fail
// (crypto/tls sets one around the close_notify alert; see encode).
func (b *tlsBuffer) SetWriteDeadline(time.Time) error { return nil }

func (b *tlsBuffer) Write(p []byte) (int, error) {
	if b.await == nil {
		b.out = append(b.out, p...)
		return len(p), nil
	}
	written := 0
	for written < len(p) {
		n, err := sendNow(b.raw, p[written:])
		switch {
		case errors.Is(err, syscall.EAGAIN):
			if err := b.await(pollOut); err != nil {
				return written, err
			}
		case errors.Is(err, syscall.EINTR):
		case err != nil:
			return written, err
		default:
			written += n
		}
	}
	return written, nil
}

// readNow reads from the socket without waiting (EAGAIN when it has
// nothing).
func readNow(raw syscall.RawConn, p []byte) (n int, err error) {
	if cerr := raw.Read(func(fd uintptr) bool {
		n, err = syscall.Read(int(fd), p)
		return true // never wait for the descriptor
	}); cerr != nil {
		return 0, cerr
	}
	return max(n, 0), err
}

// sendNow writes to the socket without waiting (EAGAIN when it takes
// nothing) and without SIGPIPE.
func sendNow(raw syscall.RawConn, p []byte) (n int, err error) {
	if cerr := raw.Write(func(fd uintptr) bool {
		n, err = syscall.SendmsgN(int(fd), p, nil, nil, syscall.MSG_DONTWAIT|syscall.MSG_NOSIGNAL)
		return true // never wait for the descriptor
	}); cerr != nil {
		return 0, cerr
	}
	return max(n, 0), err
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

// pollReady reports whether fd is readable now (a listener: a connection
// is waiting), without waiting.
func pollReady(fd int) bool {
	fds := []pollFd{{fd: int32(fd), events: pollIn}}
	return ppoll(fds, 0) == nil && fds[0].revents&pollIn != 0
}

// socketReady reports whether fd is ready now for events (or has an error
// or hang-up to report), without waiting.
func socketReady(fd int, events int16) bool {
	fds := []pollFd{{fd: int32(fd), events: events}}
	return ppoll(fds, 0) == nil && fds[0].revents != 0
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

// hostAddrs caches the addresses of this host's interfaces. They come from
// net.InterfaceAddrs (asked from the kernel over netlink; nothing is read
// from /proc) and are read again when the cache is older than
// hostAddrsMaxAge, so an address added later is noticed within that time.
// Only the accepting flow uses it, and only for sources that stalled a
// handshake.
type hostAddrs struct {
	ips []net.IP
	at  time.Time
}

const hostAddrsMaxAge = time.Second

func (h *hostAddrs) get(now time.Time) []net.IP {
	if h.at.IsZero() || now.Sub(h.at) >= hostAddrsMaxAge {
		h.ips = h.ips[:0]
		if addrs, err := net.InterfaceAddrs(); err == nil {
			for _, a := range addrs {
				if ipn, ok := a.(*net.IPNet); ok {
					h.ips = append(h.ips, ipn.IP)
				}
			}
		}
		h.at = now
	}
	return h.ips
}

// isOwnSource reports whether a source address is loopback or one of this
// host's addresses: such a source is never refused after a stalled
// handshake (an agent or ctl of this host may connect from any of them, to
// any of them).
func (s *connServer) isOwnSource(host string) bool {
	if exemptSource(host, nil) {
		return true
	}
	return exemptSource(host, s.own.get(time.Now()))
}

// exemptSource reports whether host (an IP address, optionally with a
// zone) is a loopback address or one of own.
func exemptSource(host string, own []net.IP) bool {
	if i := strings.IndexByte(host, '%'); i >= 0 {
		host = host[:i]
	}
	ip := net.ParseIP(host)
	if ip == nil {
		return false
	}
	if ip.IsLoopback() {
		return true
	}
	for _, o := range own {
		if o.Equal(ip) {
			return true
		}
	}
	return false
}
