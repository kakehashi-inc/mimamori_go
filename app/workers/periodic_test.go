package workers

import (
	"fmt"
	"log"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"mimamori/app/models"
	"mimamori/app/modules"
)

// The tests of the periodic flows use short intervals instead of
// PeriodicTick. Times have a generous margin, because other tests may run on
// the machine at the same time.

// stopPeriodicSteps closes stop and waits for the flows (once).
func stopPeriodicSteps(stop chan struct{}, wait func()) func() {
	var once sync.Once
	return func() { once.Do(func() { close(stop); wait() }) }
}

// While the mail step blocks in a mail function that does not return, the
// other five steps keep running at their interval.
func TestPeriodicIndependent(t *testing.T) {
	e := newTestEnv(t)
	// The configuration step reloads the stored configuration, so the
	// mail server is stored, not only set in memory.
	e.admin("PUT", "/config", map[string]any{"set": map[string]string{"smtp.host": "127.0.0.1", "smtp.from": "central@example.com"}})
	e.admin("PUT", "/accounts/1/notify", map[string]any{"notify": true})
	reportAlert(t, e, "curl a")

	blocked := make(chan struct{})
	release := make(chan struct{})
	var blockedOnce sync.Once
	e.c.sendMail = func(modules.SMTPConfig, []string, string, string) error {
		blockedOnce.Do(func() { close(blocked) })
		<-release
		return nil
	}
	const every = 100 * time.Millisecond
	counts := map[string]*atomic.Int64{}
	var steps []periodicStep
	for _, s := range e.c.periodicSteps() {
		n := &atomic.Int64{}
		counts[s.name] = n
		run := s.run
		steps = append(steps, periodicStep{name: s.name, every: every, run: func(now time.Time) {
			n.Add(1)
			run(now)
		}})
	}
	if len(steps) != 6 {
		t.Fatalf("periodic steps: %d", len(steps))
	}
	stop := make(chan struct{})
	end := stopPeriodicSteps(stop, e.c.startPeriodic(steps, stop))
	defer end()
	defer close(release)

	select {
	case <-blocked:
	case <-time.After(10 * time.Second):
		t.Fatal("the mail step never called the mail function")
	}
	before := map[string]int64{}
	for name, n := range counts {
		before[name] = n.Load()
	}
	time.Sleep(15 * every)
	for name, n := range counts {
		got := n.Load() - before[name]
		if name == "mail" {
			if got != 0 {
				t.Fatalf("the blocked mail step ran %d more times", got)
			}
			continue
		}
		if got < 5 {
			t.Fatalf("step %s ran %d times in %v while the mail step was blocked", name, got, 15*every)
		}
	}
}

// A step slower than its interval never overlaps itself and runs again
// right after, because the ticker keeps the tick that fell due during the run.
func TestPeriodicNoDoubleRun(t *testing.T) {
	e := newTestEnv(t)
	const every = 200 * time.Millisecond
	const work = 300 * time.Millisecond
	var active, maxActive atomic.Int32
	var mu sync.Mutex
	var starts, ends []time.Time
	step := periodicStep{name: "slow", every: every, run: func(time.Time) {
		mu.Lock()
		starts = append(starts, time.Now())
		mu.Unlock()
		n := active.Add(1)
		for {
			m := maxActive.Load()
			if n <= m || maxActive.CompareAndSwap(m, n) {
				break
			}
		}
		time.Sleep(work)
		active.Add(-1)
		mu.Lock()
		ends = append(ends, time.Now())
		mu.Unlock()
	}}
	stop := make(chan struct{})
	end := stopPeriodicSteps(stop, e.c.startPeriodic([]periodicStep{step}, stop))
	time.Sleep(every + 4*work + work/2)
	end()
	if maxActive.Load() != 1 {
		t.Fatalf("the step ran %d times at once", maxActive.Load())
	}
	mu.Lock()
	defer mu.Unlock()
	if len(starts) < 3 {
		t.Fatalf("only %d runs", len(starts))
	}
	// Without the kept tick the next run would wait for the next tick of
	// the grid, 100 ms after the end of the run.
	for i := 1; i < len(starts); i++ {
		if gap := starts[i].Sub(ends[i-1]); gap > 60*time.Millisecond {
			t.Fatalf("run %d started %v after the previous one ended", i+1, gap)
		}
	}
}

