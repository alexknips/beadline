package cli

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/alexknips/beadline/internal/calibrate"
	"github.com/alexknips/beadline/internal/config"
	"github.com/alexknips/beadline/internal/estimate"
	"github.com/alexknips/beadline/internal/forecast"
	"github.com/alexknips/beadline/internal/graph"
	"github.com/alexknips/beadline/internal/load"
	"github.com/alexknips/beadline/internal/roadmap"
)

// newBdRunner runs bd for --write-back; tests replace it.
var newBdRunner = estimate.BdRunner

const forecastHelp = `Usage: beadline forecast [flags]

Hidden: 'beadline' forecasts and writes the roadmap. This prints the forecast
of every open high-level bead and goal as a table, or as JSON, from the repos
of a beadline.toml.

Flags:
  --config FILE   beadline.toml to read (default beadline.toml)
  --json          print the forecast as JSON
  --now TIME      forecast as of this RFC 3339 time instead of now
  --runs N        simulated schedules (default expert.runs)
  --seed N        random seed (default expert.seed)
  --record        also record the forecast as a snapshot for 'beadline check'
                  (the first run of each day)
  --snapshots DIR snapshot directory for --record (default .beadline/snapshots
                  beside beadline.toml)
  --write-back    write each open work bead's cycle-time P50 and P80 back to
                  its repository with bd update
  --bd PATH       bd binary (default bd)
  -h, --help      show this help
`

// runForecast loads the configured exports, learns agent and human-gate
// durations from their history, simulates the remaining graph and prints
// the forecast of every open high-level bead and goal: a table, or JSON.
func runForecast(args []string, stdout, stderr io.Writer) int {
	fs := newFlagSet()
	configPath := fs.String("config", "beadline.toml", "path to beadline.toml")
	asJSON := fs.Bool("json", false, "print the forecast as JSON")
	nowFlag := fs.String("now", "", "forecast as of this RFC 3339 time instead of the current time")
	runs := fs.Int("runs", 0, "number of simulated schedules (default: model.simulations)")
	seed := fs.Uint64("seed", 0, "random seed (default: model.seed)")
	writeBack := fs.Bool("write-back", false, "write each open work bead's cycle-time P50 and P80 back to its repository with bd update")
	bd := fs.String("bd", "bd", "")
	record := fs.Bool("record", false, "also record the forecast as a snapshot for 'beadline check' (the first run of each day)")
	snapshots := fs.String("snapshots", "", "snapshot directory for -record (default: .beadline/snapshots beside beadline.toml)")
	if err := fs.Parse(args); err != nil {
		return flagError(stdout, stderr, "forecast", forecastHelp, err)
	}
	if fs.NArg() > 0 {
		return usageError(stderr, "forecast", "unexpected argument %q", fs.Arg(0))
	}
	if *record && *nowFlag != "" {
		fmt.Fprintf(stderr, "beadline forecast: -record takes no -now: a snapshot must not be dated before the data it was made from\n")
		return ExitUsage
	}
	now := time.Now().UTC().Truncate(time.Second)
	if *nowFlag != "" {
		t, err := time.Parse(time.RFC3339, *nowFlag)
		if err != nil {
			fmt.Fprintf(stderr, "beadline forecast: -now: %v\n", err)
			return ExitUsage
		}
		now = t.UTC()
	}
	fail := func(err error) int {
		fmt.Fprintf(stderr, "beadline forecast: %v\n", err)
		return ExitFailure
	}

	cfg, err := config.Load(*configPath)
	if err != nil {
		return fail(err)
	}
	set := map[string]bool{}
	fs.Visit(func(f *flag.Flag) { set[f.Name] = true })
	if set["runs"] {
		cfg.Model.Simulations = *runs
	}
	if set["seed"] {
		cfg.Model.Seed = *seed
	}
	if cfg.Model.Simulations < 1 {
		fmt.Fprintf(stderr, "beadline forecast: -runs must be at least 1\n")
		return ExitUsage
	}
	// Read each repo once: the snapshot names exactly the data it was made
	// from.
	exports := load.ReadAll(context.Background(), cfg, newExporter(*bd))
	for _, e := range exports {
		if e.Err != nil {
			return fail(e.Err)
		}
	}
	g, rep, err := load.FromExports(&cfg.Conventions, exports)
	if err != nil {
		return fail(err)
	}
	inputs := roadmap.InputsOf(exports)
	in, err := forecastInputs(cfg, g, rep, now)
	if err != nil {
		return fail(err)
	}
	if *record {
		in.options.Grid = calibrate.Levels
	}
	res, err := forecast.Run(g, in.options)
	if err != nil {
		return fail(err)
	}

	if *asJSON {
		enc := json.NewEncoder(stdout)
		enc.SetIndent("", "  ")
		if err := enc.Encode(res); err != nil {
			return fail(err)
		}
	} else {
		printForecast(stdout, res, in)
	}

	if *record {
		snap, err := calibrate.New(res, g, calibrate.Meta{
			GeneratedAt:     time.Now(),
			BeadlineVersion: version(),
			BeadlineCommit:  commit(),
			Config:          roadmap.ConfigOf(cfg),
			Inputs:          inputs,
		})
		if err != nil {
			return fail(err)
		}
		path, existing, err := calibrate.Record(snapshotDir(cfg, *snapshots), snap)
		switch {
		case err != nil:
			return fail(fmt.Errorf("-record: %w", err))
		case existing != "":
			fmt.Fprintf(stderr, "kept today's snapshot %s\n", existing)
		default:
			fmt.Fprintf(stderr, "recorded snapshot %s\n", path)
		}
	}

	if *writeBack {
		updates := in.model.PlanWriteBack(in.open)
		dirs, err := repoDirs(cfg)
		if err != nil {
			return fail(fmt.Errorf("-write-back: %w", err))
		}
		if err := estimate.WriteBack(context.Background(), updates, newBdRunner(*bd, dirs)); err != nil {
			return fail(err)
		}
		fmt.Fprintf(stderr, "wrote estimates to %s\n", plural(len(updates), "bead"))
	}
	return ExitOK
}

