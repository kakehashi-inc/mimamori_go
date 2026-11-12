package modules

import (
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/dlclark/regexp2"
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

// TestCommandPatternNumbering checks that capturing groups are numbered as
// in Python: every group, named or not, by the position of its opening
// parenthesis. The expected values were produced with Python's re.finditer
// (re.MULTILINE) by the script cases.py kept with the audit of this change;
// constructs Python does not know (\k<name>, (?<name>), (?'name'), \Q, \c,
// POSIX classes, (?-x)) were run there as their Python equivalents. Each
// construct that hides a parenthesis (escape, class, comment, x option) is
// next to a real group and a numbered reference.
func TestCommandPatternNumbering(t *testing.T) {
	ev := func(pairs ...string) []ExtractedValue {
		out := []ExtractedValue{}
		for i := 0; i+1 < len(pairs); i += 2 {
			out = append(out, ExtractedValue{Name: pairs[i], Value: pairs[i+1]})
		}
		return out
	}
	tests := []struct {
		name    string
		pattern string
		output  string
		want    []ExtractedValue
	}{
		{"brief: reference after a named group", `(?P<dbl>(\w)\2)`, `aa bb ab`, ev(`dbl`, `aa`, `dbl`, `bb`)},
		{"unnamed then named, \\1", `(\w)(?P<n>\d)\1`, `a1a b2c c3c`, ev(`n`, `1`, `n`, `3`)},
		{"named then unnamed, \\2", `(?P<n>\d)(\w)\2`, `1aa 2bc 3cc`, ev(`n`, `1`, `n`, `3`)},
		{"named then unnamed, \\1", `(?P<n>\d)(\w)\1`, `1a1 2b3 3c3`, ev(`n`, `1`, `n`, `3`)},
		{"several, \\3 and \\1", `(\w)(?P<a>\w)(\w)-\3\1`, `abc-ca xyz-zy`, ev(`a`, `b`)},
		{"nested", `(?P<o>(\w)(?P<i>(\d)\4))\2`, `a11a b22c c33c`, ev(`o`, `a11`, `i`, `11`, `o`, `c33`, `i`, `33`)},
		{"nested unnamed outside", `((?P<a>\w)(\d))\3`, `a11 b23 c44`, ev(`a`, `a`, `a`, `c`)},
		{"alternation", `(?:(\d)|(?P<w>[a-z]))(?P<t>x|y)\1?`, `1x1 ay 2y`, ev(`t`, `x`, `w`, `a`, `t`, `y`, `t`, `y`)},
		{"condition on an unnamed group", `(<)?(?P<v>\w+)(?(1)>|;)`, `<a> b; <c; d>`, ev(`v`, `a`, `v`, `b`, `v`, `c`)},
		{"condition after a named group", `(?P<q>')?(\w+)(?(1)'|!)(?P<r>\d)`, `'ab'1 cd!2 'ef!3`, ev(`q`, `'`, `r`, `1`, `r`, `2`, `r`, `3`)},
		{"condition on a named group's number", `(?P<q>')?(?P<w>\w+)(?(1)')`, `'ab' cd`, ev(`q`, `'`, `w`, `ab`, `w`, `cd`)},
		{"condition with a forward number", `(?P<a>a)(?(2)x|y)(b)?`, `ayb ay`, ev(`a`, `a`, `a`, `a`)},
		{"group in look-ahead", `(?P<a>\w)(?=(\d))\2`, `a11 b12 c33`, ev(`a`, `a`, `a`, `b`, `a`, `c`)},
		{"group in look-behind", `(?<=(\w))(?P<d>\d)\1`, `a1a b1c x2x`, ev(`d`, `1`, `d`, `2`)},
		{"negative look-behind", `(?<!x)(?P<w>(\w)\2)`, `aa xbb cc`, ev(`w`, `aa`, `w`, `cc`)},
		{"(?P=name)", `(?P<w>\w+) (?P=w) (\w)\2`, `ab ab cc ab ab cd`, ev(`w`, `ab`)},
		{"\\k<name>", `(?P<w>\w+) \k<w> (\w)\2`, `ab ab cc ab ab cd`, ev(`w`, `ab`)},
		{"(?<name>) and (?'name')", `(?<a>\w)(\w)(?'b'\d)\2`, `ab1b cd2e`, ev(`a`, `a`, `b`, `1`)},
		{"escaped parentheses", `\((\w)\)(?P<n>\d)\1`, `(a)1a (b)2c`, ev(`n`, `1`)},
		{"escaped parentheses inside a group", `(?P<p>\(\w\))(\d)\2`, `(a)11 (b)23`, ev(`p`, `(a)`)},
		{"parenthesis in a class", `[(](?P<a>\w)(x)\2`, `(axx (bxy`, ev(`a`, `a`)},
		{"closing parenthesis in a class", `[)](?P<a>\w)(\w)\2`, `)abb )abc`, ev(`a`, `a`)},
		{"bracket first in a class", `[]()](?P<a>\w)(\w)\2`, `]abb (cdd )efg`, ev(`a`, `a`, `a`, `c`)},
		{"negated class with parentheses", `[^()\s](?P<a>\w)(\w)\2`, `xabb (abb`, ev(`a`, `a`)},
		{"escaped bracket in a class", `[\]((](?P<a>\w)(\w)\2`, `]abb (cdd`, ev(`a`, `a`, `a`, `c`)},
		{"class with a range and parentheses", `[!-)](?P<a>\w)(\w)\2`, `'abb (cdd *eff`, ev(`a`, `a`, `a`, `c`)},
		{"comment with parentheses", `(?#(x)(?P<a>\w)(\w)\2`, `abb acd`, ev(`a`, `a`)},
		{"comment between groups", `(?P<a>\w)(?#)(\w)\2`, `abb acd`, ev(`a`, `a`)},
		{"x option with a comment", "(?x) (?P<a>\\w)  # a ( comment (\n (\\w) \\2", `abb acd`, ev(`a`, `a`)},
		{"x option: # in a class is not a comment", `(?x) [#(] (?P<a>\w) (\w) \2`, `#abb (cdd`, ev(`a`, `a`, `a`, `c`)},
		{"x option: escaped #", `(?x) \# (?P<a>\w) (\w)\2`, `#abb`, ev(`a`, `a`)},
		{"scoped x option", "(?x: (?P<a>\\w) # (\n)(\\w)\\2", `abb acd`, ev(`a`, `a`)},
		{"# without x is a character", `#(?P<a>\w)(\w)\2`, `#abb #cd`, ev(`a`, `a`)},
		{"local x option ends with its group", "(?x:(?P<a>\\w) # (\n) #(\\w)\\2", `a #bb a #cd`, ev(`a`, `a`)},
		{"\\Q( is Q and a group", `(?P<a>\w)\Q(\E)\1`, `zQEz zQEE`, ev(`a`, `z`)},
		{"control character \\c[", `\c[(?P<a>\w)(\w)\2`, "\u001babb \u001bacd", ev(`a`, `a`)},
		{"POSIX class with a parenthesis", `[[:digit:](](?P<a>\w)(\w)\2`, `1abb (cdd xabb`, ev(`a`, `a`, `a`, `c`)},
		{"inline option off", `(?x)(?P<a>\w)(?-x)#(\w)\2`, `a#bb a#cd`, ev(`a`, `a`)},
		{"octal escape after groups", `(?P<a>\w)(\w)\101\2`, `abAb cdAc`, ev(`a`, `a`)},
		{"hex escape", `(?P<a>\x41)(\w)\2`, `Abb Abc`, ev(`a`, `A`)},
		{"non-capturing and atomic groups", `(?:(?P<a>\w))(?>(\w))\2`, `abb acd`, ev(`a`, `a`)},
		{"options group", `(?i:(?P<a>[a-z]))(\w)\2`, `Abb acd`, ev(`a`, `A`)},
		{"a name that looks generated", `(?P<_g1>\w)(\w)(?P<g2>\w)\2`, `abcb abcd`, ev(`_g1`, `a`, `g2`, `c`)},
		{"a name with the second prefix", `(?P<_g_1>\w)(?P<_g>\w)(\w)\3`, `abcc abcd`, ev(`_g_1`, `a`, `_g`, `b`)},
		{"repeated group gives its last capture", `(?P<last>\w)+;(\d)\2`, `abc;11 de;12`, ev(`last`, `c`)},
		{"group that did not take part", `(?P<x>x)?(?P<y>y)(\d)\3`, `xy11 y22 y23`, ev(`x`, `x`, `y`, `y`, `y`, `y`)},
		{"two-digit reference", `(?P<a>a)(b)(c)(d)(e)(f)(g)(h)(i)(j)\10`, `abcdefghijj abcdefghijk`, ev(`a`, `a`)},
		{"named reference inside a repetition", `(?:(?P<w>\w)(?P=w))+`, `aabb cd`, ev(`w`, `b`)},
		{"multi-line", `^(?P<k>\w+)=(\d)\2$`, "a=11\nb=12\nc=33", ev(`k`, `a`, `k`, `c`)},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := CompileCommandPattern(tc.pattern); err != nil {
				t.Fatalf("refused: %v", err)
			}
			got := ExtractCommandValues([]string{tc.pattern}, []byte(tc.output), MaxCommandLineLimit)
			if !reflect.DeepEqual(got.Values, tc.want) || len(got.PatternProblems) != 0 {
				t.Fatalf("got %+v, want %+v", got, tc.want)
			}
		})
	}
}

