package modules

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"path"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/dlclark/regexp2"
)

// Command sets: commands an agent runs on its host as a chosen OS user,
// manually on request or on a schedule. The types below are the one
// definition of the JSON shapes used by the API, the CLI definition file and
// the agent protocol.

// Purposes of a command set.
const (
	PurposeCollectOutput = "collect_output"
	PurposeRunOnly       = "run_only"
)

// Run modes of a command set.
const (
	RunModeManual   = "manual"
	RunModeSchedule = "schedule"
)

// Joiners of the values of one group name in the display value.
const (
	JoinerComma   = "comma"
	JoinerNewline = "newline"
)

// "On problems" modes of a command: what the set does after the command.
const (
	OnProblemsStop              = "stop"                // stop after a failure or an error
	OnProblemsContinueOnFailure = "continue_on_failure" // go on after a failure, stop after an error
	OnProblemsContinueAlways    = "continue_always"     // go on in any case
)

// Kinds of a code spec.
const (
	CodeSingle = "single" // exactly A
	CodeRange  = "range"  // A to B (A < B)
	CodeMin    = "min"    // A or more
	CodeMax    = "max"    // A or less
)

// Result kinds of a command (and states of a finished run).
const (
	ResultSuccess = "success"
	ResultFailure = "failure"
	ResultError   = "error"
)

// Reasons of an error result.
const (
	ReasonUnexpectedCode = "unexpected_code" // the exit code is in neither spec
	ReasonStartFailed    = "start_failed"    // the switch to the OS user or the shell failed
	ReasonWorkdirFailed  = "workdir_failed"  // the working directory could not be entered
	ReasonTimeout        = "timeout"         // the time limit passed
	ReasonSignaled       = "signaled"        // a signal ended the command
	ReasonInterrupted    = "interrupted"     // the agent was stopped (or found the run unfinished at start)
)

// CodeSpec selects exit codes: single (A), range (A to B), min (A or more) or
// max (A or less). Values are 0 to 255. B is used only by range.
type CodeSpec struct {
	Kind string `json:"kind"`
	A    int    `json:"a"`
	B    int    `json:"b,omitempty"`
}

// DefaultSuccessCodes and DefaultFailureCodes are the specs used when a
// command does not give one: success is exactly 0, failure is 1 or more.
var (
	DefaultSuccessCodes = CodeSpec{Kind: CodeSingle, A: 0}
	DefaultFailureCodes = CodeSpec{Kind: CodeMin, A: 1}
)

// Contains reports whether code is selected by the spec.
func (c CodeSpec) Contains(code int) bool {
	switch c.Kind {
	case CodeSingle:
		return code == c.A
	case CodeRange:
		return code >= c.A && code <= c.B
	case CodeMin:
		return code >= c.A
	case CodeMax:
		return code <= c.A
	}
	return false
}

// bounds returns the lowest and highest selected code within 0 to 255.
func (c CodeSpec) bounds() (int, int) {
	switch c.Kind {
	case CodeRange:
		return c.A, c.B
	case CodeMin:
		return c.A, MaxCommandExitCode
	case CodeMax:
		return 0, c.A
	}
	return c.A, c.A
}

// Overlaps reports whether two specs select a common exit code.
func (c CodeSpec) Overlaps(o CodeSpec) bool {
	lo1, hi1 := c.bounds()
	lo2, hi2 := o.bounds()
	return lo1 <= hi2 && lo2 <= hi1
}

// String renders the spec for the CLI ("0", "1-3", ">= 1", "<= 125").
func (c CodeSpec) String() string {
	switch c.Kind {
	case CodeRange:
		return fmt.Sprintf("%d-%d", c.A, c.B)
	case CodeMin:
		return fmt.Sprintf(">= %d", c.A)
	case CodeMax:
		return fmt.Sprintf("<= %d", c.A)
	}
	return strconv.Itoa(c.A)
}

