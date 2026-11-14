package main

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/alecthomas/kong"
)

// runMainEnv makes the test binary run main() instead of the tests (set by
// runMain only).
const runMainEnv = "MIMAMORI_TEST_RUN_MAIN"

func TestMain(m *testing.M) {
	if os.Getenv(runMainEnv) == "1" {
		main()
		os.Exit(0)
	}
	os.Exit(m.Run())
}

// runMain runs the program with args in a process of its own and returns
// what it wrote on standard output and standard error and its exit code.
func runMain(t *testing.T, args ...string) (string, string, int) {
	t.Helper()
	cmd := exec.Command(os.Args[0], args...)
	cmd.Env = append(os.Environ(), runMainEnv+"=1")
	cmd.Stdin = strings.NewReader("")
	var out, errOut bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errOut
	err := cmd.Run()
	code := 0
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		code = ee.ExitCode()
	} else if err != nil {
		t.Fatal(err)
	}
	if s := out.String() + errOut.String(); strings.Contains(s, negativeMark) || strings.Contains(s, `\x00`) {
		t.Errorf("%q: the mark is in the output:\n%q", args, s)
	}
	return out.String(), errOut.String(), code
}

// exitCalled ends a parse in a test where kong would end the process.
type exitCalled int

// testParser returns the real parser of the command line, writing to out
// and ending a parse with a panic of exitCalled instead of the process.
func testParser(t *testing.T, cli *CLI, out io.Writer) *kong.Kong {
	t.Helper()
	p, err := newParser(cli, kong.Writers(out, out), kong.Exit(func(code int) { panic(exitCalled(code)) }))
	if err != nil {
		t.Fatal(err)
	}
	return p
}

// valueKind is how a test gives and checks a value of the command line.
type valueKind int

const (
	kindInt valueKind = iota
	kindString
	kindDuration
	kindBool
)

// kindOf returns the kind of the value, false for a type no mapper of ours
// reads (intListMapper reads lists of int64 that are flags).
func kindOf(v *kong.Value) (valueKind, bool) {
	t := v.Target.Type()
	switch {
	case t == reflect.TypeOf(time.Duration(0)):
		return kindDuration, true
	case t == reflect.TypeOf([]int64(nil)):
		return kindInt, v.Flag != nil
	case t.Kind() == reflect.Pointer || t.Kind() == reflect.Slice:
		t = t.Elem()
	}
	switch t.Kind() {
	case reflect.Int, reflect.Int64:
		return kindInt, true
	case reflect.String:
		return kindString, true
	case reflect.Bool:
		return kindBool, true
	}
	return 0, false
}

// cliValue is a positional argument or a flag of a command.
type cliValue struct {
	path []string // the command, from the top
	name string
	flag bool
}

func (c cliValue) String() string {
	if c.flag {
		return strings.Join(c.path, " ") + " --" + c.name
	}
	return strings.Join(c.path, " ") + " <" + c.name + ">"
}

// commandPath returns the names of the commands from the top to n.
func commandPath(n *kong.Node) []string {
	var path []string
	for ; n != nil && n.Type == kong.CommandNode; n = n.Parent {
		path = append([]string{n.Name}, path...)
	}
	return path
}

// findNode returns the command at path.
func findNode(t *testing.T, app *kong.Application, path []string) *kong.Node {
	t.Helper()
	n := app.Node
	for _, name := range path {
		var next *kong.Node
		for _, c := range n.Children {
			if c.Name == name {
				next = c
			}
		}
		if next == nil {
			t.Fatalf("no command %q", path)
		}
		n = next
	}
	return n
}

// findValue returns the value c in the model of a parser.
func findValue(t *testing.T, app *kong.Application, c cliValue) *kong.Value {
	t.Helper()
	n := findNode(t, app, c.path)
	if c.flag {
		for ; n != nil; n = n.Parent {
			for _, f := range n.Flags {
				if f.Name == c.name {
					return f.Value
				}
			}
		}
	} else {
		for _, p := range n.Positional {
			if p.Name == c.name {
				return p
			}
		}
	}
	t.Fatalf("no value %s", c)
	return nil
}