// TestCommandPatternRefused checks that patterns whose numbered or named
// references Python refuses, and constructs whose numbering cannot be
// guaranteed, are refused by the validation and give no value on the agent.
func TestCommandPatternRefused(t *testing.T) {
	tests := []struct {
		name    string
		pattern string
	}{
		{"brief: reference to the open group", `(?P<dbl>(\w)\1)`},             // Python: cannot refer to an open group at position 12
		{"reference to an open unnamed group", `(?P<a>(\w\1))`},               // Python: cannot refer to an open group at position 9
		{"forward reference", `\1(?P<a>a)`},                                   // Python: invalid group reference 1 at position 1
		{"forward reference to an unnamed group", `(?P<a>a)\2(b)`},            // Python: invalid group reference 2 at position 9
		{"reference to a group that does not exist", `(?P<a>a)\2`},            // Python: invalid group reference 2 at position 9
		{"two-digit reference to a group that does not exist", `(?P<a>a)\12`}, // Python: invalid group reference 12 at position 9
		{"three digits that are not octal", `(?P<a>a)\189`},                   // Python: invalid group reference 18 at position 9
		{"angled number to a group that does not exist", `(?P<a>a)\k<2>`},     // Python: invalid group reference 2 at position 9
		{"condition on a group that does not exist", `(?P<a>a)(?(2)b|c)`},     // Python: invalid group reference 2 at position 11
		{"condition on group 0", `(?P<a>a)(?(0)b|c)`},                         // Python: bad group number at position 11
		{"(?P=name) inside its group", `(?P<a>a(?P=a))`},                      // Python: cannot refer to an open group at position 11
		{"(?P=name) before its group", `(?P=a)(?P<a>a)`},                      // Python: unknown group name 'a' at position 4
		{"(?P=name) of a name that does not exist", `(?P<a>a)(?P=b)`},         // Python: unknown group name 'b' at position 12
		{"duplicate name with a number", `(?P<a>a)(b)|(?P<a>c)\2`},            // Python: redefinition of group name 'a' as group 3; was group 1 at position 16
		{"option n", `(?n)(?P<a>a)(b)`},                                       // Python: unknown extension ?n at position 1
		{"scoped option n", `(?n:(?P<a>a)(b))`},                               // Python: unknown extension ?n at position 1
		{"numbered group", `(?<1>a)(?P<b>b)`},                                 // Python: bad character in group name '1' at position 4
		{"balancing group", `(?<a>a)(?<b-a>b)`},                               // Python: bad character in group name 'b-a' at position 12
		{"empty group", `(?)(?P<a>a)`},                                        // Python: unknown extension ?) at position 1
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := CompileCommandPattern(tc.pattern); err == nil {
				t.Fatal("accepted")
			}
			got := ExtractCommandValues([]string{tc.pattern}, []byte("aabbcc"), MaxCommandLineLimit)
			want := []PatternProblem{{Pattern: 0, Problem: PatternProblemInvalid}}
			if len(got.Values) != 0 || !reflect.DeepEqual(got.PatternProblems, want) {
				t.Fatalf("got %+v", got)
			}
		})
	}
}

