package cli

import (
	"fmt"
	"io"
	"sort"
	"strings"
	"time"

	"github.com/alexknips/beadline/internal/graph"
	"github.com/alexknips/beadline/internal/load"
)

const doctorHelp = `Usage: beadline doctor [PATH...] [flags]

Read the repos, say what was loaded and report problems in the data: cycles,
dependencies on beads that no loaded repo has, and duplicate IDs. A cycle
among open beads is an error, since no schedule can satisfy it; the rest are
warnings. PATH works as for 'beadline'.

Flags:
  -c, --config FILE   settings and repos (default ./beadline.toml, if present)
      --bd PATH       bd binary that reads repository directories (default bd)
  -h, --help          show this help

Exit status: 0 no errors; 1 an error in the data, or a repo that could not be
read; 2 usage error.
`

// runDoctor reads the repos, prints what was loaded and reports graph
// problems. Cycles among open beads are errors (they can never be
// scheduled); everything else is a warning.
func runDoctor(args []string, stdout, stderr io.Writer) int {
	fs := newFlagSet()
	var repos repoFlags
	repos.register(fs)
	paths, err := parseArgs(fs, args)
	if err != nil {
		return flagError(stdout, stderr, "doctor", doctorHelp, err)
	}
	cfg, code := repos.resolve("doctor", paths, stderr)
	if code != ExitOK {
		return code
	}
	exports, failed := repos.read(cfg, stderr)
	l, bad, err := parseExports(cfg, exports, time.Time{}, stderr)
	failed += bad
	if err != nil {
		fmt.Fprintf(stderr, "beadline doctor: %v\n", err)
		return ExitFailure
	}
	g, rep := l.graph, l.report

	live := map[string]bool{}
	for _, e := range l.exports {
		live[e.Repo] = e.Live
	}
	for _, r := range rep.Repos {
		from := r.Source
		if live[r.Repo] {
			from = "bd export in " + from
		}
		fmt.Fprintf(stdout, "repo %s: %s from %s%s\n", r.Repo, plural(r.Issues, "issue"), from, skipped(r))
	}
	gates := 0
	for _, i := range g.Issues() {
		if i.HumanGate {
			gates++
		}
	}
	fmt.Fprintf(stdout, "%s, %d high-level, %s, %s\n",
		plural(g.Len(), "issue"), len(g.HighLevel()), plural(len(g.Goals()), "goal"), plural(gates, "human gate"))

	errs, warns := 0, 0
	report := func(isErr bool, format string, a ...any) {
		level := "warning"
		if isErr {
			level = "error"
			errs++
		} else {
			warns++
		}
		fmt.Fprintf(stderr, "%s: %s\n", level, fmt.Sprintf(format, a...))
	}
	for _, c := range rep.Cycles {
		state := "closed"
		if c.Open {
			state = "open"
		}
		kind := "blocking"
		if c.Kind == graph.ParentChild {
			kind = "parent-child"
		}
		report(c.Open, "%s cycle among %s beads: %s", kind, state, strings.Join(c.IDs, ", "))
	}
	for _, d := range rep.Dangling {
		report(false, "%s (repo %s): %s dependency %s is not in any loaded repo", d.From, d.Repo, d.Kind, d.To)
	}
	for _, d := range rep.Duplicates {
		report(false, "%s at %s:%d (repo %s) duplicates the issue loaded from repo %s; ignored", d.ID, d.Source, d.Line, d.Repo, d.Kept)
	}
	if errs+warns > 0 {
		fmt.Fprintf(stdout, "%s, %s\n", plural(errs, "error"), plural(warns, "warning"))
	}
	if errs > 0 || failed > 0 {
		return ExitFailure
	}
	return ExitOK
}

func skipped(r load.RepoStats) string {
	var parts []string
	reasons := make([]string, 0, len(r.Infra))
	for reason := range r.Infra {
		reasons = append(reasons, reason)
	}
	sort.Strings(reasons)
	for _, reason := range reasons {
		parts = append(parts, fmt.Sprintf("%d %s", r.Infra[reason], reason))
	}
	if r.Other > 0 {
		parts = append(parts, fmt.Sprintf("%d non-issue", r.Other))
	}
	if len(parts) == 0 {
		return ""
	}
	return " (skipped " + strings.Join(parts, ", ") + ")"
}

func plural(n int, noun string) string {
	if n == 1 {
		return "1 " + noun
	}
	return fmt.Sprintf("%d %ss", n, noun)
}