// A panicking step is logged with its stack and runs again at its next
// tick; the other steps are untouched.
func TestPeriodicPanic(t *testing.T) {
	e := newTestEnv(t)
	logs := &syncWriter{}
	e.c.logger = log.New(logs, "", 0)
	var booms, calm atomic.Int64
	steps := []periodicStep{
		{name: "boom", every: 30 * time.Millisecond, run: func(time.Time) {
			booms.Add(1)
			panic("test panic")
		}},
		{name: "calm", every: 30 * time.Millisecond, run: func(time.Time) { calm.Add(1) }},
	}
	stop := make(chan struct{})
	end := stopPeriodicSteps(stop, e.c.startPeriodic(steps, stop))
	time.Sleep(600 * time.Millisecond)
	end()
	if booms.Load() < 3 || calm.Load() < 5 {
		t.Fatalf("runs: boom %d, calm %d", booms.Load(), calm.Load())
	}
	l := logs.String()
	if strings.Count(l, "periodic boom: panic: test panic\n") != int(booms.Load()) || !strings.Contains(l, "goroutine ") {
		t.Fatalf("log: %s", l)
	}
	if strings.Contains(l, "calm") {
		t.Fatalf("the calm step was touched: %s", l)
	}
}

// After stop no run starts, and wait returns only when the run in progress
// has ended.
func TestPeriodicStop(t *testing.T) {
	e := newTestEnv(t)
	started := make(chan struct{}, 10)
	release := make(chan struct{})
	var runs atomic.Int64
	steps := []periodicStep{{name: "long", every: 20 * time.Millisecond, run: func(time.Time) {
		runs.Add(1)
		started <- struct{}{}
		<-release
	}}}
	stop := make(chan struct{})
	wait := e.c.startPeriodic(steps, stop)
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("the step never ran")
	}
	close(stop)
	done := make(chan struct{})
	go func() { wait(); close(done) }()
	select {
	case <-done:
		t.Fatal("wait returned while a run was in progress")
	case <-time.After(300 * time.Millisecond):
	}
	close(release)
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("wait did not return after the run ended")
	}
	if runs.Load() != 1 {
		t.Fatalf("%d runs; none may start after stop", runs.Load())
	}
}

// Reloads from many flows next to configuration changes: the configuration
// in memory equals the stored one at the end.
func TestReloadConfigSerialised(t *testing.T) {
	e := newTestEnv(t)
	var wg sync.WaitGroup
	var failed atomic.Value
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range 60 {
				e.c.reloadConfig()
			}
		}()
	}
	const changes = 40
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 1; i <= changes; i++ {
			code, body := e.adminRaw("PUT", "/config", map[string]any{"set": map[string]string{"notify.max_lines": strconv.Itoa(100 + i)}})
			if code != 200 {
				failed.Store(fmt.Sprintf("%d %s", code, body))
				return
			}
		}
	}()
	wg.Wait()
	if f := failed.Load(); f != nil {
		t.Fatalf("configuration change: %v", f)
	}
	stored, err := modules.LoadCentralConfig(e.c.db.Read())
	if err != nil {
		t.Fatal(err)
	}
	if got := e.c.conf().Notify.MaxLines; got != stored.Notify.MaxLines || got != 100+changes {
		t.Fatalf("in memory %d, stored %d, want %d", got, stored.Notify.MaxLines, 100+changes)
	}
}

