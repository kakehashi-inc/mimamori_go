package modules

import (
	"strings"
	"time"
	"unicode/utf8"
)

// Display values of output-collecting command sets. The agent extracts the
// values of each command when it ends, with the patterns of the definition
// it runs, and reports them; the central stores them as they are.

// ExtractedValue is one value: what a named group captured (Name), or the
// console output of a command without patterns (Name is empty).
type ExtractedValue struct {
	Name  string `json:"name"`
	Value string `json:"value"`
}

// Problems of a pattern that therefore contributed nothing.
const (
	PatternProblemTimeout = "timeout" // the match time limit passed
	PatternProblemInvalid = "invalid" // the pattern could not be compiled
)

// PatternProblem names a pattern (position from 0) that contributed nothing.
type PatternProblem struct {
	Pattern int    `json:"pattern"`
	Problem string `json:"problem"`
}

// CommandValues are the values of one command, in order (patterns in
// definition order, matches in output order, groups in pattern order), after
// the command's line limit. Truncated is set when the line limit dropped
// something.
type CommandValues struct {
	Values          []ExtractedValue `json:"values"`
	Truncated       bool             `json:"truncated"`
	PatternProblems []PatternProblem `json:"pattern_problems,omitempty"`
}

// DisplayRow is one row of the display value of a run: a group name (empty
// for console output used without patterns), its values in order of
// appearance and the values joined with the set's joiner.
type DisplayRow struct {
	Name   string   `json:"name"`
	Values []string `json:"values"`
	Value  string   `json:"value"`
}

// DisplayValue is the display value of a run: one row per group name in
// order of first appearance. Truncated is set when a command's line limit
// dropped something.
type DisplayValue struct {
	Rows      []DisplayRow `json:"rows"`
	Truncated bool         `json:"truncated"`
}

// DisplayText returns console output as text for display and pattern
// matching: invalid UTF-8 is replaced with U+FFFD (the stored bytes are kept
// as they are).
func DisplayText(b []byte) string {
	return strings.ToValidUTF8(string(b), "\uFFFD")
}

// ExtractCommandValues applies the patterns of a command to its whole
// console output and cuts the result to lineLimit lines (and to
// MaxCommandValueBytes bytes). Without patterns the console output is the
// value.
func ExtractCommandValues(patterns []string, output []byte, lineLimit int) CommandValues {
	return extractCommandValues(patterns, output, lineLimit, CommandPatternTimeout)
}

func extractCommandValues(patterns []string, output []byte, lineLimit int, timeout time.Duration) CommandValues {
	text := DisplayText(output)
	cv := CommandValues{Values: []ExtractedValue{}}
	lim := newValueLimiter(lineLimit, MaxCommandValueBytes)
	if len(patterns) == 0 {
		v := strings.TrimSuffix(text, "\n")
		v = strings.TrimSuffix(v, "\r")
		if v != "" {
			lim.add(&cv.Values, ExtractedValue{Value: v})
		}
	}
	for i, p := range patterns {
		if lim.full {
			// Everything after the limits is dropped anyway: the remaining
			// patterns are not applied.
			break
		}
		problem := matchPattern(p, text, timeout, func(v ExtractedValue) bool { return lim.add(&cv.Values, v) })
		if problem != "" {
			cv.PatternProblems = append(cv.PatternProblems, PatternProblem{Pattern: i, Problem: problem})
		}
	}
	cv.Truncated = lim.full
	return cv
}

// matchPattern passes the values of every match of pattern p in text, in
// order, to add until add returns false. The whole search of one pattern is
// bounded by timeout; a pattern that passes it or cannot be compiled gives
// no value, so the values are collected first and passed at the end. The
// collection stops once it holds more than any limit lets through
// (MaxCommandValueBytes, MaxCommandLineLimit lines), which bounds the
// memory of patterns that match very often or capture much.
func matchPattern(p, text string, timeout time.Duration, add func(ExtractedValue) bool) string {
	re, err := CompileCommandPattern(p)
	if err != nil {
		return PatternProblemInvalid
	}
	deadline := time.Now().Add(timeout)
	var out []ExtractedValue
	size, lines := 0, 0
	re.MatchTimeout = timeout
	m, err := re.FindStringMatch(text)
collect:
	for err == nil && m != nil {
		for _, g := range m.Groups() {
			if isGroupNumber(g.Name) || len(g.Captures) == 0 || g.Length == 0 {
				continue
			}
			v := g.String()
			out = append(out, ExtractedValue{Name: g.Name, Value: v})
			size += len(v)
			lines += strings.Count(v, "\n") + 1
			if size > MaxCommandValueBytes || lines > MaxCommandLineLimit {
				break collect
			}
		}
		remaining := time.Until(deadline)
		if remaining <= 0 {
			return PatternProblemTimeout
		}
		re.MatchTimeout = remaining
		m, err = re.FindNextMatch(m)
	}
	if err != nil {
		return PatternProblemTimeout
	}
	for _, v := range out {
		if !add(v) {
			break
		}
	}
	return ""
}

