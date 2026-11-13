package workers

import (
	"context"
	"crypto/tls"
	"log"
	"net"
	"net/http"

	"mimamori/app/modules"
)

// connContextKey is the context key of a request's connection
// (*modules.IdleConn), which the request gate pauses while the handler
// works.
type connContextKey struct{}

// connOf returns the connection a request arrived on (nil when it did not
// arrive through a connLimit listener).
func connOf(ctx context.Context) *modules.IdleConn {
	c, _ := ctx.Value(connContextKey{}).(*modules.IdleConn)
	return c
}

// newHTTPServer builds the server of one listener: handler behind the
// request gate (with the listener's pre-check admit and the process's body
// budget), HTTP/1.1 only, TLS when tlsCfg is set (served with ServeTLS).
// The server sets no time limit of its own: time is governed by the
// connections' idle time alone (modules.IdleConn, given by the listener of
// connLimit). Handlers learn the peer address (r.RemoteAddr), the local
// address (http.LocalAddrContextKey) and the TLS state (r.TLS) from the
// library.
func newHTTPServer(handler http.Handler, admit func(*http.Request) bool, tlsCfg *tls.Config, budget *bodyBudget, logger *log.Logger) *http.Server {
	protocols := new(http.Protocols)
	protocols.SetHTTP1(true)
	return &http.Server{
		Handler:        &requestGate{next: handler, admit: admit, budget: budget, logger: logger},
		MaxHeaderBytes: modules.MaxHeadBytes,
		// More header lines than a head of MaxHeadBytes can hold: only the
		// size of the head decides.
		MaxHeaderValueCount:          modules.MaxHeadBytes,
		TLSConfig:                    tlsCfg,
		Protocols:                    protocols,
		ErrorLog:                     logger,
		DisableGeneralOptionsHandler: true,
		ConnContext: func(ctx context.Context, c net.Conn) context.Context {
			if tc, ok := c.(*tls.Conn); ok {
				c = tc.NetConn()
			}
			ic, _ := c.(*modules.IdleConn)
			return context.WithValue(ctx, connContextKey{}, ic)
		},
	}
}
