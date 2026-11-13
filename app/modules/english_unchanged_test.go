package modules

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

// The inputs and expected texts of TestEnglishUnchanged. The expected texts
// were produced by running the validators of the version before they were
// given coded messages (commit c7fa753) on these inputs: the English of the
// coded messages must be exactly the same.

// englishLong is the long value of the inputs below; in the expected texts
// it is written as englishLongMark.
var englishLong = strings.Repeat("a", 4100)

const englishLongMark = "<LONG>"

var englishEmailInputs = []string{
	"", "  ", strings.Repeat("a", 250) + "@b.cd", "a b@example.com", "a<b@example.com",
	"a,b@example.com", "\xff@example.com", "a\x01b@example.com", "not-an-address", "a@", "@b", "a@@b",
}

var englishDisplayNameInputs = []string{
	"", " \t", strings.Repeat("\u3042", 65), "a\x01b", "\xff",
}

var englishPasswordInputs = []string{"", "short", strings.Repeat("a", 257)}

var englishAbsPathInputs = []string{
	"rel", "", "/a/../b", "/a/./b", "/a//b", "/a/", "/a b", "/a\"b", "/a'b", "/a\tb", "/a\u3000b",
	"/" + englishLong,
}

var englishRelPathInputs = []string{"", "a//b", "../a", "a/./b", "/a", "a/", "a\x00b"}

var englishRuleJSONInputs = []string{
	"[]", "null", "", "1", `{"users": "x"}`, `{"users": [1]}`, `{"users": [{"value": true}]}`,
	`{"check_interval_sec": "x"}`, `{"check_interval_sec": 1.5}`, `{"exclude_interactive": 1}`,
	`{"read": []}`, `{"read": {"dirs": {}}}`, `{"unknown": 1}`, `{`, `{"users": [}`,
	`{"exec_files": {"legit": [{"extensions": "x"}]}}`,
}

var englishCommandJSONInputs = []string{
	"[]", "null", "", `{} {}`, `{"commands": {}}`, `{"keep_runs": "x"}`, `{"schedule": []}`,
	`{"commands": [{"success": 1}]}`, `{"commands": [{"success": {"a": "x"}}]}`, `{"unknown": 1}`,
	`{`, `{"schedule": {"minute": "x"}}`, `{"name": 1}`,
}

var englishPatternInputs = []string{
	"", `(?P<a>(x)`, `(?P<dbl>(\w)\1)`, `(?P<a>a)\2`, `(?P<a>a)\12`, `(?P<a>a)\189`, `(?P<a>a)\k<2>`,
	`\1(?P<a>a)`, `(?P<a>a)(?(2)b|c)`, `(?P<a>a)(?(0)b|c)`, `(?P<a>a(?P=a))`, `(?P=a)(?P<a>a)`,
	`(?P<a>a)(?P=b)`, `(?P<a>a)(b)|(?P<a>c)\2`, `(?n)(?P<a>a)(b)`, `(?n:(?P<a>a)(b))`, `(?<1>a)(?P<b>b)`,
	`(?<a>a)(?<b-a>b)`, `(?<-a>b)(?P<c>c)`, `(?)(?P<a>a)`, `(?#\)(x)(?P<a>a)`, "(?x)(?P<a>a) #c\\\n(b)\\2",
	`(?P<a>\Qa.b\E)`, `(?P<a>x\Q)`, `(?P<a>[x\E])`, `(\d+)%`, `abc`, `(?P<a>[)`, `(?P<a>a{2,1})`,
}

var englishScheduleAtInputs = []string{"", "x", "2026-13-01 03:00", "2026-12-01 03:00", "2026-12-01T03:00:00"}

var englishOSUserInputs = []string{"", strings.Repeat("u", 257), "a b", "a\x01", "-a"}

var englishWorkDirInputs = []string{"rel", "/a\x00", "/a\nb", "/a/", "/a//b", "/a/../b", "/" + englishLong}

var englishFreeTextInputs = []string{strings.Repeat("t", 1001), "a\x01b", "a\x7fb"}

// englishNow is the time of the command set checks of the tables.
const englishNow = 1800000000

func englishRuleContents() []*RuleContent {
	many := func(n int, v string) []Element {
		out := make([]Element, n)
		for i := range out {
			out[i] = Element{Value: v}
		}
		return out
	}
	manyStrings := func(n int, v string) []string {
		out := make([]string, n)
		for i := range out {
			out[i] = v
		}
		return out
	}
	legit := make([]LegitLocation, MaxRuleElements+1)
	for i := range legit {
		legit[i] = LegitLocation{Path: "a", Extensions: []string{".a"}, Category: CategoryAlert}
	}
	return []*RuleContent{
		{},
		{
			Users:            []Element{{Value: "Bad User"}, {Value: "ok"}, {Value: ""}},
			CheckIntervalSec: 1, ScanIntervalSec: 1,
			Exec: CategoryOnly{Category: "x"}, Denied: CategoryOnly{Category: "y"},
			Read:     ReadSpec{Dirs: []Element{{Value: "rel"}, {Value: "/a/../b"}, {Value: "/a//b"}, {Value: "/a b"}, {Value: "/" + englishLong}}, Category: "z"},
			Network:  NetworkSpec{Executables: []Element{{Value: "x"}, {Value: "/ok"}}, Category: "w"},
			Writable: WritableSpec{Base: "/", AllowedPaths: []string{"", "../a", "ok/x"}, Category: "v"},
			ExecFiles: ExecFilesSpec{
				Extensions: []string{"exe", ".toolongextension16", ".ok"},
				Legit: []LegitLocation{
					{Path: "../a"},
					{Path: "ok", Extensions: []string{"bad", ".ok"}, Category: CategoryAlert},
					{Path: "", Extensions: []string{".a"}, Category: CategoryNormal},
				},
				OtherCategory: "u",
			},
		},
		{
			Users:            many(MaxRuleElements+1, "u"),
			CheckIntervalSec: MaxCheckInterval + 1, ScanIntervalSec: MaxScanInterval + 1,
			Read:      ReadSpec{Dirs: many(MaxRuleElements+1, "/a")},
			Network:   NetworkSpec{Executables: many(MaxRuleElements+1, "/a")},
			Writable:  WritableSpec{Base: "rel", AllowedPaths: manyStrings(MaxRuleElements+1, "a")},
			ExecFiles: ExecFilesSpec{Extensions: manyStrings(MaxExtensions+1, ".a"), Legit: legit},
		},
		{Writable: WritableSpec{Base: "/a/"}, ExecFiles: ExecFilesSpec{Legit: []LegitLocation{{Path: "a/./b", Extensions: []string{".a"}, Category: "x"}}}},
	}
}

