package modules

import (
	"bufio"
	"bytes"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"errors"
	"fmt"
	"io"
	"log"
	"math/big"
	"net"
	"net/http"
	"os"
	"strings"
	"syscall"
	"testing"
	"time"
)

// The client tests run over real loopback sockets with TLS and an idle time
// of 1 s instead of IdleTimeout. Times have a generous margin above, because
// other tests may run on the machine at the same time.

const clientIdle = time.Second

// clientTestTLS returns the server's and the client's TLS configuration of a
// self-signed certificate for 127.0.0.1.
func clientTestTLS(t *testing.T) (server, client *tls.Config) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "test"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour),
		KeyUsage: x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign, IsCA: true, BasicConstraintsValid: true,
		IPAddresses: []net.IP{net.IPv4(127, 0, 0, 1)}}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	pool := x509.NewCertPool()
	pool.AddCert(cert)
	return &tls.Config{Certificates: []tls.Certificate{{Certificate: [][]byte{der}, PrivateKey: key}}, MinVersion: tls.VersionTLS12},
		&tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12}
}

// clientTestListener returns a loopback listener whose accepted sockets have
// the given receive buffer (0: the default).
func clientTestListener(t *testing.T, rcvbuf int) *net.TCPListener {
	t.Helper()
	ln, err := net.ListenTCP("tcp", &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	if rcvbuf > 0 {
		rc, err := ln.SyscallConn()
		if err != nil {
			t.Fatal(err)
		}
		_ = rc.Control(func(fd uintptr) { _ = syscall.SetsockoptInt(int(fd), syscall.SOL_SOCKET, syscall.SO_RCVBUF, rcvbuf) })
	}
	t.Cleanup(func() { ln.Close() })
	return ln
}

// serveOne accepts one connection, performs the TLS handshake, reads the
// request head and gives the connection and the request to handle.
func serveOne(t *testing.T, ln net.Listener, cfg *tls.Config, handle func(c *tls.Conn, req *http.Request)) {
	t.Helper()
	go func() {
		raw, err := ln.Accept()
		if err != nil {
			return
		}
		defer raw.Close()
		c := tls.Server(raw, cfg)
		req, err := http.ReadRequest(bufio.NewReaderSize(c, 512))
		if err != nil {
			return
		}
		handle(c, req)
	}()
}

func testClient(ln net.Listener, cfg *tls.Config) *HTTPSClient {
	return &HTTPSClient{Address: ln.Addr().String(), TLS: cfg, Headers: map[string]string{}, idle: clientIdle}
}

func TestClientSilentServerCut(t *testing.T) {
	sc, cc := clientTestTLS(t)
	ln := clientTestListener(t, 0)
	serveOne(t, ln, sc, func(c *tls.Conn, req *http.Request) {
		_, _ = io.Copy(io.Discard, req.Body)
		time.Sleep(3 * clientIdle)
		_, _ = c.Write([]byte("HTTP/1.1 200 OK\r\nContent-Length: 2\r\nConnection: close\r\n\r\n{}"))
	})
	start := time.Now()
	_, _, err := testClient(ln, cc).Do("POST", "/agent/v1/report", []byte(`{"x":1}`))
	within(t, "client cut", time.Since(start), clientIdle-200*time.Millisecond, clientIdle+800*time.Millisecond)
	if err == nil || !IsSilenceCut(err) {
		t.Fatalf("want a silence cut, got %v", err)
	}
}

func TestClientSlowResponseNotCut(t *testing.T) {
	sc, cc := clientTestTLS(t)
	ln := clientTestListener(t, 0)
	body := bytes.Repeat([]byte("0123456789abcdef"), 4)
	serveOne(t, ln, sc, func(c *tls.Conn, req *http.Request) {
		_, _ = io.Copy(io.Discard, req.Body)
		head := fmt.Sprintf("HTTP/1.1 200 OK\r\nContent-Length: %d\r\nConnection: close\r\n\r\n", len(body))
		pieces := [][]byte{[]byte(head[:9]), []byte(head[9:])}
		for i := 0; i < len(body); i += 11 {
			pieces = append(pieces, body[i:min(i+11, len(body))])
		}
		// 8 pieces, one every 0.5 s: 4 s.
		for _, p := range pieces {
			time.Sleep(clientIdle / 2)
			if _, err := c.Write(p); err != nil {
				return
			}
		}
	})
	start := time.Now()
	status, data, err := testClient(ln, cc).Do("POST", "/agent/v1/sync", []byte(`{}`))
	if err != nil || status != 200 || !bytes.Equal(data, body) {
		t.Fatalf("want the whole answer: %d %q %v", status, data, err)
	}
	if el := time.Since(start); el < 4*clientIdle-100*time.Millisecond {
		t.Fatalf("the answer took only %v, the test proves nothing", el)
	}
}

func TestClientSlowUploadNotCut(t *testing.T) {
	sc, cc := clientTestTLS(t)
	ln := clientTestListener(t, 64<<10)
	const size = 8 << 20
	got := make(chan int64, 1)
	serveOne(t, ln, sc, func(c *tls.Conn, req *http.Request) {
		// About 200 KiB per second for 4 s, then at full speed.
		buf := make([]byte, 20<<10)
		total := int64(0)
		end := time.Now().Add(4 * clientIdle)
		for time.Now().Before(end) {
			n, err := io.ReadFull(req.Body, buf)
			total += int64(n)
			if err != nil {
				break
			}
			time.Sleep(100 * time.Millisecond)
		}
		rest, _ := io.Copy(io.Discard, req.Body)
		got <- total + rest
		_, _ = c.Write([]byte("HTTP/1.1 200 OK\r\nContent-Length: 2\r\nConnection: close\r\n\r\n{}"))
	})
	body := make([]byte, size)
	_, _ = rand.Read(body)
	start := time.Now()
	status, _, err := testClient(ln, cc).Do("POST", "/agent/v1/report", body)
	if err != nil || status != 200 {
		t.Fatalf("the upload must complete: %d %v after %v", status, err, time.Since(start))
	}
	if el := time.Since(start); el < 4*clientIdle {
		t.Fatalf("the upload took only %v, the test proves nothing", el)
	}
	if n := <-got; n != size {
		t.Fatalf("the server read %d bytes", n)
	}
}

func TestClientSilentHandshakeCut(t *testing.T) {
	_, cc := clientTestTLS(t)
	ln := clientTestListener(t, 0)
	go func() {
		raw, err := ln.Accept()
		if err != nil {
			return
		}
		time.Sleep(3 * clientIdle)
		raw.Close()
	}()
	start := time.Now()
	_, _, err := testClient(ln, cc).Do("GET", "/agent/v1/ca", nil)
	within(t, "handshake cut", time.Since(start), clientIdle-200*time.Millisecond, clientIdle+800*time.Millisecond)
	if !IsSilenceCut(err) {
		t.Fatalf("want a silence cut, got %v", err)
	}
}

func TestClientLargeAnswerLimit(t *testing.T) {
	sc, cc := clientTestTLS(t)
	ln := clientTestListener(t, 0)
	const extra = 1000
	serveOne(t, ln, sc, func(c *tls.Conn, req *http.Request) {
		_, _ = io.Copy(io.Discard, req.Body)
		bw := bufio.NewWriter(c)
		_, _ = fmt.Fprintf(bw, "HTTP/1.1 200 OK\r\nContent-Length: %d\r\nConnection: close\r\n\r\n", MaxRequestBody+extra)
		chunk := bytes.Repeat([]byte{'a'}, 64<<10)
		for left := MaxRequestBody + extra; left > 0; left -= len(chunk) {
			if _, err := bw.Write(chunk[:min(left, len(chunk))]); err != nil {
				return
			}
		}
		_ = bw.Flush()
	})
	status, data, err := testClient(ln, cc).Do("GET", "/x", nil)
	if err != nil || status != 200 || len(data) != MaxRequestBody {
		t.Fatalf("want the answer cut at the limit: %d, %d bytes, %v", status, len(data), err)
	}
}

// ctl and the agent's log name the silence; ctl keeps exit code 5 and says
// that the central may still carry out the request.
func TestSilenceMessages(t *testing.T) {
	sc, cc := clientTestTLS(t)
	ln := clientTestListener(t, 0)
	serveOne(t, ln, sc, func(c *tls.Conn, req *http.Request) {
		_, _ = io.Copy(io.Discard, req.Body)
		time.Sleep(3 * clientIdle)
	})
	cl := &ctlClient{http: testClient(ln, cc)}
	_, err := cl.call("GET", "/admin/v1/agents", nil)
	var ee *ExitError
	if !errors.As(err, &ee) || ee.Code != ExitExec {
		t.Fatalf("want exit code %d, got %v", ExitExec, err)
	}
	if !strings.HasPrefix(ee.Message, "the central sent nothing for 60 seconds; it may still carry out the request: ") {
		t.Fatalf("message: %q", ee.Message)
	}

	var logged strings.Builder
	rt := &AgentRuntime{Logger: log.New(&logged, "", 0)}
	cut := &net.OpError{Op: "read", Net: "tcp", Err: os.ErrDeadlineExceeded}
	rt.logConnectionError("report", cut)
	rt.logConnectionError("sync", cut)
	if logged.String() != "report: the central sent nothing for 60 seconds: read tcp: i/o timeout\n"+
		"sync: the central sent nothing for 60 seconds: read tcp: i/o timeout\n" {
		t.Fatalf("log: %q", logged.String())
	}
}
