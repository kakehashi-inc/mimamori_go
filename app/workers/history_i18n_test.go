package workers

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"strings"
	"testing"

	"mimamori/app/models"
	"mimamori/app/modules"
)

// The coded history details render exactly as the English the call sites
// wrote before they had codes (the "old" column copies those expressions).
func TestHistoryDetailEnglish(t *testing.T) {
	const host, at = "192.0.2.7", "2026-01-02T03:04:05Z"
	sendErr := fmt.Errorf("dial tcp 192.0.2.9:25: connection refused")
	cases := []struct {
		name string
		got  modules.Msg
		old  string
	}{
		{"login smtp off", loginDetail(host, "history.detail.login_how.smtp_off"), "from " + host + "; two-step verification skipped (SMTP not configured)"},
		{"login trusted", loginDetail(host, "history.detail.login_how.trusted"), "from " + host + "; verified browser"},
		{"login code", loginDetail(host, "history.detail.login_how.code"), "from " + host + "; code verified"},
		{"login code trust", loginDetail(host, "history.detail.login_how.code_trust"), "from " + host + "; code verified" + ", browser trusted"},
		{"locked", modules.M("history.detail.login_locked", "host", host), "too many failed logins from " + host},
		{"login code failed", modules.M("history.detail.login_code_failed", "host", host, "cause", modules.AsMsg(sendErr)),
			"the login code for a login from " + host + " could not be sent: " + sendErr.Error()},
		{"step-up code failed", modules.M("history.detail.step_up_code_failed", "host", host, "cause", modules.AsMsg(sendErr)),
			"the re-authentication code for a request from " + host + " could not be sent: " + sendErr.Error()},
		{"email code failed", modules.M("history.detail.email_code_failed", "email", "n@example.com", "cause", modules.AsMsg(sendErr)),
			"the code for the change to " + "n@example.com" + " could not be sent: " + sendErr.Error()},
		{"own password", modules.M("history.detail.own_password_changed"), "own password changed"},
		{"account created", modules.M("history.detail.account_created", "role", "viewer", "language", "ja"), "role " + "viewer" + ", language " + "ja"},
		{"role", modules.M("history.detail.change", "from", "viewer", "to", "admin"), "viewer" + " -> " + "admin"},
		{"password reset", modules.M("history.detail.password_reset_admin"), "password reset by an administrator"},
		{"email admin", modules.M("history.detail.email_changed_admin", "from", "a@x", "to", "b@x"), "a@x" + " -> " + "b@x" + " (changed by an administrator)"},
		{"own change", modules.M("history.detail.change_own", "from", "Old", "to", "New"), "Old" + " -> " + "New" + " (own change)"},
		{"notify on", modules.M("history.detail.notify_on"), "notification mails on"},
		{"notify off", modules.M("history.detail.notify_off"), "notification mails off"},
		{"rule saved", modules.M("history.detail.rule_saved", "version", 3, "comment", "tighten; more"), "version " + itoa(3) + ": " + "tighten; more"},
		{"rule saved empty", modules.M("history.detail.rule_saved", "version", 3, "comment", ""), "version " + itoa(3) + ": "},
		{"rule applied", modules.M("history.detail.rule_applied", "version", 2, "agents", []string{"web-1", "(central)"}),
			fmt.Sprintf("version %d to %s", 2, strings.Join([]string{"web-1", "(central)"}, ", "))},
		{"rule unassigned", modules.M("history.detail.rule_unassigned", "version", 2, "agent", "web-1"), fmt.Sprintf("version %d from %s", 2, "web-1")},
		{"rule fallback", modules.M("history.detail.rule_fallback", "id", int64(12)), "rule " + itoa64(12)},
		{"apply result", modules.M("history.detail.apply_result", "rule", modules.Raw("r1"), "version", 4,
			"result", historyParts([]modules.Msg{modules.M("apply.applied_with_warnings", "count", 1), modules.M("apply.warn_user_missing", "value", `"bob"`)})),
			fmt.Sprintf("%s version %d: %s", "r1", 4, strings.Join([]string{"applied with 1 warning(s)", `OS user "bob" does not exist`}, "; "))},
		{"apply result fallback", modules.M("history.detail.apply_result", "rule", modules.M("history.detail.rule_fallback", "id", int64(9)), "version", 1,
			"result", historyParts([]modules.Msg{modules.Raw("applied (3 audit rules)")})),
			fmt.Sprintf("%s version %d: %s", fmt.Sprintf("rule %d", 9), 1, "applied (3 audit rules)")},
		{"template builtin", modules.M("history.detail.template_from_builtin", "name", "sshd"), "from built-in template " + "sshd"},
		{"template rule id", modules.M("history.detail.template_from_rule_id", "id", int64(5), "version", 2), fmt.Sprintf("from rule %d version %d", 5, 2)},
		{"template rule name", modules.M("history.detail.template_from_rule_name", "rule", "r1", "version", 2), fmt.Sprintf("from rule %s version %d", "r1", 2)},
		{"template created", modules.M("history.detail.template_created"), "created"},
		{"token issued", modules.M("history.detail.token_issued", "expires", at), "expires " + at},
		{"token issued rule", modules.M("history.detail.token_issued_rule", "expires", at, "rule", int64(3), "version", 2),
			"expires " + at + "; rule id " + itoa64(3) + " version " + itoa(2)},
		{"token reissued", modules.M("history.detail.token_reissued", "expires", at), "previous values invalidated; expires " + at},
		{"token unbound", modules.M("history.detail.token_unbound", "instance", "m-1"), "binding to the agent instance removed (was " + "m-1" + ")"},
		{"token expired", modules.M("history.detail.token_expired"), "the token expired; connections with it are refused"},
		{"token renewed", modules.M("history.detail.token_renewed"), "agent switched to the renewed token; the previous value is no longer accepted"},
		{"token bound", modules.M("history.detail.token_bound", "instance", "m-1"), "bound to agent instance " + "m-1"},
		{"renewal issued", modules.M("history.detail.token_renewal_issued", "expires", at), "new token value handed to the agent; expires " + at},
		{"ca discarded", modules.M("history.detail.ca_discarded"), "expired CA removed"},
		{"ca created", modules.M("history.detail.ca_created"), "private CA created"},
		{"ca next", modules.M("history.detail.ca_next_created", "at", at), fmt.Sprintf("new CA created; distributing to agents; server certificate switches after %s", at)},
		{"ca switched", modules.M("history.detail.ca_switched"), "server certificate now signed by the new CA"},
		{"server cert", modules.M("history.detail.server_cert_names", "names", []string{"localhost", "127.0.0.1"}), "names: " + strings.Join([]string{"localhost", "127.0.0.1"}, ", ")},
		{"suppression created", modules.M("history.detail.suppression_created", "cond", "type=exec exe=/usr/bin/*", "count", 2),
			"type=exec exe=/usr/bin/*" + "; " + "2" + " open detection(s) now suppressed"},
		{"suppression deleted", modules.M("history.detail.suppression_deleted", "cond", "type=exec", "count", 0), "type=exec" + "; " + "0" + " detection(s) no longer suppressed"},
		{"acknowledged", modules.M("history.detail.acknowledged", "count", 5), "5" + " detection(s)"},
		{"dropped", modules.M("history.detail.dropped", "count", 7), fmt.Sprintf("the agent dropped %d detection(s) at its storage limit", 7)},
		{"config", historyParts([]modules.Msg{modules.M("history.detail.config_default", "name", "a"), modules.M("history.detail.config_secret", "name", "smtp.password"),
			modules.M("history.detail.config_value", "name", "server_names", "value", "a.example,b.example")}),
			strings.Join([]string{"a" + " = (default)", "smtp.password" + " changed", "server_names" + " = " + "a.example,b.example"}, "; ")},
		{"set created", modules.M("history.detail.command_set_rev", "set", int64(4), "revision", 1), fmt.Sprintf("set %d revision %d", 4, 1)},
		{"set updated", historyParts([]modules.Msg{modules.M("history.detail.command_set_rev", "set", int64(4), "revision", 2)}), fmt.Sprintf("set %d revision %d", 4, 2)},
		{"set renamed", historyParts([]modules.Msg{modules.M("history.detail.command_set_rev", "set", int64(4), "revision", 2), modules.M("history.detail.command_renamed", "name", "old")}),
			fmt.Sprintf("set %d revision %d", 4, 2) + "; renamed from " + "old"},
		{"set deleted", historyParts([]modules.Msg{modules.M("history.detail.command_set", "set", int64(4))}), fmt.Sprintf("set %d", 4)},
		{"set deleted with request", historyParts([]modules.Msg{modules.M("history.detail.command_set", "set", int64(4)), modules.M("history.detail.command_request_discarded_with", "request", int64(8))}),
			fmt.Sprintf("set %d", 4) + fmt.Sprintf("; request %d discarded", 8)},
		{"copied", modules.M("history.detail.command_copied", "source", "web-1 / backup"), "copied from " + "web-1 / backup"},
		{"request", modules.M("history.detail.command_request", "request", int64(8)), fmt.Sprintf("request %d", 8)},
		{"not delivered", modules.M("history.detail.request_not_delivered", "request", int64(8), "expiry", modules.CommandRequestExpiry.String()),
			fmt.Sprintf("request %d was not delivered to the agent within %s", 8, modules.CommandRequestExpiry)},
		{"ended expired", modules.M("history.detail.request_ended", "request", int64(8), "why", modules.M("history.detail.request_ended_why.expired")),
			fmt.Sprintf("request %d ended without a run: %s", 8, "it reached the agent after its expiry")},
		{"ended unknown", modules.M("history.detail.request_ended", "request", int64(8), "why", modules.M("history.detail.request_ended_why.unknown_set")),
			fmt.Sprintf("request %d ended without a run: %s", 8, "the agent does not hold this command set")},
		{"set target fallback", modules.M("history.target.command_set", "agent", "web-1", "set", modules.M("history.target.command_set_fallback", "set", int64(4))),
			"web-1" + " / " + fmt.Sprintf("set %d", 4)},
	}
	for _, c := range cases {
		if c.got.Text() != c.old {
			t.Errorf("%s: %q, want %q", c.name, c.got.Text(), c.old)
		}
		// The stored JSON renders the same English after decoding.
		stored := modules.HistoryTextsJSON(modules.Msg{}, c.got)
		if got := modules.ParseHistoryTexts(stored, "", c.old); got.Detail == nil {
			t.Errorf("%s: stored %s does not render as %q", c.name, stored, c.old)
		}
	}
}