func englishCommandSets() []*CommandSetSpec {
	minute := func(n int) *int { return &n }
	ok := func() CommandSpec {
		return CommandSpec{CommandLine: "true", Success: DefaultSuccessCodes, Failure: DefaultFailureCodes,
			OnProblems: OnProblemsStop, TimeoutSec: 10, LineLimit: 10}
	}
	set := func(mod func(s *CommandSetSpec)) *CommandSetSpec {
		s := &CommandSetSpec{Name: "n", Purpose: PurposeCollectOutput, OSUser: "u", RunMode: RunModeManual,
			KeepRuns: 1, Joiner: JoinerComma, Commands: []CommandSpec{ok()}}
		mod(s)
		return s
	}
	bad1 := ok()
	bad1.CommandLine, bad1.Note = " ", "a\x01"
	bad1.Success, bad1.Failure = CodeSpec{Kind: "x"}, CodeSpec{Kind: CodeRange, A: -1, B: 300}
	bad1.OnProblems, bad1.TimeoutSec, bad1.LineLimit = "x", 0, 0
	bad1.Patterns = []string{"(?P<a>a)"}
	bad2 := ok()
	bad2.CommandLine, bad2.Note = "a\x00", strings.Repeat("t", 1001)
	bad2.Success, bad2.Failure = CodeSpec{Kind: CodeRange, A: 5, B: 3}, CodeSpec{Kind: CodeSingle, A: 1, B: 2}
	bad2.TimeoutSec, bad2.LineLimit = MaxCommandSetTimeoutSec+1, MaxCommandLineLimit+1
	bad2.Patterns = englishPatternInputs
	bad3 := ok()
	bad3.CommandLine = strings.Repeat("c", MaxCommandLineBytes+1)
	bad3.Success, bad3.Failure = CodeSpec{Kind: CodeSingle, A: 0}, CodeSpec{Kind: CodeRange, A: 0, B: 3}
	bad4 := ok()
	bad4.Success, bad4.Failure = CodeSpec{Kind: CodeMin, A: 256}, CodeSpec{Kind: CodeMax, A: 1, B: 1}
	bad5 := ok()
	bad5.Success, bad5.Failure = CodeSpec{Kind: CodeMin, A: 1}, CodeSpec{Kind: CodeMax, A: 125}
	return []*CommandSetSpec{
		set(func(s *CommandSetSpec) {}),
		set(func(s *CommandSetSpec) {
			s.Name, s.Description, s.Purpose, s.OSUser, s.WorkDir = "", "a\x01", "x", "", "rel"
			s.RunMode, s.KeepRuns, s.Joiner, s.Commands = "x", 0, "x", nil
		}),
		set(func(s *CommandSetSpec) {
			s.Name, s.Description, s.Purpose = strings.Repeat("n", MaxCommandSetNameLength+1)+"\x01", strings.Repeat("t", 1001), PurposeRunOnly
			s.OSUser, s.WorkDir = "a b", "/a\nb"
			s.Schedule = &CommandSchedule{Kind: ScheduleDaily, Time: "01:00"}
			s.KeepRuns = MaxCommandKeepRuns + 1
			s.Commands = []CommandSpec{bad1, bad2, bad3, bad4, bad5}
		}),
		set(func(s *CommandSetSpec) { s.OSUser, s.WorkDir, s.RunMode = "-a", "/a/", RunModeSchedule }),
		set(func(s *CommandSetSpec) {
			s.OSUser, s.WorkDir = strings.Repeat("u", MaxCommandOSUserLength+1), "/"+englishLong
			s.RunMode, s.Schedule = RunModeSchedule, &CommandSchedule{Kind: ScheduleOnce}
		}),
		set(func(s *CommandSetSpec) {
			s.RunMode, s.Schedule = RunModeSchedule, &CommandSchedule{Kind: ScheduleOnce, At: "tomorrow"}
		}),
		set(func(s *CommandSetSpec) {
			s.RunMode, s.Schedule = RunModeSchedule, &CommandSchedule{Kind: ScheduleOnce, At: "2020-01-01T00:00:00Z"}
		}),
		set(func(s *CommandSetSpec) {
			s.RunMode, s.Schedule = RunModeSchedule, &CommandSchedule{Kind: ScheduleWeekly, Time: "24:00"}
		}),
		set(func(s *CommandSetSpec) {
			s.RunMode, s.Schedule = RunModeSchedule, &CommandSchedule{Kind: ScheduleWeekly, Weekdays: []string{"mon", "xyz", "Sun"}, Time: "1:00"}
		}),
		set(func(s *CommandSetSpec) {
			s.RunMode, s.Schedule = RunModeSchedule, &CommandSchedule{Kind: ScheduleDaily, Time: "12:60"}
		}),
		set(func(s *CommandSetSpec) {
			s.RunMode, s.Schedule = RunModeSchedule, &CommandSchedule{Kind: ScheduleEveryHours, Every: 5}
		}),
		set(func(s *CommandSetSpec) {
			s.RunMode, s.Schedule = RunModeSchedule, &CommandSchedule{Kind: ScheduleEveryMinutes, Every: 7}
		}),
		set(func(s *CommandSetSpec) {
			s.RunMode, s.Schedule = RunModeSchedule, &CommandSchedule{Kind: ScheduleHourlyAt}
		}),
		set(func(s *CommandSetSpec) {
			s.RunMode, s.Schedule = RunModeSchedule, &CommandSchedule{Kind: ScheduleHourlyAt, Minute: minute(60)}
		}),
		set(func(s *CommandSetSpec) {
			s.RunMode, s.Schedule = RunModeSchedule, &CommandSchedule{Kind: "x"}
		}),
	}
}

