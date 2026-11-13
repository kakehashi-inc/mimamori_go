package workers

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"

	"mimamori/app/models"
	"mimamori/app/modules"
)

// The management API is used only by a client of this host: a loopback
// source address, or a source address equal to the local address the
// connection was accepted on. Forwarding headers are never used.
func TestIsLocalPeer(t *testing.T) {
	local := &net.TCPAddr{IP: net.ParseIP("192.0.2.7"), Port: 9081}
	local6 := &net.TCPAddr{IP: net.ParseIP("2001:db8::7"), Port: 9081}
	mapped := &net.TCPAddr{IP: net.ParseIP("::ffff:192.0.2.7"), Port: 9081}
	cases := []struct {
		remote string
		local  net.Addr
		want   bool
	}{
		{"127.0.0.1:5000", nil, true},
		{"127.0.0.2:5000", local, true},
		{"[::1]:5000", nil, true},
		{"[::ffff:127.0.0.1]:5000", nil, true},
		{"192.0.2.7:5000", local, true},
		{"[::ffff:192.0.2.7]:5000", local, true},
		{"192.0.2.7:5000", mapped, true},
		{"[2001:db8::7]:5000", local6, true},
		{"192.0.2.8:5000", local, false},
		{"192.0.2.7:5000", nil, false},
		{"192.0.2.7:5000", local6, false},
		{"0.0.0.0:5000", &net.TCPAddr{IP: net.IPv4zero}, false},
		{"[fe80::7%eth0]:5000", &net.TCPAddr{IP: net.ParseIP("fe80::7"), Zone: "eth0"}, true},
		{"[fe80::7%eth1]:5000", &net.TCPAddr{IP: net.ParseIP("fe80::7"), Zone: "eth0"}, false},
		{"garbage", local, false},
	}
	for _, c := range cases {
		if got := isLocalPeer(c.remote, c.local); got != c.want {
			t.Errorf("isLocalPeer(%q, %v) = %v, want %v", c.remote, c.local, got, c.want)
		}
	}
}

// The management API answers a client that connected from the host's own
// address to that address, and stays hidden for every other peer.
func TestAdminAPIFromOwnAddress(t *testing.T) {
	e := newTestEnv(t)
	call := func(remote, local string) int {
		req, _ := http.NewRequest("GET", "/admin/v1/agents", nil)
		req.RemoteAddr = remote
		req.Header.Set("Authorization", "Bearer "+e.cliKey)
		addr, _ := net.ResolveTCPAddr("tcp", local)
		req = req.WithContext(context.WithValue(req.Context(), http.LocalAddrContextKey, addr))
		rec := httptest.NewRecorder()
		e.agent.ServeHTTP(rec, req)
		return rec.Code
	}
	if code := call("192.0.2.7:4000", "192.0.2.7:9081"); code != http.StatusOK {
		t.Fatalf("own address: status %d, want 200", code)
	}
	if code := call("192.0.2.8:4000", "192.0.2.7:9081"); code != http.StatusNotFound {
		t.Fatalf("other address: status %d, want 404", code)
	}
	if code := call("[::ffff:192.0.2.7]:4000", "192.0.2.7:9081"); code != http.StatusOK {
		t.Fatalf("IPv4-mapped own address: status %d, want 200", code)
	}
}

// The large-body checks change nothing: no token use, no binding, no
// session extension.
func TestAdmitBodyHasNoSideEffects(t *testing.T) {
	e := newTestEnv(t)
	tok, a, _ := commandAgent(t, e, "quiet-host")
	ck := login(t, e, testAdminEmail, "Admin-pass-123")
	sess, err := models.GetSession(e.c.db.Read(), modules.HashSecret(ck))
	if err != nil {
		t.Fatal(err)
	}
	if err := e.c.db.Write(func(tx models.Writer) error {
		return models.TouchSession(tx, sess.ID, sess.LastSeenAt-3600, sess.ExpiresAt)
	}); err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest("POST", "/agent/v1/report", nil)
	req.Header.Set("Authorization", "Bearer "+tok)
	req.Header.Set(modules.HeaderInstanceID, "m-quiet")
	if !e.c.admitAgentBody(req) {
		t.Fatal("valid token refused")
	}
	if tk, _ := models.GetToken(e.c.db.Read(), a.TokenID.Int64); tk.UsedAt != 0 || tk.InstanceID != "" || tk.LastSeenAt != 0 {
		t.Fatalf("the check recorded a token use: %+v", tk)
	}
	web := httptest.NewRequest("POST", "/api/v1/rules", nil)
	for k, v := range browserHeaders(ck) {
		web.Header.Set(k, v)
	}
	if !e.c.admitWebBody(web) {
		t.Fatal("valid session refused")
	}
	if after, _ := models.GetSession(e.c.db.Read(), sess.ID); after.LastSeenAt != sess.LastSeenAt-3600 || after.ExpiresAt != sess.ExpiresAt {
		t.Fatalf("the check extended the session: %+v", after)
	}
	cli := httptest.NewRequest("POST", "/admin/v1/rules", nil)
	cli.RemoteAddr = "192.0.2.9:4000"
	cli.Header.Set("Authorization", "Bearer "+e.cliKey)
	if e.c.admitAgentBody(cli) {
		t.Fatal("management request of another host admitted")
	}
}
