package modules

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"
)

// frontendOnlyPrefixes are "srv" key groups that have no Go code: the
// frontend translates them from values the server stores as they are
// (history operations: "history.action.<category>.<action>").
var frontendOnlyPrefixes = []string{"history.action."}

func frontendOnly(code string) bool {
	for _, p := range frontendOnlyPrefixes {
		if strings.HasPrefix(code, p) {
			return true
		}
	}
	return false
}

// readSrv returns the "srv" section of a frontend i18n file, flattened to
// code -> text.
func readSrv(t *testing.T, rel string) map[string]string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("..", "..", "frontend", "src", filepath.FromSlash(rel)))
	if err != nil {
		t.Fatal(err)
	}
	var doc map[string]any
	if err := json.Unmarshal(b, &doc); err != nil {
		t.Fatalf("%s: %v", rel, err)
	}
	out := map[string]string{}
	var walk func(prefix string, v any)
	walk = func(prefix string, v any) {
		switch x := v.(type) {
		case string:
			out[prefix] = x
		case map[string]any:
			for k, c := range x {
				walk(prefix+"."+k, c)
			}
		default:
			t.Errorf("%s: srv%s is neither a text nor a section", rel, prefix)
		}
	}
	srv, ok := doc["srv"].(map[string]any)
	if !ok {
		t.Fatalf("%s: no srv section", rel)
	}
	walk("", srv)
	flat := make(map[string]string, len(out))
	for k, v := range out {
		flat[strings.TrimPrefix(k, ".")] = v
	}
	return flat
}

func sortedPlaceholders(s string) []string {
	p := placeholders(s)
	sort.Strings(p)
	return p
}

// TestMessageCatalog keeps the Go catalog and the frontend's "srv" sections
// equal: every code is in the bundle of its audience with the identical
// English template and nowhere else, the Japanese file has the same keys
// with the same placeholders, and no key exists without a Go code except
// the frontend-only groups.
func TestMessageCatalog(t *testing.T) {
	bundles := map[bool]string{false: "i18n", true: "admin/i18n"}
	en := map[bool]map[string]string{}
	ja := map[bool]map[string]string{}
	for admin, dir := range bundles {
		en[admin] = readSrv(t, dir+"/en.json")
		ja[admin] = readSrv(t, dir+"/ja.json")
	}
	for code, def := range msgCatalog {
		if code == "" || frontendOnly(code) {
			t.Errorf("code %q must not be in the Go catalog", code)
			continue
		}
		if !def.isASCII() {
			t.Errorf("%s: the English template is not ASCII", code)
		}
		got, ok := en[def.admin][code]
		if !ok {
			t.Errorf("%s: missing in %s/en.json", code, bundles[def.admin])
		} else if got != def.en {
			t.Errorf("%s: %s/en.json has %q, Go has %q", code, bundles[def.admin], got, def.en)
		}
		if _, ok := en[!def.admin][code]; ok {
			t.Errorf("%s: also in %s/en.json (wrong audience)", code, bundles[!def.admin])
		}
	}
	for admin, dir := range bundles {
		for code, text := range en[admin] {
			if _, ok := msgCatalog[code]; !ok && !frontendOnly(code) {
				t.Errorf("%s/en.json: srv.%s has no Go code", dir, code)
			}
			jt, ok := ja[admin][code]
			if !ok {
				t.Errorf("%s/ja.json: srv.%s missing", dir, code)
				continue
			}
			if !reflect.DeepEqual(sortedPlaceholders(text), sortedPlaceholders(jt)) {
				t.Errorf("srv.%s: placeholders of %s/en.json %v and ja.json %v differ", code, dir, sortedPlaceholders(text), sortedPlaceholders(jt))
			}
		}
		for code := range ja[admin] {
			if _, ok := en[admin][code]; !ok {
				t.Errorf("%s/ja.json: srv.%s is not in en.json", dir, code)
			}
		}
	}
}

