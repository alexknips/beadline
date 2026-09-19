// Package cli implements the beadline command line: the one command that
// forecasts and writes the roadmap, the check and doctor subcommands, help,
// version and exit codes. main only wires os.Args and the standard streams.
package cli

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"runtime/debug"
	"strings"

	"github.com/alexknips/beadline/internal/config"
)

// Exit codes.
const (
	ExitOK      = 0
	ExitFailure = 1 // includes a partial failure: some repos could not be read
	ExitUsage   = 2 // bad flags, stray arguments, paths that are not repos
)

// Version is set at build time with
// -ldflags "-X github.com/alexknips/beadline/internal/cli.Version=v0.1.0".
var Version = ""

type command struct {
	name    string
	summary string
	help    string // full help: usage line and flags
	run     func(args []string, stdout, stderr io.Writer) int
	hidden  bool // works, but is not listed in the usage
}

func commands() []command {
	return []command{
		{"check", "grade past forecasts against what closed since", checkHelp, runCheck, false},
		{"doctor", "check the data: cycles, dangling dependencies, duplicate IDs", doctorHelp, runDoctor, false},
		{"version", "print the beadline version", versionHelp, runVersion, false},
		{"help", "show help for beadline or a command", helpHelp, runHelp, false},
		{"forecast", "print the forecast table of a beadline.toml", forecastHelp, runForecast, true},
		{"render", "render a roadmap.json as a single-file HTML page", renderHelp, runRender, true},
	}
}

func lookup(name string) (command, bool) {
	for _, c := range commands() {
		if c.name == name {
			return c, true
		}
	}
	return command{}, false
}

// Run executes the command line args (without the program name) and returns
// the process exit code. A first argument that names a command runs it;
// anything else is the one command: beadline [PATH...] [flags].
func Run(args []string, stdout, stderr io.Writer) int {
	if len(args) > 0 {
		if c, ok := lookup(args[0]); ok {
			return c.run(args[1:], stdout, stderr)
		}
	}
	return runRoot(args, stdout, stderr)
}

const usageText = `beadline: roadmaps and AI-delivery forecasts for beads

Usage:
  beadline [PATH...] [flags]   forecast; write roadmap.html and roadmap.json; record a snapshot
  beadline check [PATH...]     grade past forecasts against what closed since
  beadline doctor [PATH...]    check the data: cycles, dangling dependencies, duplicate IDs
  beadline version             print the beadline version
  beadline help [COMMAND]      show help for beadline or a command

PATH is a repository directory with .beads, read live with 'bd export', or a
'bd export' JSONL file. Without PATH, beadline reads the repos listed in
./beadline.toml, else the current directory. One repo needs no config.
A PATH that is also a command name needs a directory prefix: ./check.
`

func usage(w io.Writer) {
	fmt.Fprint(w, usageText)
	fmt.Fprint(w, "\n"+rootFlagsHelp)
}

const helpHelp = `Usage: beadline help [COMMAND]

Show help for beadline, or for one command: check, doctor, version.
`

func runHelp(args []string, stdout, stderr io.Writer) int {
	args, err := parseArgs(newFlagSet(), args)
	if err != nil {
		return flagError(stdout, stderr, "help", helpHelp, err)
	}
	switch {
	case len(args) == 0:
		usage(stdout)
		return ExitOK
	case len(args) > 1:
		return usageError(stderr, "help", "takes one command, got %q", strings.Join(args, " "))
	}
	if c, ok := lookup(args[0]); ok {
		fmt.Fprint(stdout, c.help)
		return ExitOK
	}
	return usageError(stderr, "help", "unknown command %q%s", args[0], didYouMean(args[0]))
}

const versionHelp = `Usage: beadline version

Print the beadline version. Also: beadline --version, beadline -v.
`

func runVersion(args []string, stdout, stderr io.Writer) int {
	fs := newFlagSet()
	args, err := parseArgs(fs, args)
	if err != nil {
		return flagError(stdout, stderr, "version", versionHelp, err)
	}
	if len(args) > 0 {
		return usageError(stderr, "version", "unexpected argument %q", args[0])
	}
	fmt.Fprintf(stdout, "beadline %s\n", version())
	return ExitOK
}

// version prefers the linker-set Version, then the module version recorded
// by `go install ...@vX.Y.Z`, then the VCS revision the binary was built
// from ("dev-<revision>"), then "dev".
func version() string {
	if Version != "" {
		return Version
	}
	if info, ok := debug.ReadBuildInfo(); ok && info.Main.Version != "" && info.Main.Version != "(devel)" {
		return info.Main.Version
	}
	if rev := commit(); rev != "" {
		short, dirty := strings.CutSuffix(rev, "-dirty")
		if len(short) > 12 {
			short = short[:12]
		}
		if dirty {
			short += "-dirty"
		}
		return "dev-" + short
	}
	return "dev"
}

// commit is the VCS revision the binary was built from, with "-dirty" for
// uncommitted changes; empty when the build did not record one.
func commit() string {
	info, ok := debug.ReadBuildInfo()
	if !ok {
		return ""
	}
	var rev string
	dirty := false
	for _, s := range info.Settings {
		switch s.Key {
		case "vcs.revision":
			rev = s.Value
		case "vcs.modified":
			dirty = s.Value == "true"
		}
	}
	if rev != "" && dirty {
		rev += "-dirty"
	}
	return rev
}

// usageError reports a usage error of a command ("" for the one command)
// and returns ExitUsage.
func usageError(stderr io.Writer, cmd, format string, a ...any) int {
	name := "beadline"
	if cmd != "" {
		name += " " + cmd
	}
	fmt.Fprintf(stderr, "%s: %s\n", name, fmt.Sprintf(format, a...))
	fmt.Fprintf(stderr, "Run '%s' for usage.\n", strings.Replace(name, "beadline", "beadline help", 1))
	return ExitUsage
}

// parseArgs parses flags that may come before, between or after the
// positional arguments, which it returns. "--" ends the flags.
func parseArgs(fs *flag.FlagSet, args []string) ([]string, error) {
	var pos []string
	for {
		if err := fs.Parse(args); err != nil {
			return nil, err
		}
		rest := fs.Args()
		if len(rest) == 0 {
			return pos, nil
		}
		if consumed := len(args) - len(rest); consumed > 0 && args[consumed-1] == "--" {
			return append(pos, rest...), nil
		}
		pos = append(pos, rest[0])
		args = rest[1:]
	}
}

// newFlagSet returns a flag set that prints nothing: the caller reports
// its errors, and each command prints its own help.
func newFlagSet() *flag.FlagSet {
	fs := flag.NewFlagSet("beadline", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	fs.Usage = func() {}
	return fs
}

// flagError handles a flag parsing error: -h and --help print the
// command's help to stdout and succeed, anything else is a usage error.
func flagError(stdout, stderr io.Writer, cmd, help string, err error) int {
	if errors.Is(err, flag.ErrHelp) {
		fmt.Fprint(stdout, help)
		return ExitOK
	}
	return usageError(stderr, cmd, "%v", err)
}

// didYouMean suggests the command a mistyped word most likely meant.
func didYouMean(word string) string {
	best, bestDist := "", 3
	for _, c := range commands() {
		if c.hidden {
			continue
		}
		if d := config.Distance(word, c.name); d < bestDist {
			best, bestDist = c.name, d
		}
	}
	if best == "" {
		return ""
	}
	return fmt.Sprintf("; did you mean 'beadline %s'?", best)
}