// englishConfigSets are configurations (item, value pairs set on the
// defaults) for the configuration check.
var englishConfigSets = [][][2]string{
	{},
	{{"web_listen", "x"}, {"agent_listen", "host:0"}, {"server_names", "a b, ok.example.com, x_y"},
		{"smtp.security", "x"}, {"smtp.host", "a b"}, {"smtp.from", "not mail"}, {"workers", "1000"},
		{"smtp.helo_name", "a\rb"}, {"smtp.username", "a\x00b"}, {"smtp.password", "p\x01"},
		{"default_check_interval_sec", "1"}, {"token_valid_days", "5"}, {"token_renew_before_days", "5"}},
	{{"web_listen", "127.0.0.1:1"}, {"agent_listen", "127.0.0.1:1"}, {"retention_days", "200000"}},
	{{"web_listen", "127.0.0.1:65536"}, {"agent_listen", "[::1]:abc"}, {"notify.subject_prefix", "a\nb"}},
}

// englishConfigItemInputs are item, value pairs for the check of one value.
var englishConfigItemInputs = [][2]string{
	{"nope", ""}, {"workers", "x"}, {"workers", "0"}, {"workers", "-1"}, {"builtin_agent", "maybe"}, {"smtp.auth", ""},
}

// englishConfigChanges are changes for ApplyCentralConfigChanges on a new
// database: the items set and the items unset.
var englishConfigChanges = []struct {
	set   map[string]string
	unset []string
}{
	{set: map[string]string{"nope": "1"}},
	{unset: []string{"nope"}},
	{set: map[string]string{"web_listen": "127.0.0.1:9"}, unset: []string{"web_listen"}},
	{set: map[string]string{"web_listen": " "}},
	{set: map[string]string{"workers": "x"}},
	{set: map[string]string{"workers": "65"}},
	{set: map[string]string{"smtp.security": "x", "web_listen": "a"}},
	{set: map[string]string{"server_names": "a b,c d"}},
}