func (d msgDef) isASCII() bool {
	for i := 0; i < len(d.en); i++ {
		if d.en[i] >= 0x80 {
			return false
		}
	}
	return true
}

// TestMsgText checks the English rendering of messages, nested messages,
// lists and problems.
func TestMsgText(t *testing.T) {
	cases := []struct {
		m    Msg
		want string
	}{
		{M("common.internal_error"), "internal error"},
		{M("rule.content.range", "min", 10, "max", 86400), "must be between 10 and 86400"},
		{M("common.invalid_body", "detail", "unexpected EOF"), "invalid request body: unexpected EOF"},
		{M("rule.content.path_not_absolute", "path", fmt.Sprintf("%q", "a\"b")), `"a\"b" is not an absolute path`},
		{M("builtin.auditd_unusable", "cause", M("auditd.not_running")), "auditd is not usable: auditd is not running"},
		{M("command.spec.weekday_invalid", "value", `"x"`, "choices", []string{"sun", "mon"}), `"x" is not one of sun, mon`},
		{M("command.spec.every_choices", "choices", []int{1, 2, 3}), "must be one of 1, 2, 3"},
		{M("rule.content.wrong_type", "field", M("json.kind.the_content"), "expected", M("json.kind.object"), "found", "array"),
			"invalid rule content: the content must be an object, not array"},
		{M("pattern.quote", "char", "Q"), `\Q is not supported (quoting with \Q...\E); escape each special character with a backslash instead`},
		{M("apply.validation_failed", "problems", []Problem{
			{Where: []Msg{M("loc.field", "name", "users")}, Msg: M("rule.content.too_many", "max", 100)},
			{Msg: M("apply.and_more", "count", 2)},
		}), "validation failed: users: at most 100 entries; and 2 more"},
		{M("common.lines", "lines", []Msg{Raw("a"), M("auditd.not_running")}), "a\nauditd is not running"},
		{M("rule.version.from_templates", "names", []string{"base", "nginx"}), "expanded from templates: base, nginx"},
		{Raw("exit status 1"), "exit status 1"},
		{Msg{Code: "no.such.code"}, "no.such.code"},
	}
	for _, c := range cases {
		if got := c.m.Text(); got != c.want {
			t.Errorf("%s: got %q, want %q", c.m.Code, got, c.want)
		}
		if got := c.m.Error(); got != c.want {
			t.Errorf("%s: Error() %q, want %q", c.m.Code, got, c.want)
		}
	}

	p := Problem{
		Where: []Msg{M("loc.command", "n", 2), M("loc.field", "name", "timeout_sec")},
		Msg:   M("command.spec.range_1", "max", 86400),
	}
	if got, want := p.Text(), "command 2: timeout_sec: must be between 1 and 86400"; got != want {
		t.Errorf("problem: got %q, want %q", got, want)
	}
	if p.Error() != p.Text() {
		t.Errorf("problem: Error() %q differs from Text()", p.Error())
	}
	ps := Problems{p, {Msg: M("command.spec.commands_required")}}
	if got, want := ps.Error(), "command 2: timeout_sec: must be between 1 and 86400; at least one command is required"; got != want {
		t.Errorf("problems: got %q, want %q", got, want)
	}
	if got := ProblemTexts(ps); !reflect.DeepEqual(got, []string{p.Text(), "at least one command is required"}) {
		t.Errorf("ProblemTexts: %q", got)
	}
}

