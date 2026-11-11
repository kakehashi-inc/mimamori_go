package modules

import (
	"bufio"
	"bytes"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// HTTPSClient is a minimal HTTPS client that runs entirely in the calling
// flow. Unlike net/http.Client it creates no goroutines: it dials, performs
// the TLS handshake, writes one request and reads one response, all bounded
// by a deadline on the connection.
type HTTPSClient struct {
	BaseURL *url.URL
	TLS     *tls.Config
	Timeout time.Duration
	Headers map[string]string
}

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

// NewHTTPSClient parses base (https://host:port) and prepares a client.
func NewHTTPSClient(base string, tlsCfg *tls.Config, timeout time.Duration) (*HTTPSClient, error) {
	u, err := url.Parse(strings.TrimRight(base, "/"))
	if err != nil {
		return nil, err
	}
	if u.Scheme != "https" || u.Host == "" {
		return nil, fmt.Errorf("server URL must be https://host[:port]: %q", base)
	}
	if u.Port() == "" {
		u.Host = net.JoinHostPort(u.Hostname(), fmt.Sprint(DefaultAgentPort))
	}
	return &HTTPSClient{BaseURL: u, TLS: tlsCfg, Timeout: timeout, Headers: map[string]string{}}, nil
}

// Do sends one request and returns the status and body.
func (c *HTTPSClient) Do(method, path string, body []byte) (int, []byte, error) {
	deadline := time.Now().Add(c.Timeout)
	d := net.Dialer{Deadline: deadline, FallbackDelay: -1}
	raw, err := d.Dial("tcp", c.BaseURL.Host)
	if err != nil {
		return 0, nil, err
	}
	defer raw.Close()
	if err := raw.SetDeadline(deadline); err != nil {
		return 0, nil, err
	}
	cfg := c.TLS.Clone()
	if cfg.ServerName == "" {
		cfg.ServerName = c.BaseURL.Hostname()
	}
	conn := tls.Client(raw, cfg)
	if err := conn.Handshake(); err != nil {
		return 0, nil, err
	}
	u := *c.BaseURL
	u.Path = c.BaseURL.Path + path
	if i := strings.Index(path, "?"); i >= 0 {
		u.Path = c.BaseURL.Path + path[:i]
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
		return 0, nil, err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, MaxRequestBody))
	if err != nil {
		return resp.StatusCode, nil, err
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
