package cli

import (
	"bytes"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/alexknips/beadline/internal/calibrate"
	"github.com/alexknips/beadline/internal/config"
	"github.com/alexknips/beadline/internal/forecast"
	"github.com/alexknips/beadline/internal/graph"
	"github.com/alexknips/beadline/internal/load"
	"github.com/alexknips/beadline/internal/render"
	"github.com/alexknips/beadline/internal/roadmap"
)

// modelVersion names the forecasting model revision recorded in
// roadmap.json and in snapshots.
const modelVersion = forecast.ModelVersion

const rootFlagsHelp = `Flags:
  -o, --out DIR|FILE   where to write roadmap.html and roadmap.json (default .);
                       a FILE ending in .html or .json names one, and the other
                       goes beside it
  -c, --config FILE    settings and repos (default ./beadline.toml, if present)
      --no-record      do not record this forecast for 'beadline check'
      --json           print roadmap.json to stdout instead of the summary
  -q, --quiet          print only errors and warnings
      --explain ID     explain the forecast of one milestone, epic, goal or bead
  -h, --help           show this help
  -v, --version        print the version

Expert flags:
      --as-of DATE     forecast from the data as it stood at DATE (2026-09-19
                       or RFC 3339), not now; records nothing
      --seed N         random seed (default expert.seed, 1)
      --runs N         simulated schedules (default expert.runs, 2000)
      --agents REPO=N  agents that work REPO at once, or REPO=measure (the
                       default: measured from history); repeatable
      --bd PATH        bd binary that reads repository directories (default bd)

Exit status: 0 ok; 1 failure, including a repo that could not be read (the
page is still written, with a banner); 2 usage error.
`

// loaded is the graph of the repos that could be read.
type loaded struct {
	graph   *graph.Graph
	report  *load.Report
	exports []load.Export
	horizon time.Time // the latest moment the exports know about
	// activity is the cross-repo activity timeline as of the same asOf the
	// graph was rewound to: idle.Infer's input (docs/design.md, ADR-3).
	activity []time.Time
}

