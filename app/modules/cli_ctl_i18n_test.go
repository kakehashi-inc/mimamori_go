package modules

import (
	"io"
	"os"
	"strings"
	"testing"
)

// showOutput returns what show prints for v.
func showOutput(t *testing.T, cl *ctlClient, v any, columns ...string) string {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	saved := os.Stdout
	os.Stdout = w
	cl.show(v, columns...)
	os.Stdout = saved
	w.Close()
	out, err := io.ReadAll(r)
	r.Close()
	if err != nil {
		t.Fatal(err)
	}
	return string(out)
}

// The human output of ctl never shows the coded texts for the screens
// (keys ending in "_i18n", at any depth); --json prints them as received.
func TestCtlHumanOutputLeavesOutI18n(t *testing.T) {
	msg := map[string]any{"code": "apply.applied", "params": map[string]any{"rule": "web"}}
	agent := map[string]any{
		"id":        float64(3),
		"apply":     map[string]any{"status": "applied", "message": "applied", "message_i18n": msg, "warnings": []any{"w"}, "warnings_i18n": []any{msg}},
		"host_info": map[string]any{"last_apply": map[string]any{"message": "applied", "message_i18n": msg}},
		"token_history": []any{
			map[string]any{"category": "token", "action": "issued", "detail": "expires x", "detail_i18n": msg, "target_i18n": msg},
		},
		"error_i18n": msg,
	}
	human := showOutput(t, &ctlClient{}, agent)
	if strings.Contains(human, "_i18n") || strings.Contains(human, "apply.applied") {
		t.Errorf("human output shows coded texts:\n%s", human)
	}
	for _, want := range []string{`"message": "applied"`, `"warnings": [`, `"detail": "expires x"`} {
		if !strings.Contains(human, want) {
			t.Errorf("human output lacks %s:\n%s", want, human)
		}
	}
	if agent["apply"].(map[string]any)["message_i18n"] == nil {
		t.Error("the answer itself was changed")
	}

	plain := showOutput(t, &ctlClient{}, map[string]any{"name": "x", "description_i18n": msg}, "name")
	if strings.Contains(plain, "_i18n") || strings.Contains(plain, "apply.applied") {
		t.Errorf("output with columns shows coded texts:\n%s", plain)
	}

	js := showOutput(t, &ctlClient{json: true}, agent)
	for _, want := range []string{`"message_i18n"`, `"warnings_i18n"`, `"detail_i18n"`, `"target_i18n"`, `"error_i18n"`} {
		if !strings.Contains(js, want) {
			t.Errorf("--json output lacks %s:\n%s", want, js)
		}
	}
}