func (c CodeSpec) validate(field string) []Problem {
	var errs []Problem
	switch c.Kind {
	case CodeSingle, CodeRange, CodeMin, CodeMax:
	default:
		return []Problem{FieldProblem(field+".kind", M("command.spec.code_kind"))}
	}
	if c.A < 0 || c.A > MaxCommandExitCode {
		errs = append(errs, FieldProblem(field+".a", M("command.spec.code_range", "max", MaxCommandExitCode)))
	}
	if c.Kind == CodeRange {
		if c.B < 0 || c.B > MaxCommandExitCode {
			errs = append(errs, FieldProblem(field+".b", M("command.spec.code_range", "max", MaxCommandExitCode)))
		} else if c.B <= c.A {
			errs = append(errs, FieldProblem(field+".b", M("command.spec.code_b_greater")))
		}
	} else if c.B != 0 {
		errs = append(errs, FieldProblem(field+".b", M("command.spec.code_b_unused")))
	}
	return errs
}

// ErrorCodes returns the exit codes (0 to 255) that are in neither spec and
// therefore give an error, as ranges [from, to].
func ErrorCodes(success, failure CodeSpec) [][2]int {
	var out [][2]int
	for code := 0; code <= MaxCommandExitCode; code++ {
		if success.Contains(code) || failure.Contains(code) {
			continue
		}
		if n := len(out); n > 0 && out[n-1][1] == code-1 {
			out[n-1][1] = code
			continue
		}
		out = append(out, [2]int{code, code})
	}
	return out
}

// CommandSchedule is the schedule of a set (null for a manual set). Kinds and
// their fields:
//
//	once           at: an absolute time (RFC 3339 with an offset; stored in UTC)
//	weekly         weekdays (sun, mon, tue, wed, thu, fri, sat) and time ("HH:MM")
//	daily          time ("HH:MM")
//	every_hours    every: 1, 2, 3, 4, 6, 8 or 12 (from 0:00)
//	every_minutes  every: 1, 2, 3, 4, 5, 6, 10, 12, 15, 20 or 30 (from minute 0)
//	hourly_at      minute: 0 to 59
//
// Recurring kinds are interpreted in the local time of the monitored host.
type CommandSchedule struct {
	Kind     string   `json:"kind"`
	At       string   `json:"at,omitempty"`
	Weekdays []string `json:"weekdays,omitempty"`
	Time     string   `json:"time,omitempty"`
	Every    int      `json:"every,omitempty"`
	Minute   *int     `json:"minute,omitempty"`
}

// Schedule kinds.
const (
	ScheduleOnce         = "once"
	ScheduleWeekly       = "weekly"
	ScheduleDaily        = "daily"
	ScheduleEveryHours   = "every_hours"
	ScheduleEveryMinutes = "every_minutes"
	ScheduleHourlyAt     = "hourly_at"
)

// ScheduleEveryHoursChoices and ScheduleEveryMinutesChoices are the allowed
// n of every_hours and every_minutes (divisors of 24 and of 60, so that the
// interval stays the same across midnight and across the hour).
var (
	ScheduleEveryHoursChoices   = []int{1, 2, 3, 4, 6, 8, 12}
	ScheduleEveryMinutesChoices = []int{1, 2, 3, 4, 5, 6, 10, 12, 15, 20, 30}
)

// WeekdayNames are the weekday names of a weekly schedule, Sunday first
// (index = time.Weekday).
var WeekdayNames = []string{"sun", "mon", "tue", "wed", "thu", "fri", "sat"}

// scheduleAtLayout is the canonical form of a once time (UTC).
const scheduleAtLayout = "2006-01-02T15:04:05Z"

