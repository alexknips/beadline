package render

import (
	"bytes"
	"crypto/sha256"
	_ "embed"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"html/template"
	"io"
	"math"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/alexknips/beadline/internal/roadmap"
)

var (
	//go:embed page.html
	pageHTML string
	//go:embed style.css
	styleCSS string
	//go:embed script.js
	scriptJS string
)

var page = template.Must(template.New("page").Funcs(template.FuncMap{"num": num}).Parse(pageHTML))

// Options control the page.
type Options struct {
	Title   string // heads the page and names the tab; default "Roadmap"
	Version string // the beadline version named in the footer
}

// Render writes r as one self-contained HTML page: inline CSS, the timeline
// as inline SVG, the same data as a table, a small optional script and r
// itself as JSON in <script type="application/json" id="roadmap-data">.
//
// The page makes no network requests and contains no links, so it works
// from file://, from any directory of a static server and beside other
// pages. Its Content-Security-Policy allows only its own inline style and
// script, by hash.
func Render(w io.Writer, r *roadmap.Roadmap, opts Options) error {
	if err := r.Validate(); err != nil {
		return fmt.Errorf("render: %w", err)
	}
	if opts.Title == "" {
		opts.Title = "Roadmap"
	}
	// json.Marshal escapes <, > and &, so no title can end the <script>.
	data, err := json.Marshal(r)
	if err != nil {
		return fmt.Errorf("render: %w", err)
	}
	v := newView(r, opts)
	v.Data = template.JS(data)
	v.CSS, v.JS = template.CSS(styleCSS), template.JS(scriptJS)
	v.CSP = fmt.Sprintf("default-src 'none'; style-src '%s'; script-src '%s'; base-uri 'none'; form-action 'none'",
		cspHash(styleCSS), cspHash(scriptJS))

	var buf bytes.Buffer
	if err := page.Execute(&buf, v); err != nil {
		return fmt.Errorf("render: %w", err)
	}
	_, err = w.Write(buf.Bytes())
	return err
}

func cspHash(s string) string {
	sum := sha256.Sum256([]byte(s))
	return "sha256-" + base64.StdEncoding.EncodeToString(sum[:])
}

type view struct {
	r *roadmap.Roadmap

	Title, Version string
	Facts          []string
	NoForecast     bool
	Summary        []chip
	Calibration    string
	Timeline       timeline
	Groups         []group
	Repos          []string

	CSS  template.CSS
	JS   template.JS
	Data template.JS
	CSP  string

	titles map[string]string // milestone ID -> title
	repos  map[string]*roadmap.Repo
}

type chip struct{ Class, Text string }

type group struct {
	Name, Repo, Empty string
	Rows              []tableRow
}

type tableRow struct {
	ID, Title, Repos, Kind              string
	Status, StatusClass                 string
	Schedule, ScheduleClass             string
	Left, Done                          string
	P50, P80, P95, Target, Agent, Human string
}

func newView(r *roadmap.Roadmap, opts Options) *view {
	v := &view{r: r, Title: opts.Title, Version: opts.Version,
		titles: map[string]string{}, repos: map[string]*roadmap.Repo{}}
	if v.Version == "" {
		v.Version = r.BeadlineVersion
	}
	for _, m := range r.Milestones {
		v.titles[m.ID] = m.Title
	}
	for n := range r.Repos {
		v.repos[r.Repos[n].Name] = &r.Repos[n]
		v.Repos = append(v.Repos, r.Repos[n].Name)
	}

	v.Facts = append(v.Facts, "Generated "+r.GeneratedAt.UTC().Format("2006-01-02 15:04")+" UTC",
		plural(float64(len(r.Repos)), "repo"))
	forecasts := false
	for _, m := range r.Milestones {
		forecasts = forecasts || m.P80 != nil
	}
	for _, g := range r.Goals {
		forecasts = forecasts || g.P80 != nil
	}
	if r.ModelVersion != "" {
		v.Facts = append(v.Facts, "model "+r.ModelVersion, fmt.Sprintf("seed %d", r.Config.Model.Seed),
			plural(float64(r.Config.Model.Simulations), "run"))
	}
	v.NoForecast = !forecasts
	if fp := strings.TrimPrefix(r.Inputs.Fingerprint, "sha256:"); len(fp) >= 12 {
		v.Facts = append(v.Facts, "inputs "+fp[:12])
	}

	v.summary()
	v.Calibration = "Calibration: no scored forecasts yet."
	if c := r.Calibration; c != nil && c.Samples > 0 {
		v.Calibration = fmt.Sprintf("Calibration: P80 held for %.0f%% of %s (P50 for %.0f%%).",
			100*c.P80Coverage, plural(float64(c.Samples), "scored forecast"), 100*c.P50Coverage)
	}
	v.Timeline = layout(r, v)
	v.table()
	return v
}

