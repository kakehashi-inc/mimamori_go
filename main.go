package main

import (
	"errors"
	"fmt"
	"os"
	"reflect"
	"strconv"
	"strings"
	"syscall"
	"time"

	"mimamori/app/modules"
	_ "mimamori/app/workers" // registers modules.StartCentral via init()

	"github.com/alecthomas/kong"
)

// version is injected at release time via -ldflags "-X main.version=...".
var version = "dev"

// CLI defines the top-level command structure. Startup parameter parsing lives
// here; the commands themselves are implemented in app/modules.
type CLI struct {
	DataDir string `help:"Data directory (default: data/ next to the executable)" name:"data-dir" type:"datadir" placeholder:"DIR"`

	Central modules.CentralCmd `cmd:"" help:"Central service and its local administration"`
	Agent   modules.AgentCmd   `cmd:"" help:"Agent initialization and service"`
	Ctl     modules.CtlCmd     `cmd:"" help:"Management commands (run on the central host; uses the management API on the agent_listen address, the loopback address when it listens on every address)"`
	Version VersionCmd         `cmd:"" help:"Show version"`
}

// VersionCmd prints the version.
type VersionCmd struct{}

func (v *VersionCmd) Run() error {
	fmt.Println(version)
	return nil
}

// negativeMark is put in front of every argument that begins with '-' and a
// digit before the command line is parsed (markNegativeNumbers). kong takes
// every argument that begins with '-' for a flag; an argument with the mark
// is a value wherever it stands: a positional argument, or the value of the
// flag before it. The mark is a NUL byte, which no argument of a process
// can contain. Every value is read by a mapper of ours (popValue), which
// removes the mark, and the text of a parse error is shown without it
// (unmarkedError).
const negativeMark = "\x00"

// isNegativeNumber tells whether an argument begins with '-' and a digit.
func isNegativeNumber(arg string) bool {
	return len(arg) >= 2 && arg[0] == '-' && arg[1] >= '0' && arg[1] <= '9'
}

// markNegativeNumbers returns args with negativeMark in front of every
// argument that begins with '-' and a digit.
func markNegativeNumbers(args []string) []string {
	out := make([]string, len(args))
	for i, a := range args {
		if isNegativeNumber(a) {
			a = negativeMark + a
		}
		out[i] = a
	}
	return out
}

// popValue takes the next value for a mapper and returns its text without
// negativeMark. what names the kind of value in kong's message for a
// missing value. An element after the separator of a list flag (--agent
// 3,-5) is not marked, because the mark is put on whole arguments: an
// element that begins with '-' and a digit is taken as a value as well.
func popValue(ctx *kong.DecodeContext, what string) (string, error) {
	if t := ctx.Scan.Peek(); t.Type == kong.UntypedToken {
		if s, ok := t.Value.(string); ok && isNegativeNumber(s) {
			ctx.Scan.Pop()
			return s, nil
		}
	}
	t, err := ctx.Scan.PopValue(what)
	if err != nil {
		return "", err
	}
	s, ok := t.Value.(string)
	if !ok {
		s = fmt.Sprint(t.Value)
	}
	return strings.TrimPrefix(s, negativeMark), nil
}

// parseInt reads a number of bits bits as a whole number of the digits 0-9
// with an optional leading '-' (modules.ParseWholeNumber: 010 is ten). kong
// puts the name of the argument or flag in front of the error.
func parseInt(s string, bits int) (int64, error) {
	n, err := modules.ParseWholeNumber(s, bits)
	if errors.Is(err, modules.ErrWholeNumberRange) {
		return 0, fmt.Errorf("%q is out of range (%d to %d)", s, int64(-1)<<(bits-1), int64(uint64(1)<<(bits-1)-1))
	}
	if err != nil {
		return 0, fmt.Errorf("%q is not a whole number written with the digits 0-9 (a negative one with a leading -)", s)
	}
	return n, nil
}

// intMapper reads an integer argument or flag value of bits bits
// (parseInt).
func intMapper(bits int) kong.MapperFunc {
	return func(ctx *kong.DecodeContext, target reflect.Value) error {
		s, err := popValue(ctx, "int")
		if err != nil {
			return err
		}
		n, err := parseInt(s, bits)
		if err != nil {
			return err
		}
		target.SetInt(n)
		return nil
	}
}

// intListMapper reads the value of a list flag of int64 numbers (--agent
// 1,2, also repeated): each element between the flag's separators is read
// with parseInt and appended. A value without any element is refused like
// an empty number.
func intListMapper(ctx *kong.DecodeContext, target reflect.Value) error {
	s, err := popValue(ctx, "int")
	if err != nil {
		return err
	}
	elems := kong.SplitEscaped(s, ctx.Value.Tag.Sep)
	if len(elems) == 0 {
		elems = []string{s}
	}
	for _, e := range elems {
		n, err := parseInt(e, 64)
		if err != nil {
			return err
		}
		target.Set(reflect.Append(target, reflect.ValueOf(n)))
	}
	return nil
}

