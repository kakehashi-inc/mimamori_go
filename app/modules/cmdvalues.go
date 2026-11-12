package modules

import (
	"strings"
	"time"
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
	return strings.ToValidUTF8(string(b), "�")
}

// ExtractCommandValues applies the patterns of a command to its whole
// console output and cuts the result to lineLimit lines. Without patterns
// the console output is the value.
func ExtractCommandValues(patterns []string, output []byte, lineLimit int) CommandValues {
	return extractCommandValues(patterns, output, lineLimit, CommandPatternTimeout)
}

func extractCommandValues(patterns []string, output []byte, lineLimit int, timeout time.Duration) CommandValues {
	text := DisplayText(output)
	cv := CommandValues{Values: []ExtractedValue{}}
	if len(patterns) == 0 {
		v := strings.TrimSuffix(text, "\n")
		v = strings.TrimSuffix(v, "\r")
		if v != "" {
			cv.Values = append(cv.Values, ExtractedValue{Value: v})
		}
	}
	for i, p := range patterns {
		vals, problem := matchPattern(p, text, timeout)
		if problem != "" {
			cv.PatternProblems = append(cv.PatternProblems, PatternProblem{Pattern: i, Problem: problem})
			continue
		}
		cv.Values = append(cv.Values, vals...)
	}
	cv.Values, cv.Truncated = limitValueLines(cv.Values, lineLimit)
	return cv
}

// matchPattern returns the values of every match of pattern p in text. The
// whole search of one pattern is bounded by timeout; a pattern that passes
// it gives no value.
func matchPattern(p, text string, timeout time.Duration) ([]ExtractedValue, string) {
	re, err := CompileCommandPattern(p)
	if err != nil {
		return nil, PatternProblemInvalid
	}
	deadline := time.Now().Add(timeout)
	var out []ExtractedValue
	re.MatchTimeout = timeout
	m, err := re.FindStringMatch(text)
	for err == nil && m != nil {
		for _, g := range m.Groups() {
			if isGroupNumber(g.Name) || len(g.Captures) == 0 {
				continue
			}
			if v := g.String(); v != "" {
				out = append(out, ExtractedValue{Name: g.Name, Value: v})
			}
		}
		remaining := time.Until(deadline)
		if remaining <= 0 {
			return nil, PatternProblemTimeout
		}
		re.MatchTimeout = remaining
		m, err = re.FindNextMatch(m)
	}
	if err != nil {
		return nil, PatternProblemTimeout
	}
	return out, ""
}

// limitValueLines keeps at most limit lines of values in total; a line break
// inside a value counts. A value that does not fit completely is cut to the
// remaining lines and everything after it is dropped.
func limitValueLines(values []ExtractedValue, limit int) ([]ExtractedValue, bool) {
	if limit < 1 {
		limit = 1
	}
	left := limit
	for i, v := range values {
		n := strings.Count(v.Value, "\n") + 1
		if n <= left {
			left -= n
			continue
		}
		out := values[:i]
		if left > 0 {
			out = append(out, ExtractedValue{Name: v.Name, Value: firstLines(v.Value, left)})
		}
		return out, true
	}
	return values, false
}

// firstLines returns the first n lines of s (without the line break after
// the last one).
func firstLines(s string, n int) string {
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
// name with the joiner.
func BuildDisplayValue(commands []CommandValues, joiner string) DisplayValue {
	dv := DisplayValue{Rows: []DisplayRow{}}
	index := map[string]int{}
	for _, c := range commands {
		if c.Truncated {
			dv.Truncated = true
		}
		for _, v := range c.Values {
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
			s += "(truncated by the line limit)\n"
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
		b.WriteString("(truncated by the line limit)\n")
	}
	return b.String()
}
