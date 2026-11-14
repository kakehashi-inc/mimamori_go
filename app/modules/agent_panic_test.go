package modules

import (
	"bytes"
	"errors"
	"log"
	"strings"
	"testing"
	"time"
)

// A panic of a worker is logged with its stack through the agent's logger
// and stops the agent the way a stop does: the other worker's running
// command is ended and its run recorded, every worker returns and Run
// returns ErrAgentPanic.
func TestWorkerPanicStopsAgent(t *testing.T) {
	requireBash(t)
	tr := &parTransport{}
	rt := newParallelRuntime(t, tr, 2)
	var logged bytes.Buffer
	rt.Logger = log.New(&logged, "", 0)
	tr.resp.CommandSets = defsOf(testSet(1, PurposeRunOnly, "sleep 30"))
	tr.resp.CommandRequests = manualRequests(1, 1)
	// The second monitoring step runs on the idle worker while the other
	// one runs the command.
	tr.onSync = func(n int) {
		if n == 2 {
			panic("test panic in a monitoring step")
		}
	}
	start := time.Now()
	err := rt.Run(nil)
	if !errors.Is(err, ErrAgentPanic) {
		t.Fatalf("Run: %v, want ErrAgentPanic", err)
	}
	if d := time.Since(start); d > 10*time.Second {
		t.Fatalf("the agent took %v to stop after the panic", d)
	}
	out := logged.String()
	for _, want := range []string{"worker panic: test panic in a monitoring step", "(*AgentRuntime).safeWork"} {
		if !strings.Contains(out, want) {
			t.Fatalf("log lacks %q:\n%s", want, out)
		}
	}
	if all, finished := countRuns(rt.DB); all != 1 || finished != 1 {
		t.Fatalf("runs recorded %d, finished %d", all, finished)
	}
}
