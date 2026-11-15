package workers

import (
	"net/http/httptest"
	"testing"
	"time"

	"mimamori/app/models"
)

// The token list flags a token close to its expiry whose agent is not
// connecting (expiring_unconnected). For a token whose agent synced, "not
// connecting" is judged with the check interval that agent's last sync
// answer gave it (agents.check_interval_sec), as whether it responds: three
// intervals and 600 seconds since the token was last seen. A token no agent
// has synced with yet is judged with default_check_interval_sec.
//
// The edges are judged at one fixed moment (tokenOut with now); the API is
// asked for cases far from an edge, as its answer judges at its own moment.
func TestTokenExpiringUnconnectedByAgentInterval(t *testing.T) {
	e := newTestEnv(t)
	insertRule(t, e, 950, 3600)
	setDefaultInterval(e, 60)
	now := time.Now().Unix()
	// seen makes the token expire in an hour (near its expiry) and sets when
	// it was last seen.
	seen := func(tokenID, lastSeen int64) {
		t.Helper()
		if err := e.exec(`UPDATE tokens SET expires_at = ?, last_seen_at = ? WHERE id = ?`, now+3600, lastSeen, tokenID); err != nil {
			t.Fatal(err)
		}
	}
	// flagged gives the flag as tokenOut judges it at now.
	flagged := func(tokenID, lastSeen int64) bool {
		t.Helper()
		seen(tokenID, lastSeen)
		tok, err := models.GetToken(e.c.db.Read(), tokenID)
		if err != nil {
			t.Fatal(err)
		}
		return e.c.tokenOut(httptest.NewRequest("GET", "/admin/v1/tokens/"+itoa64(tokenID), nil), tok, now).ExpiringUnconnected
	}
	// listed gives the flag as the API answers it.
	listed := func(tokenID, lastSeen int64) bool {
		t.Helper()
		seen(tokenID, lastSeen)
		v, ok := e.admin("GET", "/tokens/"+itoa64(tokenID), nil)["expiring_unconnected"].(bool)
		if !ok {
			t.Fatal("expiring_unconnected missing")
		}
		return v
	}

	// An agent with a rule of 3600 s: its token last seen 1000 s ago is not
	// flagged, nor at three intervals and 600 s; one second later it is.
	A := newQueueAgent(t, e, "unconnected-rule", 0)
	e.admin("POST", "/rules/950/apply", map[string]any{"agent_ids": []int64{A.a.ID}})
	syncFree(t, e, A.tok)
	for _, tc := range []struct {
		ago  int64
		want bool
	}{{1000, false}, {3*3600 + 600, false}, {3*3600 + 601, true}} {
		if got := flagged(A.a.TokenID.Int64, now-tc.ago); got != tc.want {
			t.Fatalf("agent with a rule of 3600 s, token last seen %d s ago: flagged %v, want %v", tc.ago, got, tc.want)
		}
	}
	// The API judges with the agent's interval too: 1000 s, which the
	// default of 60 s would flag, is not flagged; a day is.
	for _, tc := range []struct {
		ago  int64
		want bool
	}{{1000, false}, {86400, true}} {
		if got := listed(A.a.TokenID.Int64, time.Now().Unix()-tc.ago); got != tc.want {
			t.Fatalf("API, agent with a rule of 3600 s, token last seen %d s ago: flagged %v, want %v", tc.ago, got, tc.want)
		}
	}

	// A token no agent has synced with: default_check_interval_sec (60 s).
	B := newQueueAgent(t, e, "unconnected-new", 0)
	for _, tc := range []struct {
		ago  int64
		want bool
	}{{700, false}, {3*60 + 600, false}, {3*60 + 601, true}, {1000, true}} {
		if got := flagged(B.a.TokenID.Int64, now-tc.ago); got != tc.want {
			t.Fatalf("token without a synced agent, last seen %d s ago: flagged %v, want %v", tc.ago, got, tc.want)
		}
	}
	for _, tc := range []struct {
		ago  int64
		want bool
	}{{600, false}, {1000, true}} {
		if got := listed(B.a.TokenID.Int64, time.Now().Unix()-tc.ago); got != tc.want {
			t.Fatalf("API, token without a synced agent, last seen %d s ago: flagged %v, want %v", tc.ago, got, tc.want)
		}
	}
	// Never seen: flagged.
	if !flagged(B.a.TokenID.Int64, 0) || !listed(B.a.TokenID.Int64, 0) {
		t.Fatal("a token never seen and close to its expiry must be flagged")
	}
	// Far from its expiry: not flagged, whatever the agent does.
	if err := e.exec(`UPDATE tokens SET expires_at = ?, last_seen_at = 0 WHERE id = ?`, now+400*86400, B.a.TokenID.Int64); err != nil {
		t.Fatal(err)
	}
	if v := e.admin("GET", "/tokens/"+itoa64(B.a.TokenID.Int64), nil)["expiring_unconnected"]; v != false {
		t.Fatalf("a token far from its expiry: %v", v)
	}
}
