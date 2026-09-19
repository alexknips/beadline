package cli

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/alexknips/beadline/internal/calibrate"
	"github.com/alexknips/beadline/internal/config"
	"github.com/alexknips/beadline/internal/forecast"
	"github.com/alexknips/beadline/internal/graph"
	"github.com/alexknips/beadline/internal/load"
)

const checkHelp = `Usage: beadline check [PATH...] [flags]

Grade past forecasts: every snapshot that 'beadline' recorded is scored
against what has closed since. With --backtest, replay history instead: fit
only on the data before each origin, forecast, and score what happened. PATH
works as for 'beadline'.

Flags:
      --backtest SPAN   replay this span of history, e.g. 60d, 8w or 36h
      --json            print the report as JSON, with every graded forecast
  -c, --config FILE     settings and repos (default ./beadline.toml, if present)
  -h, --help            show this help

Expert flags:
      --step SPAN       time between backtest origins (default 3.5d)
      --runs N          simulated schedules per backtest forecast
                        (default expert.runs, 2000)
      --snapshots DIR   snapshot directory (default .beadline/snapshots beside
                        beadline.toml, else in the current directory)
      --bd PATH         bd binary that reads repository directories (default bd)

Exit status: 0 ok; 1 failure, including a repo that could not be read; 2
usage error.
`

// runCheck grades past forecasts against what closed since: the snapshots
// that 'beadline' recorded, or, with --backtest, forecasts replayed from
// the history in the exports.
func runCheck(args []string, stdout, stderr io.Writer) int {
	fs := newFlagSet()
	var repos repoFlags
	repos.register(fs)
	snapshots := fs.String("snapshots", "", "")
	backtest := fs.String("backtest", "", "")
	step := fs.String("step", "3.5d", "")
	runs := fs.Int("runs", 0, "")
	asJSON := fs.Bool("json", false, "")
	paths, err := parseArgs(fs, args)
	if err != nil {
		return flagError(stdout, stderr, "check", checkHelp, err)
	}
	var span, every time.Duration
	if *backtest != "" {
		if span, err = parseSpan(*backtest); err != nil {
			return usageError(stderr, "check", "--backtest: %v", err)
		}
		if every, err = parseSpan(*step); err != nil {
			return usageError(stderr, "check", "--step: %v", err)
		}
	}
	set := map[string]bool{}
	fs.Visit(func(f *flag.Flag) { set[f.Name] = true })
	if set["runs"] && *runs < 1 {
		return usageError(stderr, "check", "--runs must be at least 1")
	}
	fail := func(err error) int {
		fmt.Fprintf(stderr, "beadline check: %v\n", err)
		return ExitFailure
	}

	cfg, code := repos.resolve("check", paths, stderr)
	if code != ExitOK {
		return code
	}
	if set["runs"] {
		cfg.Model.Simulations = *runs
	}
	// Grading needs every repo: a forecast of a repo that is missing would
	// look descoped.
	exports, failed := repos.read(cfg, stderr)
	if failed > 0 {
		return fail(fmt.Errorf("%s could not be read; nothing graded", plural(failed, "repo")))
	}
	ex, err := load.ParseExports(exports)
	if err != nil {
		return fail(err)
	}

	var rep *calibrate.Report
	if *backtest == "" {
		dir := snapshotDir(cfg, *snapshots)
		snaps, problems, err := calibrate.ReadDir(dir)
		if err != nil {
			return fail(err)
		}
		for _, p := range problems {
			fmt.Fprintf(stderr, "warning: skipped %v\n", p)
		}
		if len(snaps) == 0 && !*asJSON {
			fmt.Fprintf(stdout, "no snapshots in %s: every run of 'beadline' records one a day\n", dir)
			return ExitOK
		}
		g, _, err := ex.Graph(&cfg.Conventions, time.Time{})
		if err != nil {
			return fail(err)
		}
		horizon := ex.Horizon()
		rep = calibrate.NewReport("snapshots", horizon, calibrate.Grade(snaps, calibrate.NewReality(g, horizon)))
		for _, s := range snaps {
			rep.Snapshots = append(rep.Snapshots, filepath.Base(s.Path))
		}
		for _, p := range problems {
			rep.Problems = append(rep.Problems, p.Error())
		}
	} else {
		fc := func(g *graph.Graph, lr *load.Report, asOf time.Time) (*forecast.Result, error) {
			in, err := forecastInputs(cfg, g, lr, asOf)
			if err != nil {
				return nil, err
			}
			return forecast.Run(g, in.options)
		}
		pairs, origins, err := calibrate.Backtest(ex, &cfg.Conventions, fc, calibrate.BacktestOptions{
			Span: span, Step: every, WindowDays: cfg.Model.WindowDays, Seed: cfg.Model.Seed, Draws: cfg.Model.Simulations,
		})
		if err != nil {
			return fail(err)
		}
		rep = calibrate.NewReport("backtest", ex.Horizon(), pairs)
		rep.Origins, rep.Runs = origins, cfg.Model.Simulations
		if skew := ex.DependencySkew(); skew.Offset != 0 {
			since := skew.Since
			rep.DependencySkewHours, rep.SkewSince = skew.Offset.Hours(), &since
		}
	}

	if *asJSON {
		enc := json.NewEncoder(stdout)
		enc.SetIndent("", "  ")
		if err := enc.Encode(rep); err != nil {
			return fail(err)
		}
		return ExitOK
	}
	rep.WriteText(stdout)
	return ExitOK
}

// snapshotDir is where snapshots live: the --snapshots flag, or
// .beadline/snapshots beside beadline.toml, else in the working directory.
func snapshotDir(cfg *config.Config, flagValue string) string {
	if flagValue != "" {
		return flagValue
	}
	return filepath.Join(cfg.Dir, ".beadline", "snapshots")
}

// parseSpan reads a positive duration in hours, days or weeks: 36h, 3.5d,
// 2w.
func parseSpan(s string) (time.Duration, error) {
	units := map[string]time.Duration{"h": time.Hour, "d": 24 * time.Hour, "w": 7 * 24 * time.Hour}
	for suffix, unit := range units {
		if n, ok := strings.CutSuffix(s, suffix); ok {
			v, err := strconv.ParseFloat(n, 64)
			if err != nil || !(v > 0) || v > 100*365 {
				break
			}
			return time.Duration(v * float64(unit)), nil
		}
	}
	return 0, fmt.Errorf("%q is not a positive number of hours, days or weeks (e.g. 36h, 60d, 2w)", s)
}