// inputs is what a forecast learned before it simulated.
type inputs struct {
	options  forecast.Options
	model    *estimate.Model
	open     []estimate.Bead // open work beads, for the write-back
	measured map[string]bool // repositories whose concurrency was measured
	gates    int             // closed gates the human-gate lag was learned from
}

func forecastInputs(cfg *config.Config, g *graph.Graph, rep *load.Report, now time.Time) (*inputs, error) {
	m := cfg.Model
	window := time.Duration(m.WindowDays) * 24 * time.Hour

	// Agent durations are learned from work beads only: gates wait on
	// people, containers span other beads' work, goal beads coordinate.
	var work []estimate.Bead
	in := &inputs{measured: map[string]bool{}}
	for _, i := range forecast.WorkBeads(g) {
		b := bead(i)
		work = append(work, b)
		if !i.Closed() {
			in.open = append(in.open, b)
		}
	}
	model, err := estimate.Learn(work, now, estimate.Params{
		WindowDays:        m.WindowDays,
		Seed:              m.Seed,
		CyclePriorMinutes: m.CycleMinutesPrior,
		QueuePriorMinutes: m.CycleMinutesPrior,
		PoolingStrength:   m.PoolingStrength,
		TailCapFactor:     m.TailCapFactor,
	})
	if err != nil {
		return nil, err
	}
	in.model = model
	lags := forecast.GateLags(g, now, window)
	in.gates = len(lags)

	var names []string
	for _, r := range cfg.Repos {
		names = append(names, r.Name)
	}
	measured := forecast.MeasureConcurrency(g, names, now, window)
	concurrency := map[string]int{}
	for _, r := range cfg.Repos {
		concurrency[r.Name] = r.Concurrency.Max
		if r.Concurrency.Measure() {
			concurrency[r.Name] = measured[r.Name]
			in.measured[r.Name] = true
		}
	}
	outside := map[string][]string{}
	for _, d := range rep.Dangling {
		if d.Kind == graph.Blocking {
			outside[d.From] = append(outside[d.From], d.To)
		}
	}
	in.options = forecast.Options{
		Now:         now,
		Runs:        m.Simulations,
		Seed:        m.Seed,
		Concurrency: concurrency,
		Agent:       func(i *graph.Issue) forecast.Sampler { return model.Sampler(bead(i)) },
		Human:       forecast.HumanLag(lags, m.HumanGateHoursPrior*60, m.PoolingStrength, m.TailCapFactor),
		Outside:     outside,
	}
	return in, nil
}

