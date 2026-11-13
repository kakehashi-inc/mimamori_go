package workers

import (
	"runtime/debug"
	"sync"
	"time"

	"mimamori/app/modules"
)

// periodicStep is one step of the central's periodic work. Each step runs on
// a flow of its own (startPeriodic), so it never runs twice at the same time
// and a slow or failing step delays no other step.
type periodicStep struct {
	name  string
	every time.Duration
	run   func(now time.Time)
}

// periodicSteps is the list of the central's periodic steps. Each acts as
// its function says: the certificates when a threshold is reached, the
// retention once an hour by its stored mark, the mail when a mail is unsent
// or the minimum interval allows a new one; the others at every run.
func (c *core) periodicSteps() []periodicStep {
	return []periodicStep{
		{name: "configuration", every: modules.PeriodicTick, run: func(time.Time) { c.reloadConfig() }},
		{name: "certificates", every: modules.PeriodicTick, run: func(time.Time) {
			if err := c.manageCertificates(); err != nil {
				c.logger.Printf("certificates: %v", err)
			}
		}},
		{name: "tokens", every: modules.PeriodicTick, run: c.processTokenExpiry},
		{name: "run requests", every: modules.PeriodicTick, run: c.expireCommandRequests},
		{name: "retention", every: modules.PeriodicTick, run: c.retention},
		{name: "mail", every: modules.PeriodicTick, run: c.notify},
	}
}

// startPeriodic starts one flow per step. A flow runs its step at every tick
// of a ticker with the step's interval (a fixed grid from the start; the
// first run is one interval after the start). A run that takes longer than
// the interval makes the ticker drop the ticks in between, and the next run
// starts at once when a tick fell due meanwhile. A panic of a run is logged
// and the step runs again at its next tick. After stop is closed no new run
// starts; wait returns when every flow has ended, that is when the runs in
// progress have finished.
func (c *core) startPeriodic(steps []periodicStep, stop <-chan struct{}) (wait func()) {
	var wg sync.WaitGroup
	for _, s := range steps {
		wg.Add(1)
		go func() {
			defer wg.Done()
			t := time.NewTicker(s.every)
			defer t.Stop()
			for {
				select {
				case <-stop:
					return
				case <-t.C:
					select {
					case <-stop:
						return
					default:
					}
					// The time of the run, not of the tick: a tick kept by
					// the ticker during a slow run is older.
					c.runStep(s, time.Now())
				}
			}
		}()
	}
	return wg.Wait
}

// runStep runs one step once; a panic is logged with its stack.
func (c *core) runStep(s periodicStep, now time.Time) {
	defer func() {
		if v := recover(); v != nil {
			c.logger.Printf("periodic %s: panic: %v\n%s", s.name, v, debug.Stack())
		}
	}()
	s.run(now)
}