// runRoot is the one command: read the repos, forecast, write roadmap.json
// and roadmap.html, record a snapshot for 'beadline check' and print a
// summary.
func runRoot(args []string, stdout, stderr io.Writer) int {
	fs := newFlagSet()
	var repos repoFlags
	repos.register(fs)
	var out string
	fs.StringVar(&out, "o", ".", "")
	fs.StringVar(&out, "out", ".", "")
	noRecord := fs.Bool("no-record", false, "")
	asJSON := fs.Bool("json", false, "")
	var quiet, showVersion bool
	fs.BoolVar(&quiet, "q", false, "")
	fs.BoolVar(&quiet, "quiet", false, "")
	fs.BoolVar(&showVersion, "v", false, "")
	fs.BoolVar(&showVersion, "version", false, "")
	explain := fs.String("explain", "", "")
	asOf := fs.String("as-of", "", "")
	seed := fs.Uint64("seed", 0, "")
	runs := fs.Int("runs", 0, "")
	agents := agentFlags{}
	fs.Var(agents, "agents", "")

	paths, err := parseArgs(fs, args)
	if err != nil {
		var help bytes.Buffer
		usage(&help)
		return flagError(stdout, stderr, "", help.String(), err)
	}
	if showVersion {
		fmt.Fprintf(stdout, "beadline %s\n", version())
		return ExitOK
	}
	set := map[string]bool{}
	fs.Visit(func(f *flag.Flag) { set[f.Name] = true })
	if set["runs"] && *runs < 1 {
		return usageError(stderr, "", "--runs must be at least 1")
	}
	now := time.Now().UTC().Truncate(time.Second)
	var rewind time.Time // --as-of: the data as it stood then
	if *asOf != "" {
		t, err := parseDate(*asOf)
		if err != nil {
			return usageError(stderr, "", "--as-of: %v", err)
		}
		now, rewind = t, t
	}
	if *asJSON && *explain != "" {
		return usageError(stderr, "", "--json and --explain both print to stdout; pick one")
	}
	page, jsonPath := outputs(out)

	cfg, code := repos.resolve("", paths, stderr)
	if code != ExitOK {
		return code
	}
	if set["seed"] {
		cfg.Model.Seed = *seed
	}
	if set["runs"] {
		cfg.Model.Simulations = *runs
	}
	for repo, c := range agents {
		cfg.Agents[repo] = c
	}
	warnAgents(cfg, stderr)

	exports, failed := repos.read(cfg, stderr)
	l, bad, err := parseExports(cfg, exports, rewind, stderr)
	failed += bad
	if err != nil {
		fmt.Fprintf(stderr, "beadline: %v\n", err)
		return ExitFailure
	}
	if failed == len(exports) {
		fmt.Fprintf(stderr, "beadline: no repo could be read; nothing written\n")
		return ExitFailure
	}
	g := l.graph
	if *explain != "" && !known(g, *explain) {
		return usageError(stderr, "", "--explain: %s is not a bead or goal in the loaded repos", *explain)
	}

	in, err := forecastInputs(cfg, g, l.report, now, l.activity)
	if err != nil {
		fmt.Fprintf(stderr, "beadline: %v\n", err)
		return ExitFailure
	}
	// A snapshot must hold every repo, and must not be dated before the
	// data it was made from.
	record := !*noRecord && *asOf == "" && failed == 0
	res, err := forecast.Run(g, in.options)
	if err != nil {
		fmt.Fprintf(stderr, "beadline: %v\n", err)
		return ExitFailure
	}
	r := roadmap.Build(g, l.report, cfg, now)
	r.BeadlineVersion, r.ModelVersion = version(), modelVersion
	r.SetInputs(l.exports)
	r.SetForecast(res, g)
	r.Idle = in.idle
	for n := range r.Repos {
		// A measured agent count sets no limit, but it is shown.
		if peak, ok := in.measured[r.Repos[n].Name]; ok {
			c := float64(peak)
			r.Repos[n].Concurrency = &c
		}
	}
	r.Hide(cfg.Hide)
	if rewind.IsZero() {
		r.Calibration = trackRecord(snapshotDir(cfg, ""), g, l.horizon)
	}

	if err := write(r, page, jsonPath); err != nil {
		fmt.Fprintf(stderr, "beadline: %v\n", err)
		return ExitFailure
	}
	if !quiet {
		fmt.Fprintf(stderr, "wrote %s and %s\n", page, jsonPath)
		if n := problems(l.report); n > 0 {
			fmt.Fprintf(stderr, "note: %s in the data; 'beadline doctor' lists them\n", plural(n, "problem"))
		}
	}
	if record {
		if err := recordSnapshot(res, g, cfg, r.Inputs, quiet, stderr); err != nil {
			fmt.Fprintf(stderr, "beadline: %v\n", err)
			failed++ // the roadmap is written; report it, then fail
		}
	} else if failed > 0 && !*noRecord && *asOf == "" && !quiet {
		fmt.Fprintln(stderr, "no snapshot recorded: a repo could not be read")
	}

	switch {
	case *asJSON:
		if err := roadmap.Write(stdout, r); err != nil {
			fmt.Fprintf(stderr, "beadline: %v\n", err)
			return ExitFailure
		}
	case *explain != "":
		explainItem(stdout, *explain, r, g, in)
	case !quiet:
		summary(stdout, r)
	}
	if failed > 0 {
		return ExitFailure
	}
	return ExitOK
}

// trackRecord grades the recorded snapshots as 'beadline check' does and
// returns how the 80% dates of milestones and goals held, or nil before
// any outcome is known. Problems with snapshots are for check to report.
func trackRecord(dir string, g *graph.Graph, horizon time.Time) *roadmap.Calibration {
	snaps, _, err := calibrate.ReadDir(dir)
	if err != nil || len(snaps) == 0 {
		return nil
	}
	rep := calibrate.NewReport("snapshots", horizon, calibrate.Grade(snaps, calibrate.NewReality(g, horizon)))
	share := func(level float64) (held, known int) {
		for _, c := range rep.HighLevel.Model.Coverage {
			if c.Level == level {
				return c.Held, c.Known
			}
		}
		return 0, 0
	}
	held80, known80 := share(0.8)
	if known80 == 0 {
		return nil
	}
	c := &roadmap.Calibration{Samples: known80, P80Coverage: float64(held80) / float64(known80)}
	if held50, known50 := share(0.5); known50 > 0 {
		c.P50Coverage = float64(held50) / float64(known50)
	}
	return c
}

