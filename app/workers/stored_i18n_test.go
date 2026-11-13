package workers

import (
	"encoding/json"
	"strings"
	"testing"

	"mimamori/app/models"
	"mimamori/app/modules"
)

// codedField returns the message of an answer field ("<field>_i18n"), nil
// when the field is absent or null.
func codedField(t *testing.T, obj map[string]any, key string) *modules.Problem {
	t.Helper()
	v, ok := obj[key]
	if !ok || v == nil {
		return nil
	}
	b, _ := json.Marshal(v)
	var p modules.Problem
	if err := json.Unmarshal(b, &p); err != nil {
		t.Fatalf("%s: %v", key, err)
	}
	return &p
}

// codedList returns the messages of a list answer field.
func codedList(t *testing.T, obj map[string]any, key string) []modules.Problem {
	t.Helper()
	v, ok := obj[key]
	if !ok || v == nil {
		return nil
	}
	b, _ := json.Marshal(v)
	var ps []modules.Problem
	if err := json.Unmarshal(b, &ps); err != nil {
		t.Fatalf("%s: %v", key, err)
	}
	return ps
}

// The application result an agent reports is answered with its coded
// message and warnings; texts that do not match the English are not shown,
// and an agent of an older build (English only) is answered without them.
func TestApplyResultCodedTexts(t *testing.T) {
	e := newTestEnv(t)
	tok, a, _ := commandAgent(t, e, "host-i18n")
	msg := modules.M("apply.applied_with_warnings", "count", 2)
	w1 := modules.M("apply.warn_user_missing", "value", `"alice"`)
	w2 := modules.M("apply.warn_dir_missing", "value", `"/srv"`)
	ap := &modules.ApplyResult{Status: "warning", Message: msg.Text(), At: 100, Warnings: []string{w1.Text(), "changed by hand"},
		MessageI18n: &msg, WarningsI18n: []modules.Msg{w1, w2}}
	reportAgent(t, e, tok, modules.ReportRequest{Apply: ap, HostInfo: &modules.HostInfo{Hostname: "h", LastApply: ap}})

	check := func(name string, apply map[string]any) {
		t.Helper()
		if apply["message"] != "applied with 2 warning(s)" {
			t.Fatalf("%s: %v", name, apply)
		}
		if m := codedField(t, apply, "message_i18n"); m == nil || m.Code != "apply.applied_with_warnings" || m.Text() != apply["message"] {
			t.Fatalf("%s: message_i18n %v", name, apply)
		}
		ws := codedList(t, apply, "warnings_i18n")
		if len(ws) != 2 || ws[0].Code != "apply.warn_user_missing" || ws[0].Text() != `OS user "alice" does not exist` ||
			ws[1].Code != "" || ws[1].Text() != "changed by hand" {
			t.Fatalf("%s: warnings_i18n %v", name, apply)
		}
	}
	detail := e.admin("GET", "/agents/"+itoa64(a.ID), nil)
	check("detail", detail["apply"].(map[string]any))
	check("host info", detail["host_info"].(map[string]any)["last_apply"].(map[string]any))
	for _, item := range e.admin("GET", "/agents", nil)["list"].([]any) {
		if s := item.(map[string]any); s["id"].(float64) == float64(a.ID) {
			check("list", s["apply"].(map[string]any))
		}
	}

	// A coded message that does not render as the English one is dropped.
	other := modules.M("apply.applied", "count", 3)
	reportAgent(t, e, tok, modules.ReportRequest{Apply: &modules.ApplyResult{Status: "ok", Message: "applied (4 audit rules)", At: 101, MessageI18n: &other}})
	apply := e.admin("GET", "/agents/"+itoa64(a.ID), nil)["apply"].(map[string]any)
	if _, ok := apply["message_i18n"]; ok || apply["message"] != "applied (4 audit rules)" {
		t.Fatalf("mismatch: %v", apply)
	}
	if _, ok := apply["warnings_i18n"]; ok {
		t.Fatalf("no warnings: %v", apply)
	}

	// An agent of an older build: English only.
	reportAgent(t, e, tok, modules.ReportRequest{Apply: &modules.ApplyResult{Status: "error", Message: "validation failed: x", At: 102, Warnings: []string{"w"}}})
	apply = e.admin("GET", "/agents/"+itoa64(a.ID), nil)["apply"].(map[string]any)
	if _, ok := apply["message_i18n"]; ok || apply["message"] != "validation failed: x" {
		t.Fatalf("old agent: %v", apply)
	}
	if _, ok := apply["warnings_i18n"]; ok {
		t.Fatalf("old agent: %v", apply)
	}

	// The message the central stores when it removes the rules of the
	// disabled built-in agent.
	b, err := models.GetBuiltinAgent(e.c.db)
	if err != nil {
		t.Fatal(err)
	}
	none := modules.M("apply.builtin_disabled")
	if err := models.SetAgentApplyNone(e.c.db, b.ID, none.Text(), applyI18nText(&none, nil), 5); err != nil {
		t.Fatal(err)
	}
	apply = e.admin("GET", "/agents/"+itoa64(b.ID), nil)["apply"].(map[string]any)
	if m := codedField(t, apply, "message_i18n"); m == nil || m.Code != "apply.builtin_disabled" ||
		apply["message"] != "the built-in agent is disabled; its audit rules were removed" {
		t.Fatalf("built-in agent: %v", apply)
	}
}

