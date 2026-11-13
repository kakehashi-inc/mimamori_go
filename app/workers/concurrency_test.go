package workers

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"mimamori/app/models"
	"mimamori/app/modules"
)

// concClient sends requests to both test servers over real connections
// (persistent where the server keeps them).
type concClient struct {
	s      *serveEnv
	web    *http.Client
	agent  *http.Client
	cookie string // session of the administrator on the browser listener
}

func newConcClient(t *testing.T, s *serveEnv) *concClient {
	t.Helper()
	tr := func() *http.Transport {
		return &http.Transport{TLSClientConfig: testClientTLS(), MaxIdleConnsPerHost: 64, ForceAttemptHTTP2: false}
	}
	c := &concClient{s: s, web: &http.Client{Transport: tr()}, agent: &http.Client{Transport: tr()}}
	t.Cleanup(func() {
		c.web.CloseIdleConnections()
		c.agent.CloseIdleConnections()
	})
	code, body, ck := c.login(testAdminEmail, "Admin-pass-123")
	if code != http.StatusOK || ck == "" {
		t.Fatalf("login: %d %s", code, body)
	}
	c.cookie = ck
	return c
}

// do sends one request; the answer's status, body and session cookie.
func (c *concClient) do(cl *http.Client, method, url string, body any, hdr map[string]string) (int, string, string) {
	var r io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		r = bytes.NewReader(b)
	}
	req, err := http.NewRequest(method, url, r)
	if err != nil {
		return 0, err.Error(), ""
	}
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	resp, err := cl.Do(req)
	if err != nil {
		return 0, err.Error(), ""
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	ck := ""
	for _, x := range resp.Cookies() {
		if x.Name == sessionCookie {
			ck = x.Value
		}
	}
	return resp.StatusCode, string(b), ck
}

func (c *concClient) login(email, pass string) (int, string, string) {
	return c.do(c.web, "POST", "http://"+c.s.webAddr+"/api/v1/auth/login", map[string]string{"email": email, "password": pass}, map[string]string{csrfHeader: "1"})
}

// browser calls the browser API as the administrator.
func (c *concClient) browser(method, path string, body any) (int, string) {
	code, b, _ := c.do(c.web, method, "http://"+c.s.webAddr+"/api/v1"+path, body, map[string]string{"Cookie": sessionCookie + "=" + c.cookie, csrfHeader: "1"})
	return code, b
}

// management calls the management API on the agent listener.
func (c *concClient) management(method, path string, body any) (int, string) {
	code, b, _ := c.do(c.agent, method, "https://"+c.s.agentAddr+"/admin/v1"+path, body, map[string]string{"Authorization": "Bearer " + c.s.e.cliKey})
	return code, b
}

// agentCall calls the agent API as an agent.
func (c *concClient) agentCall(path, token, machine string, body any) (int, string) {
	code, b, _ := c.do(c.agent, "POST", "https://"+c.s.agentAddr+"/agent/v1/"+path, body, map[string]string{"Authorization": "Bearer " + token, modules.HeaderInstanceID: machine})
	return code, b
}

// concAgent is an agent of the test: its token, instance ID and row ID.
type concAgent struct {
	token, machine string
	id             int64
}

// issueAgents issues n tokens and makes each an agent by its first sync.
func issueAgents(t *testing.T, c *concClient, prefix string, n int) []concAgent {
	t.Helper()
	out := make([]concAgent, n)
	for i := range n {
		code, body := c.management("POST", "/tokens", map[string]any{"name": fmt.Sprintf("%s-%d", prefix, i)})
		if code != http.StatusOK {
			t.Fatalf("issue %d: %d %s", i, code, body)
		}
		var issued struct {
			Value string `json:"value"`
			Token struct {
				ID int64 `json:"id"`
			} `json:"token"`
		}
		_ = json.Unmarshal([]byte(body), &issued)
		out[i] = concAgent{token: issued.Value, machine: fmt.Sprintf("m-%s-%d", prefix, i)}
		if code, body := c.agentCall("sync", out[i].token, out[i].machine, modules.SyncRequest{AgentVersion: "test", RunningRequests: []int64{}}); code != http.StatusOK {
			t.Fatalf("first sync %d: %d %s", i, code, body)
		}
		a, err := models.GetAgentByToken(c.s.e.c.db.Read(), issued.Token.ID)
		if err != nil {
			t.Fatal(err)
		}
		out[i].id = a.ID
	}
	return out
}

