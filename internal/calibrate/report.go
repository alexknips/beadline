package calibrate

import (
	"fmt"
	"io"
	"sort"
	"strings"
	"text/tabwriter"
	"time"
)

// The release gate (ADR-2, decision D3): beadline is not released until
// the leaf-bead backtest on its own history holds P80 GateLow to GateHigh
// of the time.
const (
	GateLevel = 0.8
	GateLow   = 0.70
	GateHigh  = 0.90
)

// Report is the result of beadline check.
type Report struct {
	// Mode is "snapshots" or "backtest".
	Mode string `json:"mode"`
	// Horizon is the data's now, the moment outcomes are known up to.
	Horizon time.Time `json:"horizon"`
	// Snapshots lists the graded snapshot files; Origins the backtest's
	// forecast moments.
	Snapshots []string    `json:"snapshots,omitempty"`
	Origins   []time.Time `json:"origins,omitempty"`
	// Runs is the number of simulated schedules behind each backtest
	// forecast.
	Runs int `json:"runs,omitempty"`
	// DependencySkewHours corrects dependency timestamps from SkewSince on
	// (load.Skew); absent when there is none.
	DependencySkewHours float64    `json:"dependency_skew_hours,omitempty"`
	SkewSince           *time.Time `json:"skew_since,omitempty"`
	// Problems are snapshots that could not be graded, and why.
	Problems []string `json:"problems,omitempty"`

	Leaf      Group `json:"leaf"`
	HighLevel Group `json:"high_level"`
	// Gate is the release gate; backtests only.
	Gate  *Gate  `json:"gate,omitempty"`
	Pairs []Pair `json:"pairs"`
}

// Group scores one level: the model, the baseline when there is one, and
// for high-level targets the fixed-scope and changed-scope pairs apart.
type Group struct {
	Model        Summary  `json:"model"`
	Baseline     *Summary `json:"baseline,omitempty"`
	FixedScope   *Summary `json:"fixed_scope,omitempty"`
	ScopeChanged *Summary `json:"scope_changed,omitempty"`
}

// Gate is the release gate's verdict.
type Gate struct {
	Level float64  `json:"level"`
	Low   float64  `json:"low"`
	High  float64  `json:"high"`
	Share *float64 `json:"share,omitempty"`
	Pass  bool     `json:"pass"`
	// Reason explains a failure.
	Reason string `json:"reason,omitempty"`
}

// NewReport summarizes graded pairs. A backtest report carries the gate.
func NewReport(mode string, horizon time.Time, pairs []Pair) *Report {
	r := &Report{Mode: mode, Horizon: horizon, Pairs: pairs}
	if r.Pairs == nil {
		r.Pairs = []Pair{}
	}
	pick := func(level, method string, keep func(*Pair) bool) []Pair {
		var out []Pair
		for n := range pairs {
			p := &pairs[n]
			if p.Level == level && p.Method == method && (keep == nil || keep(p)) {
				out = append(out, *p)
			}
		}
		return out
	}
	optional := func(ps []Pair) *Summary {
		if len(ps) == 0 {
			return nil
		}
		s := Summarize(ps)
		return &s
	}
	r.Leaf.Model = Summarize(pick(LevelLeaf, MethodModel, nil))
	r.Leaf.Baseline = optional(pick(LevelLeaf, MethodBaseline, nil))
	r.HighLevel.Model = Summarize(pick(LevelHighLevel, MethodModel, nil))
	r.HighLevel.Baseline = optional(pick(LevelHighLevel, MethodBaseline, nil))
	r.HighLevel.FixedScope = optional(pick(LevelHighLevel, MethodModel, func(p *Pair) bool { return p.Outcome != OutcomeVoid && p.FixedScope() }))
	r.HighLevel.ScopeChanged = optional(pick(LevelHighLevel, MethodModel, func(p *Pair) bool { return p.Outcome != OutcomeVoid && !p.FixedScope() }))
	if mode == "backtest" {
		r.Gate = gate(r.Leaf.Model)
	}
	return r
}

func gate(s Summary) *Gate {
	g := &Gate{Level: GateLevel, Low: GateLow, High: GateHigh}
	var c *Coverage
	for n := range s.Coverage {
		if s.Coverage[n].Level == GateLevel {
			c = &s.Coverage[n]
		}
	}
	switch {
	case c == nil || c.Share == nil:
		g.Reason = "no leaf forecast has a known outcome"
	case s.FewTargets:
		g.Share = c.Share
		g.Reason = fmt.Sprintf("only %d leaf beads were forecast; at least %d are needed", s.Targets, MinTargets)
	default:
		g.Share = c.Share
		g.Pass = *c.Share >= GateLow && *c.Share <= GateHigh
		switch {
		case *c.Share < GateLow:
			g.Reason = "intervals too narrow: too many leaves closed after their P80"
		case *c.Share > GateHigh:
			g.Reason = "intervals too wide: P80 is padded"
		}
	}
	return g
}

const dateFormat = "2006-01-02 15:04"