// filler is a valid text for a value that a test does not look at.
func filler(v *kong.Value) string {
	if v.Enum != "" {
		for _, e := range v.EnumSlice() {
			if e != "" {
				return e
			}
		}
	}
	switch k, _ := kindOf(v); k {
	case kindInt:
		return "1"
	case kindDuration:
		return "1s"
	case kindBool:
		return "true"
	}
	return "x"
}

// cliValues returns every positional argument and flag of the command line
// (the flags of the top level with the command version, where nothing else
// is needed) other than --help.
func cliValues(t *testing.T) []cliValue {
	t.Helper()
	var cli CLI
	p := testParser(t, &cli, io.Discard)
	var out []cliValue
	for _, f := range p.Model.Flags {
		if f.Name != "help" {
			out = append(out, cliValue{path: []string{"version"}, name: f.Name, flag: true})
		}
	}
	for _, leaf := range p.Model.Leaves(false) {
		path := commandPath(leaf)
		for _, v := range leaf.Positional {
			out = append(out, cliValue{path: path, name: v.Name})
		}
		for _, f := range leaf.Flags {
			out = append(out, cliValue{path: path, name: f.Name, flag: true})
		}
	}
	if len(out) < 100 {
		t.Fatalf("only %d values found", len(out))
	}
	return out
}

// argsWith returns a command line of the command of c that gives text to c
// (for a flag after a space, or with '=' when equals) and valid values to
// every other required argument and flag.
func argsWith(t *testing.T, c cliValue, text string, equals bool) []string {
	t.Helper()
	var cli CLI
	p := testParser(t, &cli, io.Discard)
	n := findNode(t, p.Model, c.path)
	args := append([]string{}, c.path...)
	for _, v := range n.Positional {
		if !c.flag && v.Name == c.name {
			args = append(args, text)
		} else if v.Required {
			args = append(args, filler(v))
		}
	}
	for _, f := range n.Flags {
		if f.Required && (!c.flag || f.Name != c.name) {
			args = append(args, "--"+f.Name+"="+filler(f.Value))
		}
	}
	if c.flag {
		if equals {
			args = append(args, "--"+c.name+"="+text)
		} else {
			args = append(args, "--"+c.name, text)
		}
	}
	return args
}

// parseValue parses args with the real parser and returns the value c
// received.
func parseValue(t *testing.T, c cliValue, args []string) (reflect.Value, error) {
	t.Helper()
	var cli CLI
	var out bytes.Buffer
	p := testParser(t, &cli, &out)
	v := findValue(t, p.Model, c)
	_, err := parseArgs(p, args)
	if err != nil {
		if s := err.Error(); strings.Contains(s, negativeMark) || strings.Contains(s, `\x00`) {
			t.Errorf("%q: the mark is in the error %q", args, s)
		}
		return reflect.Value{}, err
	}
	return v.Target, nil
}

// intValue returns the number an int value received (an int, a pointer to
// one or the only element of a list).
func intValue(v reflect.Value) (int64, bool) {
	switch v.Kind() {
	case reflect.Pointer:
		if v.IsNil() {
			return 0, false
		}
		v = v.Elem()
	case reflect.Slice:
		if v.Len() != 1 {
			return 0, false
		}
		v = v.Index(0)
	}
	return v.Int(), true
}

// stringValue returns the text a string value received (a string, a
// pointer to one or the only element of a list).
func stringValue(v reflect.Value) (string, bool) {
	switch v.Kind() {
	case reflect.Pointer:
		if v.IsNil() {
			return "", false
		}
		v = v.Elem()
	case reflect.Slice:
		if v.Len() != 1 {
			return "", false
		}
		v = v.Index(0)
	}
	return v.String(), true
}

