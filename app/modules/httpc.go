package modules

import (
	"bufio"
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// HTTPSClient is a minimal HTTPS client that runs in the calling flow: it
// dials, performs the TLS handshake, writes one request and reads one
// response over one connection per request. Nothing limits the total time of
// a request: every wait for the server (connecting, each step of the TLS
// handshake, the progress of sending, the wait for the answer and the
// progress of receiving it) ends only after idle of silence (IdleConn). It
// sets no deadline of its own. (When the host is a name rather than an IP
// address, the standard library's name resolution may use goroutines
// internally while the call waits for it.)
type HTTPSClient struct {
	// Address is host:port of the server.
	Address string
	TLS     *tls.Config
	Headers map[string]string
	// idle is the longest silence waited for: IdleTimeout (zero), less in
	// tests.
	idle time.Duration
	// cut, when not nil, ends the requests when it is closed (the agent's
	// stop): none starts after it, and one in progress (connecting, in the
	// TLS handshake, sending or waiting for the answer) ends at once, its
	// connection closed, with errRequestCut.
	cut <-chan struct{}
}

// errRequestCut is the error of a request that the agent's stop cut.
var errRequestCut = errors.New("the request was cut by the agent's stop")

// HTTPError is returned for a non-2xx response.
type HTTPError struct {
	Status int
	Body   string
}

func (e *HTTPError) Error() string {
	msg := strings.TrimSpace(e.Body)
	var parsed struct {
		Error string `json:"error"`
	}
	if json.Unmarshal([]byte(e.Body), &parsed) == nil && parsed.Error != "" {
		msg = parsed.Error
	}
	if msg == "" {
		msg = http.StatusText(e.Status)
	}
	return fmt.Sprintf("HTTP %d: %s", e.Status, msg)
}

// NewHTTPSClient prepares a client for host and port.
func NewHTTPSClient(host string, port int, tlsCfg *tls.Config) (*HTTPSClient, error) {
	if host == "" || port <= 0 || port > 65535 {
		return nil, fmt.Errorf("invalid host %q or port %d", host, port)
	}
	return &HTTPSClient{Address: net.JoinHostPort(host, strconv.Itoa(port)), TLS: tlsCfg, Headers: map[string]string{}}, nil
}

// sentError marks an error of Do that happened after the whole request was
// written: the server may have received the request and may carry it out.
type sentError struct{ err error }

func (e *sentError) Error() string { return e.err.Error() }
func (e *sentError) Unwrap() error { return e.err }

// IsSilenceCut reports whether err is the idle limit of the connection after
// the whole request was sent: the server sent nothing of its answer for
// IdleTimeout (the client sets no other deadline). A cut while connecting,
// during the TLS handshake or while the request was being written is not one:
// the server cannot have received the request, so the caller reports that it
// cannot reach the server.
func IsSilenceCut(err error) bool {
	var se *sentError
	var ne net.Error
	return errors.As(err, &se) && errors.As(se.err, &ne) && ne.Timeout()
}

// Do sends one request and returns the status and body.
func (c *HTTPSClient) Do(method, path string, body []byte) (int, []byte, error) {
	status, data, err := c.do(method, path, body)
	if err != nil && isClosed(c.cut) {
		return 0, nil, errRequestCut
	}
	return status, data, err
}

// do is Do; an error after the cut closed is replaced by errRequestCut.
func (c *HTTPSClient) do(method, path string, body []byte) (int, []byte, error) {
	if isClosed(c.cut) {
		return 0, nil, errRequestCut
	}
	idle := c.idle
	if idle <= 0 {
		idle = IdleTimeout
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if c.cut != nil {
		go func() {
			select {
			case <-c.cut:
				cancel()
			case <-ctx.Done():
			}
		}()
	}
	raw, err := dialIdle(ctx, c.Address, idle, net.DefaultResolver)
	if err != nil {
		return 0, nil, err
	}
	defer func() { _ = raw.Close() }()
	// The connection is closed when the cut comes while the request runs.
	stop := context.AfterFunc(ctx, func() { _ = raw.Close() })
	defer stop()
	cfg := c.TLS.Clone()
	if cfg.ServerName == "" {
		cfg.ServerName, _, _ = net.SplitHostPort(c.Address)
	}
	conn := tls.Client(raw, cfg)
	if err := conn.Handshake(); err != nil {
		return 0, nil, err
	}
	u := url.URL{Scheme: "https", Host: c.Address, Path: path}
	if i := strings.Index(path, "?"); i >= 0 {
		u.Path = path[:i]
		u.RawQuery = path[i+1:]
	}
	req, err := http.NewRequest(method, u.String(), bytes.NewReader(body))
	if err != nil {
		return 0, nil, err
	}
	req.Close = true
	req.Header.Set("User-Agent", AppName+"/"+AppVersion)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	for k, v := range c.Headers {
		req.Header.Set(k, v)
	}
	bw := bufio.NewWriter(conn)
	if err := req.Write(bw); err != nil {
		return 0, nil, err
	}
	if err := bw.Flush(); err != nil {
		return 0, nil, err
	}
	resp, err := http.ReadResponse(bufio.NewReader(conn), req)
	if err != nil {
		return 0, nil, &sentError{err}
	}
	defer resp.Body.Close()
	// The answer is read completely, whatever its size (the command set
	// definitions of an agent have no total limit).
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return resp.StatusCode, nil, &sentError{err}
	}
	return resp.StatusCode, data, nil
}

// JSON sends in (when not nil) as JSON and decodes the response into out.
func (c *HTTPSClient) JSON(method, path string, in, out any) error {
	var body []byte
	if in != nil {
		var err error
		body, err = json.Marshal(in)
		if err != nil {
			return err
		}
	}
	status, data, err := c.Do(method, path, body)
	if err != nil {
		return err
	}
	if status < 200 || status > 299 {
		return &HTTPError{Status: status, Body: string(data)}
	}
	if out == nil || len(data) == 0 {
		return nil
	}
	return json.Unmarshal(data, out)
}

// IsCertificateError reports whether err is a TLS verification failure.
func IsCertificateError(err error) bool {
	var unknown x509UnknownAuthority
	if errors.As(err, &unknown) {
		return true
	}
	s := err.Error()
	return strings.Contains(s, "x509:") || strings.Contains(s, "certificate")
}