// recordSnapshot records the forecast for 'beadline check': the first run
// of each day, beside beadline.toml or in the working directory.
func recordSnapshot(res *forecast.Result, g *graph.Graph, cfg *config.Config, inputs roadmap.Inputs, quiet bool, stderr io.Writer) error {
	snap, err := calibrate.New(res, g, calibrate.Meta{
		GeneratedAt:     time.Now(),
		BeadlineVersion: version(),
		BeadlineCommit:  commit(),
		Config:          roadmap.ConfigOf(cfg),
		Inputs:          inputs,
	})
	if err != nil {
		return err
	}
	path, existing, err := calibrate.Record(snapshotDir(cfg, ""), snap)
	switch {
	case err != nil:
		return fmt.Errorf("recording the snapshot: %w", err)
	case quiet:
	case existing != "":
		fmt.Fprintf(stderr, "kept today's snapshot %s\n", existing)
	default:
		fmt.Fprintf(stderr, "recorded snapshot %s\n", path)
	}
	return nil
}

// parseDate reads a day (taken as 00:00 UTC) or an RFC 3339 time.
func parseDate(s string) (time.Time, error) {
	if t, err := time.Parse(time.DateOnly, s); err == nil {
		return t.UTC(), nil
	}
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		return time.Time{}, fmt.Errorf("%q is not a date (2026-09-19) or an RFC 3339 time (2026-09-19T12:00:00Z)", s)
	}
	return t.UTC(), nil
}

// outputs returns the page and JSON paths that -o names. A path ending in
// .html, .htm or .json names one file, and the other goes beside it with
// the same stem; anything else is a directory.
func outputs(out string) (page, js string) {
	ext := filepath.Ext(out)
	stem := strings.TrimSuffix(out, ext)
	switch strings.ToLower(ext) {
	case ".html", ".htm":
		return out, stem + ".json"
	case ".json":
		return stem + ".html", out
	}
	return filepath.Join(out, "roadmap.html"), filepath.Join(out, "roadmap.json")
}

// write writes roadmap.json, then the page, each atomically, creating the
// directory if needed.
func write(r *roadmap.Roadmap, page, js string) error {
	if err := os.MkdirAll(filepath.Dir(page), 0o755); err != nil {
		return err
	}
	var html bytes.Buffer
	if err := render.Render(&html, r, render.Options{Title: "Roadmap", Version: version()}); err != nil {
		return err
	}
	if err := roadmap.WriteFile(js, r); err != nil {
		return err
	}
	return writeFile(page, html.Bytes())
}

// problems counts what doctor would report.
func problems(rep *load.Report) int {
	return len(rep.Cycles) + len(rep.Dangling) + len(rep.Duplicates)
}

// known reports whether id is a loaded bead or a goal.
func known(g *graph.Graph, id string) bool {
	if g.Issue(id) != nil {
		return true
	}
	for _, gl := range g.Goals() {
		if gl.ID == id {
			return true
		}
	}
	return false
}

// summary prints the answer: every open milestone, epic and goal with a
// forecast, soonest plan date first, then the ones without a date, grouped
// by why.
func summary(w io.Writer, r *roadmap.Roadmap) {
	beads, open := 0, 0
	for _, rp := range r.Repos {
		beads += rp.Issues
		open += rp.Open
	}
	fmt.Fprintf(w, "Roadmap as of %s UTC: %s, %s (%d open)\n",
		r.GeneratedAt.Format(dateFormat), plural(len(r.Repos), "repo"), plural(beads, "bead"), open)
	if c := r.Calibration; c != nil && c.Samples > 0 {
		fmt.Fprintf(w, "Track record: 80%% dates held %.0f of %d.\n", c.P80Coverage*float64(c.Samples), c.Samples)
	}
	rows := answerRows(r)
	if len(rows) == 0 {
		fmt.Fprintln(w, "\nNo open milestones, epics or goals.")
		return
	}
	undated := map[string][]answer{}
	first := true
	for _, a := range rows {
		if a.o.P80 == nil {
			undated[a.o.Status] = append(undated[a.o.Status], a)
			continue
		}
		if first {
			fmt.Fprintln(w)
			first = false
		}
		fmt.Fprintf(w, "  %s  %s\n      %s\n", a.id, a.title, strings.Join(a.facts(), " · "))
	}
	for _, st := range []string{roadmap.StatusReadyToClose, roadmap.StatusStalled, roadmap.StatusBlockedOutside,
		roadmap.StatusNotPlanned, roadmap.StatusDeferred, roadmap.StatusForecast} {
		if len(undated[st]) == 0 {
			continue
		}
		fmt.Fprintf(w, "\n%s:\n", undatedHeading(st, undated[st]))
		for _, a := range undated[st] {
			fmt.Fprintf(w, "  %s  %s\n", a.id, a.title)
		}
	}
}

