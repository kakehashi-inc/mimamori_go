package modules

import (
	"reflect"
	"strings"
	"testing"
	"time"
	"unicode/utf8"
)

func TestExtractCommandValues(t *testing.T) {
	ev := func(pairs ...string) []ExtractedValue {
		out := []ExtractedValue{}
		for i := 0; i+1 < len(pairs); i += 2 {
			out = append(out, ExtractedValue{Name: pairs[i], Value: pairs[i+1]})
		}
		return out
	}
	tests := []struct {
		name      string
		patterns  []string
		output    string
		limit     int
		want      []ExtractedValue
		truncated bool
	}{
		{"no patterns: the output", nil, "a\nb\n", 20, ev("", "a\nb"), false},
		{"no patterns: CRLF end", nil, "a\r\n", 20, ev("", "a"), false},
		{"no patterns: empty output", nil, "", 20, ev(), false},
		{"no patterns: line limit", nil, "1\n2\n3\n4\n", 2, ev("", "1\n2"), true},
		{"no patterns: exactly the limit", nil, "1\n2\n", 2, ev("", "1\n2"), false},
		{"no patterns: carriage return is not a line", nil, "a\rb\rc\n", 1, ev("", "a\rb\rc"), false},
		{"all matches in order", []string{`(?P<use>\d+)%`}, "/ 41%\n/var 77%\n", 20, ev("use", "41", "use", "77"), false},
		{"only named groups", []string{`(\w+)=(?<v>\d+)`}, "a=1 b=2", 20, ev("v", "1", "v", "2"), false},
		{"several groups in pattern order", []string{`(?P<k>\w+)=(?P<v>\d+)`}, "a=1\nb=2", 20, ev("k", "a", "v", "1", "k", "b", "v", "2"), false},
		{"patterns in definition order", []string{`b=(?P<b>\d)`, `a=(?P<a>\d)`}, "a=1 b=2 a=3", 20, ev("b", "2", "a", "1", "a", "3"), false},
		{"group that did not take part", []string{`(?P<x>x)|(?P<y>y)`}, "xy", 20, ev("x", "x", "y", "y"), false},
		{"empty capture", []string{`a(?P<v>\d*)`}, "a a5", 20, ev("v", "5"), false},
		{"no match", []string{`(?P<v>zzz)`}, "abc", 20, ev(), false},
		{"multi-line anchors", []string{`^(?P<first>\w+)`}, "one two\nthree four\n", 20, ev("first", "one", "first", "three"), false},
		{"match spanning lines", []string{`start\n(?P<body>[^\n]+)\nend`}, "x\nstart\nmiddle\nend\n", 20, ev("body", "middle"), false},
		{"dot-all", []string{`(?s)<(?P<block>.*?)>`}, "<a\nb>", 20, ev("block", "a\nb"), false},
		{"carriage return in a pattern", []string{`(?P<v>\w+)\r\n`}, "a\r\nb\r\n", 20, ev("v", "a", "v", "b"), false},
		{"look-around", []string{`(?<=size: )(?<n>\d+)(?= MB)`}, "size: 12 MB, size: 13 GB", 20, ev("n", "12"), false},
		{"back-reference", []string{`(?P<w>\b\w+) \k<w>\b`}, "the the cat cat dog", 20, ev("w", "the", "w", "cat"), false},
		{"line limit across values", []string{`(?P<v>\d+)`}, "1 2 3 4", 3, ev("v", "1", "v", "2", "v", "3"), true},
		{"line breaks inside values count", []string{`(?s)\[(?P<v>.*?)\]`}, "[a\nb][c\nd\ne]", 3, ev("v", "a\nb", "v", "c"), true},
		{"invalid UTF-8 is replaced for matching", []string{`(?P<v>a.b)`}, "a\xffb", 20, ev("v", "a\uFFFDb"), false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := ExtractCommandValues(tc.patterns, []byte(tc.output), tc.limit)
			if !reflect.DeepEqual(got.Values, tc.want) || got.Truncated != tc.truncated || len(got.PatternProblems) != 0 {
				t.Fatalf("got %+v, want %+v (truncated %v)", got, tc.want, tc.truncated)
			}
		})
	}
}

func TestExtractPatternProblems(t *testing.T) {
	// A pattern that passes the time limit contributes nothing; the other
	// patterns still do.
	out := []byte(strings.Repeat("a", 30) + "!")
	got := extractCommandValues([]string{`(?P<v>(a+)+b)`, `(?P<e>!)`, `(?P<x>`}, out, 20, 50*time.Millisecond)
	want := []PatternProblem{{Pattern: 0, Problem: PatternProblemTimeout}, {Pattern: 2, Problem: PatternProblemInvalid}}
	if !reflect.DeepEqual(got.PatternProblems, want) {
		t.Fatalf("problems: %+v", got.PatternProblems)
	}
	if !reflect.DeepEqual(got.Values, []ExtractedValue{{Name: "e", Value: "!"}}) {
		t.Fatalf("values: %+v", got.Values)
	}
}

