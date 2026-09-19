package cli

import (
	"encoding/json"
	"errors"
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

// runCheck grades past forecasts against what closed since: the snapshots
// that forecast -record wrote, or, with -backtest, forecasts replayed from
// the history in the exports.
func runCheck(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("check", flag.ContinueOnError)
	fs.SetOutput(stderr)
	configPath := fs.String("config", "beadline.toml", "path to beadline.toml")
	snapshots := fs.String("snapshots", "", "snapshot directory (default: .beadline/snapshots beside beadline.toml)")
	backtest := fs.String("backtest", "", "replay history instead: forecast every -step over this span before the data horizon, e.g. 60d")
	step := fs.String("step", "3.5d", "time between backtest origins")
	runs := fs.Int("runs", 0, "simulated schedules per backtest forecast (default: model.simulations)")
	asJSON := fs.Bool("json", false, "print the report as JSON, with every graded forecast")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return ExitOK
		}
		return ExitUsage
	}
	if fs.NArg() > 0 {
		fmt.Fprintf(stderr, "beadline check: unexpected argument %q\n", fs.Arg(0))
		return ExitUsage
	}
	usage := func(format string, a ...any) int {
		fmt.Fprintf(stderr, "beadline check: "+format+"\n", a...)
		return ExitUsage
	}
	var span, every time.Duration
	if *backtest != "" {
		var err error
		if span, err = parseSpan(*backtest); err != nil {
			return usage("-backtest: %v", err)
		}
		if every, err = parseSpan(*step); err != nil {
			return usage("-step: %v", err)
		}
	}
	fail := func(err error) int {
		fmt.Fprintf(stderr, "beadline check: %v\n", err)
		return ExitFailure
	}

	cfg, err := config.Load(*configPath)
	if err != nil {
		return fail(err)
	}
	set := map[string]bool{}
	fs.Visit(func(f *flag.Flag) { set[f.Name] = true })
	if set["runs"] {
		if *runs < 1 {
			return usage("-runs must be at least 1")
		}
		cfg.Model.Simulations = *runs
	}
	ex, err := load.ParseFiles(cfg)
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
			fmt.Fprintf(stdout, "no snapshots in %s: 'beadline forecast -record' records one a day\n", dir)
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
			in.options.Grid = calibrate.Levels
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

// snapshotDir is where snapshots live: the -snapshots flag, or
// .beadline/snapshots beside beadline.toml.
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