// summary counts open milestones and goals by schedule.
func (v *view) summary() {
	count := map[string]int{}
	targets := 0
	add := func(o *roadmap.Outlook) {
		if o.Status == roadmap.StatusDone {
			return
		}
		count[o.Schedule]++
		if o.TargetDueAt != nil {
			targets++
		}
	}
	for n := range v.r.Milestones {
		add(&v.r.Milestones[n].Outlook)
	}
	for n := range v.r.Goals {
		add(&v.r.Goals[n].Outlook)
	}
	if targets == 0 {
		v.Summary = []chip{{"s-none", "No targets (due_at) set"}}
		return
	}
	for _, s := range []string{roadmap.OnTrack, roadmap.AtRisk, roadmap.Late} {
		v.Summary = append(v.Summary, chip{scheduleClass(s), fmt.Sprintf("%d %s", count[s], scheduleText(s))})
	}
	if n := count[""]; n > 0 {
		v.Summary = append(v.Summary, chip{"s-none", fmt.Sprintf("%d without a target or forecast", n)})
	}
}

// table lists the same lanes as the timeline, done items included.
func (v *view) table() {
	r := v.r
	goals := group{Name: "Goals", Repo: laneGoals, Empty: "No goals."}
	for _, n := range openFirst(goalOutlooks(r)) {
		g := &r.Goals[n]
		goals.Rows = append(goals.Rows, v.tableRow(g.ID, g.Title, strings.Join(v.goalRepos(g), " "), "goal", &g.Outlook))
	}
	v.Groups = append(v.Groups, goals)
	for _, rp := range r.Repos {
		grp := group{Name: rp.Name, Repo: rp.Name, Empty: "No open or recently closed milestones or epics."}
		for _, n := range openFirst(milestoneOutlooks(r, rp.Name)) {
			m := &r.Milestones[n]
			grp.Rows = append(grp.Rows, v.tableRow(m.ID, m.Title, m.Repo, m.Type, &m.Outlook))
		}
		v.Groups = append(v.Groups, grp)
	}
}

func (v *view) tableRow(id, title, repos, kind string, o *roadmap.Outlook) tableRow {
	t := tableRow{ID: id, Title: title, Repos: repos, Kind: kind,
		Status: v.statusText(o), StatusClass: "st-" + o.Status,
		Schedule: scheduleText(o.Schedule), ScheduleClass: scheduleClass(o.Schedule),
		Left: fmt.Sprintf("%d of %d", o.Remaining, o.Total), Done: num(o.DonePct) + "%",
		P50: date(o.P50), P80: date(o.P80), P95: date(o.P95), Target: date(o.TargetDueAt),
		Agent: hours(o.AgentHours), Human: hours(o.HumanHours)}
	switch {
	case o.Schedule != "":
	case o.TargetDueAt == nil:
		t.Schedule, t.ScheduleClass = "no target", ""
	default:
		t.ScheduleClass = "" // a target, but nothing to compare it with yet
	}
	if o.Total == 0 {
		t.Left, t.Done = "—", "—"
	}
	return t
}

func (v *view) statusText(o *roadmap.Outlook) string {
	switch o.Status {
	case roadmap.StatusForecast:
		if o.P80 == nil {
			return "no forecast yet"
		}
		return "forecast"
	case roadmap.StatusDone:
		if o.ClosedAt != nil {
			return "done " + date(o.ClosedAt)
		}
		return "done"
	case roadmap.StatusReadyToClose:
		return "ready to close"
	case roadmap.StatusNotPlanned:
		return "not planned"
	case roadmap.StatusBlockedOutside:
		return "waits on an unloaded repo"
	}
	return o.Status
}

func scheduleText(s string) string {
	switch s {
	case roadmap.OnTrack:
		return "on track"
	case roadmap.AtRisk:
		return "at risk"
	case roadmap.Late:
		return "late"
	}
	return "—"
}

