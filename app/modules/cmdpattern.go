package modules

import (
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"unicode"

	"github.com/dlclark/regexp2"
	"github.com/dlclark/regexp2/syntax"
)

// Group numbers of value patterns. The library numbers capturing groups the
// .NET way (all unnamed groups first, then the named ones). Python, the
// editors and PCRE number every capturing group, named or not, by the
// position of its opening parenthesis. Before a pattern is compiled, every
// unnamed capturing group therefore gets a generated name that cannot clash
// with a name in the pattern (then all groups are named and the library
// numbers them by appearance), and "(?P=name)" becomes "\k<name>". The scan
// follows the library's own pre-scan (countCaptures in syntax/parser.go), so
// that it sees the same groups; the compiled expression is checked against
// the scan afterwards, and a pattern that does not agree is refused.

// commandPattern is a compiled value pattern and the names of the named
// groups the person wrote (only these give values).
type commandPattern struct {
	re    *regexp2.Regexp
	names map[string]bool
}

// patternGroup is one capturing group of a pattern, in the order of the
// opening parentheses.
type patternGroup struct {
	name      string
	generated bool
	closed    bool
}

// patternRef is a numbered back-reference as written ("\N", "\k<N>").
type patternRef struct {
	text    string // the reference as written, for messages
	num     int    // the group number
	defined int    // groups opened before the reference
	open    bool   // the group was still open at the reference
	octal   bool   // three or more digits starting with three octal digits
	digits  int    // number of digits of a "\N" reference (0 for "\k<N>")
	angled  bool   // "\k<N>", "\<N>" or "\'N'"
}

// patternFrame is an open parenthesis of the scan.
type patternFrame struct {
	group int  // index into groups, -1 when the group does not capture
	x     bool // the x option before the parenthesis
}

// patternScanner rewrites a pattern for compilation.
type patternScanner struct {
	in     []rune
	pos    int
	out    strings.Builder
	prefix string

	groups      []patternGroup
	frames      []patternFrame
	x           bool
	ignoreNext  bool
	refs        []patternRef
	conds       []int
	numbersUsed bool
	err         error
}

// compileCommandPattern compiles a value pattern with the group numbering
// of Python and the editors (see above).
func compileCommandPattern(p string) (*commandPattern, error) {
	if p == "" {
		return nil, errors.New("must not be empty")
	}
	sc := &patternScanner{in: []rune(p), prefix: generatedGroupPrefix(p)}
	rewritten, err := sc.rewrite()
	if err != nil {
		return nil, err
	}
	re, err := regexp2.Compile(rewritten, regexp2.RE2|regexp2.Multiline)
	if err != nil {
		var se *syntax.Error
		if errors.As(err, &se) {
			// Name the pattern as the person wrote it, not the rewritten one.
			c := *se
			c.Expr = p
			err = &c
		}
		return nil, fmt.Errorf("invalid regular expression: %v", err)
	}
	// Every capturing group must now be named, in the order of the scan:
	// then the library's numbers are the numbers of the opening
	// parentheses. Anything else is refused rather than numbered another way.
	var want []string
	seen := map[string]bool{}
	names := map[string]bool{}
	for _, g := range sc.groups {
		if !seen[g.name] {
			seen[g.name] = true
			want = append(want, g.name)
		}
		if !g.generated {
			names[g.name] = true
		}
	}
	got := re.GetGroupNames()
	if len(got) != len(want)+1 || !slices.Equal(got[1:], want) {
		return nil, errors.New("the groups of this pattern cannot be numbered in the order of their opening parentheses; simplify the pattern")
	}
	if len(names) == 0 {
		return nil, errors.New("must have at least one named group such as (?P<name>...) or (?<name>...)")
	}
	return &commandPattern{re: re, names: names}, nil
}

// generatedGroupPrefix returns a prefix for generated group names that does
// not occur in the pattern, so that no generated name equals a name the
// pattern defines or refers to.
func generatedGroupPrefix(p string) string {
	prefix := "_g"
	for strings.Contains(p, prefix) {
		prefix += "_"
	}
	return prefix
}

// isPatternWordChar is the library's IsWordChar (letters, non-spacing
// marks, decimal digits, connector punctuation, ZWJ, ZWNJ).
func isPatternWordChar(r rune) bool {
	return unicode.In(r, unicode.L, unicode.Mn, unicode.Nd, unicode.Pc) || r == '‍' || r == '‌'
}

func isOctalDigit(r rune) bool { return r >= '0' && r <= '7' }

