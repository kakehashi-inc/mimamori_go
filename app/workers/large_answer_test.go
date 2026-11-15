package workers

import (
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"mimamori/app/models"
	"mimamori/app/modules"
)

// The command set definitions of one agent together larger than the largest
// request body (32 MiB) reach the agent over HTTPS: the agent reads the
// whole sync answer, stores the definitions and runs a request delivered
// with them; ctl command-sets list reads the whole list. What the last report
// of the stop leaves queued goes with the round of the agent's next start.
func TestDefinitionsLargerThanRequestBody(t *testing.T) {
	s := startServe(t, serveOpts{idle: 30 * time.Second})
	e := s.e
	tok, a, tdb := commandAgent(t, e, "host-big")
	line := "true " + strings.Repeat("x", modules.MaxCommandLineBytes-10)
	total := 0
	for i := range 4 {
		var cmds []map[string]any
		for range 90 {
			cmds = append(cmds, map[string]any{"command_line": line})
		}
		e.admin("POST", "/agents/"+itoa64(a.ID)+"/command-sets", map[string]any{"name": fmt.Sprintf("big-%d", i),
			"purpose": "run_only", "os_user": "svc", "commands": cmds})
		total += len(cmds) * len(line)
	}
	if total <= modules.MaxRequestBody {
		t.Fatalf("definitions of %d bytes do not test the limit", total)
	}
	setID := seamSet(t, e, a, "echo version=7.1")
	reqID, _ := requestRun(t, e, a, setID)

	host, port, err := net.SplitHostPort(s.agentAddr)
	if err != nil {
		t.Fatal(err)
	}
	portNum, _ := strconv.Atoi(port)
	transport := func() *modules.RemoteTransport {
		tr, err := modules.NewRemoteTransport(&modules.AgentConfig{AgentID: "m-big", CentralHost: host, CentralPort: portNum,
			Token: tok, CAPEMs: e.c.currentCAPEMs()})
		if err != nil {
			t.Fatal(err)
		}
		return tr
	}
	db := agentLocalDB(t)
	var logs lockedBuffer
	rt := modules.NewAgentRuntime(db, transport(), log.New(&logs, "", 0))
	stop, quit := make(chan struct{}), make(chan struct{})
	defer close(quit)
	go func() {
		deadline := time.Now().Add(60 * time.Second)
		for time.Now().Before(deadline) {
			var finished int
			err := db.Read().QueryRow(`SELECT count(*) FROM command_executions WHERE finished_at > 0`).Scan(&finished)
			if err == nil && finished > 0 {
				break
			}
			select {
			case <-quit:
				return
			case <-time.After(10 * time.Millisecond):
			}
		}
		close(stop)
	}()
	rt.Run(stop)
	// The stop cuts the requests of the transport Run used: the round of
	// the next start sends what is left.
	modules.NewAgentRuntime(db, transport(), log.New(&logs, "", 0)).Round()
	if stored := models.GetLocalState(db.Read(), modules.StateCommandDefinitions); len(stored) <= modules.MaxRequestBody {
		t.Fatalf("stored definitions: %d bytes; log: %s", len(stored), logs.String())
	}
	if st := reqState(t, e, a.ID, reqID); st != models.RequestDone {
		t.Fatalf("request: %s; log: %s", st, logs.String())
	}
	var runs int
	if err := tdb.Read().QueryRow(`SELECT count(*) FROM command_runs WHERE set_id = ? AND finished_at > 0`, setID).Scan(&runs); err != nil || runs != 1 {
		t.Fatalf("finished runs of the set: %d %v", runs, err)
	}

	// ctl command-sets list, through the management API of the same
	// listener. ctl reads the central's data directory; its central.db is
	// open in this process, so ctl gets a copy of what it reads: a
	// central.db with agent_listen, the CA certificate and the CLI secret.
	ctlDir := t.TempDir()
	cdb, err := modules.OpenSQLite(filepath.Join(ctlDir, modules.CentralDBFileName), modules.SchemaCentral)
	if err != nil {
		t.Fatal(err)
	}
	err = cdb.Write(func(tx models.Writer) error {
		_, err := tx.Exec(`INSERT INTO settings (key, value) VALUES ('config.agent_listen', ?)`, s.agentAddr)
		return err
	})
	cdb.Close()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(ctlDir, modules.CABundleFileName), []byte(strings.Join(e.c.currentCAPEMs(), "")), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(ctlDir, modules.CLISecretFileName), []byte(e.cliKey), 0o600); err != nil {
		t.Fatal(err)
	}
	if b, _ := json.Marshal(e.admin("GET", "/agents/"+itoa64(a.ID)+"/command-sets", nil)); len(b) <= modules.MaxRequestBody {
		t.Fatalf("the list answer of %d bytes does not test the limit", len(b))
	}
	modules.SetDataDir(ctlDir)
	defer modules.SetDataDir(e.c.dataDir)
	out, err := os.CreateTemp(t.TempDir(), "stdout")
	if err != nil {
		t.Fatal(err)
	}
	saved := os.Stdout
	os.Stdout = out
	err = (&modules.CtlCommandSetsListCmd{Agent: a.ID}).Run(&modules.CtlCmd{})
	os.Stdout = saved
	if err != nil {
		t.Fatalf("ctl command-sets list: %v", err)
	}
	_, _ = out.Seek(0, io.SeekStart)
	printed, _ := io.ReadAll(out)
	for _, name := range []string{"big-0", "big-3", "seam"} {
		if !strings.Contains(string(printed), name) {
			t.Fatalf("ctl command-sets list printed %q", printed)
		}
	}
}
