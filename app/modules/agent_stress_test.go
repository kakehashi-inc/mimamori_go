package modules

import (
	"encoding/json"
	"errors"
	"fmt"
	"math/rand"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"mimamori/app/models"
)

// A randomized stress run of the agent (AgentRuntime.Run, with rounds every
// few milliseconds: roundPace) against a fake central, for 1, 2 and 4
// workers: the central changes the definitions, hands over manual requests
// (also for running and waiting sets, with short expiries, for unknown
// sets), changes the rule assignment, loses requests and answers, and moves
// the agent's clock forward. The stop comes at a random moment, or after
// everything handed over was answered (the drain). The invariants of the
// queue (the file comment of agent_scheduler.go) are checked all the time on
// the queue and after every round on the records. MIMAMORI_STRESS_SEED gives
// the seed (by default the time; it is logged).

// stressClock is the agent's clock, moved forward by the central.
type stressClock struct{ off atomic.Int64 }

func (c *stressClock) now() time.Time       { return time.Now().Add(time.Duration(c.off.Load())) }
func (c *stressClock) jump(d time.Duration) { c.off.Add(int64(d)) }

// stressCentral is the fake central.
type stressCentral struct {
	mu  sync.Mutex
	rng *rand.Rand
	clk *stressClock
	dir string
	// quiet: nothing changes any more and nothing is lost (drain phase).
	quiet bool
	// calm: only new requests come (no other change, nothing lost), so that
	// a waiting worker is woken by a new entry or the end of a run only.
	calm bool
	// e1: requests of sets that are or may become scheduled may expire.
	e1 bool
	// burst is how many changes one sync brings (more with rounds every
	// second).
	burst int

	sets    []CommandSetDef
	sigN    int
	sig     string
	nextSet int64
	nextReq int64
	removed []int64

	queued    []CommandRunRequest
	delivered map[int64]CommandRunRequest
	handed    map[int64]CommandRunRequest
	acks      map[int64][]CommandRequestAck
	rule      *RuleAssignment
	ruleVer   int

	lastReportID string
	batches      int
	dupBatches   int
	runs         map[string][]CommandRunReport
	periodic     int
	syncs        int
	freeSyncs    int
	stoppings    int
	afterStop    []string
	onStopping   func() []string
	violations   []string
	lostRequests int
}

func newStressCentral(seed int64, clk *stressClock, dir string, e1 bool) *stressCentral {
	c := &stressCentral{rng: rand.New(rand.NewSource(seed)), clk: clk, dir: dir, e1: e1, nextSet: 1, nextReq: 1,
		delivered: map[int64]CommandRunRequest{}, handed: map[int64]CommandRunRequest{}, acks: map[int64][]CommandRequestAck{},
		runs: map[string][]CommandRunReport{}}
	for i := 0; i < 4; i++ {
		c.addSet()
	}
	c.resign()
	return c
}

func (c *stressCentral) resign() {
	c.sigN++
	c.sig = fmt.Sprintf("stress%d", c.sigN)
}

// neverScheduled: sets with these IDs stay manual for ever.
func stressNeverScheduled(id int64) bool { return id%3 == 0 }

