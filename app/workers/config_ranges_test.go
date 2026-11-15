package workers

import (
	"encoding/json"
	"strconv"
	"testing"
)

// configProblem is one message of a refused PUT /config (error_i18n, or each
// of errors_i18n).
type configProblem struct {
	Code   string         `json:"code"`
	Params map[string]any `json:"params"`
}

// configRefusal returns the messages of a refused PUT /config.
func configRefusal(t *testing.T, body string) []configProblem {
	t.Helper()
	var out struct {
		Error  configProblem   `json:"error_i18n"`
		Errors []configProblem `json:"errors_i18n"`
	}
	if err := json.Unmarshal([]byte(body), &out); err != nil {
		t.Fatalf("refusal %q: %v", body, err)
	}
	if len(out.Errors) > 0 {
		return out.Errors
	}
	return []configProblem{out.Error}
}

// configItems returns the items of a GET /config answer by name.
func configItems(t *testing.T, out map[string]any) map[string]map[string]any {
	t.Helper()
	items := map[string]map[string]any{}
	for _, it := range out["items"].([]any) {
		item := it.(map[string]any)
		items[item["name"].(string)] = item
	}
	return items
}

// GET /config gives every number item its range (min and max, numbers) from
// the definitions the validation uses: the default lies in it, PUT /config
// takes both ends and refuses the number below and the number above, naming
// the item. Items of other kinds have no range.
func TestConfigItemRanges(t *testing.T) {
	e := newTestEnv(t)
	items := configItems(t, e.admin("GET", "/config", nil))
	ints := 0
	for name, item := range items {
		if item["kind"] != "int" {
			if _, ok := item["min"]; ok {
				t.Errorf("%s (%v) has min %v", name, item["kind"], item["min"])
			}
			if _, ok := item["max"]; ok {
				t.Errorf("%s (%v) has max %v", name, item["kind"], item["max"])
			}
			continue
		}
		ints++
		lo, okLo := item["min"].(float64)
		hi, okHi := item["max"].(float64)
		if !okLo || !okHi {
			t.Errorf("%s: min %v, max %v, want numbers", name, item["min"], item["max"])
			continue
		}
		def, err := strconv.Atoi(item["default"].(string))
		if err != nil || lo < 1 || float64(def) < lo || float64(def) > hi {
			t.Errorf("%s: min %v default %q max %v", name, lo, item["default"], hi)
			continue
		}
		// The token validity must stay above the renewal: the partner gets a
		// value that leaves the range of the item itself to decide.
		partner := map[string]map[string]string{
			"token_valid_days":        {"token_renew_before_days": "1"},
			"token_renew_before_days": {"token_valid_days": strconv.Itoa(int(hi))},
		}[name]
		put := func(v int) (int, string) {
			set := map[string]string{name: strconv.Itoa(v)}
			for k, pv := range partner {
				set[k] = pv
			}
			return e.adminRaw("PUT", "/config", map[string]any{"set": set})
		}
		for _, v := range []int{int(lo) - 1, int(hi) + 1} {
			code, body := put(v)
			if code != 400 {
				t.Errorf("%s = %d: status %d %s", name, v, code, body)
				continue
			}
			named := false
			for _, p := range configRefusal(t, body) {
				switch p.Code {
				case "config.int_min", "config.int_max", "config.not_positive_int":
					named = named || p.Params["name"] == name
				}
			}
			if !named {
				t.Errorf("%s = %d: the refusal does not name the item's range: %s", name, v, body)
			}
		}
		for _, v := range []int{int(lo), int(hi)} {
			code, body := put(v)
			switch {
			case code == 200:
			case name == "token_valid_days" && v == int(lo), name == "token_renew_before_days" && v == int(hi):
				// The renewal must be shorter than the validity, so these
				// ends are refused by that rule alone.
				for _, p := range configRefusal(t, body) {
					if p.Code != "config.renew_before" {
						t.Errorf("%s = %d: refused with %s %v, want only config.renew_before", name, v, p.Code, p.Params)
					}
				}
			default:
				t.Errorf("%s = %d: status %d %s", name, v, code, body)
			}
		}
		e.admin("PUT", "/config", map[string]any{"unset": []string{"token_valid_days", "token_renew_before_days", name}})
	}
	if ints != 11 {
		t.Errorf("%d number items, want 11", ints)
	}
	// The browser API gives the same ranges.
	cookie := login(t, e, testAdminEmail, "Admin-pass-123")
	rec := e.do(e.web, "GET", "/api/v1/config", nil, map[string]string{"Cookie": sessionCookie + "=" + cookie}, "192.0.2.1:1000")
	var browser map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &browser); err != nil || rec.Code != 200 {
		t.Fatalf("browser GET /config: %d %s", rec.Code, rec.Body.String())
	}
	check := configItems(t, browser)["default_check_interval_sec"]
	if check["min"] != float64(10) || check["max"] != float64(3600) {
		t.Errorf("browser default_check_interval_sec: min %v max %v, want 10 and 3600", check["min"], check["max"])
	}
}

// The default of smtp.port that GET /config gives (what "reset to default"
// stores) is the port of the stored smtp.security.
func TestConfigSMTPPortDefaultFollowsSecurity(t *testing.T) {
	e := newTestEnv(t)
	for _, c := range []struct {
		set  map[string]string
		want string
	}{
		{map[string]string{"smtp.security": "tls"}, "465"},
		{map[string]string{"smtp.security": "none", "smtp.port": "2525"}, "25"},
		{map[string]string{"smtp.security": "starttls"}, "587"},
	} {
		items := configItems(t, e.admin("PUT", "/config", map[string]any{"set": c.set}))
		port := items["smtp.port"]
		if port["default"] != c.want {
			t.Errorf("%v: smtp.port default %v, want %s", c.set, port["default"], c.want)
		}
		if items["smtp.security"]["default"] != "starttls" {
			t.Errorf("%v: smtp.security default %v, want starttls", c.set, items["smtp.security"]["default"])
		}
	}
	items := configItems(t, e.admin("PUT", "/config", map[string]any{
		"set": map[string]string{"smtp.security": "tls"}, "unset": []string{"smtp.port"}}))
	if p := items["smtp.port"]; p["value"] != "465" || p["default"] != "465" || p["stored"] != false {
		t.Errorf("smtp.port after reset with tls: %v", p)
	}
}

// GET /config gives, with the item smtp.port, its default for every value of
// smtp.security, so that the configuration screen shows the port that will
// be saved for the security chosen in the form; the other items have none.
func TestConfigSMTPPortDefaultsBySecurity(t *testing.T) {
	e := newTestEnv(t)
	items := configItems(t, e.admin("PUT", "/config", map[string]any{"set": map[string]string{"smtp.security": "tls"}}))
	got, ok := items["smtp.port"]["security_defaults"].(map[string]any)
	want := map[string]any{"starttls": "587", "tls": "465", "none": "25"}
	if !ok || len(got) != len(want) {
		t.Fatalf("smtp.port security_defaults: %v", items["smtp.port"]["security_defaults"])
	}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("security_defaults[%s] = %v, want %v", k, got[k], v)
		}
	}
	for name, it := range items {
		if _, has := it["security_defaults"]; has && name != "smtp.port" {
			t.Errorf("%s has security_defaults", name)
		}
	}
}
