package calibrate

import (
	"errors"
	"fmt"
	"hash/fnv"
	"math"
	"math/rand/v2"
	"sort"
	"time"

	"github.com/alexknips/beadline/internal/config"
	"github.com/alexknips/beadline/internal/forecast"
	"github.com/alexknips/beadline/internal/graph"
	"github.com/alexknips/beadline/internal/load"
)

// Forecaster forecasts g as it stood at asOf, with forecast.Options.Grid
// set to Levels. It must learn only from g, which holds nothing after
// asOf. The caller wires in the model.
type Forecaster func(g *graph.Graph, rep *load.Report, asOf time.Time) (*forecast.Result, error)

// BacktestOptions configure a backtest.
type BacktestOptions struct {
	// Span is how far before the data horizon the first origin lies, Step
	// the time between origins.
	Span, Step time.Duration
	// WindowDays is the model window: the baseline measures throughput
	// over it.
	WindowDays int
	// Seed and Draws drive the baseline's bootstrap.
	Seed  uint64
	Draws int
}

// MinLead is how long before the data horizon the last origin lies: its
// forecasts need time to resolve.
const MinLead = 7 * 24 * time.Hour

// Origins returns the forecast moments of a backtest ending at horizon:
// from the midnight (UTC) on or before horizon − span, every step, up to
// MinLead before horizon.
func Origins(horizon time.Time, span, step time.Duration) []time.Time {
	var out []time.Time
	if step <= 0 {
		return nil
	}
	last := horizon.Add(-MinLead)
	for t := horizon.Add(-span).UTC().Truncate(24 * time.Hour); !t.After(last); t = t.Add(step) {
		out = append(out, t)
	}
	return out
}

// Backtest replays history from one set of exports. At each origin it
// rewinds the exports to that moment (load.Exports.Graph), forecasts from
// what was visible then, and grades every leaf and dated high-level
// forecast against the exports as they are now. A throughput-only baseline
// is scored alongside.
func Backtest(ex *load.Exports, cv *config.Conventions, fc Forecaster, o BacktestOptions) ([]Pair, []time.Time, error) {
	if o.Draws < 1 || o.WindowDays < 1 {
		return nil, nil, errors.New("calibrate: backtest needs draws and a window")
	}
	horizon := ex.Horizon()
	origins := Origins(horizon, o.Span, o.Step)
	if len(origins) == 0 {
		return nil, nil, fmt.Errorf("calibrate: no backtest origin: the span must reach more than %s before the data horizon %s",
			MinLead, horizon.Format(time.RFC3339))
	}
	gNow, _, err := ex.Graph(cv, time.Time{})
	if err != nil {
		return nil, nil, err
	}
	reality := NewReality(gNow, horizon)

	var pairs []Pair
	for _, t := range origins {
		g, rep, err := ex.Graph(cv, t)
		if err != nil {
			return nil, nil, err
		}
		res, err := fc(g, rep, t)
		if err != nil {
			return nil, nil, fmt.Errorf("forecast as of %s: %w", t.Format(time.RFC3339), err)
		}
		base := newThroughput(g, NewReality(g, t), t, o.WindowDays)
		goals := map[string]graph.Goal{}
		for _, gl := range g.Goals() {
			goals[gl.ID] = gl
		}
		add := func(p Pair, then []string, n int) {
			reality.grade(&p, then)
			pairs = append(pairs, p)
			p.Method = MethodBaseline
			p.Quantiles = NewGrid(Levels, base.draw(p.Repo, p.Goal, n, o.Draws, seed(o.Seed, p.ID, t)))
			reality.grade(&p, then)
			pairs = append(pairs, p)
		}
		for _, l := range res.Leaves {
			add(Pair{Source: "backtest", AsOf: t, Method: MethodModel, Level: LevelLeaf, ID: l.ID, Repo: l.Repo,
				Quantiles: NewGrid(res.Grid, l.GridHours)}, nil, 1)
		}
		items := func(list []forecast.Item, goal bool) {
			for _, it := range list {
				if it.GridHours == nil {
					continue
				}
				then := breakdown(g, it.ID)
				if goal {
					then = goalBreakdown(g, goals[it.ID])
				}
				n := len(it.Remaining) - len(it.Deferred) - len(it.Stuck)
				add(Pair{Source: "backtest", AsOf: t, Method: MethodModel, Level: LevelHighLevel, ID: it.ID, Repo: it.Repo,
					Goal: goal, Quantiles: NewGrid(res.Grid, it.GridHours)}, then, max(1, n))
			}
		}
		items(res.Items, false)
		items(res.Goals, true)
	}
	return pairs, origins, nil
}