func TestBuildDisplayValue(t *testing.T) {
	cmds := []CommandValues{
		{Values: []ExtractedValue{{Name: "host", Value: "a"}, {Name: "use", Value: "41"}}},
		{Values: []ExtractedValue{}},
		{Values: []ExtractedValue{{Name: "use", Value: "77"}, {Name: "", Value: "raw\nout"}}, Truncated: true},
		{Values: []ExtractedValue{{Name: "", Value: "more"}, {Name: "host", Value: "b"}}},
	}
	dv := BuildDisplayValue(cmds, JoinerComma)
	want := []DisplayRow{
		{Name: "host", Values: []string{"a", "b"}, Value: "a,b"},
		{Name: "use", Values: []string{"41", "77"}, Value: "41,77"},
		{Name: "", Values: []string{"raw\nout", "more"}, Value: "raw\nout,more"},
	}
	if !reflect.DeepEqual(dv.Rows, want) || !dv.Truncated {
		t.Fatalf("rows: %+v", dv)
	}
	dv = BuildDisplayValue(cmds, JoinerNewline)
	if dv.Rows[0].Value != "a\nb" || dv.Rows[1].Value != "41\n77" {
		t.Fatalf("newline joiner: %+v", dv.Rows)
	}
	empty := BuildDisplayValue(nil, JoinerComma)
	if empty.Rows == nil || len(empty.Rows) != 0 || empty.Truncated {
		t.Fatalf("empty: %+v", empty)
	}
}

func TestRenderDisplayValue(t *testing.T) {
	tests := []struct {
		dv   DisplayValue
		want string
	}{
		{DisplayValue{}, "(no values)\n"},
		{DisplayValue{Rows: []DisplayRow{{Name: "use", Value: "41,77"}}}, "use: 41,77\n"},
		{DisplayValue{Rows: []DisplayRow{{Name: "", Value: "a\nb"}}, Truncated: true}, "(output):\n  a\n  b\n(truncated by the line or size limit)\n"},
	}
	for _, tc := range tests {
		if got := RenderDisplayValue(tc.dv); got != tc.want {
			t.Errorf("%q, want %q", got, tc.want)
		}
	}
	if DisplayText([]byte("ok\xfe")) != "ok\uFFFD" {
		t.Fatal("invalid UTF-8 not replaced")
	}
}

// TestValueSizeLimits checks that patterns which capture the same large text
// many times stay within MaxCommandValueBytes per command and
// MaxDisplayValueBytes per run.
func TestValueSizeLimits(t *testing.T) {
	big := []byte(strings.Repeat("x", MaxCommandOutputBytes))
	// Three named groups capture the whole output: only the first fits.
	got := ExtractCommandValues([]string{`(?<a>(?<b>(?<c>x+)))`}, big, MaxCommandLineLimit)
	size := 0
	for _, v := range got.Values {
		size += len(v.Value)
	}
	if !got.Truncated || size > MaxCommandValueBytes || len(got.Values) != 1 || got.Values[0].Name != "a" {
		t.Fatalf("values: %d of %d bytes, truncated %v", len(got.Values), size, got.Truncated)
	}
	// A look-ahead captures the rest of the output at every position: the
	// collection stops at the limits instead of growing with each match.
	start := time.Now()
	got = ExtractCommandValues([]string{`(?=(?<rest>x{1000}))`}, big[:200000], MaxCommandLineLimit)
	size = 0
	for _, v := range got.Values {
		size += len(v.Value)
	}
	if !got.Truncated || size > MaxCommandValueBytes || time.Since(start) > 10*time.Second {
		t.Fatalf("look-ahead: %d values, %d bytes, truncated %v", len(got.Values), size, got.Truncated)
	}
	// A cut keeps whole characters.
	multi := []byte(strings.Repeat("あ", MaxCommandValueBytes/3+1))
	got = ExtractCommandValues(nil, multi, 1)
	if !got.Truncated || len(got.Values) != 1 || len(got.Values[0].Value) > MaxCommandValueBytes || !utf8.ValidString(got.Values[0].Value) {
		t.Fatalf("cut: truncated %v, %d bytes", got.Truncated, len(got.Values[0].Value))
	}
	// The display value of a run is bounded across its commands.
	cmds := []CommandValues{
		{Values: []ExtractedValue{{Name: "v", Value: strings.Repeat("a", MaxDisplayValueBytes-10)}}},
		{Values: []ExtractedValue{{Name: "v", Value: strings.Repeat("b", 100)}, {Name: "w", Value: "c"}}},
	}
	dv := BuildDisplayValue(cmds, JoinerComma)
	if !dv.Truncated || len(dv.Rows) != 1 || len(dv.Rows[0].Values) != 2 || len(dv.Rows[0].Values[1]) != 10 {
		t.Fatalf("display: truncated %v, rows %d", dv.Truncated, len(dv.Rows))
	}
}
