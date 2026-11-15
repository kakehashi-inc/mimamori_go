package workers

import (
	"testing"

	"mimamori/app/models"
	"mimamori/app/modules"
)

// TestAgentListSaysWhetherTheAgentResponds: the agents list and the detail
// give the central's judgement whether the agent is responding (it synced
// within three of its check intervals and ten minutes and did not say since
// then that it stops) and the check interval the last sync gave it, so that
// the screens mark the last report as old by that judgement.
func TestAgentListSaysWhetherTheAgentResponds(t *testing.T) {
	e := newTestEnv(t)
	issued := e.admin("POST", "/tokens", map[string]any{"name": "host-a"})
	tok := issued["value"].(string)
	id := int64(issued["token"].(map[string]any)["agent_id"].(float64))
	find := func() map[string]any {
		t.Helper()
		for _, a := range e.admin("GET", "/agents", nil)["list"].([]any) {
			if m := a.(map[string]any); m["id"] == float64(id) {
				detail := e.admin("GET", "/agents/"+itoa64(id), nil)
				if detail["responding"] != m["responding"] || detail["check_interval_sec"] != m["check_interval_sec"] {
					t.Fatalf("list %v and detail %v differ", m, detail)
				}
				return m
			}
		}
		t.Fatal("agent not listed")
		return nil
	}
	if a := find(); a["responding"] != false || a["check_interval_sec"] != nil {
		t.Fatalf("never synced: responding %v, interval %v", a["responding"], a["check_interval_sec"])
	}
	syncAgent(t, e, tok, "")
	if a := find(); a["responding"] != true || a["check_interval_sec"] != float64(modules.DefaultCheckIntervalSec) {
		t.Fatalf("synced: responding %v, interval %v", a["responding"], a["check_interval_sec"])
	}
	// The agent said that it stops: not responding until it syncs again.
	stopping := batchOf()
	stopping.Stopping = true
	if rec := e.agentCall("report", tok, "m-"+tok[len(tok)-6:], stopping); rec.Code != 200 {
		t.Fatalf("report: %d", rec.Code)
	}
	if a := find(); a["responding"] != false {
		t.Fatalf("stopping: responding %v", a["responding"])
	}
	syncAgent(t, e, tok, "")
	// Silent longer than three intervals and ten minutes.
	if err := e.c.db.Write(func(tx models.Writer) error {
		_, err := tx.Exec(`UPDATE agents SET check_interval_sec = 3600, last_sync_at = last_sync_at - 3*3600 - 601 WHERE id = ?`, id)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if a := find(); a["responding"] != false || a["check_interval_sec"] != float64(3600) {
		t.Fatalf("silent: responding %v, interval %v", a["responding"], a["check_interval_sec"])
	}
}