// Every value of the command line is read by a mapper of ours: the test
// knows how to give each kind, and no other kind exists.
func TestEveryValueHasOurMapper(t *testing.T) {
	kinds := map[valueKind]int{}
	for _, c := range cliValues(t) {
		var cli CLI
		p := testParser(t, &cli, io.Discard)
		v := findValue(t, p.Model, c)
		k, ok := kindOf(v)
		if !ok {
			t.Errorf("%s: no mapper of ours reads a %s (add one to valueMappers)", c, v.Target.Type())
		}
		kinds[k]++
	}
	if kinds[kindInt] < 50 || kinds[kindString] < 30 || kinds[kindDuration] != 2 {
		t.Fatalf("values found by kind: %v", kinds)
	}
}

// Every integer argument and flag reads a whole number of the digits 0-9
// with an optional leading '-' (010 is ten), a negative one also written
// after a space; every other spelling is refused with an error that names
// the argument or flag and shows the value as given.
func TestEveryIntValue(t *testing.T) {
	good := map[string]int64{"-5": -5, "010": 10, "0": 0, "-010": -10, "7": 7}
	bad := []string{"+3", " 3", "3 ", "0x10", "-0x10", "1_0", "1e1", "1.5", "-1.5", "", "\uff13", "99999999999999999999"}
	n := 0
	for _, c := range cliValues(t) {
		var cli CLI
		v := findValue(t, testParser(t, &cli, io.Discard).Model, c)
		if k, _ := kindOf(v); k != kindInt {
			continue
		}
		n++
		forms := []bool{false}
		if c.flag {
			forms = append(forms, true)
		}
		for _, equals := range forms {
			for text, want := range good {
				args := argsWith(t, c, text, equals)
				got, err := parseValue(t, c, args)
				if err != nil {
					t.Errorf("%q: %v", args, err)
					continue
				}
				if n, ok := intValue(got); !ok || n != want {
					t.Errorf("%q: %s = %v, want %d", args, c, got, want)
				}
			}
			for _, text := range bad {
				args := argsWith(t, c, text, equals)
				_, err := parseValue(t, c, args)
				if err == nil {
					t.Errorf("%q accepted", args)
					continue
				}
				summary := "<" + c.name + ">"
				if c.flag {
					summary = "--" + c.name
				}
				if s := err.Error(); !strings.Contains(s, summary) || !strings.Contains(s, fmt.Sprintf("%q", text)) {
					t.Errorf("%q: error %q does not name %s and %q", args, s, summary, text)
				}
			}
		}
	}
	if n < 50 {
		t.Fatalf("only %d int values", n)
	}
}

// A text value that begins with '-' and a digit is a value wherever it
// stands, and it arrives as given (the mark is never part of it).
func TestEveryStringValue(t *testing.T) {
	n := 0
	for _, c := range cliValues(t) {
		var cli CLI
		v := findValue(t, testParser(t, &cli, io.Discard).Model, c)
		if k, _ := kindOf(v); k != kindString {
			continue
		}
		n++
		forms := []bool{false}
		if c.flag {
			forms = append(forms, true)
		}
		for _, equals := range forms {
			args := argsWith(t, c, "-1-", equals)
			got, err := parseValue(t, c, args)
			if v.Enum != "" {
				// A value outside the choices is refused and shown as given.
				if err == nil || !strings.Contains(err.Error(), `"-1-"`) {
					t.Errorf("%q: %v", args, err)
				}
				continue
			}
			if err != nil {
				t.Errorf("%q: %v", args, err)
				continue
			}
			s, ok := stringValue(got)
			if c.name == "data-dir" {
				// Made absolute.
				ok = ok && filepath.IsAbs(s)
				s = filepath.Base(s)
			}
			if !ok || s != "-1-" {
				t.Errorf("%q: %s = %q", args, c, s)
			}
		}
	}
	if n < 30 {
		t.Fatalf("only %d string values", n)
	}
}

