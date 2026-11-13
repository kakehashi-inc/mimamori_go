package workers

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log"
	"net/http"
	"runtime/debug"
	"strconv"
	"sync"

	"mimamori/app/modules"
)

// errBodyRefused is what the body of a request reads whose large body was
// not read because the request is not authenticated for its route.
var errBodyRefused = errors.New("request body not read: a body this large needs an authenticated request")

// internalErrorBody is the body of the answer to a handler that panicked.
var internalErrorBody = errorBodyBytes(modules.M("common.internal_error"))

// lengthRequiredBody is the body of the answer to a request whose body has
// no Content-Length (Transfer-Encoding).
var lengthRequiredBody = errorBodyBytes(modules.M("common.length_required"))

// errorBodyBytes returns the JSON body of an error answer (see errorBody).
func errorBodyBytes(m modules.Msg) []byte {
	b, err := json.Marshal(errorBody(m))
	if err != nil {
		panic(err) // a message of strings and numbers always encodes
	}
	return b
}

// errReader returns its error on every read.
type errReader struct{ err error }

func (e errReader) Read([]byte) (int, error) { return 0, e.err }

// bodyBudget bounds the memory held by request bodies larger than
// modules.MaxHeadBytes, in arrival and in work. A body reserves its whole
// length before its first byte is read and keeps it until its answer is
// written; reservations are granted first come, first served, and a waiter
// is never refused and never gives up. Since the whole length is reserved
// at once, two half-read bodies can never wait for each other.
type bodyBudget struct {
	mu      sync.Mutex
	size    int64
	free    int64
	peak    int64 // most held at once (read by the tests)
	waiters []*budgetWaiter
}

type budgetWaiter struct {
	n     int64
	ready chan struct{}
}

func newBodyBudget(size int64) *bodyBudget {
	return &bodyBudget{size: size, free: size}
}

// acquire reserves n bytes, waiting behind every earlier waiter.
func (b *bodyBudget) acquire(n int64) {
	b.mu.Lock()
	if len(b.waiters) == 0 && b.free >= n {
		b.take(n)
		b.mu.Unlock()
		return
	}
	w := &budgetWaiter{n: n, ready: make(chan struct{})}
	b.waiters = append(b.waiters, w)
	b.mu.Unlock()
	<-w.ready
}

// release gives n bytes back and grants the waiters at the head of the
// line that fit now.
func (b *bodyBudget) release(n int64) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.free += n
	for len(b.waiters) > 0 && b.waiters[0].n <= b.free {
		w := b.waiters[0]
		b.waiters = b.waiters[1:]
		b.take(w.n)
		close(w.ready)
	}
}

// take reserves n bytes (mu held).
func (b *bodyBudget) take(n int64) {
	b.free -= n
	b.peak = max(b.peak, b.size-b.free)
}

// responseRecorder keeps a handler's answer in memory, so that it is
// written with a Content-Length and a panic can still be answered with a
// clean 500.
type responseRecorder struct {
	header http.Header
	status int
	body   bytes.Buffer
}

func newResponseRecorder() *responseRecorder {
	return &responseRecorder{header: http.Header{}}
}

func (w *responseRecorder) Header() http.Header { return w.header }

func (w *responseRecorder) WriteHeader(code int) {
	if w.status == 0 {
		w.status = code
	}
}

func (w *responseRecorder) Write(b []byte) (int, error) {
	if w.status == 0 {
		w.status = http.StatusOK
	}
	return w.body.Write(b)
}

// requestGate is the only handler net/http calls on both listeners. For
// every request it reads the whole body into memory before the handler
// runs (within the size limits, the authentication pre-check admit and the
// memory budget), takes the idle time off the connection while the handler
// works, and writes the collected answer with its length. A handler's
// context is never cancelled.
type requestGate struct {
	next   http.Handler
	admit  func(*http.Request) bool
	budget *bodyBudget
	logger *log.Logger
}