// undatedHeading says why a group of items has no date.
func undatedHeading(status string, group []answer) string {
	switch status {
	case roadmap.StatusReadyToClose:
		return "Ready to close (all their work is done)"
	case roadmap.StatusStalled:
		return "Stalled (none of their work can start)"
	case roadmap.StatusBlockedOutside:
		var ids []string
		for _, a := range group {
			ids = append(ids, a.o.WaitsOnUnloaded...)
		}
		return "Waiting on repos that are not loaded (" + strings.Join(ids, ", ") + ")"
	case roadmap.StatusNotPlanned:
		return "Not planned (nothing is filed under them yet)"
	case roadmap.StatusDeferred:
		return "Deferred"
	}
	return "No forecast"
}

// answer is one open milestone, epic or goal in the summary.
type answer struct {
	id, title string
	o         roadmap.Outlook
}

func answerRows(r *roadmap.Roadmap) []answer {
	var rows []answer
	for _, gl := range r.Goals {
		if gl.Status != roadmap.StatusDone {
			title := cut(gl.Title, 60) + " (goal)"
			if gl.Title == gl.ID { // its bead is in no loaded repo
				title = "(goal)"
			}
			rows = append(rows, answer{gl.ID, title, gl.Outlook})
		}
	}
	for _, m := range r.Milestones {
		if m.Status != roadmap.StatusDone {
			rows = append(rows, answer{m.ID, cut(m.Title, 60), m.Outlook})
		}
	}
	sort.SliceStable(rows, func(a, b int) bool {
		pa, pb := rows[a].o.P80, rows[b].o.P80
		switch {
		case pa != nil && pb != nil:
			return pa.Before(*pb)
		default:
			return pa != nil && pb == nil
		}
	})
	return rows
}

// facts are the summary line of a dated item: the plan date first.
func (a answer) facts() []string {
	o := a.o
	f := []string{"plan for " + day(o.P80) + " (80% chance)", "50/50: " + day(o.P50),
		fmt.Sprintf("%d of %d left", o.Remaining, o.Total)}
	if o.HumanGates > 0 {
		f = append(f, "waits on a person")
	}
	if o.TargetDueAt != nil {
		f = append(f, fmt.Sprintf("target %s: %s", day(o.TargetDueAt), strings.ReplaceAll(o.Schedule, "_", " ")))
	}
	switch o.Status {
	case roadmap.StatusStalled:
		f = append(f, "stalled: some work can never start, so the dates cover the rest")
	case roadmap.StatusBlockedOutside:
		f = append(f, "waits on "+strings.Join(o.WaitsOnUnloaded, ", ")+", which is not loaded")
	}
	return f
}

// statusText says why an item has no date.
func statusText(o *roadmap.Outlook) string {
	switch o.Status {
	case roadmap.StatusReadyToClose:
		return "ready to close: all its work is done"
	case roadmap.StatusNotPlanned:
		return "not planned: nothing is filed under it"
	case roadmap.StatusDeferred:
		return "deferred"
	case roadmap.StatusStalled:
		return "stalled: none of its work can start"
	case roadmap.StatusBlockedOutside:
		return "waits on " + strings.Join(o.WaitsOnUnloaded, ", ") + ", which is not loaded"
	case roadmap.StatusForecast:
		return "no forecast"
	}
	return o.Status
}

func day(t *time.Time) string {
	if t == nil {
		return "—"
	}
	return t.UTC().Format(time.DateOnly)
}

// cut shortens s to n runes with an ellipsis.
func cut(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n-1]) + "…"
}