// englishWant holds the English texts of commit c7fa753 by case (see TestEnglishUnchanged).
var englishWant = map[string][]string{
	"abs_path/0":          {"\"rel\" is not an absolute path"},
	"abs_path/1":          {"\"\" is not an absolute path"},
	"abs_path/10":         {"\"/a\\u3000b\" contains a forbidden character"},
	"abs_path/11":         {"\"/<LONG>\" is too long"},
	"abs_path/2":          {"\"/a/../b\" must not contain dot segments"},
	"abs_path/3":          {"\"/a/./b\" must not contain dot segments"},
	"abs_path/4":          {"\"/a//b\" is not a clean path"},
	"abs_path/5":          {"\"/a/\" is not a clean path"},
	"abs_path/6":          {"\"/a b\" contains a forbidden character"},
	"abs_path/7":          {"\"/a\\\"b\" contains a forbidden character"},
	"abs_path/8":          {"\"/a'b\" contains a forbidden character"},
	"abs_path/9":          {"\"/a\\tb\" contains a forbidden character"},
	"command_json/0":      {"invalid command set: the definition must be a JSON object"},
	"command_json/1":      {"invalid command set: the definition must be a JSON object"},
	"command_json/10":     {"invalid command set: unexpected EOF"},
	"command_json/11":     {"invalid command set: schedule.minute must be an integer, not string"},
	"command_json/12":     {"invalid command set: name must be a string, not number"},
	"command_json/2":      {"invalid command set: the definition must be a JSON object"},
	"command_json/3":      {"invalid command set: only one JSON object is allowed"},
	"command_json/4":      {"invalid command set: commands must be an array, not object"},
	"command_json/5":      {"invalid command set: keep_runs must be an integer, not string"},
	"command_json/6":      {"invalid command set: schedule must be an object, not array"},
	"command_json/7":      {"invalid command set: commands.0.success must be an object, not number"},
	"command_json/8":      {"invalid command set: commands.0.success.a must be an integer, not string"},
	"command_json/9":      {"invalid command set: unknown field \"unknown\""},
	"command_set/0":       {},
	"command_set/1":       {"name: is required", "description: must not contain control characters other than line breaks and tabs", "purpose: must be collect_output or run_only", "os_user: is required", "work_dir: must be an absolute path", "run_mode: must be manual or schedule", "keep_runs: must be between 1 and 10000", "joiner: must be comma or newline", "commands: at least one command is required"},
	"command_set/10":      {"schedule.every: must be one of 1, 2, 3, 4, 6, 8, 12"},
	"command_set/11":      {"schedule.every: must be one of 1, 2, 3, 4, 5, 6, 10, 12, 15, 20, 30"},
	"command_set/12":      {"schedule.minute: a minute from 0 to 59 is required"},
	"command_set/13":      {"schedule.minute: must be between 0 and 59"},
	"command_set/14":      {"schedule.kind: must be once, weekly, daily, every_hours, every_minutes or hourly_at"},
	"command_set/2":       {"name: must be at most 100 characters", "name: must not contain control characters", "description: must be at most 1000 characters", "os_user: must not contain white space or control characters", "work_dir: must not contain NUL or line breaks", "schedule: must be null when run_mode is manual", "keep_runs: must be between 1 and 10000", "command 1: command_line: is required", "command 1: note: must not contain control characters other than line breaks and tabs", "command 1: success.kind: must be single, range, min or max", "command 1: failure.a: must be between 0 and 255", "command 1: failure.b: must be between 0 and 255", "command 1: on_problems: must be stop, continue_on_failure or continue_always", "command 1: timeout_sec: must be between 1 and 86400", "command 1: line_limit: must be between 1 and 10000", "command 1: patterns: are used only when purpose is collect_output", "command 2: command_line: must not contain a NUL byte", "command 2: note: must be at most 1000 characters", "command 2: success.b: must be greater than a", "command 2: failure.b: is used only by the kind range", "command 2: timeout_sec: must be between 1 and 86400", "command 2: line_limit: must be between 1 and 10000", "command 2: patterns: are used only when purpose is collect_output", "command 2: pattern 1: must not be empty", "command 2: pattern 2: invalid regular expression: error parsing regexp: missing closing ) in `(?P<a>(x)`", "command 2: pattern 3: \\1 refers to group 1, which is still open at that point", "command 2: pattern 4: \\2 refers to group 2, which is not defined before it", "command 2: pattern 5: \\12 refers to group 12, which is not defined before it", "command 2: pattern 6: \\189: write references to groups with at most two digits and octal escapes with three octal digits", "command 2: pattern 7: \\k<2> refers to group 2, which is not defined before it", "command 2: pattern 8: \\1 refers to group 1, which is not defined before it", "command 2: pattern 9: (?(2)...) refers to group 2, which does not exist", "command 2: pattern 10: (?(0)...) refers to group 0, which does not exist", "command 2: pattern 11: (?P=a) refers to a group that is still open at that point", "command 2: pattern 12: (?P=a) refers to a group name that is not defined before it", "command 2: pattern 13: (?P=b) refers to a group name that is not defined before it", "command 2: pattern 14: group numbers are ambiguous because the name \"a\" is used for more than one group", "command 2: pattern 15: the option n (explicit capture) is not supported", "command 2: pattern 16: the option n (explicit capture) is not supported", "command 2: pattern 17: numbered groups such as (?<1>...) are not supported; give the group a name", "command 2: pattern 18: balancing groups such as (?<name1-name2>...) are not supported", "command 2: pattern 19: balancing groups such as (?<-name>...) are not supported", "command 2: pattern 20: the empty group (?) is not supported", "command 2: pattern 21: a comment (?#...) that ends with a backslash is not supported", "command 2: pattern 22: a # comment that ends with a backslash is not supported", "command 2: pattern 23: \\Q is not supported (quoting with \\Q...\\E); escape each special character with a backslash instead", "command 2: pattern 24: \\Q is not supported (quoting with \\Q...\\E); escape each special character with a backslash instead", "command 2: pattern 25: \\E is not supported (quoting with \\Q...\\E); escape each special character with a backslash instead", "command 2: pattern 26: must have at least one named group such as (?P<name>...) or (?<name>...)", "command 2: pattern 27: must have at least one named group such as (?P<name>...) or (?<name>...)", "command 2: pattern 28: invalid regular expression: error parsing regexp: unterminated [] set in `(?P<a>[)`", "command 2: pattern 29: invalid regular expression: error parsing regexp: invalid repeat count in `(?P<a>a{2,1})`", "command 3: command_line: must be at most 102400 bytes", "command 3: success, failure: the success codes (0) and the failure codes (0-3) must not overlap", "command 4: success.a: must be between 0 and 255", "command 4: failure.b: is used only by the kind range", "command 5: success, failure: the success codes (>= 1) and the failure codes (<= 125) must not overlap"},
	"command_set/3":       {"os_user: must not start with \"-\"", "work_dir: must be a clean path (no empty, . or .. parts and no trailing slash)", "schedule: is required when run_mode is schedule"},
	"command_set/4":       {"os_user: must be at most 256 characters", "work_dir: must be at most 4096 bytes", "schedule.at: a date and time is required"},
	"command_set/5":       {"schedule.at: \"tomorrow\" is not a date and time with a UTC offset such as 2026-12-01T03:00:00+09:00"},
	"command_set/6":       {"schedule.at: must be in the future"},
	"command_set/7":       {"schedule.weekdays: at least one weekday is required", "schedule.time: must be a time of day from 00:00 to 23:59"},
	"command_set/8":       {"schedule.weekdays: \"xyz\" is not one of sun, mon, tue, wed, thu, fri, sat", "schedule.weekdays: \"Sun\" is not one of sun, mon, tue, wed, thu, fri, sat", "schedule.time: must be a time of day from 00:00 to 23:59"},
	"command_set/9":       {"schedule.time: must be a time of day from 00:00 to 23:59"},
	"command_set_now0/0":  {},
	"command_set_now0/1":  {"name: is required", "description: must not contain control characters other than line breaks and tabs", "purpose: must be collect_output or run_only", "os_user: is required", "work_dir: must be an absolute path", "run_mode: must be manual or schedule", "keep_runs: must be between 1 and 10000", "joiner: must be comma or newline", "commands: at least one command is required"},
	"command_set_now0/10": {"schedule.every: must be one of 1, 2, 3, 4, 6, 8, 12"},
	"command_set_now0/11": {"schedule.every: must be one of 1, 2, 3, 4, 5, 6, 10, 12, 15, 20, 30"},
	"command_set_now0/12": {"schedule.minute: a minute from 0 to 59 is required"},
	"command_set_now0/13": {"schedule.minute: must be between 0 and 59"},
	"command_set_now0/14": {"schedule.kind: must be once, weekly, daily, every_hours, every_minutes or hourly_at"},
	"command_set_now0/2":  {"name: must be at most 100 characters", "name: must not contain control characters", "description: must be at most 1000 characters", "os_user: must not contain white space or control characters", "work_dir: must not contain NUL or line breaks", "schedule: must be null when run_mode is manual", "keep_runs: must be between 1 and 10000", "command 1: command_line: is required", "command 1: note: must not contain control characters other than line breaks and tabs", "command 1: success.kind: must be single, range, min or max", "command 1: failure.a: must be between 0 and 255", "command 1: failure.b: must be between 0 and 255", "command 1: on_problems: must be stop, continue_on_failure or continue_always", "command 1: timeout_sec: must be between 1 and 86400", "command 1: line_limit: must be between 1 and 10000", "command 1: patterns: are used only when purpose is collect_output", "command 2: command_line: must not contain a NUL byte", "command 2: note: must be at most 1000 characters", "command 2: success.b: must be greater than a", "command 2: failure.b: is used only by the kind range", "command 2: timeout_sec: must be between 1 and 86400", "command 2: line_limit: must be between 1 and 10000", "command 2: patterns: are used only when purpose is collect_output", "command 2: pattern 1: must not be empty", "command 2: pattern 2: invalid regular expression: error parsing regexp: missing closing ) in `(?P<a>(x)`", "command 2: pattern 3: \\1 refers to group 1, which is still open at that point", "command 2: pattern 4: \\2 refers to group 2, which is not defined before it", "command 2: pattern 5: \\12 refers to group 12, which is not defined before it", "command 2: pattern 6: \\189: write references to groups with at most two digits and octal escapes with three octal digits", "command 2: pattern 7: \\k<2> refers to group 2, which is not defined before it", "command 2: pattern 8: \\1 refers to group 1, which is not defined before it", "command 2: pattern 9: (?(2)...) refers to group 2, which does not exist", "command 2: pattern 10: (?(0)...) refers to group 0, which does not exist", "command 2: pattern 11: (?P=a) refers to a group that is still open at that point", "command 2: pattern 12: (?P=a) refers to a group name that is not defined before it", "command 2: pattern 13: (?P=b) refers to a group name that is not defined before it", "command 2: pattern 14: group numbers are ambiguous because the name \"a\" is used for more than one group", "command 2: pattern 15: the option n (explicit capture) is not supported", "command 2: pattern 16: the option n (explicit capture) is not supported", "command 2: pattern 17: numbered groups such as (?<1>...) are not supported; give the group a name", "command 2: pattern 18: balancing groups such as (?<name1-name2>...) are not supported", "command 2: pattern 19: balancing groups such as (?<-name>...) are not supported", "command 2: pattern 20: the empty group (?) is not supported", "command 2: pattern 21: a comment (?#...) that ends with a backslash is not supported", "command 2: pattern 22: a # comment that ends with a backslash is not supported", "command 2: pattern 23: \\Q is not supported (quoting with \\Q...\\E); escape each special character with a backslash instead", "command 2: pattern 24: \\Q is not supported (quoting with \\Q...\\E); escape each special character with a backslash instead", "command 2: pattern 25: \\E is not supported (quoting with \\Q...\\E); escape each special character with a backslash instead", "command 2: pattern 26: must have at least one named group such as (?P<name>...) or (?<name>...)", "command 2: pattern 27: must have at least one named group such as (?P<name>...) or (?<name>...)", "command 2: pattern 28: invalid regular expression: error parsing regexp: unterminated [] set in `(?P<a>[)`", "command 2: pattern 29: invalid regular expression: error parsing regexp: invalid repeat count in `(?P<a>a{2,1})`", "command 3: command_line: must be at most 102400 bytes", "command 3: success, failure: the success codes (0) and the failure codes (0-3) must not overlap", "command 4: success.a: must be between 0 and 255", "command 4: failure.b: is used only by the kind range", "command 5: success, failure: the success codes (>= 1) and the failure codes (<= 125) must not overlap"},
	"command_set_now0/3":  {"os_user: must not start with \"-\"", "work_dir: must be a clean path (no empty, . or .. parts and no trailing slash)", "schedule: is required when run_mode is schedule"},
	"command_set_now0/4":  {"os_user: must be at most 256 characters", "work_dir: must be at most 4096 bytes", "schedule.at: a date and time is required"},
	"command_set_now0/5":  {"schedule.at: \"tomorrow\" is not a date and time with a UTC offset such as 2026-12-01T03:00:00+09:00"},
	"command_set_now0/6":  {},
	"command_set_now0/7":  {"schedule.weekdays: at least one weekday is required", "schedule.time: must be a time of day from 00:00 to 23:59"},
	"command_set_now0/8":  {"schedule.weekdays: \"xyz\" is not one of sun, mon, tue, wed, thu, fri, sat", "schedule.weekdays: \"Sun\" is not one of sun, mon, tue, wed, thu, fri, sat", "schedule.time: must be a time of day from 00:00 to 23:59"},
	"command_set_now0/9":  {"schedule.time: must be a time of day from 00:00 to 23:59"},
	"config/0":            {},
	"config/1":            {"web_listen: address x: missing port in address; agent_listen: the address must be an IP address; agent_listen: the port must be 1-65535; server_names: \"a b\" is not a host name or IP address; server_names: \"x_y\" is not a host name or IP address; smtp.security must be starttls, tls or none; smtp.host must be a host name or IP address; smtp.from must be one mail address; workers must be 64 or less; smtp.username must not contain control characters; smtp.password must not contain control characters; smtp.helo_name must not contain control characters; default_check_interval_sec must be 10 or more; token_renew_before_days must be less than token_valid_days"},
	"config/2":            {"web_listen and agent_listen must differ; retention_days must be 36500 or less"},
	"config/3":            {"web_listen: the port must be 1-65535; agent_listen: the port must be 1-65535; notify.subject_prefix must not contain control characters"},
	"config_change/0":     {"unknown configuration item \"nope\" (items: agent_listen, builtin_agent, default_check_interval_sec, notify.dedupe_hours, notify.max_lines, notify.min_interval_min, notify.subject_prefix, retention_days, server_names, session_remember_days, smtp.auth, smtp.from, smtp.helo_name, smtp.host, smtp.password, smtp.port, smtp.security, smtp.timeout_sec, smtp.username, token_renew_before_days, token_valid_days, two_step_trust_days, web_listen, workers)"},
	"config_change/1":     {"unknown configuration item \"nope\" (items: agent_listen, builtin_agent, default_check_interval_sec, notify.dedupe_hours, notify.max_lines, notify.min_interval_min, notify.subject_prefix, retention_days, server_names, session_remember_days, smtp.auth, smtp.from, smtp.helo_name, smtp.host, smtp.password, smtp.port, smtp.security, smtp.timeout_sec, smtp.username, token_renew_before_days, token_valid_days, two_step_trust_days, web_listen, workers)"},
	"config_change/2":     {"web_listen is both set and unset"},
	"config_change/3":     {"a value is required for web_listen"},
	"config_change/4":     {"workers must be a positive integer"},
	"config_change/5":     {"workers must be 64 or less"},
	"config_change/6":     {"web_listen: address a: missing port in address; smtp.security must be starttls, tls or none"},
	"config_change/7":     {"server_names: \"a b\" is not a host name or IP address; server_names: \"c d\" is not a host name or IP address"},
	"config_item/0":       {"unknown configuration item \"nope\" (items: agent_listen, builtin_agent, default_check_interval_sec, notify.dedupe_hours, notify.max_lines, notify.min_interval_min, notify.subject_prefix, retention_days, server_names, session_remember_days, smtp.auth, smtp.from, smtp.helo_name, smtp.host, smtp.password, smtp.port, smtp.security, smtp.timeout_sec, smtp.username, token_renew_before_days, token_valid_days, two_step_trust_days, web_listen, workers)"},
	"config_item/1":       {"workers must be a positive integer"},
	"config_item/2":       {"workers must be a positive integer"},
	"config_item/3":       {"workers must be a positive integer"},
	"config_item/4":       {"builtin_agent must be true or false"},
	"config_item/5":       {"smtp.auth must be true or false"},
	"display_name/0":      {"a display name is required"},
	"display_name/1":      {"a display name is required"},
	"display_name/2":      {"the display name must be at most 64 characters"},
	"display_name/3":      {"the display name must not contain control characters"},
	"display_name/4":      {"the display name must be at most 64 characters"},
	"email/0":             {"an e-mail address is required"},
	"email/1":             {"an e-mail address is required"},
	"email/10":            {"the e-mail address is not valid"},
	"email/11":            {"the e-mail address is not valid"},
	"email/2":             {"the e-mail address must be at most 254 characters"},
	"email/3":             {"the e-mail address must not contain spaces, control characters or \" < > ,"},
	"email/4":             {"the e-mail address must not contain spaces, control characters or \" < > ,"},
	"email/5":             {"the e-mail address must not contain spaces, control characters or \" < > ,"},
	"email/6":             {"the e-mail address must not contain spaces, control characters or \" < > ,"},
	"email/7":             {"the e-mail address must not contain spaces, control characters or \" < > ,"},
	"email/8":             {"the e-mail address is not valid"},
	"email/9":             {"the e-mail address is not valid"},
	"free_text/0":         {"must be at most 1000 characters"},
	"free_text/1":         {"must not contain control characters other than line breaks and tabs"},
	"free_text/2":         {"must not contain control characters other than line breaks and tabs"},
	"os_user/0":           {"is required"},
	"os_user/1":           {"must be at most 256 characters"},
	"os_user/2":           {"must not contain white space or control characters"},
	"os_user/3":           {"must not contain white space or control characters"},
	"os_user/4":           {"must not start with \"-\""},
	"password/0":          {"password must be at least 10 characters"},
	"password/1":          {"password must be at least 10 characters"},
	"password/2":          {"password is too long"},
	"pattern/0":           {"must not be empty"},
	"pattern/1":           {"invalid regular expression: error parsing regexp: missing closing ) in `(?P<a>(x)`"},
	"pattern/10":          {"(?P=a) refers to a group that is still open at that point"},
	"pattern/11":          {"(?P=a) refers to a group name that is not defined before it"},
	"pattern/12":          {"(?P=b) refers to a group name that is not defined before it"},
	"pattern/13":          {"group numbers are ambiguous because the name \"a\" is used for more than one group"},
	"pattern/14":          {"the option n (explicit capture) is not supported"},
	"pattern/15":          {"the option n (explicit capture) is not supported"},
	"pattern/16":          {"numbered groups such as (?<1>...) are not supported; give the group a name"},
	"pattern/17":          {"balancing groups such as (?<name1-name2>...) are not supported"},
	"pattern/18":          {"balancing groups such as (?<-name>...) are not supported"},
	"pattern/19":          {"the empty group (?) is not supported"},
	"pattern/2":           {"\\1 refers to group 1, which is still open at that point"},
	"pattern/20":          {"a comment (?#...) that ends with a backslash is not supported"},
	"pattern/21":          {"a # comment that ends with a backslash is not supported"},
	"pattern/22":          {"\\Q is not supported (quoting with \\Q...\\E); escape each special character with a backslash instead"},
	"pattern/23":          {"\\Q is not supported (quoting with \\Q...\\E); escape each special character with a backslash instead"},
	"pattern/24":          {"\\E is not supported (quoting with \\Q...\\E); escape each special character with a backslash instead"},
	"pattern/25":          {"must have at least one named group such as (?P<name>...) or (?<name>...)"},
	"pattern/26":          {"must have at least one named group such as (?P<name>...) or (?<name>...)"},
	"pattern/27":          {"invalid regular expression: error parsing regexp: unterminated [] set in `(?P<a>[)`"},
	"pattern/28":          {"invalid regular expression: error parsing regexp: invalid repeat count in `(?P<a>a{2,1})`"},
	"pattern/3":           {"\\2 refers to group 2, which is not defined before it"},
	"pattern/4":           {"\\12 refers to group 12, which is not defined before it"},
	"pattern/5":           {"\\189: write references to groups with at most two digits and octal escapes with three octal digits"},
	"pattern/6":           {"\\k<2> refers to group 2, which is not defined before it"},
	"pattern/7":           {"\\1 refers to group 1, which is not defined before it"},
	"pattern/8":           {"(?(2)...) refers to group 2, which does not exist"},
	"pattern/9":           {"(?(0)...) refers to group 0, which does not exist"},
	"rel_path/0":          {"empty path"},
	"rel_path/1":          {"\"a//b\" must be directory names separated by slashes, without dot segments"},
	"rel_path/2":          {"\"../a\" must be directory names separated by slashes, without dot segments"},
	"rel_path/3":          {"\"a/./b\" must be directory names separated by slashes, without dot segments"},
	"rel_path/4":          {"\"/a\" must be directory names separated by slashes, without dot segments"},
	"rel_path/5":          {"\"a/\" must be directory names separated by slashes, without dot segments"},
	"rel_path/6":          {"\"a\\x00b\" must be directory names separated by slashes, without dot segments"},
	"rule_content/0":      {},
	"rule_content/1":      {"check_interval_sec: must be between 10 and 3600", "denied.category: must be alert, normal or empty", "exec.category: must be alert, normal or empty", "exec_files.extensions: invalid extension \".toolongextension16\"", "exec_files.extensions: invalid extension \"exe\"", "exec_files.legit \"../a\": at least one extension is required", "exec_files.legit \"../a\": category must be alert or normal", "exec_files.legit \"ok\": invalid extension \"bad\"", "exec_files.legit: \"../a\" must be directory names separated by slashes, without dot segments", "exec_files.legit: empty path", "exec_files.other_category: must be alert, normal or empty", "network.category: must be alert, normal or empty", "network.executables: \"x\" is not an absolute path", "read.category: must be alert, normal or empty", "read.dirs: \"/a b\" contains a forbidden character", "read.dirs: \"/a/../b\" must not contain dot segments", "read.dirs: \"/a//b\" is not a clean path", "read.dirs: \"/<LONG>\" is too long", "read.dirs: \"rel\" is not an absolute path", "scan_interval_sec: must be between 60 and 604800", "users: invalid user name \"\"", "users: invalid user name \"Bad User\"", "writable.allowed_paths: \"../a\" must be directory names separated by slashes, without dot segments", "writable.allowed_paths: empty path", "writable.base: / is not allowed", "writable.category: must be alert, normal or empty"},
	"rule_content/2":      {"check_interval_sec: must be between 10 and 3600", "exec_files.extensions: at most 50 entries", "exec_files.legit: at most 200 entries", "network.executables: at most 200 entries", "read.dirs: at most 200 entries", "scan_interval_sec: must be between 60 and 604800", "users: at most 200 entries", "writable.allowed_paths: at most 200 entries", "writable.base: \"rel\" is not an absolute path"},
	"rule_content/3":      {"exec_files.legit \"a/./b\": category must be alert or normal", "exec_files.legit: \"a/./b\" must be directory names separated by slashes, without dot segments", "writable.base: \"/a/\" is not a clean path"},
	"rule_json/0":         {"invalid rule content: the content must be a JSON object"},
	"rule_json/1":         {"invalid rule content: the content must be a JSON object"},
	"rule_json/10":        {"invalid rule content: read must be an object, not array"},
	"rule_json/11":        {"invalid rule content: read.dirs must be an array, not object"},
	"rule_json/12":        {"invalid rule content: unknown field \"unknown\""},
	"rule_json/13":        {"invalid rule content: unexpected EOF"},
	"rule_json/14":        {"invalid rule content: invalid character '}' looking for beginning of value"},
	"rule_json/15":        {"invalid rule content: exec_files.legit.0.extensions must be an array, not string"},
	"rule_json/2":         {"invalid rule content: the content must be a JSON object"},
	"rule_json/3":         {"invalid rule content: the content must be a JSON object"},
	"rule_json/4":         {"invalid rule content: users must be an array, not string"},
	"rule_json/5":         {"invalid rule content: users.0 must be an object, not number"},
	"rule_json/6":         {"invalid rule content: users.0.value must be a string, not bool"},
	"rule_json/7":         {"invalid rule content: check_interval_sec must be an integer, not string"},
	"rule_json/8":         {"invalid rule content: check_interval_sec must be an integer, not number 1.5"},
	"rule_json/9":         {"invalid rule content: exclude_interactive must be true or false, not number"},
	"schedule_at/0":       {"\"\" is not a date and time with a UTC offset such as 2026-12-01T03:00:00+09:00"},
	"schedule_at/1":       {"\"x\" is not a date and time with a UTC offset such as 2026-12-01T03:00:00+09:00"},
	"schedule_at/2":       {"\"2026-13-01 03:00\" is not a date and time with a UTC offset such as 2026-12-01T03:00:00+09:00"},
	"schedule_at/3":       {"\"2026-12-01 03:00\" is not a date and time with a UTC offset such as 2026-12-01T03:00:00+09:00"},
	"schedule_at/4":       {"\"2026-12-01T03:00:00\" is not a date and time with a UTC offset such as 2026-12-01T03:00:00+09:00"},
	"schedule_at_local/0": {"\"\" is not a date and time such as 2026-12-01 03:00"},
	"schedule_at_local/1": {"\"x\" is not a date and time such as 2026-12-01 03:00"},
	"schedule_at_local/2": {"\"2026-13-01 03:00\" is not a date and time such as 2026-12-01 03:00"},
	"schedule_at_local/3": {},
	"schedule_at_local/4": {},
	"work_dir/0":          {"must be an absolute path"},
	"work_dir/1":          {"must not contain NUL or line breaks"},
	"work_dir/2":          {"must not contain NUL or line breaks"},
	"work_dir/3":          {"must be a clean path (no empty, . or .. parts and no trailing slash)"},
	"work_dir/4":          {"must be a clean path (no empty, . or .. parts and no trailing slash)"},
	"work_dir/5":          {"must be a clean path (no empty, . or .. parts and no trailing slash)"},
	"work_dir/6":          {"must be at most 4096 bytes"},
}