// line returns one command line of set id (and its time limit).
func (c *stressCentral) line(id int64) (string, int) {
	lock := fmt.Sprintf(`if ! mkdir %[1]s/lock.%[2]d 2>/dev/null; then echo "OVERLAP %[2]d $$" >> %[1]s/overlap.log; fi; echo "S %[2]d $$ $EPOCHREALTIME" >> %[1]s/cmd.log; `, c.dir, id)
	unlock := fmt.Sprintf(`; rc=$?; echo "E %[2]d $$ $EPOCHREALTIME" >> %[1]s/cmd.log; rmdir %[1]s/lock.%[2]d; exit $rc`, c.dir, id)
	switch c.rng.Intn(16) {
	case 0:
		return lock + "sleep 0.3" + unlock, 0
	case 1:
		// Ignores SIGTERM: ended by SIGKILL after the grace at a stop.
		return lock + "trap '' TERM; sleep 0.5" + unlock, 0
	case 2:
		// Leaves a writer behind that holds the pipe for a moment.
		return lock + "( sleep 0.15; echo late ) & echo main" + unlock, 0
	case 3:
		return lock + `head -c 200000 /dev/zero | tr '\0' x` + unlock, 0
	case 4:
		return lock + "( exit 3 )" + unlock, 0
	case 5:
		// Reaches its time limit (no lock: it is ended before it unlocks).
		return fmt.Sprintf(`echo "S %[2]d $$ $EPOCHREALTIME" >> %[1]s/cmd.log; sleep 30`, c.dir, id), 1
	case 6:
		if c.rng.Intn(3) == 0 {
			// Leaves a silent holder of the pipe behind for longer than the
			// reading waits (the pipe is kept and read by its own reader).
			return lock + "( exec -a stressleft sleep 1.2 ) & echo main" + unlock, 0
		}
	}
	if c.rng.Intn(2) == 0 {
		return lock + ":" + unlock, 0
	}
	return lock + fmt.Sprintf("sleep 0.0%d", 1+c.rng.Intn(8)) + unlock, 0
}

// newSetDef returns a definition for set id.
func (c *stressCentral) newSetDef(id, revision int64) CommandSetDef {
	n := 1 + c.rng.Intn(3)
	s := CommandSetSpec{Name: fmt.Sprintf("s%d", id), Purpose: PurposeRunOnly, OSUser: agentUserName(), WorkDir: "/"}
	for i := 0; i < n; i++ {
		l, limit := c.line(id)
		s.Commands = append(s.Commands, CommandSpec{CommandLine: l, TimeoutSec: limit, OnProblems: ""})
	}
	if !stressNeverScheduled(id) {
		switch c.rng.Intn(6) {
		case 0, 1:
			s.Schedule = &CommandSchedule{Kind: ScheduleEveryMinutes, Every: []int{1, 2, 3}[c.rng.Intn(3)]}
			s.RunMode = RunModeSchedule
		case 2:
			at := c.clk.now().UTC().Add(time.Duration(1+c.rng.Intn(3)) * time.Minute)
			s.Schedule = &CommandSchedule{Kind: ScheduleDaily, Time: at.Format("15:04")}
			s.RunMode = RunModeSchedule
		case 3:
			// Refused by the agent: requests for it are answered unknown_set.
			// It stays refused (its ID is never used for a valid set again).
			s.Purpose = "refused"
		}
	}
	s.Normalize()
	return CommandSetDef{ID: id, Revision: revision, CommandSetSpec: s}
}

func (c *stressCentral) addSet() {
	c.sets = append(c.sets, c.newSetDef(c.nextSet, 1))
	c.nextSet++
}

// mutate changes something, as administrators and time do.
func (c *stressCentral) mutate() {
	switch c.rng.Intn(12) {
	case 0:
		c.addSet()
		c.resign()
	case 1:
		if len(c.sets) > 2 {
			i := c.rng.Intn(len(c.sets))
			c.removed = append(c.removed, c.sets[i].ID)
			c.sets = slices.Delete(slices.Clone(c.sets), i, i+1)
			c.resign()
		}
	case 2:
		if len(c.sets) > 0 {
			i := c.rng.Intn(len(c.sets))
			old := c.sets[i]
			if old.Purpose == "refused" {
				return
			}
			c.sets = slices.Clone(c.sets)
			for {
				d := c.newSetDef(old.ID, old.Revision+1)
				if d.Purpose != "refused" {
					c.sets[i] = d
					break
				}
			}
			c.resign()
		}
	case 3, 4, 5, 6:
		c.newRequest()
	case 7:
		// The rule assignment: none, or a version the agent refuses (nothing
		// of auditd is touched).
		if c.rng.Intn(3) == 0 {
			c.rule = nil
		} else {
			c.ruleVer++
			c.rule = &RuleAssignment{ID: 9, Version: c.ruleVer, Name: "stress", Content: RuleContent{CheckIntervalSec: 3}}
		}
	case 8:
		c.clk.jump(time.Duration(5+c.rng.Intn(90)) * time.Second)
	}
}