// explainItem prints what a forecast rests on, for one item or bead.
func explainItem(w io.Writer, id string, r *roadmap.Roadmap, g *graph.Graph, in *inputs) {
	var o *roadmap.Outlook
	head := id
	repos := map[string]bool{} // the repos whose agents and pace the dates rest on
	for n := range r.Milestones {
		if m := &r.Milestones[n]; m.ID == id {
			o, head = &m.Outlook, fmt.Sprintf("%s · %s (%s in %s)", m.ID, m.Title, m.Type, m.Repo)
		}
	}
	for n := range r.Goals {
		if gl := &r.Goals[n]; gl.ID == id {
			o, head = &gl.Outlook, fmt.Sprintf("%s · %s (goal, %s)", gl.ID, gl.Title, plural(len(gl.Members), "member"))
		}
	}
	if o == nil {
		explainBead(w, g.Issue(id), r, in)
		return
	}
	fmt.Fprintln(w, head)
	if o.P80 != nil {
		fmt.Fprintf(w, "plan for %s (80%% chance) · 50/50: %s · 95%%: %s\n", day(o.P80), day(o.P50), day(o.P95))
	} else {
		fmt.Fprintf(w, "no date: %s\n", statusText(o))
	}
	fmt.Fprintf(w, "Work: %d of %d beads left (%.0f%% done)", o.Remaining, o.Total, o.DonePct)
	if o.HumanGates > 0 {
		fmt.Fprintf(w, "; %s of them wait on a person", plural(o.HumanGates, "bead"))
	}
	fmt.Fprintln(w, ".")
	if o.AgentHours != nil {
		fmt.Fprintf(w, "The median run takes %.1f h of agent time and %.1f h waiting on people.\n", *o.AgentHours, *o.HumanHours)
	}
	for _, rid := range o.RemainingIDs {
		if i := g.Issue(rid); i != nil {
			repos[i.Repo] = true
		}
	}
	for _, rp := range r.Repos {
		if repos[rp.Name] && rp.Concurrency != nil && rp.RatePerDay != nil {
			n := plural(int(*rp.Concurrency), "agent")
			agents := n + " (configured)"
			if rp.ConcurrencySource == "measured" {
				agents = "up to " + n + " at once (measured, not a limit)"
			}
			fmt.Fprintf(w, "Repo %s: %s, %.2f beads closed a day over the last %d days.\n",
				rp.Name, agents, *rp.RatePerDay, r.Config.Model.WindowDays)
		}
	}
	fmt.Fprintf(w, "Durations %s.\n", learnedFrom(in.model.Sample()))
	if len(o.CriticalChain) > 0 {
		fmt.Fprintln(w, "\nCritical chain (it sets the 80% date; each bead waits for the one before):")
		for _, cid := range o.CriticalChain {
			fmt.Fprintf(w, "  %s\n", beadLine(g.Issue(cid), in))
		}
	}
	if len(o.RemainingIDs) > 0 {
		fmt.Fprintln(w, "\nWork left:")
		for _, rid := range o.RemainingIDs {
			fmt.Fprintf(w, "  %s\n", beadLine(g.Issue(rid), in))
		}
	}
}

// explainBead explains a bead that is not on the roadmap: its estimate and
// the items it counts towards.
func explainBead(w io.Writer, i *graph.Issue, r *roadmap.Roadmap, in *inputs) {
	if i == nil {
		fmt.Fprintln(w, "This goal has no bead in the loaded repos and is not on the roadmap: it is done or hidden.")
		return
	}
	fmt.Fprintf(w, "%s · %s (%s in %s, %s)\n", i.ID, i.Title, i.Type, i.Repo, i.Status)
	fmt.Fprintln(w, beadLine(i, in))
	var items []string
	for _, m := range r.Milestones {
		if contains(m.RemainingIDs, i.ID) {
			items = append(items, m.ID)
		}
	}
	for _, gl := range r.Goals {
		if contains(gl.RemainingIDs, i.ID) {
			items = append(items, gl.ID+" (goal)")
		}
	}
	if len(items) > 0 {
		fmt.Fprintf(w, "Counts towards: %s\n", strings.Join(items, ", "))
	}
}

// beadLine is one bead with what the model expects of it.
func beadLine(i *graph.Issue, in *inputs) string {
	if i == nil {
		return "?"
	}
	line := fmt.Sprintf("%s  %s  [%s]", i.ID, cut(i.Title, 50), i.Status)
	switch {
	case i.Closed():
		return line
	case i.HumanGate:
		return line + "  waits on a person"
	case i.HighLevel || len(i.Children) > 0:
		return line + "  done when its work is"
	}
	b := in.bead(i)
	left := in.model.Estimate(b).Remaining
	if b.Blocked && i.Status != graph.StatusInProgress {
		return line + fmt.Sprintf("  about %s once its blockers close (80%%: %s)", span(left.P50), span(left.P80))
	}
	return line + fmt.Sprintf("  about %s to go (80%%: %s)", span(left.P50), span(left.P80))
}

// span formats minutes as minutes, hours or days.
func span(minutes float64) string {
	switch {
	case minutes < 60:
		return fmt.Sprintf("%.0f min", minutes)
	case minutes < 48*60:
		return fmt.Sprintf("%.1f h", minutes/60)
	}
	return fmt.Sprintf("%.1f d", minutes/60/24)
}

func contains(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}
