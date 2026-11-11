package modules

// GlobMatch matches s against pattern where "*" matches any sequence of
// characters (including "/") and every other character matches itself. An
// empty pattern matches anything.
func GlobMatch(pattern, s string) bool {
	if pattern == "" {
		return true
	}
	p, t := 0, 0
	star, mark := -1, 0
	for t < len(s) {
		switch {
		case p < len(pattern) && pattern[p] == '*':
			star, mark = p, t
			p++
		case p < len(pattern) && pattern[p] == s[t]:
			p++
			t++
		case star >= 0:
			p = star + 1
			mark++
			t = mark
		default:
			return false
		}
	}
	for p < len(pattern) && pattern[p] == '*' {
		p++
	}
	return p == len(pattern)
}