// valueLimiter keeps the values of a command within its line limit and a
// byte limit: a line break inside a value counts as a line; a value that
// does not fit completely is cut to what is left and everything after it
// is dropped (full is then set).
type valueLimiter struct {
	lines, bytes int
	full         bool
}

func newValueLimiter(lineLimit, byteLimit int) *valueLimiter {
	return &valueLimiter{lines: max(lineLimit, 1), bytes: byteLimit}
}

// add appends v to list within the limits and reports whether more values
// may follow.
func (l *valueLimiter) add(list *[]ExtractedValue, v ExtractedValue) bool {
	if l.full {
		return false
	}
	n := strings.Count(v.Value, "\n") + 1
	if n <= l.lines && len(v.Value) <= l.bytes {
		*list = append(*list, v)
		l.lines -= n
		l.bytes -= len(v.Value)
		return true
	}
	l.full = true
	cut := v.Value
	if n > l.lines {
		cut = firstLines(cut, l.lines)
	}
	cut = cutUTF8(cut, l.bytes)
	if l.lines > 0 && cut != "" {
		*list = append(*list, ExtractedValue{Name: v.Name, Value: cut})
	}
	return false
}

// cutUTF8 returns at most n bytes of s, not splitting a character.
func cutUTF8(s string, n int) string {
	if len(s) <= n {
		return s
	}
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return s[:n]
}

// firstLines returns the first n lines of s (without the line break after
// the last one).
func firstLines(s string, n int) string {
	if n <= 0 {
		return ""
	}
	idx := 0
	for i := 0; i < n; i++ {
		j := strings.IndexByte(s[idx:], '\n')
		if j < 0 {
			return s
		}
		idx += j + 1
	}
	return s[:idx-1]
}

// JoinerSeparator returns the separator of a joiner.
func JoinerSeparator(joiner string) string {
	if joiner == JoinerNewline {
		return "\n"
	}
	return ","
}

// BuildDisplayValue groups the values of a run's commands (in command order)
// by group name, in order of first appearance, and joins the values of each
// name with the joiner. The values of a run are kept within
// MaxDisplayValueBytes; what exceeds is dropped and marked truncated.
func BuildDisplayValue(commands []CommandValues, joiner string) DisplayValue {
	dv := DisplayValue{Rows: []DisplayRow{}}
	index := map[string]int{}
	left := MaxDisplayValueBytes
	for _, c := range commands {
		if c.Truncated {
			dv.Truncated = true
		}
		for _, v := range c.Values {
			if left <= 0 {
				dv.Truncated = true
				break
			}
			if len(v.Value) > left {
				// The values of a run are bounded too; what exceeds is
				// dropped.
				dv.Truncated = true
				v.Value, left = cutUTF8(v.Value, left), 0
				if v.Value == "" {
					break
				}
			} else {
				left -= len(v.Value)
			}
			i, ok := index[v.Name]
			if !ok {
				i = len(dv.Rows)
				index[v.Name] = i
				dv.Rows = append(dv.Rows, DisplayRow{Name: v.Name})
			}
			dv.Rows[i].Values = append(dv.Rows[i].Values, v.Value)
		}
	}
	sep := JoinerSeparator(joiner)
	for i := range dv.Rows {
		dv.Rows[i].Value = strings.Join(dv.Rows[i].Values, sep)
	}
	return dv
}

// RenderDisplayValue renders a display value as plain text for the CLI: one
// "name: value" line per row (a multi-line value goes on indented lines
// below its name; output used without patterns is named "(output)").
func RenderDisplayValue(dv DisplayValue) string {
	if len(dv.Rows) == 0 {
		s := "(no values)\n"
		if dv.Truncated {
			s += "(truncated by the line or size limit)\n"
		}
		return s
	}
	var b strings.Builder
	for _, r := range dv.Rows {
		name := r.Name
		if name == "" {
			name = "(output)"
		}
		if !strings.Contains(r.Value, "\n") {
			b.WriteString(name + ": " + r.Value + "\n")
			continue
		}
		b.WriteString(name + ":\n")
		for _, l := range strings.Split(r.Value, "\n") {
			b.WriteString("  " + l + "\n")
		}
	}
	if dv.Truncated {
		b.WriteString("(truncated by the line or size limit)\n")
	}
	return b.String()
}
