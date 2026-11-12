package modules

import (
	"errors"
	"slices"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/alecthomas/kong"
)

// Whole numbers in ctl tables are printed as integers, never in the
// exponent form.
func TestCtlCellNumbers(t *testing.T) {
	cases := map[float64]string{1234567: "1234567", 2.5e6: "2500000", 999999: "999999", 0: "0", 1.5: "1.5", -3: "-3"}
	for in, want := range cases {
		if got := cell("id", in, false); got != want {
			t.Errorf("cell(%v) = %q, want %q", in, got, want)
		}
	}
	if got := cell("ids", []any{float64(1000000), float64(2)}, false); got != "1000000,2" {
		t.Errorf("list cell = %q", got)
	}
}

// Only time fields (and the history detail) are converted to local time;
// other text is printed as typed.
func TestCtlLocalizesOnlyTimeFields(t *testing.T) {
	saved := time.Local
	time.Local = time.FixedZone("JST", 9*3600)
	defer func() { time.Local = saved }()

	path := "/var/backups/db-2026-11-11T03:00:00Z.sql.gz"
	if got := cell("target", path, true); got != path {
		t.Errorf("target changed: %q", got)
	}
	if got := cell("last_at", "2026-11-11T03:00:00Z", true); got != "2026-11-11 12:00:00" {
		t.Errorf("last_at = %q", got)
	}
	if got := cell("token.expires_at", "2026-11-11T03:00:00Z", true); got != "2026-11-11 12:00:00" {
		t.Errorf("token.expires_at = %q", got)
	}
	if got := cell("detail", "expires 2026-11-11T03:00:00Z", true); got != "expires 2026-11-11 12:00:00" {
		t.Errorf("detail = %q", got)
	}
	v := localizeValue(map[string]any{
		"comment":    "window 2026-11-11T03:00:00+09:00",
		"created_at": "2026-11-11T03:00:00Z",
		"items":      []any{map[string]any{"at": "2026-11-11T03:00:00Z", "target": path}},
		"cert":       map[string]any{"not_after": "2026-11-11T03:00:00Z"},
	}).(map[string]any)
	if v["comment"] != "window 2026-11-11T03:00:00+09:00" {
		t.Errorf("comment changed: %v", v["comment"])
	}
	if v["created_at"] != "2026-11-11 12:00:00" {
		t.Errorf("created_at = %v", v["created_at"])
	}
	item := v["items"].([]any)[0].(map[string]any)
	if item["at"] != "2026-11-11 12:00:00" || item["target"] != path {
		t.Errorf("item = %v", item)
	}
	if v["cert"].(map[string]any)["not_after"] != "2026-11-11 12:00:00" {
		t.Errorf("not_after = %v", v["cert"])
	}
}

// Shortened cells end on a character boundary; JSON output keeps & < >.
func TestCtlOutputKeepsText(t *testing.T) {
	s := "ab" + strings.Repeat("\u3042", 70)
	got := cell("description", s, false)
	if !utf8.ValidString(got) || utf8.RuneCountInString(got) != 60 || !strings.HasSuffix(got, "...") {
		t.Errorf("shortened = %q", got)
	}
	short := strings.Repeat("\u3042", 60)
	if got := cell("description", short, false); got != short {
		t.Errorf("60 characters were shortened: %q", got)
	}
	if got := marshalIndent(map[string]string{"name": "R&D <web> servers"}); got != "{\n  \"name\": \"R&D <web> servers\"\n}" {
		t.Errorf("marshalIndent = %q", got)
	}
}

