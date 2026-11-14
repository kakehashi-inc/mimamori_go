package workers

import (
	"net/http"
	"strings"
	"testing"
)

// Integers in API paths and queries are read as the number rule says
// (modules.ParseWholeNumber): leading zeros are decimal and give the same
// answer as the plain number; a sign or another spelling is no number and
// gets the answer every other text that is no number gets there (404 for
// an id in a path, 400 with the parameter's message for a query).
func TestAPINumberSpelling(t *testing.T) {
	e := newTestEnv(t)
	rule := e.admin("POST", "/rules", map[string]any{"name": "web", "templates": []string{"base"}})
	ruleID := itoa64(int64(rule["id"].(float64)))
	_, a, _ := commandAgent(t, e, "host-n")
	agent := "/agents/" + itoa64(a.ID)
	set := commandSetBody("s1", "run_only")
	set["os_user"] = "svc"
	setID := itoa64(int64(e.admin("POST", agent+"/command-sets", set)["id"].(float64)))
	tokenID := itoa64(a.TokenID.Int64)

	for _, tc := range []struct {
		path      string // with % in place of the number
		number    string // the plain number
		status    int    // the answer to a text that is no number
		code      string
		postValid bool // POST instead of GET
	}{
		{"/agents/%", itoa64(a.ID), 404, "agent.not_found", false},
		{"/tokens/%", tokenID, 404, "token.not_found", false},
		{"/rules/%", ruleID, 404, "rule.not_found", false},
		{"/rules/" + ruleID + "/versions/%", "1", 404, "rule.version_not_found", false},
		{"/rules/" + ruleID + "/diff?from=%&to=1", "1", 400, "rule.diff_from_to", false},
		{"/rules/" + ruleID + "/diff?from=1&to=%", "1", 400, "rule.diff_from_to", false},
		{agent + "/command-sets/%", setID, 404, "command.set_not_found", false},
		{agent + "/command-sets/%/runs", setID, 404, "command.set_not_found", false},
		{agent + "/command-sets/" + setID + "/runs?limit=%", "5", 400, "common.query_not_integer", false},
		{agent + "/command-sets/" + setID + "/runs?before=%", "5", 400, "common.query_not_integer", false},
		{"/history?limit=%", "5", 400, "common.query_not_integer", false},
		{"/history?offset=%", "5", 400, "common.query_not_integer", false},
		{"/detections?limit=%", "5", 400, "common.query_not_integer", false},
		{"/detections?offset=%", "5", 400, "common.query_not_integer", false},
		{"/detections?agent_id=%", itoa64(a.ID), 400, "common.query_agent_id", false},
		{agent + "/command-sets/validate?set_id=%", setID, 400, "common.query_not_integer", true},
	} {
		method := "GET"
		var body any
		if tc.postValid {
			method, body = "POST", set
		}
		call := func(n string) (int, string) {
			path := strings.Replace(tc.path, "%", n, 1)
			rec := e.cli(method, path, body)
			return rec.Code, rec.Body.String()
		}
		for _, n := range []string{tc.number, "0" + tc.number, "00" + tc.number} {
			if st, out := call(n); st != http.StatusOK {
				t.Errorf("%s with %s: %d %s", tc.path, n, st, out)
			}
		}
		for _, n := range []string{"x", "%2B" + tc.number, "+" + tc.number, "%20" + tc.number, tc.number + "%20", "0x1", "1_0", "%EF%BC%91"} {
			st, out := call(n)
			if st != tc.status || !strings.Contains(out, `"code":"`+tc.code+`"`) {
				t.Errorf("%s with %q: %d %s, want %d %s", tc.path, n, st, out, tc.status, tc.code)
			}
		}
	}
}

// The guide gives the port of agent_listen read by the number rule: a
// stored port with leading zeros is the same port, a spelling that is no
// number gives 0, as any other port that is no number.
func TestGuidePortSpelling(t *testing.T) {
	e := newTestEnv(t)
	for listen, want := range map[string]float64{"127.0.0.1:09081": 9081, "127.0.0.1:9081": 9081, "127.0.0.1:+9081": 0, "127.0.0.1:x": 0} {
		cfg := *e.c.conf()
		cfg.AgentListen = listen
		e.c.cfg.Store(&cfg)
		if got := e.admin("GET", "/guide", nil)["central_port"]; got != want {
			t.Errorf("agent_listen %s: central_port %v, want %v", listen, got, want)
		}
	}
}