// WriteText prints the report for people: leaves first, the honest sample,
// then high-level items, then the gate.
func (r *Report) WriteText(w io.Writer) {
	switch r.Mode {
	case "backtest":
		first, last := r.Origins[0], r.Origins[len(r.Origins)-1]
		fmt.Fprintf(w, "backtest: %s from %s to %s, %d runs each; outcomes known to %s UTC\n",
			plural(len(r.Origins), "origin"), first.Format(dateFormat), last.Format(dateFormat), r.Runs, r.Horizon.Format(dateFormat))
		fmt.Fprintln(w, "each forecast used only the data visible at its origin")
	default:
		fmt.Fprintf(w, "%s graded against data as of %s UTC\n", plural(len(r.Snapshots), "snapshot"), r.Horizon.Format(dateFormat))
	}
	if r.SkewSince != nil {
		fmt.Fprintf(w, "dependency timestamps corrected by -%gh from %s UTC (exported local time labelled UTC)\n",
			r.DependencySkewHours, r.SkewSince.Format(dateFormat))
	}

	fmt.Fprintf(w, "\nLEAF BEADS (the honest sample)\n")
	r.Leaf.write(w, "")
	fmt.Fprintf(w, "\nHIGH-LEVEL (milestones, epics, goals)\n")
	r.HighLevel.write(w, "item")
	if f, c := r.HighLevel.FixedScope, r.HighLevel.ScopeChanged; f != nil || c != nil {
		for _, part := range []struct {
			name string
			s    *Summary
		}{{"fixed scope", f}, {"scope changed", c}} {
			if part.s == nil {
				continue
			}
			fmt.Fprintf(w, "  %s: %s", part.name, plural(part.s.Pairs, "forecast"))
			for _, cv := range part.s.Coverage {
				if cv.Level == GateLevel {
					fmt.Fprintf(w, ", P80 held %s", held(cv))
				}
			}
			fmt.Fprintln(w)
		}
	}

	if r.Gate != nil {
		verdict := "FAIL"
		if r.Gate.Pass {
			verdict = "PASS"
		}
		share := "-"
		if r.Gate.Share != nil {
			share = pct(*r.Gate.Share)
		}
		fmt.Fprintf(w, "\nrelease gate (leaf P80 held %s-%s): %s %s", pct(GateLow), pct(GateHigh), verdict, share)
		if r.Gate.Reason != "" {
			fmt.Fprintf(w, " (%s)", r.Gate.Reason)
		}
		fmt.Fprintln(w)
	}
}

func (g Group) write(w io.Writer, noun string) {
	s := g.Model
	if s.Pairs == 0 {
		fmt.Fprintln(w, "  no forecasts to grade")
		return
	}
	what := "bead"
	if noun != "" {
		what = noun
	}
	fmt.Fprintf(w, "  %s: %d resolved, %d still open (right-censored)", plural(s.Pairs, "forecast"), s.Resolved, s.Open)
	if len(s.Void) > 0 {
		var reasons []string
		total := 0
		for reason, n := range s.Void {
			reasons = append(reasons, fmt.Sprintf("%d %s", n, reason))
			total += n
		}
		sort.Strings(reasons)
		fmt.Fprintf(w, ", %d void (%s)", total, strings.Join(reasons, ", "))
	}
	fmt.Fprintf(w, "; %s scored\n", plural(s.Targets, "distinct "+what))

	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "  METHOD\tQUANTILE\tHELD\tSHARE\t90% CI\tIF OPEN RESOLVE\tSTRETCH")
	rows := func(method string, s *Summary) {
		if s == nil {
			return
		}
		for _, c := range s.Coverage {
			share, ci, bounds, str := "-", "-", "-", "-"
			if c.Share != nil {
				share = pct(*c.Share)
			}
			if c.CILow != nil {
				ci = pct(*c.CILow) + "-" + pct(*c.CIHigh)
			}
			if c.Low != nil {
				bounds = pct(*c.Low) + "-" + pct(*c.High)
			}
			if c.Stretch != nil {
				str = fmt.Sprintf("x%.2f", *c.Stretch)
			}
			fmt.Fprintf(tw, "  %s\tP%.0f\t%d/%d\t%s\t%s\t%s\t%s\n", method, c.Level*100, c.Held, c.Known, share, ci, bounds, str)
		}
	}
	rows("model", &g.Model)
	rows("baseline", g.Baseline)
	tw.Flush()

	errs := func(method string, s *Summary) {
		if s == nil || s.BiasDays == nil {
			return
		}
		bias := "bias 0.0 d"
		switch {
		case *s.BiasDays >= 0.05:
			bias = fmt.Sprintf("bias %+.1f d (forecasts later than reality)", *s.BiasDays)
		case *s.BiasDays <= -0.05:
			bias = fmt.Sprintf("bias %+.1f d (forecasts earlier than reality)", *s.BiasDays)
		}
		fmt.Fprintf(w, "  %s error: %s, median |P50 - actual| %.1f d, CRPS %.1f d\n", method, bias, *s.MedianAbsErrorDays, *s.CRPSDays)
	}
	errs("model", &g.Model)
	errs("baseline", g.Baseline)
	if len(s.PIT) > 0 {
		fmt.Fprintf(w, "  model PIT by fifths (even when calibrated; a pile at the right = later than forecast): %s\n", ints(s.PIT))
	}
	if s.FewTargets {
		fmt.Fprintf(w, "  ! only %s (< %d): coverage is indicative; do not tune intervals on it\n", plural(s.Targets, "distinct "+what), MinTargets)
	}
}

// held formats a coverage as "held/known (share)".
func held(c Coverage) string {
	if c.Share == nil {
		return fmt.Sprintf("%d/%d", c.Held, c.Known)
	}
	return fmt.Sprintf("%d/%d (%s)", c.Held, c.Known, pct(*c.Share))
}

func pct(v float64) string { return fmt.Sprintf("%.0f%%", v*100) }

func ints(v []int) string {
	s := make([]string, len(v))
	for n, x := range v {
		s[n] = fmt.Sprint(x)
	}
	return strings.Join(s, " ")
}

func plural(n int, noun string) string {
	if n == 1 {
		return "1 " + noun
	}
	return fmt.Sprintf("%d %ss", n, noun)
}