// TestCommandPatternMessages checks the messages of refused patterns: they
// name the pattern as written (not the rewritten one) and the reason.
func TestCommandPatternMessages(t *testing.T) {
	tests := []struct{ pattern, want string }{
		{`(?P<a>(x)`, "invalid regular expression: error parsing regexp: missing closing ) in `(?P<a>(x)`"},
		{`(?P<dbl>(\w)\1)`, `\1 refers to group 1, which is still open at that point`},
		{`(?P<a>a)\2`, `\2 refers to group 2, which is not defined before it`},
		{`(?P<a>a)(?(2)b|c)`, `(?(2)...) refers to group 2, which does not exist`},
		{`(?P<a>a(?P=a))`, `(?P=a) refers to a group that is still open at that point`},
		{`(?P=a)(?P<a>a)`, `(?P=a) refers to a group name that is not defined before it`},
		{`(?n)(?P<a>a)`, `the option n (explicit capture) is not supported`},
		{`(?<1>a)(?P<b>b)`, `numbered groups such as (?<1>...) are not supported`},
		{`(?P<a>a)(b)|(?P<a>c)\2`, `group numbers are ambiguous because the name "a" is used for more than one group`},
		{`(\d+)%`, `must have at least one named group`},
	}
	for _, tc := range tests {
		_, err := CompileCommandPattern(tc.pattern)
		if err == nil || !strings.Contains(err.Error(), tc.want) || strings.Contains(err.Error(), "_g") {
			t.Errorf("%s: %v, want %q", tc.pattern, err, tc.want)
		}
	}
}