func (c *stressCentral) newRequest() {
	var id int64
	switch r := c.rng.Intn(12); {
	case r == 0 && len(c.removed) > 0:
		id = c.removed[c.rng.Intn(len(c.removed))]
	case r == 1:
		id = 100000 + c.nextReq // never existed
	case len(c.sets) > 0:
		id = c.sets[c.rng.Intn(len(c.sets))].ID
	default:
		return
	}
	// At most two unanswered requests per set (the central allows one; the
	// agent leaves a second one for a later delivery).
	open := 0
	for _, r := range c.queued {
		if r.SetID == id {
			open++
		}
	}
	for _, r := range c.delivered {
		if r.SetID == id {
			open++
		}
	}
	if open >= 2 {
		return
	}
	now := c.clk.now().Unix()
	exp := now + 7200
	if (c.e1 || stressNeverScheduled(id)) && c.rng.Intn(4) == 0 {
		exp = now + int64(c.rng.Intn(4))
	}
	c.queued = append(c.queued, CommandRunRequest{ID: c.nextReq, SetID: id, RequestedAt: now, ExpiresAt: exp})
	c.nextReq++
}

func (c *stressCentral) Sync(req *SyncRequest) (*SyncResponse, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.syncs++
	if c.stoppings > 0 {
		c.afterStop = append(c.afterStop, "sync")
	}
	switch {
	case c.quiet:
	case c.calm:
		if c.rng.Intn(2) == 0 {
			c.newRequest()
		}
	default:
		for i := 0; i < max(1, c.burst); i++ {
			c.mutate()
		}
		if c.rng.Intn(25) == 0 {
			c.lostRequests++
			return nil, errors.New("the fake central: the sync is lost")
		}
	}
	resp := &SyncResponse{DefaultIntervalSec: 1, Rule: c.rule}
	if req.CommandSig != c.sig {
		resp.CommandSets = &CommandDefinitions{Signature: c.sig, Sets: c.sets}
	}
	now := c.clk.now().Unix()
	// A request is handed over with a sync that says free_worker; now and
	// then also without (the agent's queue must cope with every delivery).
	if req.FreeWorker {
		c.freeSyncs++
	}
	if req.FreeWorker || (!c.quiet && !c.calm && c.rng.Intn(3) == 0) {
		for i, r := range c.queued {
			if r.ExpiresAt < now {
				continue
			}
			c.delivered[r.ID], c.handed[r.ID] = r, r
			c.queued = slices.Delete(slices.Clone(c.queued), i, i+1)
			break
		}
	}
	ids := make([]int64, 0, len(c.delivered))
	for id := range c.delivered {
		ids = append(ids, id)
	}
	slices.Sort(ids)
	for _, id := range ids {
		resp.CommandRequests = append(resp.CommandRequests, c.delivered[id])
	}
	// As a real transport does: the agent gets objects of its own.
	b, err := json.Marshal(resp)
	if err != nil {
		return nil, err
	}
	out := &SyncResponse{}
	if err := json.Unmarshal(b, out); err != nil {
		return nil, err
	}
	return out, nil
}