func isHexDigit(r rune) bool {
	return (r >= '0' && r <= '9') || (r >= 'a' && r <= 'f') || (r >= 'A' && r <= 'F')
}

// at returns the character i places after the current position, or 0.
func (s *patternScanner) at(i int) rune {
	if s.pos+i < len(s.in) {
		return s.in[s.pos+i]
	}
	return 0
}

func (s *patternScanner) left() int { return len(s.in) - s.pos }

// copy writes n characters from the current position unchanged.
func (s *patternScanner) copy(n int) {
	end := min(s.pos+n, len(s.in))
	s.out.WriteString(string(s.in[s.pos:end]))
	s.pos = end
}

func (s *patternScanner) fail(format string, a ...any) {
	if s.err == nil {
		s.err = fmt.Errorf(format, a...)
	}
}

// rewrite scans the whole pattern and returns the rewritten pattern, or the
// reason why the numbering of Python cannot be guaranteed.
func (s *patternScanner) rewrite() (string, error) {
	for s.pos < len(s.in) && s.err == nil {
		switch s.in[s.pos] {
		case '\\':
			s.escape()
		case '[':
			start := s.pos
			s.pos = s.classEnd(s.pos + 1)
			s.out.WriteString(string(s.in[start:s.pos]))
		case '#':
			if s.x {
				// A comment up to the end of the line.
				n := 0
				for s.at(n) != 0 && s.at(n) != '\n' {
					n++
				}
				s.copy(n)
			} else {
				s.copy(1)
			}
		case ')':
			if k := len(s.frames); k > 0 {
				f := s.frames[k-1]
				s.frames = s.frames[:k-1]
				s.x = f.x
				if f.group >= 0 {
					s.groups[f.group].closed = true
				}
			}
			s.copy(1)
		case '(':
			s.open()
		default:
			s.copy(1)
		}
	}
	if s.err == nil {
		s.finish()
	}
	if s.err != nil {
		return "", s.err
	}
	return s.out.String(), nil
}

// finish checks the numbered references and conditions against the number
// of groups of the whole pattern.
func (s *patternScanner) finish() {
	total := len(s.groups)
	for _, r := range s.refs {
		switch {
		case !r.angled && r.digits >= 3 && r.octal && r.num > total:
			// An octal escape, as in Python and in the library.
			continue
		case !r.angled && r.digits >= 3:
			s.fail(`%s: write references to groups with at most two digits and octal escapes with three octal digits`, r.text)
		case r.num < 1 || r.num > r.defined:
			s.fail(`%s refers to group %d, which is not defined before it`, r.text, r.num)
		case r.open:
			s.fail(`%s refers to group %d, which is still open at that point`, r.text, r.num)
		default:
			s.numbersUsed = true
		}
		if s.err != nil {
			return
		}
	}
	for _, n := range s.conds {
		if n < 1 || n > total {
			s.fail(`(?(%d)...) refers to group %d, which does not exist`, n, n)
			return
		}
		s.numbersUsed = true
	}
	if s.numbersUsed {
		seen := map[string]bool{}
		for _, g := range s.groups {
			if seen[g.name] {
				s.fail(`group numbers are ambiguous because the name %q is used for more than one group`, g.name)
				return
			}
			seen[g.name] = true
		}
	}
}