// TestCommandPatternKeepsValuesWithoutNumbers checks that a pattern the
// library accepted before groups were renumbered, and that has no numbered
// reference or condition, gives the same values as the library alone.
func TestCommandPatternKeepsValuesWithoutNumbers(t *testing.T) {
	output := "(a)1a ]abb (cdd #x1 \x1babb Q(aQE) 1abb 'ab' cd ab ab aabb size: 12 MB x=1 y=2\nk=v ab3"
	patterns := []string{
		`\((\w)\)(?P<n>\d)`,
		`[]()](?P<a>\w)(\w)`,
		`[^()\s](?P<a>\w)(\w)`,
		`[[:digit:](](?P<a>\w)(\w)`,
		`(?#(x)(?P<a>\w)(\w)`,
		"(?x) (?P<a>\\w)  # a ( comment (\n (\\w)",
		"(?x:(?P<a>\\w) # (\n) #(\\w)",
		`\c[(?P<a>\w)(\w)`,
		`Q\((?P<a>\w)\Q(\E)\)`,
		`(?<q>')?(?<w>\w+)(?(q)')`,
		`(?P<w>\w+) \k<w>`,
		`(?:(?P<w>\w)\k<w>)+`,
		`(?<=size: )(?<n>\d+)(?= MB)`,
		`(?P<x>x)|(?P<x>y)`,
		`(\w)=(?<v>\d)|(?'k'\w)=(\w)`,
		`(?<a>(?<b>\w)(\w))(?<c>\d)`,
		`(?i)(?P<_g1>A)(b)`,
	}
	old := func(p, text string) []ExtractedValue {
		re := regexp2.MustCompile(p, regexp2.RE2|regexp2.Multiline)
		out := []ExtractedValue{}
		m, err := re.FindStringMatch(text)
		for err == nil && m != nil {
			for _, g := range m.Groups() {
				if _, numErr := strconv.Atoi(g.Name); numErr == nil || len(g.Captures) == 0 || g.Length == 0 {
					continue
				}
				out = append(out, ExtractedValue{Name: g.Name, Value: g.String()})
			}
			m, err = re.FindNextMatch(m)
		}
		return out
	}
	for _, p := range patterns {
		want := old(p, output)
		got := ExtractCommandValues([]string{p}, []byte(output), MaxCommandLineLimit)
		if len(want) == 0 || !reflect.DeepEqual(got.Values, want) || len(got.PatternProblems) != 0 {
			t.Errorf("%s: got %+v, want %+v", p, got, want)
		}
	}
}