// bead is what the estimator needs to know about an issue.
func bead(i *graph.Issue) estimate.Bead {
	b := estimate.Bead{
		ID:          i.ID,
		Repo:        i.Repo,
		Type:        i.Type,
		Status:      i.Status,
		Description: i.Description,
		Labels:      i.Labels,
		Children:    len(i.Children),
		CreatedAt:   i.CreatedAt,
		StartedAt:   i.StartedAt,
		ClosedAt:    i.ClosedAt,
	}
	if i.EstimatedMinutes != nil {
		b.EstimatedMinutes = *i.EstimatedMinutes
	}
	if raw, ok := i.Metadata[estimate.P80MetadataKey]; ok {
		// bd stores metadata set on the command line as a string.
		var v any
		if json.Unmarshal(raw, &v) == nil {
			switch v := v.(type) {
			case float64:
				b.P80Minutes = int(v)
			case string:
				b.P80Minutes, _ = strconv.Atoi(v)
			}
		}
	}
	return b
}

// repoDirs returns, per repository, the directory bd runs in for the
// write-back: the one holding the .beads directory the export lives in.
func repoDirs(cfg *config.Config) (map[string]string, error) {
	dirs := map[string]string{}
	for _, r := range cfg.Repos {
		path, err := filepath.Abs(r.ExportPath(cfg))
		if err != nil {
			return nil, err
		}
		beads := filepath.Dir(path)
		if filepath.Base(beads) != ".beads" {
			return nil, fmt.Errorf("repo %s: export %s is not in a .beads directory, so the repository to write to is unknown", r.Name, r.Export)
		}
		dirs[r.Name] = filepath.Dir(beads)
	}
	return dirs, nil
}

const dateFormat = "2006-01-02 15:04"

func printForecast(w io.Writer, res *forecast.Result, in *inputs) {
	fmt.Fprintf(w, "forecast as of %s UTC, %s, seed %d\n", res.Now.Format(dateFormat), plural(res.Runs, "run"), res.Seed)
	var agents []string
	for _, name := range sortedKeys(res.Concurrency) {
		a := fmt.Sprintf("%s %d", name, res.Concurrency[name])
		if in.measured[name] {
			a += " (measured)"
		}
		agents = append(agents, a)
	}
	fmt.Fprintf(w, "agents: %s; human-gate lag from %s\n\n", strings.Join(agents, ", "), plural(in.gates, "closed gate"))

	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "ITEM\tSTATUS\tDONE\tLEFT\tP50\tP80\tP95\tAGENT H\tHUMAN H\tCRITICAL CHAIN (P80)")
	row := func(prefix string, it forecast.Item) {
		date := func(p *forecast.Point) string {
			if p == nil {
				return "-"
			}
			return p.At.Format(dateFormat)
		}
		agent, human := "-", "-"
		if it.P80 != nil {
			agent, human = fmt.Sprintf("%.1f", it.P80.AgentHours), fmt.Sprintf("%.1f", it.P80.HumanHours)
		}
		fmt.Fprintf(tw, "%s%s\t%s\t%.0f%%\t%d\t%s\t%s\t%s\t%s\t%s\t%s\n", prefix, it.ID, it.Status, it.DonePct,
			len(it.Remaining), date(it.P50), date(it.P80), date(it.P95), agent, human, chain(it.CriticalChain))
	}
	for _, it := range res.Items {
		row("", it)
	}
	for _, it := range res.Goals {
		row("goal ", it)
	}
	tw.Flush()
}

// chain shortens a critical chain for the table.
func chain(ids []string) string {
	const show = 4
	if len(ids) == 0 {
		return "-"
	}
	if len(ids) > show {
		return fmt.Sprintf("%s ... %s (%d beads)", strings.Join(ids[:show-1], " > "), ids[len(ids)-1], len(ids))
	}
	return strings.Join(ids, " > ")
}

func sortedKeys(m map[string]int) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