// failOn creates a trigger on central.db (or on a target database) that
// makes the matching statements fail.
func failOn(t *testing.T, db *modules.DB, name, when string) {
	t.Helper()
	err := db.Write(func(tx models.Writer) error {
		_, err := tx.Exec(`CREATE TRIGGER ` + name + ` ` + when + ` BEGIN SELECT RAISE(ABORT, 'test failure'); END`)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
}

func dropTrigger(t *testing.T, db *modules.DB, name string) {
	t.Helper()
	err := db.Write(func(tx models.Writer) error {
		_, err := tx.Exec(`DROP TRIGGER ` + name)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
}

func rowsCount(t *testing.T, db *modules.DB, query string, args ...any) int {
	t.Helper()
	var n int
	if err := db.Read().QueryRow(query, args...).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// With a failing history insert the mark of the token check is not
// advanced, and the next run records the expiry once.
func TestTokenExpiryAtomic(t *testing.T) {
	e := newTestEnv(t)
	logs := &syncWriter{}
	e.c.logger = log.New(logs, "", 0)
	e.admin("POST", "/tokens", map[string]any{"name": "short"})
	now := time.Now()
	last := now.Add(-time.Hour).Unix()
	if err := e.exec(`UPDATE tokens SET expires_at = ? WHERE name = 'short'`, now.Add(-time.Minute).Unix()); err != nil {
		t.Fatal(err)
	}
	if err := e.exec(`INSERT OR REPLACE INTO settings (key, value) VALUES (?, ?)`, settingLastTokenCheck, strconv.FormatInt(last, 10)); err != nil {
		t.Fatal(err)
	}
	expired := func() int {
		return rowsCount(t, e.c.db, `SELECT COUNT(*) FROM history WHERE action = 'expired' AND target = 'short'`)
	}

	failOn(t, e.c.db, "fail_history", `BEFORE INSERT ON history WHEN NEW.action = 'expired'`)
	e.c.processTokenExpiry(now)
	if e.c.settingInt(settingLastTokenCheck) != last || expired() != 0 {
		t.Fatalf("after the failure: mark %d (want %d), %d history rows", e.c.settingInt(settingLastTokenCheck), last, expired())
	}
	if !strings.Contains(logs.String(), "token expiry") {
		t.Fatalf("the failure is not logged: %s", logs.String())
	}

	dropTrigger(t, e.c.db, "fail_history")
	e.c.processTokenExpiry(now)
	e.c.processTokenExpiry(now.Add(time.Second))
	if e.c.settingInt(settingLastTokenCheck) != now.Add(time.Second).Unix() || expired() != 1 {
		t.Fatalf("after the retry: mark %d, %d history rows", e.c.settingInt(settingLastTokenCheck), expired())
	}
}

// A mail whose marking as sent fails is not sent a second time; the marking
// is retried.
func TestNotifySentButNotMarked(t *testing.T) {
	e := newTestEnv(t)
	logs := &syncWriter{}
	e.c.logger = log.New(logs, "", 0)
	mb := useMailbox(e)
	e.admin("PUT", "/accounts/1/notify", map[string]any{"notify": true})
	reportAlert(t, e, "curl a")
	unsent := func() int {
		list, err := models.ListUnsentNotifications(e.c.db.Read())
		if err != nil {
			t.Fatal(err)
		}
		return len(list)
	}

	failOn(t, e.c.db, "fail_sent", `BEFORE UPDATE ON notifications WHEN NEW.sent_at > 0`)
	e.c.notify(time.Now())
	if len(mb.sent) != 1 || unsent() != 1 || e.c.settingInt(settingLastNotifyAt) != 0 {
		t.Fatalf("first run: %d mails, %d unsent, mark %d", len(mb.sent), unsent(), e.c.settingInt(settingLastNotifyAt))
	}
	e.c.notify(time.Now())
	if len(mb.sent) != 1 || unsent() != 1 {
		t.Fatalf("the mail was sent again: %d mails, %d unsent", len(mb.sent), unsent())
	}
	if !strings.Contains(logs.String(), "not marked as sent") {
		t.Fatalf("the failed marking is not logged: %s", logs.String())
	}

	dropTrigger(t, e.c.db, "fail_sent")
	e.c.notify(time.Now())
	if len(mb.sent) != 1 || unsent() != 0 || e.c.settingInt(settingLastNotifyAt) == 0 {
		t.Fatalf("after the retry: %d mails, %d unsent, mark %d", len(mb.sent), unsent(), e.c.settingInt(settingLastNotifyAt))
	}
	if n := rowsCount(t, e.c.db, `SELECT attempts FROM notifications`); n != 1 {
		t.Fatalf("attempts recorded: %d", n)
	}
}

// A failure inside the switch to the next CA leaves the old state; the next
// run completes it, and no third CA appears.
func TestCertificateSwitchAtomic(t *testing.T) {
	e := newTestEnv(t)
	now := time.Now()
	// The active CA expires soon: the next CA is created.
	if err := e.exec(`UPDATE certificates SET not_after = ? WHERE kind = 'ca' AND status = ?`,
		now.Add(modules.CARotateBefore-time.Hour).Unix(), models.CertActive); err != nil {
		t.Fatal(err)
	}
	if err := e.c.manageCertificates(); err != nil {
		t.Fatal(err)
	}
	ca := func(status string) int64 {
		var id int64
		_ = e.c.db.Read().QueryRow(`SELECT id FROM certificates WHERE kind = 'ca' AND status = ?`, status).Scan(&id)
		return id
	}
	oldActive, next := ca(models.CertActive), ca(models.CertNext)
	if oldActive == 0 || next == 0 {
		t.Fatalf("active %d, next %d", oldActive, next)
	}
	// The distribution period has passed: the switch is due.
	if err := e.exec(`UPDATE certificates SET created_at = ? WHERE id = ?`, now.Add(-modules.CASwitchAfter-time.Hour).Unix(), next); err != nil {
		t.Fatal(err)
	}

	failOn(t, e.c.db, "fail_switch", `BEFORE UPDATE ON certificates WHEN NEW.kind = 'ca' AND NEW.status = '`+models.CertActive+`'`)
	if err := e.c.manageCertificates(); err == nil {
		t.Fatal("the switch must fail")
	}
	if ca(models.CertActive) != oldActive || ca(models.CertNext) != next {
		t.Fatalf("after the failure: active %d (want %d), next %d (want %d)", ca(models.CertActive), oldActive, ca(models.CertNext), next)
	}

	dropTrigger(t, e.c.db, "fail_switch")
	if err := e.c.manageCertificates(); err != nil {
		t.Fatal(err)
	}
	if ca(models.CertActive) != next || ca(models.CertRetired) != oldActive || ca(models.CertNext) != 0 {
		t.Fatalf("after the retry: active %d, retired %d, next %d", ca(models.CertActive), ca(models.CertRetired), ca(models.CertNext))
	}
	if n := rowsCount(t, e.c.db, `SELECT COUNT(*) FROM certificates WHERE kind = 'ca'`); n != 2 {
		t.Fatalf("%d CAs", n)
	}
	if n := rowsCount(t, e.c.db, `SELECT COUNT(*) FROM history WHERE action = 'ca_switched'`); n != 1 {
		t.Fatalf("%d switch history rows", n)
	}
}

// A failing agent database is logged with the agent and what failed, and
// the others are cleaned; the mark of the run is written.
func TestRetentionLogsFailures(t *testing.T) {
	e := newTestEnv(t)
	logs := &syncWriter{}
	e.c.logger = log.New(logs, "", 0)
	tok, other, otherDB := commandAgent(t, e, "host-failing")
	syncAgent(t, e, tok, "")
	reportAgent(t, e, tok, modules.ReportRequest{Detections: []modules.DetectionReport{{Type: "exec", Category: "alert", OSUser: "u", Exe: "/bin/x", Target: "x"}}})
	reportAlert(t, e, "curl a")
	b, err := models.GetBuiltinAgent(e.c.db.Read())
	if err != nil {
		t.Fatal(err)
	}
	builtinDB := e.mustTDB(b)
	old := time.Now().Add(-400 * 24 * time.Hour).Unix()
	for _, db := range []*modules.DB{otherDB, builtinDB} {
		if err := execTDB(db, `UPDATE detections SET first_at = ?, last_at = ?`, old, old); err != nil {
			t.Fatal(err)
		}
		if rowsCount(t, db, `SELECT COUNT(*) FROM detections`) != 1 {
			t.Fatal("no detection to clean")
		}
	}
	failOn(t, otherDB, "fail_delete", `BEFORE DELETE ON detections`)

	now := time.Now()
	e.c.retention(now)
	if n := rowsCount(t, builtinDB, `SELECT COUNT(*) FROM detections`); n != 0 {
		t.Fatalf("the built-in agent's detections were not cleaned: %d", n)
	}
	if n := rowsCount(t, otherDB, `SELECT COUNT(*) FROM detections`); n != 1 {
		t.Fatalf("the failing database lost its detection: %d", n)
	}
	want := "retention of detections of " + e.c.agentDisplayName(other) + ": "
	if l := logs.String(); !strings.Contains(l, want) || !strings.Contains(l, "test failure") {
		t.Fatalf("log: %s", l)
	}
	if e.c.settingInt(settingLastRetentionAt) != now.Unix() {
		t.Fatal("the mark of the run must be written")
	}
}
