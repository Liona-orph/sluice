// Command sluice runs the gateway and the tools that read what it wrote.
//
// Subcommands:
//
//	serve    run the HTTP gateway
//	replay   re-price an audit log against a different configuration
//	version  print the build identity
//
// Configuration precedence, highest first: command-line flags, environment
// variables, the config file, the built-in defaults. That order is the
// conventional one and it is the one that makes a container work: the image
// carries a file, the orchestrator overrides an address or a log level through
// the environment, and an operator debugging by hand overrides both from the
// shell without editing anything.
//
// Precedence is implemented by applying the layers in order and letting later
// ones win, with flags detected by whether they were actually set rather than
// by whether they differ from their default -- otherwise `--log-level info`
// would be indistinguishable from not passing it, and could not override a file
// that said debug.
package main

import (
	"fmt"
	"os"
	"runtime"
	"runtime/debug"
)

// Build identity, set with -ldflags at link time:
//
//	-X main.version=v1.2.3 -X main.commit=abc1234 -X main.buildDate=2026-08-09
//
// The defaults are what an unstamped `go build` or `go install` produces, and
// they say so rather than claiming a version number nobody assigned.
var (
	version   = "dev"
	commit    = ""
	buildDate = ""
)

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "sluice: "+err.Error())
		os.Exit(1)
	}
}

func run(args []string) error {
	if len(args) == 0 {
		usage(os.Stderr)
		return errNoCommand
	}
	switch args[0] {
	case "serve":
		return runServe(args[1:])
	case "replay":
		return runReplay(args[1:])
	case "version", "--version", "-version", "-v":
		fmt.Println(versionString())
		return nil
	case "help", "--help", "-h":
		usage(os.Stdout)
		return nil
	default:
		usage(os.Stderr)
		return fmt.Errorf("unknown command %q", args[0])
	}
}

type constError string

func (e constError) Error() string { return string(e) }

const errNoCommand = constError("a command is required")

// versionString reports the build identity, falling back to the module version
// the Go toolchain stamps into a binary installed with `go install`, so that an
// unstamped build still says something true.
func versionString() string {
	v := version
	c := commit
	if info, ok := debug.ReadBuildInfo(); ok {
		if v == "dev" && info.Main.Version != "" && info.Main.Version != "(devel)" {
			v = info.Main.Version
		}
		if c == "" {
			for _, s := range info.Settings {
				if s.Key == "vcs.revision" {
					c = s.Value
					if len(c) > 12 {
						c = c[:12]
					}
				}
			}
		}
	}
	out := "sluice " + v
	if c != "" {
		out += " (" + c + ")"
	}
	if buildDate != "" {
		out += " built " + buildDate
	}
	return out + ", " + runtime.Version() + " " + runtime.GOOS + "/" + runtime.GOARCH
}

func usage(w *os.File) {
	fmt.Fprint(w, `sluice - an LLM gateway that measures cost, redacts prompts,
                caches answers and fails over between providers.

usage:
  sluice serve   [flags]   run the HTTP gateway
  sluice replay  [flags]   re-price an audit log against another configuration
  sluice version           print the build identity

Run "sluice serve --help" or "sluice replay --help" for the flags of each.
`)
}
