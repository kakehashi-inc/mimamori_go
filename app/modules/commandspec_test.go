package modules

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func validSpec() *CommandSetSpec {
	s := &CommandSetSpec{
		Name: "disk usage", Purpose: PurposeCollectOutput, OSUser: "app",
		Commands: []CommandSpec{{CommandLine: "df -h /", Patterns: []string{`(?P<use>\d+)%`}}},
	}
	s.Normalize()
	return s
}

func intPtr(n int) *int { return &n }

func TestCommandSetDefaults(t *testing.T) {
	s, err := ParseCommandSetSpec([]byte(`{"name":" x ","purpose":"run_only","os_user":"app","commands":[{"command_line":"true"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	c := s.Commands[0]
	if s.Name != "x" || s.KeepRuns != 100 || s.Joiner != JoinerComma || s.RunMode != RunModeManual ||
		c.Success != DefaultSuccessCodes || c.Failure != DefaultFailureCodes || c.OnProblems != OnProblemsStop ||
		c.TimeoutSec != 300 || c.LineLimit != 20 || c.Patterns == nil {
		t.Fatalf("defaults: %+v", s)
	}
	if errs := s.Validate(time.Now().Unix()); len(errs) != 0 {
		t.Fatalf("valid set refused: %v", errs)
	}
	s2, _ := ParseCommandSetSpec([]byte(`{"name":"x","purpose":"run_only","os_user":"app","schedule":{"kind":"daily","time":"03:00"},"commands":[{"command_line":"true"}]}`))
	if s2.RunMode != RunModeSchedule {
		t.Fatalf("run mode not derived from the schedule: %q", s2.RunMode)
	}
	for _, bad := range []string{`{"name":"x","extra":1}`, `[]`, `{"name":1}`, `{"name":"x"} {}`} {
		if _, err := ParseCommandSetSpec([]byte(bad)); err == nil {
			t.Errorf("%s accepted", bad)
		}
	}
	// The command line is kept byte for byte.
	line := "  cd /tmp && ls\n\techo \"$HOME\"  "
	b, _ := json.Marshal(map[string]any{"name": "x", "purpose": "run_only", "os_user": "a", "commands": []any{map[string]any{"command_line": line}}})
	s3, _ := ParseCommandSetSpec(b)
	if s3.Commands[0].CommandLine != line {
		t.Fatalf("command line changed: %q", s3.Commands[0].CommandLine)
	}
}

func TestCommandSetValidate(t *testing.T) {
	now := time.Date(2026, 11, 12, 10, 0, 0, 0, time.UTC).Unix()
	tests := []struct {
		name string
		edit func(s *CommandSetSpec)
		want string // a fragment of one message; "" means valid
	}{
		{"valid", func(s *CommandSetSpec) {}, ""},
		{"empty name", func(s *CommandSetSpec) { s.Name = "" }, "name: is required"},
		{"long name", func(s *CommandSetSpec) { s.Name = strings.Repeat("a", 101) }, "name: must be at most 100"},
		{"100 characters", func(s *CommandSetSpec) { s.Name = strings.Repeat("あ", 100) }, ""},
		{"control in name", func(s *CommandSetSpec) { s.Name = "a\tb" }, "name: must not contain control"},
		{"long description", func(s *CommandSetSpec) { s.Description = strings.Repeat("a", 1001) }, "description: must be at most 1000"},
		{"multi-line description", func(s *CommandSetSpec) { s.Description = "a\nb" }, ""},
		{"purpose", func(s *CommandSetSpec) { s.Purpose = "x" }, "purpose: must be"},
		{"no user", func(s *CommandSetSpec) { s.OSUser = "" }, "os_user: is required"},
		{"user with space", func(s *CommandSetSpec) { s.OSUser = "a b" }, "os_user: must not contain white space"},
		{"user with dash", func(s *CommandSetSpec) { s.OSUser = "-u" }, `os_user: must not start with "-"`},
		{"long user", func(s *CommandSetSpec) { s.OSUser = strings.Repeat("u", 257) }, "os_user: must be at most 256"},
		{"domain user", func(s *CommandSetSpec) { s.OSUser = `CORP\app.user@x` }, ""},
		{"relative dir", func(s *CommandSetSpec) { s.WorkDir = "tmp" }, "work_dir: must be an absolute path"},
		{"unclean dir", func(s *CommandSetSpec) { s.WorkDir = "/srv/../x" }, "work_dir: must be a clean path"},
		{"trailing slash", func(s *CommandSetSpec) { s.WorkDir = "/srv/" }, "work_dir: must be a clean path"},
		{"dir with newline", func(s *CommandSetSpec) { s.WorkDir = "/srv\n" }, "work_dir: must not contain NUL"},
		{"dir with space", func(s *CommandSetSpec) { s.WorkDir = "/srv/my app" }, ""},
		{"long dir", func(s *CommandSetSpec) { s.WorkDir = "/" + strings.Repeat("a", 4096) }, "work_dir: must be at most 4096"},
		{"run mode", func(s *CommandSetSpec) { s.RunMode = "cron" }, "run_mode: must be manual or schedule"},
		{"manual with schedule", func(s *CommandSetSpec) { s.Schedule = &CommandSchedule{Kind: ScheduleDaily, Time: "03:00"} }, "schedule: must be null"},
		{"schedule missing", func(s *CommandSetSpec) { s.RunMode = RunModeSchedule }, "schedule: is required"},
		{"keep 0", func(s *CommandSetSpec) { s.KeepRuns = -1 }, "keep_runs: must be between 1 and 10000"},
		{"keep 10001", func(s *CommandSetSpec) { s.KeepRuns = 10001 }, "keep_runs"},
		{"joiner", func(s *CommandSetSpec) { s.Joiner = "tab" }, "joiner: must be comma or newline"},
		{"no commands", func(s *CommandSetSpec) { s.Commands = nil }, "commands: at least one command"},
		{"empty line", func(s *CommandSetSpec) { s.Commands[0].CommandLine = " \n " }, "command 1: command_line: is required"},
		{"NUL", func(s *CommandSetSpec) { s.Commands[0].CommandLine = "a\x00b" }, "command 1: command_line: must not contain a NUL"},
		{"long line", func(s *CommandSetSpec) { s.Commands[0].CommandLine = strings.Repeat("a", 102401) }, "command_line: must be at most 102400 bytes"},
		{"longest line", func(s *CommandSetSpec) { s.Commands[0].CommandLine = strings.Repeat("a", 102400) }, ""},
		{"any shell", func(s *CommandSetSpec) {
			s.Commands[0].CommandLine = "cd /x && y | z > /dev/null 2>&1\nrm -rf \"$T\"; `id`"
		}, ""},
		{"long note", func(s *CommandSetSpec) { s.Commands[0].Note = strings.Repeat("n", 1001) }, "command 1: note: must be at most 1000"},
		{"code kind", func(s *CommandSetSpec) { s.Commands[0].Success = CodeSpec{Kind: "any"} }, "success.kind: must be single, range, min or max"},
		{"code 256", func(s *CommandSetSpec) { s.Commands[0].Failure = CodeSpec{Kind: CodeSingle, A: 256} }, "failure.a: must be between 0 and 255"},
		{"range order", func(s *CommandSetSpec) { s.Commands[0].Failure = CodeSpec{Kind: CodeRange, A: 5, B: 5} }, "failure.b: must be greater than a"},
		{"b without range", func(s *CommandSetSpec) { s.Commands[0].Failure = CodeSpec{Kind: CodeMin, A: 5, B: 9} }, "failure.b: is used only"},
		{"overlap", func(s *CommandSetSpec) { s.Commands[0].Success = CodeSpec{Kind: CodeMax, A: 1} }, "must not overlap"},
		{"overlap range", func(s *CommandSetSpec) {
			s.Commands[0].Success = CodeSpec{Kind: CodeRange, A: 0, B: 3}
			s.Commands[0].Failure = CodeSpec{Kind: CodeSingle, A: 3}
		}, "must not overlap"},
		{"no overlap", func(s *CommandSetSpec) {
			s.Commands[0].Success = CodeSpec{Kind: CodeRange, A: 0, B: 3}
			s.Commands[0].Failure = CodeSpec{Kind: CodeMin, A: 4}
		}, ""},
		{"on problems", func(s *CommandSetSpec) { s.Commands[0].OnProblems = "ignore" }, "on_problems: must be"},
		{"timeout", func(s *CommandSetSpec) { s.Commands[0].TimeoutSec = 86401 }, "timeout_sec: must be between 1 and 86400"},
		{"line limit", func(s *CommandSetSpec) { s.Commands[0].LineLimit = 10001 }, "line_limit: must be between 1 and 10000"},
		{"pattern without name", func(s *CommandSetSpec) { s.Commands[0].Patterns = []string{`(\d+)%`} }, "pattern 1: must have at least one named group"},
		{"bad pattern", func(s *CommandSetSpec) { s.Commands[0].Patterns = []string{`(?P<a>x`} }, "pattern 1: invalid regular expression"},
		{"empty pattern", func(s *CommandSetSpec) { s.Commands[0].Patterns = []string{""} }, "pattern 1: must not be empty"},
		{"look-around and back-reference", func(s *CommandSetSpec) {
			s.Commands[0].Patterns = []string{`(?<=size: )(?<size>\d+)(?= MB)`, `(?P<w>\w+) \k<w>`, `(?P<q>['"]).*?\1`}
		}, ""},
		{"patterns of run_only", func(s *CommandSetSpec) { s.Purpose = PurposeRunOnly }, "patterns: are used only when purpose is collect_output"},
		{"second command", func(s *CommandSetSpec) {
			s.Commands = append(s.Commands, CommandSpec{CommandLine: "x", Success: DefaultSuccessCodes, Failure: DefaultFailureCodes,
				OnProblems: OnProblemsStop, TimeoutSec: 0, LineLimit: 20})
		}, "command 2: timeout_sec"},
		{"once past", func(s *CommandSetSpec) {
			s.RunMode, s.Schedule = RunModeSchedule, &CommandSchedule{Kind: ScheduleOnce, At: "2026-11-12T09:59:00Z"}
		}, "schedule.at: must be in the future"},
		{"once future", func(s *CommandSetSpec) {
			s.RunMode, s.Schedule = RunModeSchedule, &CommandSchedule{Kind: ScheduleOnce, At: "2026-11-12T20:00:00+09:00"}
		}, ""},
		{"once without offset", func(s *CommandSetSpec) {
			s.RunMode, s.Schedule = RunModeSchedule, &CommandSchedule{Kind: ScheduleOnce, At: "2026-12-01 03:00"}
		}, "schedule.at:"},
		{"weekly", func(s *CommandSetSpec) {
			s.RunMode, s.Schedule = RunModeSchedule, &CommandSchedule{Kind: ScheduleWeekly, Weekdays: []string{"mon", "fri"}, Time: "03:00"}
		}, ""},
		{"weekly no day", func(s *CommandSetSpec) {
			s.RunMode, s.Schedule = RunModeSchedule, &CommandSchedule{Kind: ScheduleWeekly, Time: "03:00"}
		}, "schedule.weekdays: at least one"},
		{"weekly bad day", func(s *CommandSetSpec) {
			s.RunMode, s.Schedule = RunModeSchedule, &CommandSchedule{Kind: ScheduleWeekly, Weekdays: []string{"monday"}, Time: "03:00"}
		}, `schedule.weekdays: "monday"`},
		{"daily 24:00", func(s *CommandSetSpec) {
			s.RunMode, s.Schedule = RunModeSchedule, &CommandSchedule{Kind: ScheduleDaily, Time: "24:00"}
		}, "schedule.time"},
		{"daily 3:00", func(s *CommandSetSpec) {
			s.RunMode, s.Schedule = RunModeSchedule, &CommandSchedule{Kind: ScheduleDaily, Time: "3:00"}
		}, "schedule.time"},
		{"every 6 hours", func(s *CommandSetSpec) {
			s.RunMode, s.Schedule = RunModeSchedule, &CommandSchedule{Kind: ScheduleEveryHours, Every: 6}
		}, ""},
		{"every 5 hours", func(s *CommandSetSpec) {
			s.RunMode, s.Schedule = RunModeSchedule, &CommandSchedule{Kind: ScheduleEveryHours, Every: 5}
		}, "schedule.every: must be one of 1, 2, 3, 4, 6, 8, 12"},
		{"every 7 minutes", func(s *CommandSetSpec) {
			s.RunMode, s.Schedule = RunModeSchedule, &CommandSchedule{Kind: ScheduleEveryMinutes, Every: 7}
		}, "schedule.every: must be one of 1, 2, 3, 4, 5, 6, 10, 12, 15, 20, 30"},
		{"every 30 minutes", func(s *CommandSetSpec) {
			s.RunMode, s.Schedule = RunModeSchedule, &CommandSchedule{Kind: ScheduleEveryMinutes, Every: 30}
		}, ""},
		{"minute 0", func(s *CommandSetSpec) {
			s.RunMode, s.Schedule = RunModeSchedule, &CommandSchedule{Kind: ScheduleHourlyAt, Minute: intPtr(0)}
		}, ""},
		{"minute missing", func(s *CommandSetSpec) {
			s.RunMode, s.Schedule = RunModeSchedule, &CommandSchedule{Kind: ScheduleHourlyAt}
		}, "schedule.minute: a minute from 0 to 59 is required"},
		{"minute 60", func(s *CommandSetSpec) {
			s.RunMode, s.Schedule = RunModeSchedule, &CommandSchedule{Kind: ScheduleHourlyAt, Minute: intPtr(60)}
		}, "schedule.minute: must be between 0 and 59"},
		{"kind", func(s *CommandSetSpec) {
			s.RunMode, s.Schedule = RunModeSchedule, &CommandSchedule{Kind: "monthly"}
		}, "schedule.kind"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			s := validSpec()
			tc.edit(s)
			if s.Schedule != nil {
				s.Schedule.normalize()
			}
			errs := s.Validate(now)
			if tc.want == "" {
				if len(errs) != 0 {
					t.Fatalf("unexpected problems: %v", errs)
				}
				return
			}
			for _, e := range errs {
				if strings.Contains(e, tc.want) {
					return
				}
			}
			t.Fatalf("want %q in %v", tc.want, errs)
		})
	}
	// The past check is skipped with now = 0 (the agent re-validates).
	s := validSpec()
	s.RunMode, s.Schedule = RunModeSchedule, &CommandSchedule{Kind: ScheduleOnce, At: "2020-01-01T00:00:00Z"}
	if errs := s.Validate(0); len(errs) != 0 {
		t.Fatalf("past once refused without now: %v", errs)
	}
}

func TestScheduleNormalize(t *testing.T) {
	s, err := ParseCommandSetSpec([]byte(`{"name":"x","purpose":"run_only","os_user":"a","commands":[{"command_line":"true"}],
		"schedule":{"kind":"weekly","weekdays":["FRI","mon","fri"],"time":"03:00","every":5,"at":"x"}}`))
	if err != nil {
		t.Fatal(err)
	}
	b, _ := json.Marshal(s.Schedule)
	if string(b) != `{"kind":"weekly","weekdays":["mon","fri"],"time":"03:00"}` {
		t.Fatalf("normalized schedule: %s", b)
	}
	s, _ = ParseCommandSetSpec([]byte(`{"name":"x","purpose":"run_only","os_user":"a","commands":[{"command_line":"true"}],
		"schedule":{"kind":"once","at":"2026-12-01T03:00:00+09:00"}}`))
	if s.Schedule.At != "2026-11-30T18:00:00Z" {
		t.Fatalf("once not stored in UTC: %s", s.Schedule.At)
	}
	s, _ = ParseCommandSetSpec([]byte(`{"name":"x","purpose":"run_only","os_user":"a","commands":[{"command_line":"true"}],
		"schedule":{"kind":"hourly_at","minute":0}}`))
	b, _ = json.Marshal(s.Schedule)
	if string(b) != `{"kind":"hourly_at","minute":0}` {
		t.Fatalf("minute 0 lost: %s", b)
	}
	tokyo, _ := time.LoadLocation("Asia/Tokyo")
	for in, want := range map[string]string{
		"2026-12-01 03:00":          "2026-11-30T18:00:00Z",
		"2026-12-01T03:00":          "2026-11-30T18:00:00Z",
		"2026-12-01 03:00:30":       "2026-11-30T18:00:30Z",
		"2026-12-01T03:00:00Z":      "2026-12-01T03:00:00Z",
		"2026-12-01T03:00:00-05:00": "2026-12-01T08:00:00Z",
	} {
		got, err := ParseScheduleAt(in, tokyo)
		if err != nil || FormatScheduleAt(got) != want {
			t.Errorf("%s: %v %v, want %s", in, FormatScheduleAt(got), err, want)
		}
	}
	if _, err := ParseScheduleAt("2026-12-01 03:00", nil); err == nil {
		t.Error("local form accepted without a location")
	}
}

func TestCodeSpecs(t *testing.T) {
	tests := []struct {
		spec CodeSpec
		in   []int
		out  []int
		str  string
	}{
		{CodeSpec{Kind: CodeSingle, A: 0}, []int{0}, []int{1, 255}, "0"},
		{CodeSpec{Kind: CodeRange, A: 1, B: 3}, []int{1, 2, 3}, []int{0, 4}, "1-3"},
		{CodeSpec{Kind: CodeMin, A: 1}, []int{1, 255}, []int{0}, ">= 1"},
		{CodeSpec{Kind: CodeMax, A: 125}, []int{0, 125}, []int{126, 255}, "<= 125"},
		{CodeSpec{Kind: "bogus"}, nil, []int{0}, "0"},
	}
	for _, tc := range tests {
		for _, c := range tc.in {
			if !tc.spec.Contains(c) {
				t.Errorf("%+v should contain %d", tc.spec, c)
			}
		}
		for _, c := range tc.out {
			if tc.spec.Contains(c) {
				t.Errorf("%+v should not contain %d", tc.spec, c)
			}
		}
		if tc.spec.String() != tc.str {
			t.Errorf("%+v: %q", tc.spec, tc.spec.String())
		}
	}
	got := ErrorCodes(CodeSpec{Kind: CodeSingle, A: 0}, CodeSpec{Kind: CodeRange, A: 1, B: 3})
	if len(got) != 1 || got[0] != [2]int{4, 255} {
		t.Fatalf("error codes: %v", got)
	}
	got = ErrorCodes(CodeSpec{Kind: CodeSingle, A: 5}, CodeSpec{Kind: CodeMax, A: 2})
	if len(got) != 2 || got[0] != [2]int{3, 4} || got[1] != [2]int{6, 255} {
		t.Fatalf("error codes: %v", got)
	}
	if got := ErrorCodes(DefaultSuccessCodes, DefaultFailureCodes); len(got) != 0 {
		t.Fatalf("defaults leave error codes: %v", got)
	}
}

func TestClassifyAndRunState(t *testing.T) {
	c := CommandSpec{Success: CodeSpec{Kind: CodeRange, A: 0, B: 1}, Failure: CodeSpec{Kind: CodeSingle, A: 2}}
	for code, want := range map[int]string{0: ResultSuccess, 1: ResultSuccess, 2: ResultFailure, 3: ResultError, 255: ResultError} {
		kind, reason := c.ClassifyExit(code)
		if kind != want || (kind == ResultError) != (reason == ReasonUnexpectedCode) {
			t.Errorf("code %d: %s %s", code, kind, reason)
		}
	}
	cont := []struct {
		mode              string
		success, fail, er bool
	}{
		{OnProblemsStop, true, false, false},
		{OnProblemsContinueOnFailure, true, true, false},
		{OnProblemsContinueAlways, true, true, true},
	}
	for _, tc := range cont {
		c := CommandSpec{OnProblems: tc.mode}
		if c.ContinuesAfter(ResultSuccess) != tc.success || c.ContinuesAfter(ResultFailure) != tc.fail || c.ContinuesAfter(ResultError) != tc.er {
			t.Errorf("%s: wrong continuation", tc.mode)
		}
	}
	states := []struct {
		kinds []string
		want  string
	}{
		{nil, ""},
		{[]string{ResultSuccess, ResultSuccess}, ResultSuccess},
		{[]string{ResultSuccess, ResultFailure, ResultSuccess}, ResultFailure},
		{[]string{ResultFailure, ResultError, ResultSuccess}, ResultError},
		{[]string{ResultError, ResultFailure}, ResultError},
	}
	for _, tc := range states {
		if got := RunState(tc.kinds); got != tc.want {
			t.Errorf("%v: %q, want %q", tc.kinds, got, tc.want)
		}
	}
}

func TestCommandSignature(t *testing.T) {
	a := CommandSignature("i1", map[int64]int64{1: 2, 3: 1})
	if a != CommandSignature("i1", map[int64]int64{3: 1, 1: 2}) {
		t.Fatal("signature depends on map order")
	}
	for _, other := range []string{
		CommandSignature("i2", map[int64]int64{1: 2, 3: 1}),
		CommandSignature("i1", map[int64]int64{1: 3, 3: 1}),
		CommandSignature("i1", map[int64]int64{1: 2}),
		CommandSignature("i1", nil),
	} {
		if other == a {
			t.Fatal("different definitions give the same signature")
		}
	}
	if CommandSignature("i1", nil) == "" {
		t.Fatal("empty signature")
	}
}