func (c *stressCentral) Report(b *ReportBatch) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.stoppings > 0 {
		c.afterStop = append(c.afterStop, fmt.Sprintf("report (stopping %v)", b.Stopping))
	}
	if !c.quiet && !c.calm && c.rng.Intn(20) == 0 {
		c.lostRequests++
		return errors.New("the fake central: the report is lost")
	}
	if len(b.ReportID) != 32 {
		c.violations = append(c.violations, fmt.Sprintf("report identity %q", b.ReportID))
	}
	if b.ReportID == c.lastReportID {
		c.dupBatches++
	} else {
		c.lastReportID = b.ReportID
		c.batches++
		for _, r := range b.Reports {
			for _, a := range r.CommandRequestAcks {
				c.acks[a.RequestID] = append(c.acks[a.RequestID], a)
				delete(c.delivered, a.RequestID)
			}
			for _, run := range r.CommandRuns {
				c.runs[run.RunUID] = append(c.runs[run.RunUID], run)
			}
			if len(r.CommandRuns) == 0 && len(r.CommandRequestAcks) == 0 && r.CommandApply == nil {
				c.periodic++
			}
		}
	}
	if b.Stopping {
		c.stoppings++
		if c.onStopping != nil {
			c.violations = append(c.violations, c.onStopping()...)
		}
	}
	if !c.quiet && !c.calm && c.rng.Intn(20) == 0 {
		c.lostRequests++
		return errors.New("the fake central: the answer to the report is lost")
	}
	return nil
}

// open returns the requests that are not handed over or not answered.
func (c *stressCentral) open() (queued, delivered int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	now := c.clk.now().Unix()
	for _, r := range c.queued {
		if r.ExpiresAt >= now {
			queued++
		}
	}
	return queued, len(c.delivered)
}

func (c *stressCentral) setQuiet() {
	c.mu.Lock()
	c.quiet = true
	c.mu.Unlock()
}

func (c *stressCentral) setCalm() {
	c.mu.Lock()
	c.calm = true
	c.mu.Unlock()
}

func (c *stressCentral) periodicSeen() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.periodic
}

// stressIdleWaitMax is how long an entry that may start at once may be seen
// waiting while a worker is idle before it counts as a problem (a worker
// between two entries is idle for the queue for a moment).
const stressIdleWaitMax = 300 * time.Millisecond

// stressQueueCheck looks at the queue under its mutex and returns what breaks
// its invariants (the file comment of agent_scheduler.go).
func stressQueueCheck(s *agentScheduler) []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	var bad []string
	monitors := 0
	perSet := map[int64]int{}
	perReq := map[int64]int{}
	for _, e := range s.entries {
		switch {
		case e.monitor:
			monitors++
		default:
			perSet[e.setID]++
			if e.request != nil {
				perReq[e.request.ID]++
				if s.taken[e.request.ID] {
					bad = append(bad, fmt.Sprintf("request %d is queued while a worker has taken it", e.request.ID))
				}
			} else if s.running[e.setID] {
				bad = append(bad, fmt.Sprintf("set %d runs and has a scheduled entry queued", e.setID))
			}
		}
	}
	if s.monitoring {
		monitors++
	}
	if monitors != 1 {
		bad = append(bad, fmt.Sprintf("monitoring entries queued or processed: %d", monitors))
	}
	for id, n := range perSet {
		if n > 1 {
			bad = append(bad, fmt.Sprintf("set %d has %d entries", id, n))
		}
	}
	for id, n := range perReq {
		if n > 1 {
			bad = append(bad, fmt.Sprintf("request %d has %d entries", id, n))
		}
	}
	busy := len(s.running)
	if s.monitoring {
		busy++
	}
	if busy > s.workers {
		bad = append(bad, fmt.Sprintf("%d entries processed with %d workers", busy, s.workers))
	}
	for id, d := range s.valid {
		if d.RunMode == RunModeSchedule && d.Schedule != nil && !s.running[id] && perSet[id] == 0 {
			bad = append(bad, fmt.Sprintf("scheduled set without an entry (set %d)", id))
		}
	}
	return bad
}