// detectionRows counts the detections stored in an agent's database.
func detectionRows(t *testing.T, cr *core, id int64) int {
	t.Helper()
	a, err := models.GetAgent(cr.db.Read(), id)
	if err != nil {
		t.Fatal(err)
	}
	tdb, err := cr.targetDB(a)
	if err != nil {
		t.Fatal(err)
	}
	var n int
	if err := tdb.Read().QueryRow(`SELECT COUNT(*) FROM detections`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// Handlers of every kind run in any number at the same moment over the
// real servers (SPEC S8): 64 flows send reports and syncs of several
// agents (also the built-in agent's direct calls), acknowledge detections,
// create and delete suppressions, list, show the overview, log in and
// change the configuration, while further flows run every periodic step
// again and again. No request fails, no history row is lost, nothing
// panics, every accepted report is stored, and the race detector stays
// silent (go test -race).
func TestHandlersOfAllKindsAtOnce(t *testing.T) {
	s := startServe(t, serveOpts{idle: 10 * time.Second})
	cr := s.e.c
	cr.logger = log.New(s.logs, "", 0)
	c := newConcClient(t, s)
	const nAgents = 6
	agents := issueAgents(t, c, "all", nAgents)
	builtin, err := models.GetBuiltinAgent(cr.db.Read())
	if err != nil {
		t.Fatal(err)
	}
	inProc := &inProcessTransport{c: cr, agentID: builtin.ID}
	builtinBefore := detectionRows(t, cr, builtin.ID)

	deadline := time.Now().Add(3 * time.Second)
	var mu sync.Mutex
	var failures []string
	fail := func(what string, code int, body string) {
		mu.Lock()
		failures = append(failures, fmt.Sprintf("%s: %d %s", what, code, body))
		mu.Unlock()
	}
	stored := make([]atomic.Int64, nAgents)
	var builtinStored atomic.Int64
	var rounds sync.Map // kind -> *atomic.Int64
	count := func(kind string) {
		v, _ := rounds.LoadOrStore(kind, &atomic.Int64{})
		v.(*atomic.Int64).Add(1)
	}
	var wg sync.WaitGroup
	flows := 0
	run := func(kind string, n int, f func(flow, round int)) {
		for flow := range n {
			flows++
			wg.Add(1)
			go func() {
				defer wg.Done()
				for round := 0; time.Now().Before(deadline); round++ {
					f(flow, round)
					count(kind)
				}
			}()
		}
	}
	detection := func(kind string, flow, round int) modules.DetectionReport {
		now := time.Now().Unix()
		return modules.DetectionReport{Type: "exec", Category: "normal", OSUser: "u", Exe: "/bin/x",
			Target: fmt.Sprintf("/%s/%d/%d", kind, flow, round), Count: 1, FirstAt: now, LastAt: now}
	}
	run("report", 24, func(flow, round int) {
		i := flow % nAgents
		rep := modules.ReportRequest{Detections: []modules.DetectionReport{detection("report", flow, round)},
			HostInfo: &modules.HostInfo{Hostname: fmt.Sprintf("host-%d", i), OSName: "Test", OSVersion: "1"}}
		if code, body := c.agentCall("report", agents[i].token, agents[i].machine, rep); code != http.StatusOK {
			fail("report", code, body)
			return
		}
		stored[i].Add(1)
	})
	run("sync", 8, func(flow, round int) {
		i := (flow + round) % nAgents
		if code, body := c.agentCall("sync", agents[i].token, agents[i].machine, modules.SyncRequest{AgentVersion: "test", RunningRequests: []int64{}}); code != http.StatusOK {
			fail("sync", code, body)
		}
	})
	run("built-in agent", 2, func(flow, round int) {
		if flow == 0 {
			if err := inProc.Report(&modules.ReportRequest{Detections: []modules.DetectionReport{detection("builtin", flow, round)}}); err != nil {
				fail("built-in report: "+err.Error(), 0, "")
				return
			}
			builtinStored.Add(1)
			return
		}
		if _, err := inProc.Sync(&modules.SyncRequest{AgentVersion: "test", RunningRequests: []int64{}}); err != nil {
			fail("built-in sync: "+err.Error(), 0, "")
		}
	})
	run("acknowledge", 6, func(flow, round int) {
		code, body := c.management("GET", fmt.Sprintf("/detections?agent_id=%d&limit=5", agents[flow%nAgents].id), nil)
		if code != http.StatusOK {
			fail("list for acknowledge", code, body)
			return
		}
		var out struct {
			Items []struct {
				ID      int64 `json:"id"`
				AgentID int64 `json:"agent_id"`
			} `json:"items"`
		}
		_ = json.Unmarshal([]byte(body), &out)
		if code, body := c.management("POST", "/detections/ack", map[string]any{"items": out.Items}); code != http.StatusOK {
			fail("acknowledge", code, body)
		}
	})
	run("suppression", 4, func(flow, round int) {
		code, body := c.browser("POST", "/suppressions", map[string]any{"type": "exec", "target": fmt.Sprintf("/none/%d/%d", flow, round)})
		if code != http.StatusOK {
			fail("create suppression", code, body)
			return
		}
		var sup struct {
			ID int64 `json:"id"`
		}
		_ = json.Unmarshal([]byte(body), &sup)
		if code, body := c.browser("DELETE", "/suppressions/"+itoa64(sup.ID), nil); code != http.StatusOK {
			fail("delete suppression", code, body)
		}
	})
	run("list", 6, func(flow, round int) {
		path := []string{"/detections?limit=20", "/agents", "/history?limit=20"}[(flow+round)%3]
		if code, body := c.browser("GET", path, nil); code != http.StatusOK {
			fail("list "+path, code, body)
		}
	})
	run("overview", 4, func(flow, round int) {
		if code, body := c.browser("GET", "/overview", nil); code != http.StatusOK {
			fail("overview", code, body)
		}
	})
	run("login", 2, func(flow, round int) {
		if code, body, ck := c.login(testAdminEmail, "Admin-pass-123"); code != http.StatusOK || ck == "" {
			fail("login", code, body)
		}
	})
	run("configuration", 2, func(flow, round int) {
		days := fmt.Sprint(30 + (flow+round)%5)
		if code, body := c.browser("PUT", "/config", map[string]any{"set": map[string]string{"retention_days": days}}); code != http.StatusOK {
			fail("configuration", code, body)
		}
	})
	for _, step := range cr.periodicSteps() {
		run("periodic "+step.name, 1, func(flow, round int) {
			if step.name == "retention" {
				// The stored mark lets retention act once an hour; it is
				// set back so that it acts at every run here.
				if err := s.e.exec(`UPDATE settings SET value = '0' WHERE key = ?`, settingLastRetentionAt); err != nil {
					fail("retention mark: "+err.Error(), 0, "")
				}
			}
			cr.runStep(step, time.Now())
		})
	}
	wg.Wait()
	if flows < 64 {
		t.Fatalf("only %d flows", flows)
	}

	if len(failures) > 0 {
		t.Fatalf("%d failures, first: %s", len(failures), failures[0])
	}
	if l := s.logs.String(); strings.Contains(l, "history:") || strings.Contains(l, "panic") || strings.Contains(l, "locked") {
		t.Fatalf("log: %s", l)
	}
	rounds.Range(func(k, v any) bool {
		if v.(*atomic.Int64).Load() == 0 {
			t.Errorf("%s never ran", k)
		}
		return true
	})
	for i, a := range agents {
		if n := detectionRows(t, cr, a.id); int64(n) != stored[i].Load() {
			t.Fatalf("agent %d: %d detections stored, %d reports accepted", i, n, stored[i].Load())
		}
	}
	if n := detectionRows(t, cr, builtin.ID) - builtinBefore; int64(n) != builtinStored.Load() {
		t.Fatalf("built-in agent: %d detections stored, %d reports made", n, builtinStored.Load())
	}
	if got := cr.conf().RetentionDays; got < 30 || got > 34 {
		t.Fatalf("configuration in memory: retention_days %d", got)
	}
}

// The reports of one agent are stored one after another and the reports of
// different agents at the same time: while the lock of one agent is held,
// its report waits and a report of another agent is answered; overlapping
// reports of one agent with the same changed application result record it
// once; no lock is left behind.
func TestReportsOfOneAgentSerialised(t *testing.T) {
	s := startServe(t, serveOpts{idle: 10 * time.Second})
	cr := s.e.c
	c := newConcClient(t, s)
	agents := issueAgents(t, c, "one", 2)
	rep := func(target string) modules.ReportRequest {
		now := time.Now().Unix()
		return modules.ReportRequest{Detections: []modules.DetectionReport{{Type: "exec", Category: "normal", OSUser: "u", Exe: "/bin/x",
			Target: target, Count: 1, FirstAt: now, LastAt: now}}}
	}

	unlock := cr.reportLocks.lock(agents[0].id)
	held := make(chan int, 1)
	go func() {
		code, _ := c.agentCall("report", agents[0].token, agents[0].machine, rep("/held"))
		held <- code
	}()
	if code, body := c.agentCall("report", agents[1].token, agents[1].machine, rep("/other")); code != http.StatusOK {
		t.Fatalf("report of another agent while the lock is held: %d %s", code, body)
	}
	select {
	case code := <-held:
		t.Fatalf("report answered %d while its agent's lock was held", code)
	case <-time.After(300 * time.Millisecond):
	}
	if n := detectionRows(t, cr, agents[0].id); n != 0 {
		t.Fatalf("%d detections stored while the lock was held", n)
	}
	unlock()
	select {
	case code := <-held:
		if code != http.StatusOK {
			t.Fatalf("report after the lock was given back: %d", code)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("report not answered after the lock was given back")
	}

	at := time.Now().Unix()
	const flows = 8
	codes := make([]int, flows)
	var wg sync.WaitGroup
	for i := range flows {
		wg.Add(1)
		go func() {
			defer wg.Done()
			r := rep(fmt.Sprintf("/apply/%d", i))
			r.Apply = &modules.ApplyResult{Status: modules.ApplyStatusNone, Message: "nothing applied", At: at}
			codes[i], _ = c.agentCall("report", agents[0].token, agents[0].machine, r)
		}()
	}
	wg.Wait()
	for i, code := range codes {
		if code != http.StatusOK {
			t.Fatalf("report %d: %d", i, code)
		}
	}
	items, _, err := models.ListHistory(cr.db.Read(), models.HistRule, "", nil, 100, 0)
	if err != nil {
		t.Fatal(err)
	}
	n := 0
	for _, h := range items {
		if h.Action == "apply_result_"+modules.ApplyStatusNone {
			n++
		}
	}
	if n != 1 {
		t.Fatalf("application result recorded %d times", n)
	}
	if got := detectionRows(t, cr, agents[0].id); got != 1+flows {
		t.Fatalf("%d detections stored, want %d", got, 1+flows)
	}
	cr.reportLocks.mu.Lock()
	left := len(cr.reportLocks.locks)
	cr.reportLocks.mu.Unlock()
	if left != 0 {
		t.Fatalf("%d report locks left", left)
	}
}
