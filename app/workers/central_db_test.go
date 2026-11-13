package workers

import (
	"net/http"
	"sync"
	"testing"

	"mimamori/app/models"
	"mimamori/app/modules"
)

// The first requests of a new agent arrive at the same moment: each reads
// the token, checks it and records its use in one write transaction, so
// every request is accepted (none is refused because the row changed
// between its read and its write) and the token is bound once.
func TestAgentAuthTakesTurns(t *testing.T) {
	e := newTestEnv(t)
	issued := e.admin("POST", "/tokens", map[string]any{"name": "host-turns", "valid_days": 10})
	tok := issued["value"].(string)
	const flows = 16
	codes := make([]int, flows)
	var wg sync.WaitGroup
	for i := range flows {
		wg.Add(1)
		go func() {
			defer wg.Done()
			codes[i] = e.agentCall("sync", tok, "m-turns", modules.SyncRequest{}).Code
		}()
	}
	wg.Wait()
	for i, code := range codes {
		if code != http.StatusOK {
			t.Fatalf("request %d: %d", i, code)
		}
	}
	items, _, err := models.ListHistory(e.c.db.Read(), models.HistToken, "host-turns", nil, 100, 0)
	if err != nil {
		t.Fatal(err)
	}
	bound := 0
	for _, h := range items {
		if h.Action == "bound" {
			bound++
		}
	}
	if bound != 1 {
		t.Fatalf("bound recorded %d times", bound)
	}
}