// --from / --to are read in the local time zone and sent as UTC RFC3339.
func TestCtlParseTime(t *testing.T) {
	saved := time.Local
	time.Local = time.FixedZone("JST", 9*3600)
	defer func() { time.Local = saved }()
	ok := map[string]string{
		"2026-11-12 13:30:05":       "2026-11-12T04:30:05Z",
		"2026-11-12 13:30":          "2026-11-12T04:30:00Z",
		"2026-11-12":                "2026-11-11T15:00:00Z",
		"2026-11-12T13:30:05":       "2026-11-12T04:30:05Z",
		"2026-11-12T13:30":          "2026-11-12T04:30:00Z",
		"2026-11-12T13:30:00+09:00": "2026-11-12T04:30:00Z",
		"2026-11-12T04:30:00Z":      "2026-11-12T04:30:00Z",
	}
	for in, want := range ok {
		got, err := parseCtlTime("from", in)
		if err != nil || got != want {
			t.Errorf("parseCtlTime(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
	for _, in := range []string{"garbage", "2026/11/12", "13:30"} {
		_, err := parseCtlTime("from", in)
		var ee *ExitError
		if err == nil || !errors.As(err, &ee) || ee.Code != ExitArgument {
			t.Errorf("parseCtlTime(%q) error = %v, want an argument error", in, err)
		}
	}
	if got, err := parseCtlTime("to", ""); got != "" || err != nil {
		t.Errorf("empty value = %q, %v", got, err)
	}
}

// AGENT_ID:ID must be two integers.
func TestCtlParseDetectionRef(t *testing.T) {
	if a, id, ok := parseDetectionRef("2:5"); !ok || a != 2 || id != 5 {
		t.Errorf("2:5 = %d %d %v", a, id, ok)
	}
	for _, s := range []string{"2:5x", "2:5:7", "2", ":5", "2:", "a:5", "2: 5", "+2:5", "2:-5", "0:5", "2:0", "2:5 ", " 2:5", "2:0x5", "2:99999999999999999999"} {
		if _, _, ok := parseDetectionRef(s); ok {
			t.Errorf("%q accepted", s)
		}
	}
}

// The agent init command shown after issuing or reissuing a token.
func TestCtlAgentInitCommand(t *testing.T) {
	base := "./" + AppName + " agent init --central-host <central host>"
	if got := agentInitCommand(DefaultAgentPort, false); got != base {
		t.Errorf("default port: %q", got)
	}
	if got := agentInitCommand(19311, false); got != base+" --central-port 19311" {
		t.Errorf("other port: %q", got)
	}
	if got := agentInitCommand(19311, true); got != base+" --central-port 19311 --overwrite" {
		t.Errorf("reissue: %q", got)
	}
	if strings.Contains(agentInitCommand(19311, true), "sudo") {
		t.Error("sudo in the command")
	}
}

// --choice keeps a field name that contains commas.
func TestCtlChoiceWithComma(t *testing.T) {
	var cli struct {
		Ctl CtlCmd `cmd:""`
	}
	parser, err := kong.New(&cli)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := parser.Parse([]string{"ctl", "rules", "create", "r", "--template", "a", "--choice", "exec_files.legit:x|.php,.phtml=laravel", "--choice", "writable.base=b"}); err != nil {
		t.Fatal(err)
	}
	got := cli.Ctl.Rules.Create.Choice
	if len(got) != 2 || got[0] != "exec_files.legit:x|.php,.phtml=laravel" || got[1] != "writable.base=b" {
		t.Errorf("choices = %q", got)
	}
}

// Rule content must be a JSON object and errors do not name Go types.
func TestParseRuleContentErrors(t *testing.T) {
	for _, in := range []string{"null", "[1,2]", "\"x\"", "1", " null "} {
		if _, err := ParseRuleContent([]byte(in)); err == nil {
			t.Errorf("%s accepted", in)
		}
	}
	for _, in := range []string{"[1,2]", `{"users":"alice"}`, `{"read":{"dirs":[1]}}`, `{"check_interval_sec":"x"}`} {
		_, err := ParseRuleContent([]byte(in))
		if err == nil {
			t.Errorf("%s accepted", in)
			continue
		}
		if strings.Contains(err.Error(), "Go ") || strings.Contains(err.Error(), "modules.") {
			t.Errorf("%s: %v", in, err)
		}
	}
	_, err := ParseRuleContent([]byte(`{"users":"alice"}`))
	if err == nil || !strings.Contains(err.Error(), "users must be an array") {
		t.Errorf("users error = %v", err)
	}
	if _, err := ParseRuleContent([]byte(`{}`)); err != nil {
		t.Errorf("empty object: %v", err)
	}
}

// Byte limits never cut inside a UTF-8 sequence.
func TestTruncateKeepsUTF8(t *testing.T) {
	s := "a" + strings.Repeat("\u3042", 10)
	for n := 0; n <= len(s); n++ {
		got := truncate(s, n)
		if !utf8.ValidString(got) || len(got) > n {
			t.Errorf("truncate(%d) = %q", n, got)
		}
	}
}

// ctl connects to the address of agent_listen: the loopback address for a
// wildcard, the listen IP itself otherwise, with the port of
// agent_listen.
func TestCtlTarget(t *testing.T) {
	cases := []struct{ in, host string }{
		{":9081", "127.0.0.1"},
		{"0.0.0.0:9081", "127.0.0.1"},
		{"[::]:9081", "::1"},
		{"[::ffff:0.0.0.0]:9081", "127.0.0.1"},
		{"127.0.0.1:9081", "127.0.0.1"},
		{"127.0.0.2:9081", "127.0.0.2"},
		{"[::1]:9081", "::1"},
		{"192.0.2.7:9081", "192.0.2.7"},
		{"[::ffff:192.0.2.7]:9081", "192.0.2.7"},
		{"[2001:db8::5]:9081", "2001:db8::5"},
	}
	for _, c := range cases {
		host, port, err := CtlTarget(c.in)
		if err != nil || host != c.host || port != 9081 {
			t.Errorf("CtlTarget(%q) = %q, %d, %v; want %q, 9081", c.in, host, port, err, c.host)
		}
	}
	for _, bad := range []string{"9081", "central.example:9081", "0.0.0.0:x", "0.0.0.0:0", "0.0.0.0:70000"} {
		if _, _, err := CtlTarget(bad); err == nil {
			t.Errorf("CtlTarget(%q) accepted", bad)
		}
	}
	// The name the certificate is verified under is one every server
	// certificate of the central contains.
	if !slices.Contains(ServerCertNames(nil), ctlServerName) {
		t.Errorf("%q is not always in the server certificate", ctlServerName)
	}
}
