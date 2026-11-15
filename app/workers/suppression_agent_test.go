package workers

import (
	"net/http"
	"testing"

	"mimamori/app/models"
)

// TestSuppressionAgentID: a suppression without agent_id (absent or null)
// is for all agents; agent_id names one agent, and a value that is no
// agent's id (also 0 and negative values) is refused with 400 and nothing
// is created.
func TestSuppressionAgentID(t *testing.T) {
	e := newTestEnv(t)
	b, err := models.GetBuiltinAgent(e.c.db.Read())
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range []any{-5, 0, 99, -1} {
		code, body := e.adminRaw("POST", "/suppressions", map[string]any{"agent_id": id, "type": "exec"})
		if code != http.StatusBadRequest {
			t.Fatalf("agent_id %v: %d %s", id, code, body)
		}
	}
	if list, _ := models.ListSuppressions(e.c.db.Read()); len(list) != 0 {
		t.Fatalf("refused conditions were created: %+v", list)
	}
	all := e.admin("POST", "/suppressions", map[string]any{"type": "exec"})
	null := e.admin("POST", "/suppressions", map[string]any{"agent_id": nil, "type": "exec", "os_user": "app"})
	one := e.admin("POST", "/suppressions", map[string]any{"agent_id": b.ID, "type": "exec"})
	if all["agent_id"] != nil || null["agent_id"] != nil || one["agent_id"] != float64(b.ID) {
		t.Fatalf("all %v, null %v, one %v", all["agent_id"], null["agent_id"], one["agent_id"])
	}
}