// TestEnglishUnchanged checks that every validator answers coded messages
// whose English rendering is the text the validator gave before it was
// coded, and that its English form (the error text and the Validate lists
// the CLI and the agent use) is unchanged.
func TestEnglishUnchanged(t *testing.T) {
	seen := map[string]bool{}
	want := func(key string) []string {
		w, ok := englishWant[key]
		if !ok {
			t.Fatalf("%s: no expected text", key)
		}
		seen[key] = true
		out := make([]string, len(w))
		for i, s := range w {
			out[i] = strings.ReplaceAll(s, englishLongMark, englishLong)
		}
		return out
	}
	// one checks a validator that returns one error.
	one := func(key string, err error) {
		t.Helper()
		w := want(key)
		if err == nil {
			if len(w) != 0 {
				t.Errorf("%s: no error, want %q", key, w[0])
			}
			return
		}
		if len(w) != 1 {
			t.Errorf("%s: error %q, want none", key, err)
			return
		}
		if err.Error() != w[0] {
			t.Errorf("%s: error text\n got %q\nwant %q", key, err.Error(), w[0])
		}
		m := AsMsg(err)
		if m.Code == "" {
			t.Errorf("%s: the error %q has no code", key, err)
		}
		checkEnglishMsg(t, key, m, w[0])
	}
	// list checks a validator that returns problems.
	list := func(key string, ps []Problem, texts []string) {
		t.Helper()
		w := want(key)
		if len(w) == 0 {
			w = nil
		}
		if len(texts) == 0 {
			texts = nil
		}
		if !reflect.DeepEqual(texts, w) {
			t.Errorf("%s: English form\n got %q\nwant %q", key, texts, w)
		}
		got := ProblemTexts(ps)
		if len(got) == 0 {
			got = nil
		}
		if !reflect.DeepEqual(got, w) {
			t.Errorf("%s: problems\n got %q\nwant %q", key, got, w)
			return
		}
		for i, p := range ps {
			if p.Code == "" {
				t.Errorf("%s: problem %q has no code", key, p.Text())
			}
			for _, where := range p.Where {
				if where.Code == "" {
					t.Errorf("%s: location of %q has no code", key, p.Text())
				}
			}
			checkEnglishProblem(t, fmt.Sprintf("%s[%d]", key, i), p, w[i])
		}
	}

	for i, s := range englishEmailInputs {
		_, err := ValidateEmail(s)
		one(fmt.Sprintf("email/%d", i), err)
	}
	for i, s := range englishDisplayNameInputs {
		_, err := ValidateDisplayName(s)
		one(fmt.Sprintf("display_name/%d", i), err)
	}
	for i, s := range englishPasswordInputs {
		one(fmt.Sprintf("password/%d", i), ValidatePassword(s))
	}
	for i, s := range englishAbsPathInputs {
		one(fmt.Sprintf("abs_path/%d", i), ValidateAbsPath(s))
	}
	for i, s := range englishRelPathInputs {
		one(fmt.Sprintf("rel_path/%d", i), ValidateRelPath(s))
	}
	for i, s := range englishRuleJSONInputs {
		_, err := ParseRuleContent([]byte(s))
		one(fmt.Sprintf("rule_json/%d", i), err)
	}
	for i, s := range englishCommandJSONInputs {
		_, err := ParseCommandSetSpec([]byte(s))
		one(fmt.Sprintf("command_json/%d", i), err)
	}
	for i, s := range englishPatternInputs {
		_, err := CompileCommandPattern(s)
		one(fmt.Sprintf("pattern/%d", i), err)
	}
	for i, s := range englishScheduleAtInputs {
		_, err := ParseScheduleAt(s, nil)
		one(fmt.Sprintf("schedule_at/%d", i), err)
		_, err = ParseScheduleAt(s, time.UTC)
		one(fmt.Sprintf("schedule_at_local/%d", i), err)
	}
	for i, s := range englishOSUserInputs {
		one(fmt.Sprintf("os_user/%d", i), ValidateCommandOSUser(s))
	}
	for i, s := range englishWorkDirInputs {
		one(fmt.Sprintf("work_dir/%d", i), validateWorkDir(s))
	}
	for i, s := range englishFreeTextInputs {
		one(fmt.Sprintf("free_text/%d", i), checkFreeText(s))
	}
	for i, c := range englishRuleContents() {
		list(fmt.Sprintf("rule_content/%d", i), c.Problems(), c.Validate())
	}
	for i, s := range englishCommandSets() {
		list(fmt.Sprintf("command_set/%d", i), s.Problems(englishNow), s.Validate(englishNow))
		list(fmt.Sprintf("command_set_now0/%d", i), s.Problems(0), s.Validate(0))
	}
	for i, pairs := range englishConfigSets {
		c := DefaultCentralConfig()
		for _, p := range pairs {
			f, err := FindConfigField(p[0])
			if err != nil {
				t.Fatal(err)
			}
			if err := f.set(c, p[1]); err != nil {
				t.Fatal(p, err)
			}
		}
		err := c.Validate()
		one(fmt.Sprintf("config/%d", i), err)
		if err != nil {
			if _, ok := err.(Problems); !ok {
				t.Errorf("config/%d: Validate returned %T, want Problems", i, err)
			}
		}
	}
	for i, p := range englishConfigItemInputs {
		f, err := FindConfigField(p[0])
		if err == nil {
			err = f.set(DefaultCentralConfig(), p[1])
		}
		one(fmt.Sprintf("config_item/%d", i), err)
	}
	MigrationsFS = os.DirFS("../..")
	for i, ch := range englishConfigChanges {
		db, err := OpenSQLite(filepath.Join(t.TempDir(), "central.db"), SchemaCentral)
		if err != nil {
			t.Fatal(err)
		}
		_, err = applyConfig(db, ch.set, ch.unset)
		one(fmt.Sprintf("config_change/%d", i), err)
		switch err.(type) {
		case Msg, Problems:
		default:
			t.Errorf("config_change/%d: refusal of type %T, want Msg or Problems", i, err)
		}
		db.Close()
	}
	for key := range englishWant {
		if !seen[key] {
			t.Errorf("%s: expected text not checked", key)
		}
	}
}