func (g *requestGate) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	conn := connOf(r.Context())
	// What the handler gets: a copy of the request whose context carries
	// every value (the local address among them) and is never cancelled.
	req := r.WithContext(context.WithoutCancel(r.Context()))
	unread := false
	switch {
	case r.ContentLength < 0:
		rec := newResponseRecorder()
		rec.header.Set("Content-Type", "application/json")
		rec.status = http.StatusLengthRequired
		rec.body.Write(lengthRequiredBody)
		g.respond(w, rec, true)
		return
	case r.ContentLength == 0:
		req.Body = http.NoBody
	case r.ContentLength > modules.MaxRequestBody:
		req.Body = io.NopCloser(errReader{&http.MaxBytesError{Limit: modules.MaxRequestBody}})
		unread = true
	case r.ContentLength > modules.MaxHeadBytes && !g.admitted(req):
		req.Body = io.NopCloser(errReader{errBodyRefused})
		unread = true
	default:
		n := r.ContentLength
		if n > modules.MaxHeadBytes {
			g.budget.acquire(n)
			defer g.budget.release(n)
		}
		buf := make([]byte, n)
		if _, err := io.ReadFull(r.Body, buf); err != nil {
			if errors.Is(err, io.ErrUnexpectedEOF) || errors.Is(err, io.EOF) {
				// The peer ended its sending early; it can still read.
				g.respond(w, &responseRecorder{header: http.Header{}, status: http.StatusBadRequest}, true)
				return
			}
			// Silent for the idle time, or reset: closed without an answer
			// and without a log line.
			panic(http.ErrAbortHandler)
		}
		req.Body = io.NopCloser(bytes.NewReader(buf))
	}
	rc := http.NewResponseController(w)
	if unread {
		// The documented way to read the body after the answer was written.
		_ = rc.EnableFullDuplex()
	}
	if conn != nil {
		conn.Pause() // the central works; nothing is expected from the peer
	}
	g.respond(w, g.callHandler(req), unread)
	_ = rc.Flush()
	if conn != nil {
		conn.Resume()
	}
	if unread && r.Header.Get("Expect") == "" {
		// The answer is written; what the peer still sends of this request
		// is read and dropped, so that a peer that sends its whole request
		// before it reads gets the answer and not a reset.
		_, _ = io.CopyN(io.Discard, r.Body, min(r.ContentLength, modules.DrainMaxBytes))
	}
}

// admitted asks the pre-check whether the large body of r may be read. A
// panic of the check is logged and counts as admitted: the handler then
// authenticates as usual.
func (g *requestGate) admitted(r *http.Request) (ok bool) {
	if g.admit == nil {
		return true
	}
	defer func() {
		if p := recover(); p != nil {
			g.logger.Printf("%s %s: body check panic: %v\n%s", r.Method, r.URL.Path, p, debug.Stack())
			ok = true
		}
	}()
	return g.admit(r)
}

// callHandler runs the handler on a complete request and returns its answer. A
// panic is logged and answered with 500; whatever the handler had written
// is discarded.
func (g *requestGate) callHandler(req *http.Request) (rec *responseRecorder) {
	rec = newResponseRecorder()
	defer func() {
		if p := recover(); p != nil {
			g.logger.Printf("%s %s: panic: %v\n%s", req.Method, req.URL.Path, p, debug.Stack())
			rec = newResponseRecorder()
			rec.header.Set("Content-Type", "application/json")
			rec.status = http.StatusInternalServerError
			rec.body.Write(internalErrorBody)
		}
	}()
	g.next.ServeHTTP(rec, req)
	return rec
}

// respond writes a collected answer: nosniff and a Content-Length on every
// answer, Connection: close when the request was not read completely.
func (g *requestGate) respond(w http.ResponseWriter, rec *responseRecorder, closeAfter bool) {
	h := w.Header()
	for k, v := range rec.header {
		h[k] = v
	}
	h.Set("X-Content-Type-Options", "nosniff")
	if h.Get("Content-Length") == "" {
		h.Set("Content-Length", strconv.Itoa(rec.body.Len()))
	}
	if closeAfter {
		h.Set("Connection", "close")
	}
	status := rec.status
	if status == 0 {
		status = http.StatusOK
	}
	w.WriteHeader(status)
	_, _ = w.Write(rec.body.Bytes())
}