// stressIdleWaiting returns the entries (their sequence numbers) that may start
// at once (no time), whose set neither runs nor is held, while fewer entries
// are processed than there are workers: a worker should take such an entry
// without delay.
func stressIdleWaiting(s *agentScheduler) map[uint64]string {
	s.mu.Lock()
	defer s.mu.Unlock()
	busy := len(s.running)
	if s.monitoring {
		busy++
	}
	if busy >= s.workers || isClosed(s.stop) {
		return nil
	}
	out := map[uint64]string{}
	for _, e := range s.entries {
		if e.startAt.IsZero() && !s.blockedLocked(e) {
			what := "the monitoring entry"
			if !e.monitor {
				what = fmt.Sprintf("the entry of set %d (manual %v)", e.setID, e.request != nil)
			}
			out[e.seq] = what
		}
	}
	return out
}

// stressMarked returns the live processes that carry the round's mark in their
// environment ("pid argv0 ...").
func stressMarked(mark string) []string {
	var out []string
	names, _ := os.ReadDir("/proc")
	for _, e := range names {
		pid, err := strconv.Atoi(e.Name())
		if err != nil || pid == os.Getpid() {
			continue
		}
		env, err := os.ReadFile(filepath.Join("/proc", e.Name(), "environ"))
		if err != nil || !strings.Contains(string(env), "MIMAMORI_STRESS_MARK="+mark+"\x00") {
			continue
		}
		cmd, _ := os.ReadFile(filepath.Join("/proc", e.Name(), "cmdline"))
		out = append(out, fmt.Sprintf("%d %s", pid, strings.ReplaceAll(string(cmd), "\x00", " ")))
	}
	return out
}