// stringMapper reads a text argument or flag value.
func stringMapper(ctx *kong.DecodeContext, target reflect.Value) error {
	s, err := popValue(ctx, "string")
	if err != nil {
		return err
	}
	target.SetString(s)
	return nil
}

// durationMapper reads a duration flag value (time.ParseDuration, as kong
// does).
func durationMapper(ctx *kong.DecodeContext, target reflect.Value) error {
	s, err := popValue(ctx, "duration")
	if err != nil {
		return err
	}
	d, err := time.ParseDuration(s)
	if err != nil {
		return fmt.Errorf("expected duration but got %q: %v", s, err)
	}
	target.Set(reflect.ValueOf(d))
	return nil
}

// dataDirMapper reads the --data-dir value like kong's "path" type (made
// absolute, "~/" expanded) but rejects an empty value, which would otherwise
// become the current directory.
func dataDirMapper(ctx *kong.DecodeContext, target reflect.Value) error {
	path, err := popValue(ctx, "file")
	if err != nil {
		return err
	}
	if path == "" {
		return errors.New("the data directory must not be empty")
	}
	target.SetString(kong.ExpandPath(path))
	return nil
}

// unmarkedError is a parse error whose text shows the arguments as they
// were given: negativeMark is removed, also in the quoted form of kong's
// messages. kong's ParseError stays reachable for the usage text.
type unmarkedError struct{ err error }

func (e unmarkedError) Error() string {
	s := strings.ReplaceAll(e.err.Error(), `"\x00`, `"`)
	return strings.ReplaceAll(s, negativeMark, "")
}

func (e unmarkedError) Unwrap() error { return e.err }

// parserOptions are the options of the parser other than the mappers of
// the values (valueMappers).
func parserOptions() []kong.Option {
	return []kong.Option{
		kong.Name(modules.AppName),
		kong.Description("mimamori: detects and reports operations performed by selected OS users on each host."),
		kong.UsageOnError(),
		kong.ConfigureHelp(kong.HelpOptions{Compact: true}),
		kong.Vars{"default_agent_port": strconv.Itoa(modules.DefaultAgentPort)},
	}
}

// valueMappers are the mappers of every kind of value the commands take:
// int and int64 (also behind a pointer), list flags of int64, strings,
// durations and the data directory. Each takes its value with popValue.
func valueMappers() []kong.Option {
	return []kong.Option{
		kong.NamedMapper("datadir", kong.MapperFunc(dataDirMapper)),
		kong.KindMapper(reflect.Int, intMapper(strconv.IntSize)),
		kong.KindMapper(reflect.Int64, intMapper(64)),
		kong.TypeMapper(reflect.TypeOf([]int64(nil)), kong.MapperFunc(intListMapper)),
		kong.KindMapper(reflect.String, kong.MapperFunc(stringMapper)),
		kong.TypeMapper(reflect.TypeOf(time.Duration(0)), kong.MapperFunc(durationMapper)),
	}
}

// newParser returns the parser of the command line that fills cli, with
// further options (tests give writers and an exit function).
func newParser(cli *CLI, options ...kong.Option) (*kong.Kong, error) {
	all := append(parserOptions(), valueMappers()...)
	return kong.New(cli, append(all, options...)...)
}

// parseArgs parses the command line args (without the program name):
// arguments that begin with '-' and a digit are values, and an error shows
// the arguments as they were given.
func parseArgs(parser *kong.Kong, args []string) (*kong.Context, error) {
	ctx, err := parser.Parse(markNegativeNumbers(args))
	if err != nil {
		return nil, unmarkedError{err}
	}
	return ctx, nil
}

func main() {
	// Files created by this program (databases, configuration, keys) must not
	// be readable by other users.
	syscall.Umask(0o077)

	// Wire binary-embedded assets into the modules package.
	modules.MigrationsFS = migrationsFS
	modules.FrontendFS = frontendFS
	modules.AppVersion = version

	var cli CLI
	parser, err := newParser(&cli)
	if err != nil {
		panic(err)
	}
	ctx, err := parseArgs(parser, os.Args[1:])
	if err != nil {
		// Errors in the command line (unknown commands or flags, missing or
		// invalid values) are argument errors. --help exits inside Parse.
		parser.Exit = func(int) { os.Exit(modules.ExitArgument) }
		parser.FatalIfErrorf(err)
	}
	modules.SetDataDir(cli.DataDir)

	err = ctx.Run(&cli.Ctl)
	if err != nil {
		var exitErr *modules.ExitError
		if errors.As(err, &exitErr) {
			// An empty message only sets the exit code (for example "status"
			// of a service that is not running, which is not an error).
			if exitErr.Message != "" {
				fmt.Fprintf(os.Stderr, "Error: %s\n", exitErr.Message)
			}
			os.Exit(exitErr.Code)
		}
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		os.Exit(modules.ExitGeneral)
	}
}