// The state of the built-in agent is answered with its coded message.
func TestBuiltinStateCodedMessage(t *testing.T) {
	e := newTestEnv(t)
	e.c.setBuiltinStatus(false, modules.M("builtin.auditd_unusable", "cause", modules.M("auditd.not_running")))
	o := e.admin("GET", "/overview", nil)["builtin_agent"].(map[string]any)
	if o["message"] != "auditd is not usable: auditd is not running" {
		t.Fatalf("overview: %v", o)
	}
	if m := codedField(t, o, "message_i18n"); m == nil || m.Code != "builtin.auditd_unusable" || m.Text() != o["message"] {
		t.Fatalf("overview: %v", o)
	}
	e.c.setBuiltinStatus(true, modules.Msg{})
	o = e.admin("GET", "/overview", nil)["builtin_agent"].(map[string]any)
	if o["message"] != "" || o["message_i18n"] != nil {
		t.Fatalf("running: %v", o)
	}
}

// The comment the program writes for the first version of a rule is
// answered with its code; a comment written by a person is not.
func TestRuleVersionCommentCoded(t *testing.T) {
	e := newTestEnv(t)
	cases := []struct {
		body    map[string]any
		code    string
		comment string
	}{
		{map[string]any{"name": "r-empty"}, "rule.version.created_empty", "created empty"},
		{map[string]any{"name": "r-tpl", "templates": []string{"base", "nginx"}}, "rule.version.from_templates", "expanded from templates: base, nginx"},
		{map[string]any{"name": "r-content", "content": map[string]any{}}, "rule.version.created", "created"},
	}
	for _, tc := range cases {
		out := e.admin("POST", "/rules", tc.body)
		id := itoa64(int64(out["id"].(float64)))
		v := out["versions"].([]any)[0].(map[string]any)
		if m := codedField(t, v, "comment_i18n"); v["comment"] != tc.comment || m == nil || m.Code != tc.code || m.Text() != tc.comment {
			t.Fatalf("%s: %v", tc.code, v)
		}
		one := e.admin("GET", "/rules/"+id+"/versions/1", nil)
		if m := codedField(t, one, "comment_i18n"); one["comment"] != tc.comment || m == nil || m.Code != tc.code {
			t.Fatalf("%s version: %v", tc.code, one)
		}
	}
	out := e.admin("POST", "/rules", map[string]any{"name": "r2"})
	id := itoa64(int64(out["id"].(float64)))
	saved := e.admin("PUT", "/rules/"+id, map[string]any{"content": map[string]any{}, "comment": "by hand"})
	for _, item := range saved["versions"].([]any) {
		v := item.(map[string]any)
		if _, ok := v["comment_i18n"]; v["version"].(float64) == 2 && (ok || v["comment"] != "by hand") {
			t.Fatalf("a person's comment: %v", v)
		}
	}
}