// The stop commands take a negative duration after a space as well.
func TestDurationValues(t *testing.T) {
	for _, path := range [][]string{{"central", "stop"}, {"agent", "stop"}} {
		c := cliValue{path: path, name: "timeout", flag: true}
		for _, equals := range []bool{false, true} {
			args := argsWith(t, c, "-5s", equals)
			got, err := parseValue(t, c, args)
			if err != nil || got.Interface() != -5*time.Second {
				t.Errorf("%q: %v %v", args, got, err)
			}
		}
		if _, err := parseValue(t, c, argsWith(t, c, "-5x", false)); err == nil || !strings.Contains(err.Error(), `"-5x"`) {
			t.Errorf("-5x: %v", err)
		}
	}
}

// The cases the design names: decimal values, negative values among real
// flags, a text value, a list flag and the end of the flags.
func TestNumbersOnTheCommandLine(t *testing.T) {
	parse := func(args ...string) *CLI {
		t.Helper()
		var cli CLI
		if _, err := parseArgs(testParser(t, &cli, io.Discard), args); err != nil {
			t.Fatalf("%q: %v", args, err)
		}
		return &cli
	}
	if c := parse("ctl", "tokens", "issue", "t", "--valid-days", "010"); c.Ctl.Tokens.Issue.ValidDays != 10 {
		t.Errorf("--valid-days 010 = %d", c.Ctl.Tokens.Issue.ValidDays)
	}
	if c := parse("agent", "init", "--central-host", "h", "--central-port", "09081"); c.Agent.Init.CentralPort != 9081 {
		t.Errorf("--central-port 09081 = %d", c.Agent.Init.CentralPort)
	}
	if c := parse("agent", "set-workers", "-1", "--builtin"); c.Agent.SetWorkers.N != "-1" || !c.Agent.SetWorkers.Builtin {
		t.Errorf("set-workers -1 --builtin: %+v", c.Agent.SetWorkers)
	}
	if c := parse("central", "config", "set", "notify.subject_prefix", "-1-"); c.Central.Config.Set.Value != "-1-" {
		t.Errorf("config set: %+v", c.Central.Config.Set)
	}
	c := parse("ctl", "detections", "list", "--agent", "-1", "--limit", "-5", "--offset=-2", "--category", "alert")
	if l := c.Ctl.Detections.List; l.Agent != -1 || l.Limit != -5 || l.Offset != -2 || l.Category != "alert" {
		t.Errorf("detections list: %+v", l)
	}
	if c := parse("ctl", "rules", "apply", "3", "--agent", "-1", "--agent", "2,-4"); !reflect.DeepEqual(c.Ctl.Rules.Apply.Agent, []int64{-1, 2, -4}) {
		t.Errorf("--agent list: %v", c.Ctl.Rules.Apply.Agent)
	}
	if c := parse("ctl", "rules", "show", "--", "-5"); c.Ctl.Rules.Show.ID != -5 {
		t.Errorf("-- -5: %d", c.Ctl.Rules.Show.ID)
	}
	if c := parse("ctl", "rules", "diff", "-3", "-1", "-2"); c.Ctl.Rules.Diff.ID != -3 || c.Ctl.Rules.Diff.From != -1 || c.Ctl.Rules.Diff.To != -2 {
		t.Errorf("rules diff: %+v", c.Ctl.Rules.Diff)
	}
	if c := parse("agent", "set-central", "--host", "h", "--port", "-1"); c.Agent.SetCentral.Port == nil || *c.Agent.SetCentral.Port != -1 {
		t.Errorf("--port -1: %v", c.Agent.SetCentral.Port)
	}
	// A negative number where no value is taken is an unexpected argument,
	// shown as given.
	var cli CLI
	_, err := parseArgs(testParser(t, &cli, io.Discard), []string{"agent", "status", "-5"})
	if err == nil || err.Error() != "unexpected argument -5" {
		t.Errorf("agent status -5: %v", err)
	}
}