// checkEnglishMsg checks that a message renders as want, also after a JSON
// round trip (as the browser and stored rows receive it).
func checkEnglishMsg(t *testing.T, key string, m Msg, want string) {
	t.Helper()
	if m.Text() != want {
		t.Errorf("%s: Text()\n got %q\nwant %q", key, m.Text(), want)
	}
	b, err := json.Marshal(m)
	if err != nil {
		t.Fatalf("%s: %v", key, err)
	}
	var back Msg
	if err := json.Unmarshal(b, &back); err != nil {
		t.Fatalf("%s: %v", key, err)
	}
	if back.Text() != want {
		t.Errorf("%s: Text() after JSON\n got %q\nwant %q", key, back.Text(), want)
	}
}

// checkEnglishProblem is checkEnglishMsg for a problem.
func checkEnglishProblem(t *testing.T, key string, p Problem, want string) {
	t.Helper()
	b, err := json.Marshal(p)
	if err != nil {
		t.Fatalf("%s: %v", key, err)
	}
	var back Problem
	if err := json.Unmarshal(b, &back); err != nil {
		t.Fatalf("%s: %v", key, err)
	}
	if back.Text() != want {
		t.Errorf("%s: Text() after JSON\n got %q\nwant %q", key, back.Text(), want)
	}
}