// ParseScheduleAt parses the time of a once schedule. RFC 3339 with an
// offset is always accepted. When loc is not nil, a local form without an
// offset ("2006-01-02 15:04", "2006-01-02T15:04", optionally with seconds)
// is also accepted and interpreted in loc (the CLI passes its local time).
func ParseScheduleAt(s string, loc *time.Location) (time.Time, error) {
	s = strings.TrimSpace(s)
	if t, err := time.Parse(time.RFC3339, s); err == nil {
		return t, nil
	}
	if loc != nil {
		for _, layout := range []string{"2006-01-02 15:04", "2006-01-02T15:04", "2006-01-02 15:04:05", "2006-01-02T15:04:05"} {
			if t, err := time.ParseInLocation(layout, s, loc); err == nil {
				return t, nil
			}
		}
		return time.Time{}, M("command.spec.at_format_local", "value", strconv.Quote(s))
	}
	return time.Time{}, M("command.spec.at_format_offset", "value", strconv.Quote(s))
}

// FormatScheduleAt returns the canonical form of a once time.
func FormatScheduleAt(t time.Time) string {
	return t.UTC().Format(scheduleAtLayout)
}

// parseTimeOfDay parses "HH:MM" (00:00 to 23:59) into minutes after midnight.
func parseTimeOfDay(s string) (int, bool) {
	h, m, ok := strings.Cut(s, ":")
	if !ok || len(h) != 2 || len(m) != 2 {
		return 0, false
	}
	hh, err1 := strconv.Atoi(h)
	mm, err2 := strconv.Atoi(m)
	if err1 != nil || err2 != nil || hh < 0 || hh > 23 || mm < 0 || mm > 59 {
		return 0, false
	}
	return hh*60 + mm, true
}

// FormatTimeOfDay renders minutes after midnight as "HH:MM".
func FormatTimeOfDay(minutes int) string {
	return fmt.Sprintf("%02d:%02d", minutes/60, minutes%60)
}

// TimeOfDay returns the time of a weekly or daily schedule in minutes after
// midnight (0 when it is not valid).
func (s *CommandSchedule) TimeOfDay() int {
	m, _ := parseTimeOfDay(s.Time)
	return m
}

// AtTime returns the time of a once schedule (zero when it is not valid).
func (s *CommandSchedule) AtTime() time.Time {
	t, err := ParseScheduleAt(s.At, nil)
	if err != nil {
		return time.Time{}
	}
	return t
}

// WeekdayMask returns the weekdays as a bit mask (bit 0 = Sunday).
func (s *CommandSchedule) WeekdayMask() int {
	mask := 0
	for _, w := range s.Weekdays {
		for i, n := range WeekdayNames {
			if w == n {
				mask |= 1 << i
			}
		}
	}
	return mask
}

// WeekdaysFromMask returns the weekday names of a bit mask (bit 0 = Sunday).
func WeekdaysFromMask(mask int) []string {
	var out []string
	for i, n := range WeekdayNames {
		if mask&(1<<i) != 0 {
			out = append(out, n)
		}
	}
	return out
}

// MinuteValue returns the minute of an hourly_at schedule (0 when unset).
func (s *CommandSchedule) MinuteValue() int {
	if s.Minute == nil {
		return 0
	}
	return *s.Minute
}

// normalize clears the fields the kind does not use and puts weekdays in
// canonical order.
func (s *CommandSchedule) normalize() {
	s.Kind = strings.TrimSpace(s.Kind)
	s.Time = strings.TrimSpace(s.Time)
	if s.Kind == ScheduleOnce {
		if t, err := ParseScheduleAt(s.At, nil); err == nil {
			s.At = FormatScheduleAt(t)
		}
	}
	if s.Kind == ScheduleWeekly {
		known := true
		for i, w := range s.Weekdays {
			s.Weekdays[i] = strings.ToLower(strings.TrimSpace(w))
			if !contains(WeekdayNames, s.Weekdays[i]) {
				known = false
			}
		}
		if known {
			s.Weekdays = WeekdaysFromMask(s.WeekdayMask())
		}
	}
	if s.Kind != ScheduleOnce {
		s.At = ""
	}
	if s.Kind != ScheduleWeekly {
		s.Weekdays = nil
	}
	if s.Kind != ScheduleWeekly && s.Kind != ScheduleDaily {
		s.Time = ""
	}
	if s.Kind != ScheduleEveryHours && s.Kind != ScheduleEveryMinutes {
		s.Every = 0
	}
	if s.Kind != ScheduleHourlyAt {
		s.Minute = nil
	}
}