// historyRow is a history item as answered, with its coded parts.
type historyRow struct {
	Category    string           `json:"category"`
	Action      string           `json:"action"`
	AccountName string           `json:"account_name"`
	Target      string           `json:"target"`
	Detail      string           `json:"detail"`
	TargetI18n  *modules.Problem `json:"target_i18n"`
	DetailI18n  *modules.Problem `json:"detail_i18n"`
}

// adminOnlyCodes returns the codes the frontend keeps in the administrator
// bundle (the srv section of frontend/src/admin/i18n/en.json).
func adminOnlyCodes(t *testing.T) map[string]bool {
	t.Helper()
	b, err := os.ReadFile("../../frontend/src/admin/i18n/en.json")
	if err != nil {
		t.Fatal(err)
	}
	var doc map[string]any
	if err := json.Unmarshal(b, &doc); err != nil {
		t.Fatal(err)
	}
	out := map[string]bool{}
	var walk func(prefix string, v any)
	walk = func(prefix string, v any) {
		if m, ok := v.(map[string]any); ok {
			for k, sub := range m {
				walk(prefix+"."+k, sub)
			}
			return
		}
		out[strings.TrimPrefix(prefix, ".srv.")] = true
	}
	walk("", map[string]any{"srv": doc["srv"]})
	return out
}