// stressRound runs one agent with the fake central for a short time and
// returns what broke.
func stressRound(t *testing.T, seed int64, workers int, drain bool) []string {
	rng := rand.New(rand.NewSource(seed ^ 0x5eed))
	dir := t.TempDir()
	mark := fmt.Sprintf("%d-%d-%d", os.Getpid(), seed, time.Now().UnixNano())
	t.Setenv("MIMAMORI_STRESS_MARK", mark)
	// The audit tools of the monitoring step are replaced: nothing of the
	// host's auditd is touched, and two monitoring steps at the same time
	// would show.
	tool := fmt.Sprintf("if ! mkdir %[1]s/monlock 2>/dev/null; then echo \"OVERLAP monitoring $$\" >> %[1]s/overlap.log; fi\nsleep 0.01\nrmdir %[1]s/monlock\nexit 0\n", dir)
	for _, name := range []string{"auditctl", "ausearch", "augenrules"} {
		fakeTool(t, name, tool)
	}
	clk := &stressClock{}
	central := newStressCentral(seed, clk, dir, true)
	var logs lockedLog
	MigrationsFS = os.DirFS("../..")
	db, err := OpenSQLite(filepath.Join(dir, "agent.db"), SchemaAgentLocal)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	rt := NewAgentRuntime(db, central, logs.logger())
	rt.cmdKillGrace = 300 * time.Millisecond
	rt.SetWorkers(workers)
	rt.link.syncInterval = 1
	rt.clock = clk.now
	rt.loc = time.UTC
	rt.removeAudit = func(*AuditGlobals) error { return nil }
	var paceMu sync.Mutex
	pace := rand.New(rand.NewSource(seed))
	rt.roundPace = func() time.Duration {
		paceMu.Lock()
		defer paceMu.Unlock()
		return time.Duration(2+pace.Intn(18)) * time.Millisecond
	}
	central.onStopping = func() []string {
		// The last report comes after every worker ended.
		var bad []string
		rt.sched.mu.Lock()
		if len(rt.sched.running) > 0 || rt.sched.monitoring {
			bad = append(bad, fmt.Sprintf("the stopping report came while workers process entries (running %d, monitoring %v)",
				len(rt.sched.running), rt.sched.monitoring))
		}
		rt.sched.mu.Unlock()
		if n := rt.leftovers.held(); n > 0 {
			bad = append(bad, fmt.Sprintf("the stopping report came while %d kept pipes are open", n))
		}
		if open, err := models.ListUnfinishedCommandExecutions(db.Read()); err == nil && len(open) > 0 {
			bad = append(bad, fmt.Sprintf("the stopping report came while %d runs are unfinished", len(open)))
		}
		return bad
	}

	var bad []string
	fail := func(format string, a ...any) { bad = append(bad, fmt.Sprintf(format, a...)) }
	stop := make(chan struct{})
	done := make(chan struct{})
	go func() {
		defer close(done)
		rt.Run(stop)
	}()
	// The sampler looks at the queue all the time.
	quit := make(chan struct{})
	var sampled sync.WaitGroup
	seenBad := map[string]int{}
	waitingSince := map[uint64]time.Time{}
	waitReported := map[uint64]bool{}
	sampled.Add(1)
	go func() {
		defer sampled.Done()
		for !isClosed(quit) {
			central.mu.Lock()
			started := central.syncs > 0
			central.mu.Unlock()
			if started {
				for _, b := range stressQueueCheck(rt.sched) {
					seenBad[b]++
				}
				// An entry that may start at once must not wait while a
				// worker is idle (a lost wake-up shows as such a wait).
				now := time.Now()
				cur := stressIdleWaiting(rt.sched)
				for seq, what := range cur {
					if first, ok := waitingSince[seq]; !ok {
						waitingSince[seq] = now
					} else if now.Sub(first) > stressIdleWaitMax && !waitReported[seq] {
						waitReported[seq] = true
						seenBad[fmt.Sprintf("%s waited more than %s although a worker was idle", what, stressIdleWaitMax)]++
					}
				}
				for seq := range waitingSince {
					if _, ok := cur[seq]; !ok {
						delete(waitingSince, seq)
					}
				}
			}
			time.Sleep(time.Millisecond)
		}
	}()

	time.Sleep(time.Duration(400+rng.Intn(600)) * time.Millisecond)
	if drain {
		central.setCalm()
		time.Sleep(time.Duration(300+rng.Intn(300)) * time.Millisecond)
		central.setQuiet()
		before := central.periodicSeen()
		deadline := time.Now().Add(30 * time.Second)
		for {
			q, d := central.open()
			if q == 0 && d == 0 {
				break
			}
			if time.Now().After(deadline) {
				fail("drain: after 30 s still %d request(s) not handed over and %d not answered; queue %+v", q, d, queuedEntries(rt))
				break
			}
			time.Sleep(5 * time.Millisecond)
		}
		// The monitoring step keeps coming (check interval 1 s).
		deadline = time.Now().Add(8 * time.Second)
		for central.periodicSeen() == before {
			if time.Now().After(deadline) {
				fail("drain: no report of a monitoring step within 8 s; queue %+v", queuedEntries(rt))
				break
			}
			time.Sleep(5 * time.Millisecond)
		}
	}
	stopAt := time.Now()
	close(stop)
	select {
	case <-done:
	case <-time.After(30 * time.Second):
		buf := make([]byte, 8<<20)
		t.Fatalf("seed %d: the agent did not return within 30 s of the stop\n%s\nlog:\n%s", seed, buf[:runtime.Stack(buf, true)], logs.String())
	}
	if took := time.Since(stopAt); took > 5*time.Second {
		fail("the agent returned %s after the stop", took)
	}
	close(quit)
	sampled.Wait()
	for b, n := range seenBad {
		fail("queue (%d looks): %s", n, b)
	}
	for _, b := range stressQueueCheck(rt.sched) {
		fail("queue after the end: %s", b)
	}

	// Nothing of the agent is left.
	if left := agentGoroutines(); len(left) > 0 {
		fail("goroutines of the agent left after its end:\n%s", strings.Join(left, "\n\n"))
	}
	var procs []string
	for _, p := range stressMarked(mark) {
		if !strings.Contains(p, "stressleft") {
			procs = append(procs, p)
		}
	}
	if len(procs) > 0 {
		fail("processes of commands left after the end: %v", procs)
	}
	if n := rt.leftovers.held(); n != 0 {
		fail("kept pipes after the end: %d", n)
	}
	// Never the same set (or the monitoring step) twice at the same time.
	if b, err := os.ReadFile(filepath.Join(dir, "overlap.log")); err == nil && len(b) > 0 {
		fail("ran twice at the same time:\n%s", b)
	}
	// No command started after the stop (by the command's own clock).
	if b, err := os.ReadFile(filepath.Join(dir, "cmd.log")); err == nil {
		for _, l := range strings.Split(string(b), "\n") {
			f := strings.Fields(l)
			if len(f) == 4 && f[0] == "S" {
				if ts, err := strconv.ParseFloat(f[3], 64); err == nil && ts > float64(stopAt.UnixNano())/1e9+0.25 {
					fail("a command of set %s started %.3f s after the stop", f[1], ts-float64(stopAt.UnixNano())/1e9)
				}
			}
		}
	}
	return append(bad, stressRecords(t, central, db, drain)...)
}