func contains[T comparable](list []T, v T) bool {
	for _, x := range list {
		if x == v {
			return true
		}
	}
	return false
}

// validate checks the schedule. now (Unix seconds) is used to refuse a once
// time in the past; 0 skips that check.
func (s *CommandSchedule) validate(now int64) []Problem {
	var errs []Problem
	add := func(field string, m Msg) { errs = append(errs, FieldProblem(field, m)) }
	switch s.Kind {
	case ScheduleOnce:
		t, err := ParseScheduleAt(s.At, nil)
		switch {
		case s.At == "":
			add("schedule.at", M("command.spec.at_required"))
		case err != nil:
			add("schedule.at", AsMsg(err))
		case now > 0 && t.Unix() <= now:
			add("schedule.at", M("command.spec.at_past"))
		}
	case ScheduleWeekly:
		if len(s.Weekdays) == 0 {
			add("schedule.weekdays", M("command.spec.weekdays_required"))
		}
		for _, w := range s.Weekdays {
			if !contains(WeekdayNames, w) {
				add("schedule.weekdays", M("command.spec.weekday_invalid", "value", strconv.Quote(w), "choices", WeekdayNames))
			}
		}
		if _, ok := parseTimeOfDay(s.Time); !ok {
			add("schedule.time", M("command.spec.time_of_day"))
		}
	case ScheduleDaily:
		if _, ok := parseTimeOfDay(s.Time); !ok {
			add("schedule.time", M("command.spec.time_of_day"))
		}
	case ScheduleEveryHours:
		if !contains(ScheduleEveryHoursChoices, s.Every) {
			add("schedule.every", M("command.spec.every_choices", "choices", ScheduleEveryHoursChoices))
		}
	case ScheduleEveryMinutes:
		if !contains(ScheduleEveryMinutesChoices, s.Every) {
			add("schedule.every", M("command.spec.every_choices", "choices", ScheduleEveryMinutesChoices))
		}
	case ScheduleHourlyAt:
		if s.Minute == nil {
			add("schedule.minute", M("command.spec.minute_required"))
		} else if *s.Minute < 0 || *s.Minute > 59 {
			add("schedule.minute", M("command.spec.minute_range"))
		}
	default:
		add("schedule.kind", M("command.spec.schedule_kind"))
	}
	return errs
}

// CommandSpec is one command of a set. Commands have no name and no
// identity; their position in the set orders them.
type CommandSpec struct {
	// CommandLine is passed to bash -c exactly as written.
	CommandLine string   `json:"command_line"`
	Note        string   `json:"note"`
	Success     CodeSpec `json:"success"`
	Failure     CodeSpec `json:"failure"`
	OnProblems  string   `json:"on_problems"`
	TimeoutSec  int      `json:"timeout_sec"`
	// LineLimit and Patterns are used only by collect_output sets.
	LineLimit int      `json:"line_limit"`
	Patterns  []string `json:"patterns"`
}

// CommandSetSpec is the editable definition of a command set.
type CommandSetSpec struct {
	Name        string `json:"name"`
	Description string `json:"description"`
	Purpose     string `json:"purpose"`
	OSUser      string `json:"os_user"`
	// WorkDir is the directory every command starts in; empty means the
	// home directory of the OS user.
	WorkDir  string           `json:"work_dir"`
	RunMode  string           `json:"run_mode"`
	Schedule *CommandSchedule `json:"schedule"`
	KeepRuns int              `json:"keep_runs"`
	Joiner   string           `json:"joiner"`
	Commands []CommandSpec    `json:"commands"`
}

// CommandSetDef is a stored set: its ID and revision in the target database
// with the definition. Sync delivers these to the agent.
type CommandSetDef struct {
	ID       int64 `json:"id"`
	Revision int64 `json:"revision"`
	CommandSetSpec
}