// Refused values end the program with exit code 2 and the usage of the
// command, which is the text of its --help; the program's own refusal of
// agent set-workers -1 is reached; a text value that begins with '-' and a
// digit is saved.
func TestCommandLineExitCodes(t *testing.T) {
	help, _, code := runMain(t, "ctl", "tokens", "issue", "--help")
	if code != 0 || !strings.Contains(help, "--valid-days") {
		t.Fatalf("help: %d\n%s", code, help)
	}
	for _, v := range []string{"0x10", "+3", " 3", "1_0", "1e1", "1.5", "", "\uff13"} {
		out, errOut, code := runMain(t, "ctl", "tokens", "issue", "t", "--valid-days", v)
		want := fmt.Sprintf("mimamori: error: --valid-days: %q is not a whole number written with the digits 0-9 (a negative one with a leading -)\n", v)
		// The usage is followed by an empty line.
		if code != 2 || out != help+"\n" || errOut != want {
			t.Errorf("--valid-days %q: exit %d\n%s\n%q", v, code, out, errOut)
		}
	}
	for _, args := range [][]string{
		{"ctl", "rules", "show", "0x10"},
		{"ctl", "rules", "diff", "1", "2", "+3"},
		{"ctl", "detections", "list", "--agent", "1_0"},
		{"ctl", "rules", "apply", "1", "--agent", "2,1e1"},
		{"agent", "set-central", "--host", "h", "--port", "1.5"},
		{"agent", "init", "--central-host", "h", "--central-port", " 9081"},
		{"ctl", "command-sets", "runs", "1", "2", "--before", "-0x1"},
	} {
		if _, errOut, code := runMain(t, args...); code != 2 || !strings.HasPrefix(errOut, "mimamori: error: ") || !strings.Contains(errOut, "is not a whole number") {
			t.Errorf("%q: exit %d %q", args, code, errOut)
		}
	}

	dir := t.TempDir()
	for _, args := range [][]string{{"agent", "set-workers", "-1", "--builtin"}, {"agent", "set-workers", "-1"}, {"agent", "set-workers", "0x10"}} {
		if _, errOut, code := runMain(t, append([]string{"--data-dir", dir}, args...)...); code != 2 || errOut != "Error: the agent's worker count must be a whole number from 1 to 64\n" {
			t.Errorf("%q: exit %d %q", args, code, errOut)
		}
	}
	if out, errOut, code := runMain(t, "--data-dir", dir, "central", "config", "set", "notify.subject_prefix", "-1-"); code != 0 {
		t.Fatalf("config set: exit %d %s %s", code, out, errOut)
	}
	if out, errOut, code := runMain(t, "--data-dir", dir, "central", "config", "get", "notify.subject_prefix"); code != 0 || out != "-1-\n" {
		t.Fatalf("config get: exit %d %q %s", code, out, errOut)
	}
	if _, errOut, code := runMain(t, "--data-dir", dir, "central", "config", "set", "retention_days", "-1"); code != 2 || errOut != "Error: retention_days must be a positive integer\n" {
		t.Errorf("config set retention_days -1: exit %d %q", code, errOut)
	}
}

// The mappers of ours change no help text: the help of every command is
// the same as with kong's own mappers.
func TestHelpUnchanged(t *testing.T) {
	helpOf := func(p *kong.Kong, out *bytes.Buffer, path []string) string {
		t.Helper()
		out.Reset()
		func() {
			defer func() {
				if v := recover(); v != nil {
					if _, ok := v.(exitCalled); !ok {
						panic(v)
					}
				}
			}()
			_, _ = p.Parse(append(append([]string{}, path...), "--help"))
		}()
		return out.String()
	}
	var ours, plain CLI
	var oursOut, plainOut bytes.Buffer
	p := testParser(t, &ours, &oursOut)
	base, err := kong.New(&plain, append(parserOptions(), kong.Writers(&plainOut, &plainOut), kong.Exit(func(code int) { panic(exitCalled(code)) }))...)
	if err != nil {
		t.Fatal(err)
	}
	paths := [][]string{nil}
	for _, leaf := range p.Model.Leaves(false) {
		paths = append(paths, commandPath(leaf))
	}
	for _, path := range paths {
		got, want := helpOf(p, &oursOut, path), helpOf(base, &plainOut, path)
		if got == "" || got != want {
			t.Errorf("help of %q differs:\n%s\n---\n%s", path, got, want)
		}
	}
}