// stressRecords checks the records after a round: every run closed once
// with one start, one run per request and per occurrence, the answers
// consistent with the runs, the requests run or answered after a drain, and
// no request after the stopping report.
func stressRecords(t *testing.T, central *stressCentral, db *DB, drain bool) []string {
	var bad []string
	fail := func(format string, a ...any) { bad = append(bad, fmt.Sprintf(format, a...)) }
	central.mu.Lock()
	defer central.mu.Unlock()
	bad = append(bad, central.violations...)
	if len(central.afterStop) > 0 && central.stoppings > 0 {
		fail("requests after the stopping report: %v", central.afterStop)
	}
	if drain {
		if central.stoppings != 1 {
			fail("stopping reports received: %d", central.stoppings)
		}
		if items, err := models.ListOutbox(db.Read()); err != nil || len(items) != 0 {
			fail("items left queued after the stopping report: %d (%v)", len(items), err)
		}
	}
	execs, err := models.ListUnfinishedCommandExecutions(db.Read())
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range execs {
		fail("run %s of set %d is left unfinished", e.RunUID, e.SetID)
	}
	type execRow struct {
		uid          string
		setID        int64
		requestID    int64
		trigger      string
		scheduledFor int64
	}
	var all []execRow
	rows, err := db.Read().Query(`SELECT run_uid, set_id, request_id, run_trigger, scheduled_for FROM command_executions`)
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var e execRow
		if err := rows.Scan(&e.uid, &e.setID, &e.requestID, &e.trigger, &e.scheduledFor); err != nil {
			t.Fatal(err)
		}
		all = append(all, e)
	}
	rows.Close()
	// What the central took in and what is still queued (items of a batch
	// the central stored whose answer was lost are the same reports).
	allRuns := map[string][]CommandRunReport{}
	for uid, rs := range central.runs {
		allRuns[uid] = append(allRuns[uid], rs...)
	}
	allAcks := map[int64][]CommandRequestAck{}
	for id, as := range central.acks {
		allAcks[id] = append(allAcks[id], as...)
	}
	items, err := models.ListOutbox(db.Read())
	if err != nil {
		t.Fatal(err)
	}
	var rec outboxBatch
	_ = json.Unmarshal([]byte(models.GetLocalState(db.Read(), stateOutboxBatch)), &rec)
	stored := map[int64]bool{}
	if rec.ReportID != "" && rec.ReportID == central.lastReportID {
		for _, id := range rec.Items {
			stored[id] = true
		}
	}
	for _, it := range items {
		if stored[it.ID] {
			continue
		}
		var r ReportRequest
		if err := json.Unmarshal([]byte(it.Payload), &r); err != nil {
			t.Fatal(err)
		}
		for _, run := range r.CommandRuns {
			allRuns[run.RunUID] = append(allRuns[run.RunUID], run)
		}
		for _, a := range r.CommandRequestAcks {
			allAcks[a.RequestID] = append(allAcks[a.RequestID], a)
		}
	}
	byReq := map[int64][]execRow{}
	bySched := map[string]int{}
	for _, e := range all {
		if e.requestID > 0 {
			byReq[e.requestID] = append(byReq[e.requestID], e)
			if _, ok := central.handed[e.requestID]; !ok {
				fail("run %s names request %d, which was never handed over", e.uid, e.requestID)
			}
		}
		if e.trigger == TriggerSchedule {
			k := fmt.Sprintf("%d@%d", e.setID, e.scheduledFor)
			if bySched[k]++; bySched[k] > 1 {
				fail("occurrence %s ran %d times", k, bySched[k])
			}
		}
		finished, started := 0, 0
		for _, r := range allRuns[e.uid] {
			if r.Finished {
				finished++
			}
			if len(r.Results) == 0 && !r.Finished {
				started++
			}
		}
		if finished != 1 || started != 1 {
			fail("run %s of set %d: %d start and %d closing reports", e.uid, e.setID, started, finished)
		}
	}
	for uid := range allRuns {
		if !slices.ContainsFunc(all, func(e execRow) bool { return e.uid == uid }) {
			fail("reports of run %s without a recorded run", uid)
		}
	}
	ids := make([]int64, 0, len(central.handed))
	for id := range central.handed {
		ids = append(ids, id)
	}
	slices.Sort(ids)
	for _, id := range ids {
		runs := byReq[id]
		var startedUIDs, terminal []string
		for _, a := range allAcks[id] {
			if a.Status == RequestAckStarted {
				startedUIDs = append(startedUIDs, a.RunUID)
			} else {
				terminal = append(terminal, a.Status)
			}
		}
		sort.Strings(terminal)
		terminal = slices.Compact(terminal)
		switch {
		case len(runs) > 1:
			fail("request %d ran %d times", id, len(runs))
		case len(runs) == 1:
			if len(terminal) > 0 {
				fail("request %d ran (run %s) and was answered %v", id, runs[0].uid, terminal)
			}
			if len(startedUIDs) == 0 {
				fail("request %d ran (run %s) without the answer started", id, runs[0].uid)
			}
			for _, u := range startedUIDs {
				if u != runs[0].uid {
					fail("request %d: answered started with run %s, recorded run %s", id, u, runs[0].uid)
				}
			}
		default:
			if len(startedUIDs) > 0 {
				fail("request %d answered started (%v) without a recorded run", id, startedUIDs)
			}
			// A request that did not run may get several answers, each made
			// from what is stored at its delivery (unknown_set first,
			// expired once its expiry passed).
			if drain && len(terminal) == 0 {
				fail("request %d: neither run nor answered after the drain", id)
			}
		}
	}
	for id := range allAcks {
		if _, ok := central.handed[id]; !ok {
			fail("answer to request %d, which was never handed over", id)
		}
	}
	t.Logf("syncs %d (free_worker %d), runs %d, requests handed over %d, periodic reports %d, lost requests or answers %d, batches sent again %d",
		central.syncs, central.freeSyncs, len(all), len(central.handed), central.periodic, central.lostRequests, central.dupBatches)
	return bad
}

// TestAgentStress: the randomized stress run (see the file comment), one
// round with a stop at a random moment and one with a drain for each worker
// count.
func TestAgentStress(t *testing.T) {
	requireBash(t)
	seed := time.Now().UnixNano() % 1_000_000_000
	if v, err := strconv.ParseInt(os.Getenv("MIMAMORI_STRESS_SEED"), 10, 64); err == nil {
		seed = v
	}
	t.Logf("seed %d (MIMAMORI_STRESS_SEED)", seed)
	for _, workers := range []int{1, 2, 4} {
		for _, drain := range []bool{false, true} {
			s := seed + int64(workers)*1000
			if drain {
				s++
			}
			t.Run(fmt.Sprintf("workers %d, drain %v", workers, drain), func(t *testing.T) {
				if bad := stressRound(t, s, workers, drain); len(bad) > 0 {
					sort.Strings(bad)
					t.Errorf("seed %d: %d problem(s):\n  %s", s, len(bad), strings.Join(bad, "\n  "))
				}
			})
		}
	}
}