// Built-in templates are answered with their coded descriptions, the
// administrator's templates without.
func TestBuiltinTemplateDescriptionCoded(t *testing.T) {
	e := newTestEnv(t)
	e.admin("POST", "/templates", map[string]any{"name": "mine", "description": "my own", "from_builtin": "base"})
	n := 0
	for _, item := range e.admin("GET", "/templates", nil)["list"].([]any) {
		tpl := item.(map[string]any)
		m := codedField(t, tpl, "description_i18n")
		if tpl["builtin"] != true {
			if m != nil {
				t.Fatalf("own template: %v", tpl)
			}
			continue
		}
		n++
		if m == nil || m.Code != "template.builtin."+tpl["name"].(string) || m.Text() != tpl["description"] {
			t.Fatalf("built-in template: %v", tpl)
		}
	}
	if n != len(modules.BuiltinTemplates()) {
		t.Fatalf("%d built-in templates", n)
	}
	one := e.admin("GET", "/templates/nginx", nil)
	if m := codedField(t, one, "description_i18n"); m == nil || m.Text() != one["description"] {
		t.Fatalf("detail: %v", one)
	}
	if _, ok := e.admin("GET", "/templates/mine", nil)["description_i18n"]; ok {
		t.Fatal("own template detail has a coded description")
	}
}

// The problems an agent reports for the command sets it refuses are
// answered with their codes in the same order; those of an agent of an
// older build as their English (data), and a mismatch as well.
func TestAgentCommandProblemsCoded(t *testing.T) {
	e := newTestEnv(t)
	tok, a, _ := commandAgent(t, e, "host-cp")
	reportAgent(t, e, tok, modules.ReportRequest{HostInfo: &modules.HostInfo{Hostname: "h", OSUser: "svc"}})
	path := "/agents/" + itoa64(a.ID) + "/command-sets"
	s1 := int64(e.admin("POST", path, commandSetBody("s1", "collect_output"))["id"].(float64))
	s2 := int64(e.admin("POST", path, commandSetBody("s2", "collect_output"))["id"].(float64))
	sig := syncAgent(t, e, tok, "").CommandSets.Signature

	p1 := modules.FieldProblem("os_user", modules.M("command.spec.os_user_dash"))
	p2 := modules.FieldProblem("timeout_sec", modules.M("command.spec.range_1", "max", 86400))
	reportAgent(t, e, tok, modules.ReportRequest{CommandApply: &modules.CommandApplyResult{Signature: sig, At: 5,
		Problems: []modules.CommandApplyProblem{
			{SetID: s1, SetName: "s1", Problems: []string{p1.Text(), "timeout_sec: changed"}, ProblemsI18n: []modules.Problem{p1, p2}},
			{SetID: s2, SetName: "s2", Problems: []string{"os_user: bad"}},
		}}})
	byID := map[int64]map[string]any{}
	for _, item := range e.admin("GET", path, nil)["sets"].([]any) {
		s := item.(map[string]any)
		byID[int64(s["id"].(float64))] = s
	}
	ps := codedList(t, byID[s1], "agent_problems_i18n")
	if len(ps) != 2 || ps[0].Code != "command.spec.os_user_dash" || ps[0].Text() != `os_user: must not start with "-"` ||
		len(ps[0].Where) != 1 || ps[1].Code != "" || ps[1].Text() != "timeout_sec: changed" {
		t.Fatalf("s1: %v", byID[s1])
	}
	if ps := codedList(t, byID[s2], "agent_problems_i18n"); len(ps) != 1 || ps[0].Code != "" || ps[0].Text() != "os_user: bad" {
		t.Fatalf("s2: %v", byID[s2])
	}
	one := e.admin("GET", path+"/"+itoa64(s1), nil)
	if ps := codedList(t, one, "agent_problems_i18n"); len(ps) != 2 || !strings.Contains(one["agent_problems"].([]any)[0].(string), "os_user") {
		t.Fatalf("one set: %v", one)
	}
}