// codesIn returns every code of a decoded message, its location and its
// nested messages.
func codesIn(v any, out []string) []string {
	switch x := v.(type) {
	case map[string]any:
		if c, ok := x["code"].(string); ok && c != "" {
			out = append(out, c)
		}
		for _, sub := range x {
			out = codesIn(sub, out)
		}
	case []any:
		for _, sub := range x {
			out = codesIn(sub, out)
		}
	}
	return out
}

// The history and the token history answer the coded target and detail
// next to the unchanged English; rows without codes answer none, and a
// viewer receives neither the hidden categories nor administrator texts.
func TestHistoryCodedTexts(t *testing.T) {
	e := newTestEnv(t)
	e.admin("POST", "/accounts", map[string]any{"email": "v@example.com", "display_name": "Vee", "role": "viewer", "password": "Viewer-pass-123"})
	viewer := login(t, e, "v@example.com", "Viewer-pass-123")
	e.admin("PUT", "/config", map[string]any{"set": map[string]string{"retention_days": "90", "server_names": "a.example.com"}})
	e.admin("POST", "/rules", map[string]any{"name": "r1"})
	tok, a, tdb := commandAgent(t, e, "host-hist")
	e.admin("POST", "/rules/1/apply", map[string]any{"version": 1, "agent_ids": []int64{a.ID}})
	e.admin("POST", "/suppressions", map[string]any{"type": "exec", "exe": "/usr/bin/*"})
	e.admin("POST", "/templates", map[string]any{"name": "t1", "from_builtin": "nginx"})
	setBody := commandSetBody("backup", "run_only")
	setBody["os_user"] = "svc"
	setID := int64(e.admin("POST", "/agents/"+itoa64(a.ID)+"/command-sets", setBody)["id"].(float64))
	e.admin("DELETE", "/agents/"+itoa64(a.ID)+"/command-sets/"+itoa64(setID), nil)
	// A request for a set the agent's database does not hold names it by
	// its id.
	gone := setID + 100
	e.c.historyMsg(nil, models.HistCommand, "request_expired", e.c.commandTarget(a, tdb, gone),
		modules.M("history.detail.command_request", "request", int64(3)))
	msg := modules.M("apply.applied", "count", 2)
	reportAgent(t, e, tok, modules.ReportRequest{Apply: &modules.ApplyResult{Status: "ok", Message: msg.Text(), MessageI18n: &msg, RuleID: 1, Version: 1, At: 50}})
	// A row of an older build and a row whose stored code no longer
	// matches its English.
	if err := models.InsertHistory(e.c.db, &models.HistoryItem{AccountName: "system", Category: models.HistRule, Action: "old", Target: "r9", Detail: "old text"}); err != nil {
		t.Fatal(err)
	}
	bad := modules.HistoryTextsJSON(modules.Msg{}, modules.M("history.detail.token_expired"))
	if err := models.InsertHistory(e.c.db, &models.HistoryItem{AccountName: "system", Category: models.HistRule, Action: "edited", Target: "r9", Detail: "edited text", I18n: bad}); err != nil {
		t.Fatal(err)
	}

	rec := e.do(e.agent, "GET", "/admin/v1/history?limit=500", nil, map[string]string{"Authorization": "Bearer " + e.cliKey}, "127.0.0.1:5000")
	rows := decode[struct {
		Items []historyRow `json:"items"`
	}](t, rec).Items
	byAction := map[string]historyRow{}
	for _, h := range rows {
		byAction[h.Category+"/"+h.Action] = h
		if h.DetailI18n != nil && h.DetailI18n.Text() != h.Detail {
			t.Errorf("%s/%s: detail_i18n %q, detail %q", h.Category, h.Action, h.DetailI18n.Text(), h.Detail)
		}
		if h.TargetI18n != nil && h.TargetI18n.Text() != h.Target {
			t.Errorf("%s/%s: target_i18n %q, target %q", h.Category, h.Action, h.TargetI18n.Text(), h.Target)
		}
		// Every program-written detail carries its code.
		if h.Detail != "" && h.DetailI18n == nil && h.Action != "old" && h.Action != "edited" {
			t.Errorf("%s/%s: detail %q without a code", h.Category, h.Action, h.Detail)
		}
	}
	want := map[string]string{
		"account/created":         "history.detail.account_created",
		"account/login":           "history.detail.login",
		"config/updated":          "common.problems",
		"rule/created":            "rule.version.created_empty",
		"rule/applied":            "history.detail.rule_applied",
		"rule/apply_result_ok":    "history.detail.apply_result",
		"token/issued":            "history.detail.token_issued",
		"suppression/created":     "history.detail.suppression_created",
		"template/created":        "history.detail.template_from_builtin",
		"command/created":         "history.detail.command_set_rev",
		"command/deleted":         "history.detail.command_set",
		"command/request_expired": "history.detail.command_request",
		"certificate/ca_created":  "history.detail.ca_created",
	}
	for key, code := range want {
		h, ok := byAction[key]
		if !ok {
			t.Errorf("%s: no row", key)
			continue
		}
		if h.DetailI18n == nil || h.DetailI18n.Code != code {
			t.Errorf("%s: detail_i18n %+v, want %s", key, h.DetailI18n, code)
		}
	}
	if h := byAction["rule/apply_result_ok"]; h.Detail != "r1 version 1: applied (2 audit rules)" {
		t.Errorf("apply result detail %q", h.Detail)
	}
	if h := byAction["command/request_expired"]; h.TargetI18n == nil || h.TargetI18n.Code != "history.target.command_set" ||
		h.Target != "host-hist / set "+itoa64(gone) {
		t.Errorf("fallback target: %q %+v", h.Target, h.TargetI18n)
	}
	if h := byAction["command/created"]; h.TargetI18n != nil || h.Target != "host-hist / backup" {
		t.Errorf("set target is a value: %q %+v", h.Target, h.TargetI18n)
	}
	for _, key := range []string{"rule/old", "rule/edited"} {
		if h := byAction[key]; h.DetailI18n != nil || h.TargetI18n != nil {
			t.Errorf("%s: coded parts %+v %+v", key, h.TargetI18n, h.DetailI18n)
		}
	}
	if !strings.Contains(rec.Body.String(), `"detail_i18n"`) || strings.Count(rec.Body.String(), `"target_i18n"`) != 1 {
		t.Errorf("target_i18n is answered only where the target is coded: %s", rec.Body.String())
	}

	// The token history on the agent detail.
	detail := e.admin("GET", "/agents/"+itoa64(a.ID), nil)
	b, _ := json.Marshal(detail["token_history"])
	var tokenRows []historyRow
	if err := json.Unmarshal(b, &tokenRows); err != nil || len(tokenRows) == 0 {
		t.Fatalf("token history: %s", b)
	}
	for _, h := range tokenRows {
		if h.DetailI18n == nil || !strings.HasPrefix(h.DetailI18n.Code, "history.detail.token_") || h.DetailI18n.Text() != h.Detail {
			t.Errorf("token history %+v", h)
		}
	}

	// A viewer: no hidden category and no administrator code.
	admin := adminOnlyCodes(t)
	for _, path := range []string{"/api/v1/history?limit=500", "/api/v1/agents/" + itoa64(a.ID)} {
		vrec := e.do(e.web, "GET", path, nil, map[string]string{"Cookie": sessionCookie + "=" + viewer}, "192.0.2.1:1")
		if vrec.Code != http.StatusOK {
			t.Fatalf("viewer %s: %d", path, vrec.Code)
		}
		var body any
		_ = json.Unmarshal(vrec.Body.Bytes(), &body)
		for _, c := range codesIn(body, nil) {
			if admin[c] {
				t.Errorf("viewer %s receives the administrator code %s", path, c)
			}
		}
	}
	vrec := e.do(e.web, "GET", "/api/v1/history?limit=500", nil, map[string]string{"Cookie": sessionCookie + "=" + viewer}, "192.0.2.1:1")
	for _, h := range decode[struct {
		Items []historyRow `json:"items"`
	}](t, vrec).Items {
		if h.Category == models.HistAccount || h.Category == models.HistConfig || h.Category == models.HistCommand {
			t.Errorf("viewer receives %s/%s", h.Category, h.Action)
		}
		if h.Category == models.HistRule && h.Action == "applied" && h.DetailI18n == nil {
			t.Errorf("viewer row %+v", h)
		}
	}
}
