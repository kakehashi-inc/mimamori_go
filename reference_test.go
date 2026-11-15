package main

import (
	"io"
	"os"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/alecthomas/kong"
)

// referenceFiles are the two command references, which say the same in
// English and in Japanese.
var referenceFiles = []string{"Documents/CommandReference.md", "Documents/CommandReference-ja.md"}

// referenceRow is a command of a command reference: the synopsis in the
// first cell of its table row.
type referenceRow struct {
	line     int
	synopsis string
}

// referenceCodeRe finds the code spans of a line; referenceRowRe a table
// row whose first cell is a code span.
var (
	referenceCodeRe = regexp.MustCompile("`[^`]*`")
	referenceRowRe  = regexp.MustCompile("^\\| `([^`]*)` \\|")
)

// readReference returns the lines of a command reference.
func readReference(t *testing.T, name string) []string {
	t.Helper()
	b, err := os.ReadFile(name)
	if err != nil {
		t.Fatal(err)
	}
	return strings.Split(string(b), "\n")
}

// referenceRows returns the command rows of a command reference, with the
// markdown escape of '|' removed.
func referenceRows(lines []string) []referenceRow {
	var rows []referenceRow
	for i, l := range lines {
		if m := referenceRowRe.FindStringSubmatch(l); m != nil {
			rows = append(rows, referenceRow{line: i + 1, synopsis: strings.ReplaceAll(m[1], `\|`, "|")})
		}
	}
	return rows
}

// The two command references say the same thing line for line: the same
// number of lines, and on every line the same code spans (commands, flags,
// values, messages and output they quote; in the order of each language),
// except for the placeholders written in each language (<central>,
// <error>, <status>).
func TestCommandReferencesSayTheSame(t *testing.T) {
	en, ja := readReference(t, referenceFiles[0]), readReference(t, referenceFiles[1])
	if len(en) != len(ja) {
		t.Fatalf("the references have %d and %d lines", len(en), len(ja))
	}
	words := strings.NewReplacer("<セントラル>", "<central>", "<エラー>", "<error>", "<状態>", "<status>")
	for i := range en {
		a := slices.Sorted(slices.Values(referenceCodeRe.FindAllString(en[i], -1)))
		b := slices.Sorted(slices.Values(referenceCodeRe.FindAllString(words.Replace(ja[i]), -1)))
		if !slices.Equal(a, b) {
			t.Errorf("line %d: the code spans differ:\n%q\n%q", i+1, a, b)
		}
	}
	if a, b := referenceRows(en), referenceRows(ja); len(a) != len(b) {
		t.Errorf("the references have %d and %d command rows", len(a), len(b))
	}
}

// referenceUsage is what a synopsis of a command reference says about a
// command: its flags with the value each one takes (the empty string for a
// flag without a value) and the number of its arguments.
type referenceUsage struct {
	flags map[string]string
	order []string
	args  int
}

// synopsisWord returns a word of a synopsis without the brackets that group
// it ("[--file", "F]", "T]...") and without the "..." that repeats it; a
// bracket inside the word ("SET_ID[=NEW_NAME]") stays.
func synopsisWord(w string) string {
	w = strings.TrimLeft(w, "[")
	w = strings.TrimSuffix(w, "...")
	for strings.HasSuffix(w, "]") && strings.Count(w, "]") > strings.Count(w, "[") {
		w = strings.TrimSuffix(w, "]")
	}
	return w
}

// parseSynopsis reads the part of a synopsis after the command's name:
// brackets are only grouping, "|" between two flags separates
// alternatives, "..." repeats; a flag takes the word after it as its value
// unless it is a flag without a value (boolFlag).
func parseSynopsis(rest string, boolFlag func(string) bool) referenceUsage {
	u := referenceUsage{flags: map[string]string{}}
	var words []string
	for _, w := range strings.Fields(rest) {
		if w = synopsisWord(w); w != "" {
			words = append(words, w)
		}
	}
	for i := 0; i < len(words); i++ {
		w := words[i]
		switch {
		case w == "|":
		case strings.HasPrefix(w, "--"):
			name := w[2:]
			u.order = append(u.order, name)
			u.flags[name] = ""
			if !boolFlag(name) && i+1 < len(words) && !strings.HasPrefix(words[i+1], "--") && words[i+1] != "|" {
				i++
				u.flags[name] = words[i]
			}
		default:
			u.args++
		}
	}
	return u
}

// The command references name every command of the program once, and for
// each command the flags of its --help (besides --help, --data-dir and
// --json) and as many arguments; a flag without a default shows the same
// placeholder in --help as in the references, and a list flag shows the
// repetition.
func TestCommandReferencesMatchTheParser(t *testing.T) {
	var cli CLI
	p := testParser(t, &cli, io.Discard)
	leaves := map[string]*kong.Node{}
	for _, leaf := range p.Model.Leaves(false) {
		leaves[strings.Join(commandPath(leaf), " ")] = leaf
	}
	for _, file := range referenceFiles {
		seen := map[string]int{}
		for _, row := range referenceRows(readReference(t, file)) {
			words := strings.Fields(row.synopsis)
			var path string
			for n := len(words); n > 0; n-- {
				if _, ok := leaves[strings.Join(words[:n], " ")]; ok {
					path = strings.Join(words[:n], " ")
					words = words[n:]
					break
				}
			}
			if path == "" {
				t.Errorf("%s:%d: %q is no command of the program", file, row.line, row.synopsis)
				continue
			}
			seen[path]++
			leaf := leaves[path]
			flags := map[string]*kong.Flag{}
			for _, f := range leaf.Flags {
				if f.Name != "help" {
					flags[f.Name] = f
				}
			}
			u := parseSynopsis(strings.Join(words, " "), func(name string) bool {
				f := flags[name]
				return f != nil && f.IsBool()
			})
			var names []string
			for name := range flags {
				names = append(names, name)
			}
			slices.Sort(names)
			documented := slices.Sorted(func(yield func(string) bool) {
				for name := range u.flags {
					if !yield(name) {
						return
					}
				}
			})
			if !slices.Equal(names, documented) {
				t.Errorf("%s:%d: %s: flags %q, the program has %q", file, row.line, path, documented, names)
			}
			if u.args != len(leaf.Positional) {
				t.Errorf("%s:%d: %s: %d argument(s), the program has %d", file, row.line, path, u.args, len(leaf.Positional))
			}
			for name, value := range u.flags {
				f := flags[name]
				if f == nil {
					continue
				}
				switch {
				case f.IsBool():
					if value != "" {
						t.Errorf("%s:%d: %s: --%s takes no value", file, row.line, path, name)
					}
				case value == "":
					t.Errorf("%s:%d: %s: --%s lacks its value", file, row.line, path, name)
				case !f.HasDefault || f.PlaceHolder != "":
					if want := strings.TrimSuffix(f.FormatPlaceHolder(), ",..."); value != want {
						t.Errorf("%s:%d: %s: --%s %s, --help shows %s", file, row.line, path, name, value, f.FormatPlaceHolder())
					}
				}
			}
		}
		for path := range leaves {
			if seen[path] != 1 {
				t.Errorf("%s: %q is named %d times", file, path, seen[path])
			}
		}
	}
}