// seed derives the baseline's random source of one target at one origin.
func seed(base uint64, id string, t time.Time) *rand.Rand {
	h := fnv.New64a()
	fmt.Fprintf(h, "%s\x00%d", id, t.Unix())
	return rand.New(rand.NewPCG(base, h.Sum64()))
}

// throughput is the naive baseline: a repository closes work at the pace
// of its recent days, and a target lands when that many beads have closed.
// It assumes the whole repo works on the target and knows nothing of the
// graph, of age or of priority.
type throughput struct {
	daily map[string][]int // closes per day, per repo, over the window
	all   []int            // all repos together, for goals
}

func newThroughput(g *graph.Graph, r *Reality, asOf time.Time, windowDays int) *throughput {
	since := asOf.AddDate(0, 0, -windowDays)
	first := map[string]time.Time{}
	for _, i := range g.Issues() {
		if f, ok := first[i.Repo]; !ok || i.CreatedAt.Before(f) {
			first[i.Repo] = i.CreatedAt
		}
	}
	days := func(from time.Time) int { return max(1, int(asOf.Sub(from)/(24*time.Hour))) }
	tp := &throughput{daily: map[string][]int{}, all: make([]int, windowDays)}
	for repo, f := range first {
		from := since
		if f.After(from) {
			from = f
		}
		tp.daily[repo] = make([]int, days(from))
	}
	for _, i := range forecast.WorkBeads(g) {
		if !i.Closed() || i.ClosedAt.IsZero() || !i.ClosedAt.After(since) || i.ClosedAt.After(asOf) || r.Descoped(i) {
			continue
		}
		back := int(asOf.Sub(i.ClosedAt) / (24 * time.Hour)) // 0: the last day
		if d := tp.daily[i.Repo]; back < len(d) {
			d[len(d)-1-back]++
		}
		if back < len(tp.all) {
			tp.all[len(tp.all)-1-back]++
		}
	}
	return tp
}

// draw returns the grid of the time until n beads have closed, in hours,
// from draws bootstrapped runs of daily closes.
func (tp *throughput) draw(repo string, goal bool, n, draws int, r *rand.Rand) []float64 {
	daily := tp.daily[repo]
	if goal || daily == nil {
		daily = tp.all
	}
	busy := false
	for _, c := range daily {
		busy = busy || c > 0
	}
	if !busy {
		// No close in the window: assume one.
		daily = make([]int, max(1, len(daily)))
		daily[len(daily)-1] = 1
	}
	const maxDays = 3650
	finish := make([]float64, draws)
	for k := range finish {
		closed, d := 0, 0
		for closed < n && d < maxDays {
			closed += daily[r.IntN(len(daily))]
			d++
		}
		// It lands somewhere within its last day.
		finish[k] = (float64(d) - r.Float64()) * 24
	}
	return quantilesOf(finish, Levels)
}

// quantilesOf returns the nearest-rank quantiles of values at the levels,
// rounded to hundredths.
func quantilesOf(values, levels []float64) []float64 {
	sorted := append([]float64(nil), values...)
	sort.Float64s(sorted)
	out := make([]float64, len(levels))
	for n, q := range levels {
		k := max(0, min(len(sorted)-1, int(math.Ceil(q*float64(len(sorted))))-1))
		out[n] = *round2(sorted[k])
	}
	return out
}