// TestMsgJSON checks the shape on the wire and that a decoded message
// renders the same English as the original.
func TestMsgJSON(t *testing.T) {
	p := Problem{
		Where: []Msg{M("loc.command", "n", 2)},
		Msg:   M("apply.none_remove_failed", "cause", M("apply.immutable")),
	}
	b, err := json.Marshal(p)
	if err != nil {
		t.Fatal(err)
	}
	want := `{"where":[{"code":"loc.command","params":{"n":2}}],"code":"apply.none_remove_failed","params":{"cause":{"code":"apply.immutable"}}}`
	if string(b) != want {
		t.Errorf("json: got %s, want %s", b, want)
	}
	if b, _ := json.Marshal(Raw("x")); string(b) != `{"code":"","params":{"text":"x"}}` {
		t.Errorf("raw json: %s", b)
	}
	var round Problem
	b, _ = json.Marshal(p)
	if err := json.Unmarshal(b, &round); err != nil {
		t.Fatal(err)
	}
	if round.Text() != p.Text() {
		t.Errorf("decoded: got %q, want %q", round.Text(), p.Text())
	}
	list := M("apply.validation_failed", "problems", []Problem{p, {Msg: M("apply.and_more", "count", 1)}})
	b, _ = json.Marshal(list)
	var decoded Msg
	if err := json.Unmarshal(b, &decoded); err != nil {
		t.Fatal(err)
	}
	if decoded.Text() != list.Text() {
		t.Errorf("decoded list: got %q, want %q", decoded.Text(), list.Text())
	}
}

// TestAsMsg checks that the English of AsMsg is always the error's text.
func TestAsMsg(t *testing.T) {
	m := M("auditd.not_running")
	prob := Problem{Where: []Msg{M("loc.field", "name", "users")}, Msg: M("rule.content.too_many", "max", 3)}
	cases := []struct {
		err  error
		code string
	}{
		{m, "auditd.not_running"},
		{&m, "auditd.not_running"},
		{prob, "common.problems"},
		{Problems{prob, prob}, "common.problems"},
		{errors.Join(m, errors.New("exit status 1")), "common.lines"},
		{fmt.Errorf("wrapped: %w", m), ""},
		{errors.New("plain"), ""},
	}
	for _, c := range cases {
		got := AsMsg(c.err)
		if got.Code != c.code {
			t.Errorf("%v: code %q, want %q", c.err, got.Code, c.code)
		}
		if got.Text() != c.err.Error() {
			t.Errorf("%v: text %q, want %q", c.err, got.Text(), c.err.Error())
		}
	}
	if got := AsMsg(nil); got.Code != "" || got.Text() != "" {
		t.Errorf("nil: %+v", got)
	}
	if !IsMsg(m, "auditd.not_running") || IsMsg(fmt.Errorf("x: %w", m), "auditd.not_running") || IsMsg(errors.New("x"), "auditd.not_running") {
		t.Error("IsMsg")
	}
}

// TestMStrict checks that mistakes in a message fail under go test.
func TestMStrict(t *testing.T) {
	for name, f := range map[string]func(){
		"unknown code":      func() { _ = M("no.such.code") },
		"odd pairs":         func() { _ = M("rule.content.range", "min", 1, "max") },
		"name not a string": func() { _ = M("rule.content.range", 1, 2, "max", 3) },
		"missing parameter": func() { _ = M("rule.content.range", "min", 1) },
		"extra parameter":   func() { _ = M("common.internal_error", "x", 1) },
	} {
		func() {
			defer func() {
				if recover() == nil {
					t.Errorf("%s: no panic", name)
				}
			}()
			f()
		}()
	}
}

// TestMsgCatalogTemplates checks every template: placeholders are well
// formed and every code can be built with its placeholders.
func TestMsgCatalogTemplates(t *testing.T) {
	for code, def := range msgCatalog {
		if strings.Count(def.en, "{{") != strings.Count(def.en, "}}") {
			t.Errorf("%s: unbalanced braces in %q", code, def.en)
		}
		var kv []any
		for _, name := range placeholders(def.en) {
			kv = append(kv, name, "<"+name+">")
		}
		got := M(code, kv...).Text()
		if strings.Contains(got, "{{") {
			t.Errorf("%s: placeholder left in %q", code, got)
		}
	}
}
