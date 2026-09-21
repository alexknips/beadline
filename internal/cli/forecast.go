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
	ex, err := load.ParseExports(exports)
	if err != nil {
		return fail(err)
	}
	g, rep, err := ex.Graph(&cfg.Conventions, time.Time{})
	if err != nil {
		return fail(err)
	}
	inputs := roadmap.InputsOf(exports)
	in, err := forecastInputs(cfg, g, rep, now, ex.ActivityTimestamps(time.Time{}))
	if err != nil {
		return fail(err)
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
	options forecast.Options
	model   *estimate.Model
	open    []estimate.Bead // open work beads, for the write-back
	// beads are the work beads as the estimator sees them, ready_at
	// included, by ID; readiness tells it for other beads.
	beads     map[string]estimate.Bead
	readiness *forecast.Readiness
	now       time.Time
	// measured holds the peak concurrency of the repositories set to
	// "measure". It is shown, not simulated: measured lead times already
	// hold the wait for a free agent (ADR-2 §1).
	measured map[string]int
	gates    int // closed gates the human-gate lag was learned from
	// idle summarizes the idle-time mask this forecast learned net of, for
	// roadmap.json (ADR-3).
	idle *roadmap.Idle
}

// forecastInputs learns the model and assembles the forecaster's options.
// activity is the cross-repo timeline idle.Infer masks gaps from
// (load.Exports.ActivityTimestamps, as of the same now); nil masks nothing
// beyond any declared windows.
func forecastInputs(cfg *config.Config, g *graph.Graph, rep *load.Report, now time.Time, activity []time.Time) (*inputs, error) {
	m := cfg.Model
	window := time.Duration(m.WindowDays) * 24 * time.Hour
	since := now.AddDate(0, 0, -m.WindowDays)

	mask := idleMask(cfg.Idle, activity, now)
	avail := availability(mask, since, now)
	dateFrom := now
	if mask.CurrentlyIdle() && cfg.Idle.ResumeAt.After(now) {
		dateFrom = cfg.Idle.ResumeAt
	}

	// Agent durations are learned from work beads only: gates wait on
	// people, containers span other beads' work, goal beads coordinate.
	// Lead times run from ready_at: for a closed bead as of its close, for
	// an open one as of now.
	var work []estimate.Bead
	in := &inputs{measured: map[string]int{}, beads: map[string]estimate.Bead{}, readiness: forecast.NewReadiness(g), now: now,
		idle: idleSummary(cfg.Idle, mask, since, now, avail)}
	for _, i := range forecast.WorkBeads(g) {
		b := bead(i)
		asOf := now
		if i.Closed() && !i.ClosedAt.IsZero() {
			asOf = i.ClosedAt
		}
		b.ReadyAt, b.Blocked = in.readiness.At(i, asOf)
		work = append(work, b)
		in.beads[b.ID] = b
		if !i.Closed() {
			in.open = append(in.open, b)
		}
	}
	model, err := estimate.Learn(work, now, estimate.Params{
		WindowDays: m.WindowDays,
		Seed:       m.Seed,
		// A lead time is a queue and a cycle: its prior is two cycle priors.
		LeadPriorMinutes:  2 * m.CycleMinutesPrior,
		CyclePriorMinutes: m.CycleMinutesPrior,
		PoolingStrength:   m.PoolingStrength,
		TailCapFactor:     m.TailCapFactor,
		Active:            mask.Active,
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
		// A configured agent limit is honoured; "measure" measures and
		// shows the peak but sets no limit.
		concurrency[r.Name] = r.Concurrency.Max
		if r.Concurrency.Measure() {
			concurrency[r.Name] = 0
			in.measured[r.Name] = measured[r.Name]
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
		Agent:       func(i *graph.Issue) forecast.Sampler { return model.Sampler(in.bead(i)) },
		Human:       forecast.HumanLag(lags, m.HumanGateHoursPrior*60, m.PoolingStrength, m.TailCapFactor),
		Outside:     outside,
		// Every dated item carries its quantile grid, p05 to p99: the one
		// snapshots record and calibration scores.
		Grid: calibrate.Levels,
		// ADR-3: dates count from a declared resume when the data shows the
		// city idle right now, and durations (already learned net of idle
		// time) are stretched back to calendar time by the measured duty
		// cycle.
		DateFrom:     dateFrom,
		Availability: avail,
	}
	return in, nil
}

// bead returns the estimator's view of an issue as of the forecast's now,
// when it became ready included.
func (in *inputs) bead(i *graph.Issue) estimate.Bead {
	if b, ok := in.beads[i.ID]; ok {
		return b
	}
	b := bead(i)
	b.ReadyAt, b.Blocked = in.readiness.At(i, in.now)
	return b
}

// bead is what the estimator needs to know about an issue, but for when it
// became ready, which depends on the moment asked about.
func bead(i *graph.Issue) estimate.Bead {
	b := estimate.Bead{
		ID:          i.ID,
		Repo:        i.Repo,
		Type:        i.Type,
		Priority:    i.Priority,
		Status:      i.Status,
		CreatedAt:   i.CreatedAt,
		StartedAt:   i.StartedAt,
		ClosedAt:    i.ClosedAt,
		CloseReason: i.CloseReason,
		WorkOutcome: metadataString(i, "gc.work_outcome"),
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

// metadataString returns a string metadata value of i, or "".
func metadataString(i *graph.Issue, key string) string {
	var v string
	if raw, ok := i.Metadata[key]; ok && json.Unmarshal(raw, &v) == nil {
		return v
	}
	return ""
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
		if peak, ok := in.measured[name]; ok {
			a = fmt.Sprintf("%s no limit (peak %d)", name, peak)
		}
		agents = append(agents, a)
	}
	fmt.Fprintf(w, "agents: %s; human-gate lag from %s\n", strings.Join(agents, ", "), plural(in.gates, "closed gate"))
	fmt.Fprintf(w, "%s\n\n", learnedFrom(in.model.Sample()))

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

// learnedFrom says what the estimator learned from, and which closes it
// left out because they were not deliveries.
func learnedFrom(s estimate.Sample) string {
	out := fmt.Sprintf("learned from %s (%d with a start) and %s", plural(s.Closes, "delivered close"), s.Started,
		plural(s.Open, "open bead"))
	var skipped []string
	for _, reason := range estimate.SkipReasons {
		if n := s.Skipped[reason]; n > 0 {
			skipped = append(skipped, fmt.Sprintf("%d %s", n, reason))
		}
	}
	if len(skipped) > 0 {
		out += "; left out " + strings.Join(skipped, ", ")
	}
	return out
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