// escape handles a backslash outside a character class.
func (s *patternScanner) escape() {
	c := s.at(1)
	switch {
	case c == 0:
		s.copy(1)
	case c >= '1' && c <= '9':
		n := 1
		for s.at(n) >= '0' && s.at(n) <= '9' {
			n++
		}
		digits := string(s.in[s.pos+1 : s.pos+n])
		num, err := strconv.Atoi(digits)
		if err != nil {
			num = -1
		}
		octal := len(digits) >= 3 && isOctalDigit(rune(digits[0])) && isOctalDigit(rune(digits[1])) && isOctalDigit(rune(digits[2]))
		s.addRef(`\`+digits, num, len(digits), octal, false)
		s.copy(n)
	case c == 'k' && (s.at(2) == '<' || s.at(2) == '\''):
		s.angledRef(3)
	case (c == '<' || c == '\'') && s.left() > 2:
		s.angledRef(2)
	case c == 'c':
		// A control character: the character after "\c" belongs to it.
		s.copy(3)
	default:
		s.copy(2)
	}
}

// angledRef handles "\k<...>", "\<...>" and "\'...'" whose name starts at
// offset start; only a number followed by the closing character is a
// numbered reference, everything else is copied as it is.
func (s *patternScanner) angledRef(start int) {
	closeCh := '>'
	if s.at(start-1) == '\'' {
		closeCh = '\''
	}
	n := start
	for s.at(n) >= '0' && s.at(n) <= '9' {
		n++
	}
	if n > start && s.at(n) == closeCh {
		text := string(s.in[s.pos : s.pos+n+1])
		num, err := strconv.Atoi(string(s.in[s.pos+start : s.pos+n]))
		if err != nil {
			num = -1
		}
		s.addRef(text, num, 0, false, true)
		s.copy(n + 1)
		return
	}
	s.copy(2)
}

func (s *patternScanner) addRef(text string, num, digits int, octal, angled bool) {
	r := patternRef{text: text, num: num, defined: len(s.groups), octal: octal, digits: digits, angled: angled}
	if num >= 1 && num <= len(s.groups) {
		r.open = !s.groups[num-1].closed
	}
	s.refs = append(s.refs, r)
}

// scanName returns the length of the run of word characters at offset i.
func (s *patternScanner) scanName(i int) int {
	n := 0
	for s.at(i+n) != 0 && isPatternWordChar(s.at(i+n)) {
		n++
	}
	return n
}

// open handles an opening parenthesis.
func (s *patternScanner) open() {
	ignore := s.ignoreNext
	s.ignoreNext = false
	if s.at(1) == '?' && s.at(2) == '#' {
		// A comment up to the first ")".
		n := 3
		for s.at(n) != 0 && s.at(n) != ')' {
			n++
		}
		s.copy(n + 1)
		return
	}
	if s.at(1) != '?' {
		if ignore {
			// The condition of "(?(...)...)" does not capture.
			s.push(-1)
			s.copy(1)
			return
		}
		name := s.prefix + strconv.Itoa(len(s.groups)+1)
		s.groups = append(s.groups, patternGroup{name: name, generated: true})
		s.push(len(s.groups) - 1)
		s.pos++
		s.out.WriteString("(?<" + name + ">")
		return
	}
	c := s.at(2)
	switch {
	case (c == '<' || c == '\'') && s.left() > 3:
		ch := s.at(3)
		switch {
		case ch >= '1' && ch <= '9':
			s.fail(`numbered groups such as (?<1>...) are not supported; give the group a name`)
		case ch == '-':
			s.fail(`balancing groups such as (?<-name>...) are not supported`)
		case ch != '0' && isPatternWordChar(ch):
			n := s.scanName(3)
			if s.at(3+n) == '-' {
				s.fail(`balancing groups such as (?<name1-name2>...) are not supported`)
				return
			}
			s.groups = append(s.groups, patternGroup{name: string(s.in[s.pos+3 : s.pos+3+n])})
			s.push(len(s.groups) - 1)
			s.copy(3 + n)
			return
		}
		// Look-behind, or something the library refuses.
		s.push(-1)
		s.copy(3)
	case c == 'P' && s.at(3) == '<' && s.left() > 4:
		if isPatternWordChar(s.at(4)) {
			n := s.scanName(4)
			s.groups = append(s.groups, patternGroup{name: string(s.in[s.pos+4 : s.pos+4+n])})
			s.push(len(s.groups) - 1)
			s.copy(4 + n)
			return
		}
		s.push(-1)
		s.copy(4)
	case c == 'P' && s.at(3) == '=' && s.scanName(4) > 0 && s.at(4+s.scanName(4)) == ')':
		// Python's reference to a named group: the group must be defined
		// and closed before it, as in Python.
		n := s.scanName(4)
		name := string(s.in[s.pos+4 : s.pos+4+n])
		defined, closed := false, false
		for _, g := range s.groups {
			if g.name == name && !g.generated {
				defined = true
				closed = closed || g.closed
			}
		}
		switch {
		case !defined:
			s.fail(`(?P=%s) refers to a group name that is not defined before it`, name)
		case !closed:
			s.fail(`(?P=%s) refers to a group that is still open at that point`, name)
		}
		s.out.WriteString(`\k<` + name + `>`)
		s.pos += 4 + n + 1
	case c == ')':
		s.fail(`the empty group (?) is not supported`)
	default:
		// Options, non-capturing groups, look-ahead, atomic groups and
		// conditions. The options (as the library reads them) change the
		// x option up to the end of the enclosing group.
		saved := s.x
		n := 2
	options:
		for ; s.at(n) != 0; n++ {
			switch s.at(n) {
			case '-', '+':
			case 'n', 'N':
				s.fail(`the option n (explicit capture) is not supported`)
				return
			case 'x', 'X':
			case 'i', 'I', 'm', 'M', 's', 'S', 'd', 'D', 'u', 'U':
			default:
				break options
			}
		}
		s.x = optionX(s.in[s.pos+2:s.pos+n], saved)
		switch s.at(n) {
		case ')':
			// "(?imsx-imsx)": the options apply up to the end of the
			// enclosing group.
			s.copy(n + 1)
			return
		case '(':
			s.frames = append(s.frames, patternFrame{group: -1, x: saved})
			// A numbered condition "(?(N)...)".
			d := n + 1
			for s.at(d) >= '0' && s.at(d) <= '9' {
				d++
			}
			if d > n+1 && s.at(d) == ')' {
				num, err := strconv.Atoi(string(s.in[s.pos+n+1 : s.pos+d]))
				if err != nil {
					num = -1
				}
				s.conds = append(s.conds, num)
				s.copy(d + 1)
				return
			}
			// The parenthesis of the condition does not capture.
			s.ignoreNext = true
			s.copy(n)
			return
		}
		s.frames = append(s.frames, patternFrame{group: -1, x: saved})
		s.copy(n)
	}
}

// optionX returns the x option after the option letters opts.
func optionX(opts []rune, x bool) bool {
	on := true
	for _, r := range opts {
		switch r {
		case '-':
			on = false
		case '+':
			on = true
		case 'x', 'X':
			x = on
		}
	}
	return x
}

func (s *patternScanner) push(group int) {
	s.frames = append(s.frames, patternFrame{group: group, x: s.x})
}

// classEnd returns the position after the character class whose content
// starts at i, following the library's pre-scan (scanCharSet): a "]" right
// at the start is a character, escapes, POSIX classes such as [:alpha:] and
// subtractions such as [a-z-[aeiou]].
func (s *patternScanner) classEnd(i int) int {
	in := s.in
	n := len(in)
	if i < n && in[i] == '^' {
		i++
	}
	inRange := false
	for first := true; i < n; first = false {
		ch := in[i]
		i++
		translated := false
		switch {
		case ch == ']' && !first:
			return i
		case ch == '\\' && i < n:
			c := in[i]
			i++
			switch c {
			case 'd', 'D', 's', 'S', 'w', 'W', '-':
				continue
			case 'p', 'P':
				i = propertyEnd(in, i)
				continue
			}
			i = charEscapeEnd(in, i-1)
			translated = true
		case ch == '[' && i < n && in[i] == ':' && !inRange:
			save := i
			j := i + 1
			if n-j > 1 && in[j] == '^' {
				j++
			}
			for j < n && isPatternWordChar(in[j]) {
				j++
			}
			if n-j >= 2 && in[j] == ':' && in[j+1] == ']' {
				i = j + 2
				continue
			}
			i = save
		}
		switch {
		case inRange:
			inRange = false
		case n-i >= 2 && in[i] == '-' && in[i+1] != ']':
			inRange = true
			i++
		case n-i >= 1 && ch == '-' && !translated && in[i] == '[' && !first:
			i = s.classEnd(i + 1)
		}
	}
	return i
}

// propertyEnd returns the position after the name of "\p" starting at i.
func propertyEnd(in []rune, i int) int {
	n := len(in)
	if i < n && in[i] != '{' {
		return i + 1
	}
	if n-i < 3 {
		return n
	}
	i++
	for i < n && (isPatternWordChar(in[i]) || in[i] == '-') {
		i++
	}
	if i < n {
		i++
	}
	return i
}

// charEscapeEnd returns the position after the character escape whose
// letter is at i (octal, \x, \u, \c and single characters).
func charEscapeEnd(in []rune, i int) int {
	n := len(in)
	ch := in[i]
	i++
	switch {
	case isOctalDigit(ch):
		for k := 1; k < 3 && i < n && isOctalDigit(in[i]); k++ {
			i++
		}
	case ch == 'x' && i < n && in[i] == '{':
		for i < n && in[i] != '}' {
			i++
		}
		if i < n {
			i++
		}
	case ch == 'x' || ch == 'u':
		count := 2
		if ch == 'u' {
			count = 4
		}
		for k := 0; k < count && i < n && isHexDigit(in[i]); k++ {
			i++
		}
	case ch == 'c' && i < n:
		i++
	}
	return i
}