// goalRepos are the repos a goal's row shows under when filtering: its own
// and its members'.
func (v *view) goalRepos(g *roadmap.Goal) []string {
	set := map[string]bool{}
	if g.Repo != "" {
		set[g.Repo] = true
	}
	for _, m := range v.r.Milestones {
		for _, id := range g.Members {
			if m.ID == id {
				set[m.Repo] = true
			}
		}
	}
	out := make([]string, 0, len(set))
	for rp := range set {
		out = append(out, rp)
	}
	sort.Strings(out)
	return out
}

func (v *view) milestoneTip(m *roadmap.Milestone) []string {
	tip := []string{m.Title, fmt.Sprintf("%s · %s in %s · %s", m.ID, m.Type, m.Repo, v.statusText(&m.Outlook))}
	if len(m.Goals) > 0 {
		tip = append(tip, "Goal: "+strings.Join(m.Goals, ", "))
	}
	rate, agents := m.RatePerDay, m.Concurrency
	if rp := v.repos[m.Repo]; rp != nil {
		if rate == nil {
			rate = rp.RatePerDay
		}
		if agents == nil {
			agents = rp.Concurrency
		}
	}
	return append(tip, v.outlookTip(&m.Outlook, rate, agents)...)
}

func (v *view) goalTip(g *roadmap.Goal) []string {
	where := "goal"
	if g.Repo != "" {
		where = "goal in " + g.Repo
	}
	tip := []string{g.Title, fmt.Sprintf("%s · %s · %s", g.ID, where, v.statusText(&g.Outlook))}
	if len(g.Members) > 0 {
		names := make([]string, 0, 3)
		for _, id := range g.Members {
			if len(names) == 3 {
				names = append(names, fmt.Sprintf("and %d more", len(g.Members)-3))
				break
			}
			if t, ok := v.titles[id]; ok {
				names = append(names, cut(t, 40))
			} else {
				names = append(names, id)
			}
		}
		tip = append(tip, "Members: "+strings.Join(names, "; "))
	}
	return append(tip, v.outlookTip(&g.Outlook, g.RatePerDay, g.Concurrency)...)
}

// outlookTip lists the dates and the inputs behind them.
func (v *view) outlookTip(o *roadmap.Outlook, rate, agents *float64) []string {
	var tip []string
	if o.P80 != nil {
		tip = append(tip, fmt.Sprintf("P50 %s · P80 %s · P95 %s", date(o.P50), date(o.P80), date(o.P95)))
	}
	if o.TargetDueAt != nil {
		tip = append(tip, fmt.Sprintf("Target %s: %s", date(o.TargetDueAt), scheduleText(o.Schedule)))
	}
	left := fmt.Sprintf("Remaining %s", progress(o))
	if o.HumanGates > 0 {
		left += " · " + plural(float64(o.HumanGates), "human gate")
	}
	tip = append(tip, left)
	if o.Status != roadmap.StatusDone {
		pace, n := "—", "—"
		if rate != nil {
			pace = num(*rate) + " beads/day"
		}
		if agents != nil {
			n = num(*agents)
		}
		tip = append(tip, fmt.Sprintf("Pace %s · agents %s", pace, n))
	}
	if o.AgentHours != nil || o.HumanHours != nil {
		tip = append(tip, fmt.Sprintf("Agent time %s · waiting on humans %s", hours(o.AgentHours), hours(o.HumanHours)))
	}
	if len(o.CriticalChain) > 0 {
		tip = append(tip, "Critical chain: "+plural(float64(len(o.CriticalChain)), "bead"))
	}
	if len(o.WaitsOnUnloaded) > 0 {
		tip = append(tip, "Waits on unloaded: "+strings.Join(o.WaitsOnUnloaded, ", "))
	}
	return tip
}

func progress(o *roadmap.Outlook) string {
	if o.Total == 0 {
		return "no work"
	}
	return fmt.Sprintf("%d of %d (%s%% done)", o.Remaining, o.Total, num(o.DonePct))
}

func date(t *time.Time) string {
	if t == nil {
		return "—"
	}
	return t.UTC().Format("2006-01-02")
}

func hours(h *float64) string {
	if h == nil {
		return "—"
	}
	return num(*h) + " h"
}

// num formats a number with at most one decimal.
func num(f float64) string {
	return strconv.FormatFloat(math.Round(f*10)/10, 'f', -1, 64)
}

func plural(n float64, noun string) string {
	if n == 1 {
		return "1 " + noun
	}
	return num(n) + " " + noun + "s"
}