// ParseCommandSetSpec decodes a definition strictly (unknown fields are
// rejected), then fills defaults and normalizes it. It does not validate.
func ParseCommandSetSpec(data []byte) (*CommandSetSpec, error) {
	if !IsJSONObject(data) {
		return nil, M("command.spec.not_object")
	}
	var s CommandSetSpec
	dec := json.NewDecoder(strings.NewReader(string(data)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&s); err != nil {
		var te *json.UnmarshalTypeError
		if errors.As(err, &te) {
			var field any = te.Field
			if te.Field == "" {
				field = M("json.kind.the_definition")
			}
			return nil, M("command.spec.wrong_type", "field", field, "expected", jsonKindName(te.Type), "found", te.Value)
		}
		return nil, M("command.spec.decode", "detail", strings.TrimPrefix(err.Error(), "json: "))
	}
	if dec.More() {
		return nil, M("command.spec.one_object")
	}
	s.Normalize()
	return &s, nil
}

// Normalize fills defaults (keep count, joiner, run mode, code specs, "on
// problems", time limit, line limit) and makes the form stable. Command
// lines are never changed.
func (s *CommandSetSpec) Normalize() {
	s.Name = strings.TrimSpace(s.Name)
	s.OSUser = strings.TrimSpace(s.OSUser)
	s.Purpose = strings.TrimSpace(s.Purpose)
	if s.KeepRuns == 0 {
		s.KeepRuns = DefaultCommandKeepRuns
	}
	if s.Joiner == "" {
		s.Joiner = JoinerComma
	}
	if s.RunMode == "" {
		s.RunMode = RunModeManual
		if s.Schedule != nil {
			s.RunMode = RunModeSchedule
		}
	}
	if s.Schedule != nil {
		s.Schedule.normalize()
	}
	if s.Commands == nil {
		s.Commands = []CommandSpec{}
	}
	for i := range s.Commands {
		c := &s.Commands[i]
		if c.Success.Kind == "" {
			c.Success = DefaultSuccessCodes
		}
		if c.Failure.Kind == "" {
			c.Failure = DefaultFailureCodes
		}
		if c.OnProblems == "" {
			c.OnProblems = OnProblemsStop
		}
		if c.TimeoutSec == 0 {
			c.TimeoutSec = DefaultCommandSetTimeoutSec
		}
		if c.LineLimit == 0 {
			c.LineLimit = DefaultCommandLineLimit
		}
		if c.Patterns == nil {
			c.Patterns = []string{}
		}
	}
}

// Validate returns every problem of the definition (empty when valid), each
// naming the field and, for a command, its position (counted from 1). now
// (Unix seconds) refuses a once time in the past; 0 skips that check (the
// agent re-validates delivered definitions whose once time may have passed).
func (s *CommandSetSpec) Validate(now int64) []string {
	return ProblemTexts(s.Problems(now))
}

// Problems returns every problem of the definition as coded messages, in
// the order and with the English texts of Validate.
func (s *CommandSetSpec) Problems(now int64) []Problem {
	var errs []Problem
	add := func(field string, m Msg) { errs = append(errs, FieldProblem(field, m)) }

	switch n := utf8.RuneCountInString(s.Name); {
	case n == 0:
		add("name", M("command.spec.required"))
	case n > MaxCommandSetNameLength:
		add("name", M("command.spec.max_chars", "max", MaxCommandSetNameLength))
	}
	if hasControl(s.Name) {
		add("name", M("command.spec.control"))
	}
	if err := checkFreeText(s.Description); err != nil {
		add("description", AsMsg(err))
	}
	if s.Purpose != PurposeCollectOutput && s.Purpose != PurposeRunOnly {
		add("purpose", M("command.spec.purpose"))
	}
	if err := ValidateCommandOSUser(s.OSUser); err != nil {
		add("os_user", AsMsg(err))
	}
	if s.WorkDir != "" {
		if err := validateWorkDir(s.WorkDir); err != nil {
			add("work_dir", AsMsg(err))
		}
	}
	switch s.RunMode {
	case RunModeManual:
		if s.Schedule != nil {
			add("schedule", M("command.spec.schedule_null"))
		}
	case RunModeSchedule:
		if s.Schedule == nil {
			add("schedule", M("command.spec.schedule_required"))
		} else {
			errs = append(errs, s.Schedule.validate(now)...)
		}
	default:
		add("run_mode", M("command.spec.run_mode"))
	}
	if s.KeepRuns < 1 || s.KeepRuns > MaxCommandKeepRuns {
		add("keep_runs", M("command.spec.range_1", "max", MaxCommandKeepRuns))
	}
	if s.Joiner != JoinerComma && s.Joiner != JoinerNewline {
		add("joiner", M("command.spec.joiner"))
	}
	if len(s.Commands) == 0 {
		add("commands", M("command.spec.commands_required"))
	}
	for i, c := range s.Commands {
		at := M("loc.command", "n", i+1)
		for _, e := range c.validate(s.Purpose) {
			e.Where = append([]Msg{at}, e.Where...)
			errs = append(errs, e)
		}
	}
	return errs
}

func (c *CommandSpec) validate(purpose string) []Problem {
	var errs []Problem
	add := func(field string, m Msg) { errs = append(errs, FieldProblem(field, m)) }
	switch {
	case strings.TrimSpace(c.CommandLine) == "":
		add("command_line", M("command.spec.required"))
	case strings.IndexByte(c.CommandLine, 0) >= 0:
		add("command_line", M("command.spec.command_line_nul"))
	case len(c.CommandLine) > MaxCommandLineBytes:
		add("command_line", M("command.spec.max_bytes", "max", MaxCommandLineBytes))
	}
	if err := checkFreeText(c.Note); err != nil {
		add("note", AsMsg(err))
	}
	se := c.Success.validate("success")
	fe := c.Failure.validate("failure")
	errs = append(errs, se...)
	errs = append(errs, fe...)
	if len(se) == 0 && len(fe) == 0 && c.Success.Overlaps(c.Failure) {
		add("success, failure", M("command.spec.codes_overlap", "success", c.Success.String(), "failure", c.Failure.String()))
	}
	switch c.OnProblems {
	case OnProblemsStop, OnProblemsContinueOnFailure, OnProblemsContinueAlways:
	default:
		add("on_problems", M("command.spec.on_problems"))
	}
	if c.TimeoutSec < 1 || c.TimeoutSec > MaxCommandSetTimeoutSec {
		add("timeout_sec", M("command.spec.range_1", "max", MaxCommandSetTimeoutSec))
	}
	if c.LineLimit < 1 || c.LineLimit > MaxCommandLineLimit {
		add("line_limit", M("command.spec.range_1", "max", MaxCommandLineLimit))
	}
	if purpose == PurposeRunOnly && len(c.Patterns) > 0 {
		add("patterns", M("command.spec.patterns_run_only"))
	}
	for j, p := range c.Patterns {
		if _, err := CompileCommandPattern(p); err != nil {
			errs = append(errs, Problem{Where: []Msg{M("loc.pattern", "n", j+1)}, Msg: AsMsg(err)})
		}
	}
	return errs
}

// CompileCommandPattern compiles a value pattern (RE2 syntax compatibility
// and multi-line mode: "^" and "$" match at line breaks, "." does not match
// a line break unless "(?s)" is written). Capturing groups are numbered as
// in Python and the editors (see cmdpattern.go). A pattern must have at
// least one named group: only named groups give values.
func CompileCommandPattern(p string) (*regexp2.Regexp, error) {
	cp, err := compileCommandPattern(p)
	if err != nil {
		return nil, err
	}
	return cp.re, nil
}

// ValidateCommandOSUser checks the OS user of a set: 1 to 256 characters, no
// white space or control characters, not starting with "-".
func ValidateCommandOSUser(u string) error {
	switch n := utf8.RuneCountInString(u); {
	case n == 0:
		return M("command.spec.required")
	case n > MaxCommandOSUserLength:
		return M("command.spec.max_chars", "max", MaxCommandOSUserLength)
	}
	for _, r := range u {
		if unicode.IsSpace(r) || unicode.IsControl(r) {
			return M("command.spec.os_user_space")
		}
	}
	if strings.HasPrefix(u, "-") {
		return M("command.spec.os_user_dash")
	}
	return nil
}

// validateWorkDir checks a working directory: a clean absolute path without
// NUL or line breaks, at most 4096 bytes.
func validateWorkDir(p string) error {
	if !strings.HasPrefix(p, "/") {
		return M("command.spec.work_dir_absolute")
	}
	if strings.ContainsAny(p, "\x00\n\r") {
		return M("command.spec.work_dir_nul")
	}
	if path.Clean(p) != p {
		return M("command.spec.work_dir_clean")
	}
	if len(p) > MaxCommandWorkDirBytes {
		return M("command.spec.max_bytes", "max", MaxCommandWorkDirBytes)
	}
	return nil
}

func hasControl(s string) bool {
	for _, r := range s {
		if unicode.IsControl(r) {
			return true
		}
	}
	return false
}

// checkFreeText checks a description or note: at most 1000 characters,
// control characters only as line breaks and tabs. It returns nil when the
// text is valid.
func checkFreeText(s string) error {
	if utf8.RuneCountInString(s) > MaxCommandTextLength {
		return M("command.spec.max_chars", "max", MaxCommandTextLength)
	}
	for _, r := range s {
		if unicode.IsControl(r) && r != '\n' && r != '\r' && r != '\t' {
			return M("command.spec.text_control")
		}
	}
	return nil
}

// ClassifyExit returns the result kind (and reason for an error) of a
// command that ran and exited normally with code.
func (c *CommandSpec) ClassifyExit(code int) (kind, reason string) {
	switch {
	case c.Success.Contains(code):
		return ResultSuccess, ""
	case c.Failure.Contains(code):
		return ResultFailure, ""
	}
	return ResultError, ReasonUnexpectedCode
}

// ContinuesAfter reports whether the set goes on after the command ended
// with the result kind.
func (c *CommandSpec) ContinuesAfter(kind string) bool {
	switch kind {
	case ResultSuccess:
		return true
	case ResultFailure:
		return c.OnProblems == OnProblemsContinueOnFailure || c.OnProblems == OnProblemsContinueAlways
	}
	return c.OnProblems == OnProblemsContinueAlways
}

// resultRank orders result kinds from best to worst.
func resultRank(kind string) int {
	switch kind {
	case ResultSuccess:
		return 0
	case ResultFailure:
		return 1
	}
	return 2
}

// WorseResult returns the worse of two result kinds (error over failure over
// success). An empty kind counts as nothing.
func WorseResult(a, b string) string {
	if a == "" {
		return b
	}
	if b == "" {
		return a
	}
	if resultRank(b) > resultRank(a) {
		return b
	}
	return a
}

// RunState returns the state of a run from the result kinds of its executed
// commands: the worst of them ("" when there is none).
func RunState(kinds []string) string {
	state := ""
	for _, k := range kinds {
		state = WorseResult(state, k)
	}
	return state
}

// CommandSignature returns the signature of a target's command set
// definitions: a hash of the random value created with the target database
// (so that a recreated database is noticed) and the ID and revision of every
// set that is not deleted.
func CommandSignature(instance string, revisions map[int64]int64) string {
	ids := make([]int64, 0, len(revisions))
	for id := range revisions {
		ids = append(ids, id)
	}
	slices.Sort(ids)
	h := sha256.New()
	h.Write([]byte(instance))
	for _, id := range ids {
		fmt.Fprintf(h, "\n%d:%d", id, revisions[id])
	}
	return hex.EncodeToString(h.Sum(nil))
}
