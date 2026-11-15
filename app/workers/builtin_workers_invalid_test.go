package workers

import (
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"mimamori/app/models"
	"mimamori/app/modules"
)

// A worker count in builtin-agent.db that is not a whole number from 1 to
// 64 keeps the built-in agent from starting. The screens get a message of
// their own that names no command; the central's log names the command
// that repairs it, with --builtin.
func TestBuiltinAgentInvalidWorkers(t *testing.T) {
	e := newTestEnv(t)
	b, _ := models.GetBuiltinAgent(e.c.db.Read())
	fakeAuditTools(t)
	enabled := true
	e.c.conf().BuiltinAgent = &enabled
	db, err := modules.OpenSQLite(filepath.Join(e.c.dataDir, modules.BuiltinAgentLocalDBName), modules.SchemaAgentLocal)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Write(func(tx models.Writer) error { return models.SetLocalState(tx, "workers", "abc") }); err != nil {
		t.Fatal(err)
	}
	db.Close()

	stop := make(chan struct{})
	close(stop)
	e.c.startBuiltinAgent(b, stop)()
	running, msg := e.c.builtinStatus()
	if running || msg.Code != "builtin.workers_invalid" {
		t.Fatalf("status: running %v, %+v", running, msg)
	}
	if want := "The worker count stored for the built-in agent is not a whole number from 1 to 64."; msg.Text() != want {
		t.Fatalf("screen text %q, want %q", msg.Text(), want)
	}
	if want := `agent workers: stored value "abc" is not a whole number from 1 to 64; set it with agent set-workers N --builtin`; e.c.builtinLogReason() != want {
		t.Fatalf("log reason %q, want %q", e.c.builtinLogReason(), want)
	}

	// GET /overview carries the message with its code; the text names no
	// command.
	rec := httptest.NewRecorder()
	e.c.handleOverview(rec, httptest.NewRequest("GET", "/api/v1/overview", nil))
	o, _ := decode[map[string]any](t, rec)["builtin_agent"].(map[string]any)
	i18n, _ := o["message_i18n"].(map[string]any)
	if text, _ := o["message"].(string); o["running"] != false || text != msg.Text() || i18n["code"] != "builtin.workers_invalid" || strings.Contains(text, "set-workers") {
		t.Fatalf("overview: %v", o)
	}

	// Another reason is logged with the text the screens show.
	e.c.setBuiltinStatus(false, modules.M("builtin.auditd_unusable", "cause", modules.M("auditd.not_running")))
	if got, want := e.c.builtinLogReason(), "auditd is not usable: "+modules.M("auditd.not_running").Text(); got != want {
		t.Fatalf("log reason after another status: %q, want %q", got, want)
	}
}
