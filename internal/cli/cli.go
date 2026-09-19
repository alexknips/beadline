// Package cli implements the beadline command line: subcommand dispatch,
// usage text and exit codes. main only wires os.Args and the standard streams.
package cli

import (
	"fmt"
	"io"
	"runtime/debug"
)

// Exit codes.
const (
	ExitOK      = 0
	ExitFailure = 1
	ExitUsage   = 2
)

// Version is set at build time with
// -ldflags "-X github.com/alexknips/beadline/internal/cli.Version=v0.1.0".
var Version = ""

type command struct {
	name    string
	summary string
	run     func(args []string, stdout, stderr io.Writer) int
}

func commands() []command {
	return []command{
		{"check", "load the configured exports and report graph problems", runCheck},
		{"forecast", "simulate the remaining graph and write roadmap.json", notImplemented("forecast")},
		{"render", "render roadmap.json as a single-file HTML page", notImplemented("render")},
		{"serve", "preview the rendered roadmap locally", notImplemented("serve")},
		{"version", "print the beadline version", runVersion},
		{"help", "show this help", runHelp},
	}
}

// Run executes the command line args (without the program name) and returns
// the process exit code.
func Run(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		usage(stderr)
		return ExitUsage
	}
	name := args[0]
	if name == "-h" || name == "--help" {
		name = "help"
	}
	for _, c := range commands() {
		if c.name == name {
			return c.run(args[1:], stdout, stderr)
		}
	}
	fmt.Fprintf(stderr, "beadline: unknown command %q\n\n", args[0])
	usage(stderr)
	return ExitUsage
}

func usage(w io.Writer) {
	fmt.Fprint(w, "beadline: roadmaps and AI-delivery forecasts for beads\n\nUsage:\n  beadline <command> [flags]\n\nCommands:\n")
	for _, c := range commands() {
		fmt.Fprintf(w, "  %-9s %s\n", c.name, c.summary)
	}
}

func runHelp(_ []string, stdout, _ io.Writer) int {
	usage(stdout)
	return ExitOK
}

func runVersion(_ []string, stdout, _ io.Writer) int {
	fmt.Fprintf(stdout, "beadline %s\n", version())
	return ExitOK
}

// version prefers the linker-set Version, then the module version recorded
// by `go install ...@vX.Y.Z`, then "dev".
func version() string {
	if Version != "" {
		return Version
	}
	if info, ok := debug.ReadBuildInfo(); ok && info.Main.Version != "" && info.Main.Version != "(devel)" {
		return info.Main.Version
	}
	return "dev"
}

func notImplemented(name string) func([]string, io.Writer, io.Writer) int {
	return func(_ []string, _, stderr io.Writer) int {
		fmt.Fprintf(stderr, "beadline %s: not implemented yet\n", name)
		return ExitFailure
	}
}
