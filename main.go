package main

import (
	"errors"
	"fmt"
	"os"
	"reflect"
	"strconv"
	"syscall"

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

// dataDirMapper reads the --data-dir value like kong's "path" type (made
// absolute, "~/" expanded) but rejects an empty value, which would otherwise
// become the current directory.
func dataDirMapper(ctx *kong.DecodeContext, target reflect.Value) error {
	var path string
	if err := ctx.Scan.PopValueInto("file", &path); err != nil {
		return err
	}
	if path == "" {
		return errors.New("the data directory must not be empty")
	}
	target.SetString(kong.ExpandPath(path))
	return nil
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
	parser, err := kong.New(&cli,
		kong.Name(modules.AppName),
		kong.Description("mimamori: detects and reports operations performed by selected OS users on each host."),
		kong.UsageOnError(),
		kong.ConfigureHelp(kong.HelpOptions{Compact: true}),
		kong.Vars{"default_agent_port": strconv.Itoa(modules.DefaultAgentPort)},
		kong.NamedMapper("datadir", kong.MapperFunc(dataDirMapper)),
	)
	if err != nil {
		panic(err)
	}
	ctx, err := parser.Parse(os.Args[1:])
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
