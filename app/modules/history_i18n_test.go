package modules

import (
	"bufio"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"mimamori/app/models"
)

// The coded target and detail of a history row are stored only when they
// have a code, and are given back only while they render as the recorded
// English.
func TestHistoryTexts(t *testing.T) {
	if got := HistoryTextsJSON(Msg{}, Msg{}); got != "" {
		t.Fatalf("no texts: %q", got)
	}
	if got := HistoryTextsJSON(Raw("web-1"), Raw("some text")); got != "" {
		t.Fatalf("plain values are not stored: %q", got)
	}
	detail := M("history.detail.rule_applied", "version", 3, "agents", []string{"a", "b"})
	target := M("history.target.command_set", "agent", "web-1", "set", M("history.target.command_set_fallback", "set", 7))
	stored := HistoryTextsJSON(target, detail)
	if stored == "" {
		t.Fatal("coded texts are stored")
	}
	if detail.Text() != "version 3 to a, b" || target.Text() != "web-1 / set 7" {
		t.Fatalf("English: %q %q", detail.Text(), target.Text())
	}
	got := ParseHistoryTexts(stored, target.Text(), detail.Text())
	if got.Target == nil || got.Target.Code != "history.target.command_set" || got.Target.Text() != "web-1 / set 7" {
		t.Fatalf("target: %+v", got.Target)
	}
	if got.Detail == nil || got.Detail.Code != "history.detail.rule_applied" || got.Detail.Text() != "version 3 to a, b" {
		t.Fatalf("detail: %+v", got.Detail)
	}
	// Only the detail is coded: the target stays a value.
	only := HistoryTextsJSON(Raw("web-1"), detail)
	if got := ParseHistoryTexts(only, "web-1", detail.Text()); got.Target != nil || got.Detail == nil {
		t.Fatalf("detail only: %+v", got)
	}
	// A text that differs from the recorded English is not given back.
	if got := ParseHistoryTexts(stored, "other", detail.Text()); got.Target != nil || got.Detail == nil {
		t.Fatalf("target mismatch: %+v", got)
	}
	if got := ParseHistoryTexts(stored, target.Text(), "version 4 to a, b"); got.Detail != nil || got.Target == nil {
		t.Fatalf("detail mismatch: %+v", got)
	}
	// Rows without the column's value and damaged values give nothing.
	for _, s := range []string{"", "{", `{"detail":{"code":"","params":{"text":"x"}}}`, `[]`} {
		if got := ParseHistoryTexts(s, "", "x"); got.Target != nil || got.Detail != nil {
			t.Fatalf("%q: %+v", s, got)
		}
	}
	var shape map[string]any
	if err := json.Unmarshal([]byte(stored), &shape); err != nil || len(shape) != 2 || shape["target"] == nil || shape["detail"] == nil {
		t.Fatalf("stored shape: %s", stored)
	}
}

// The history entries of the commands on the central host keep their
// English detail and store its code next to it.
func TestCLIHistoryEntriesCoded(t *testing.T) {
	MigrationsFS = os.DirFS("../..")
	dir := t.TempDir()
	prevDir := dataDirOverride
	SetDataDir(dir)
	prevStdin := stdin
	t.Cleanup(func() { SetDataDir(prevDir); stdin = prevStdin })

	stdin = bufio.NewReader(strings.NewReader("Admin-pass-123\nOther-pass-456\n"))
	if err := (&CentralInitAdminCmd{Email: "root@example.com", DisplayName: "Root", Language: "auto"}).Run(); err != nil {
		t.Fatal(err)
	}
	if err := (&CentralResetPasswordCmd{Email: "root@example.com"}).Run(); err != nil {
		t.Fatal(err)
	}
	if err := (&CentralConfigSetCmd{Name: "server_names", Value: "b.example.com, a.example.com"}).Run(); err != nil {
		t.Fatal(err)
	}
	if err := (&CentralConfigUnsetCmd{Name: "server_names"}).Run(); err != nil {
		t.Fatal(err)
	}
	db, err := OpenSQLitePool(filepath.Join(dir, CentralDBFileName), SchemaCentral)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	items, _, err := models.ListHistory(db, "", "", nil, 10, 0)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string][2]string{
		"created":        {"history.detail.first_admin_cli", "first administrator created on the central host"},
		"password_reset": {"history.detail.admin_recovered_cli", "administrator recovered on the central host"},
		"set":            {"history.detail.config_value", "server_names = b.example.com,a.example.com"},
		"unset":          {"history.detail.config_restored", "default restored"},
	}
	if len(items) != len(want) {
		t.Fatalf("history rows: %d", len(items))
	}
	for _, h := range items {
		w, ok := want[h.Action]
		if !ok {
			t.Fatalf("unexpected row %+v", h)
		}
		if h.Detail != w[1] {
			t.Errorf("%s: detail %q, want %q", h.Action, h.Detail, w[1])
		}
		got := ParseHistoryTexts(h.I18n, h.Target, h.Detail)
		if got.Target != nil || got.Detail == nil || got.Detail.Code != w[0] {
			t.Errorf("%s: coded %+v (stored %q)", h.Action, got, h.I18n)
		}
	}
}
