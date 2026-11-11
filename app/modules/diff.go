package modules

import "strings"

// DiffLine is one line of a line-based diff.
type DiffLine struct {
	Op   string `json:"op"` // " ", "+" or "-"
	Text string `json:"text"`
}

// DiffLines computes a line diff from a to b with a longest-common-subsequence
// table. Rule contents are small, so the quadratic cost is acceptable.
func DiffLines(a, b string) []DiffLine {
	x := strings.Split(a, "\n")
	y := strings.Split(b, "\n")
	n, m := len(x), len(y)
	lcs := make([][]int, n+1)
	for i := range lcs {
		lcs[i] = make([]int, m+1)
	}
	for i := n - 1; i >= 0; i-- {
		for j := m - 1; j >= 0; j-- {
			if x[i] == y[j] {
				lcs[i][j] = lcs[i+1][j+1] + 1
			} else if lcs[i+1][j] >= lcs[i][j+1] {
				lcs[i][j] = lcs[i+1][j]
			} else {
				lcs[i][j] = lcs[i][j+1]
			}
		}
	}
	var out []DiffLine
	i, j := 0, 0
	for i < n && j < m {
		switch {
		case x[i] == y[j]:
			out = append(out, DiffLine{Op: " ", Text: x[i]})
			i++
			j++
		case lcs[i+1][j] >= lcs[i][j+1]:
			out = append(out, DiffLine{Op: "-", Text: x[i]})
			i++
		default:
			out = append(out, DiffLine{Op: "+", Text: y[j]})
			j++
		}
	}
	for ; i < n; i++ {
		out = append(out, DiffLine{Op: "-", Text: x[i]})
	}
	for ; j < m; j++ {
		out = append(out, DiffLine{Op: "+", Text: y[j]})
	}
	return out
}
