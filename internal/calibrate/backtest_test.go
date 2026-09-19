package calibrate

import (
	"fmt"
	"math/rand/v2"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/alexknips/beadline/internal/config"
	"github.com/alexknips/beadline/internal/forecast"
	"github.com/alexknips/beadline/internal/graph"
	"github.com/alexknips/beadline/internal/load"
)

var day0 = time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC)

func day(d float64) time.Time { return day0.Add(time.Duration(d * float64(24*time.Hour))) }

func stamp(d float64) string { return day(d).Format(time.RFC3339) }

// history is thirty days of one repo: task k is created on day k, started
// an hour later and closed two days after creation. Epic E is created on
// day 0 and takes tasks 20 to 29 as children, each when it is created; it
// closes on day 32. Horizon: day 32.
func history() []string {
	var lines []string
	for k := 0; k < 30; k++ {
		deps := ""
		if k >= 20 {
			deps = fmt.Sprintf(`,"dependencies":[{"depends_on_id":"E","type":"parent-child","created_at":%q}]`, stamp(float64(k)))
		}
		lines = append(lines, fmt.Sprintf(`{"id":"t%d","status":"closed","close_reason":"done","created_at":%q,"started_at":%q,"closed_at":%q,"updated_at":%q%s}`,
			k, stamp(float64(k)), stamp(float64(k)+1.0/24), stamp(float64(k+2)), stamp(float64(k+2)), deps))
	}
	lines = append(lines, fmt.Sprintf(`{"id":"E","issue_type":"epic","status":"closed","created_at":%q,"closed_at":%q,"updated_at":%q}`,
		stamp(0), stamp(32), stamp(32)))
	return lines
}

func TestOrigins(t *testing.T) {
	horizon := time.Date(2026, 9, 19, 10, 28, 0, 0, time.UTC)
	got := Origins(horizon, 10*24*time.Hour, 24*time.Hour)
	var want []time.Time
	for d := 9; d <= 12; d++ {
		want = append(want, time.Date(2026, 9, d, 0, 0, 0, 0, time.UTC))
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("Origins = %v, want %v", got, want)
	}
	if got := Origins(horizon, 5*24*time.Hour, 24*time.Hour); got != nil {
		t.Errorf("a span within MinLead: %v", got)
	}
	if got := Origins(horizon, 10*24*time.Hour, 0); got != nil {
		t.Errorf("no step: %v", got)
	}
}

func TestBacktest(t *testing.T) {
	ex := parse(t, history()...)
	cv := config.Default().Conventions
	var seen []time.Time
	fc := func(g *graph.Graph, _ *load.Report, asOf time.Time) (*forecast.Result, error) {
		seen = append(seen, asOf)
		// Nothing after the origin may reach the forecaster.
		for _, i := range g.Issues() {
			for what, x := range map[string]time.Time{"created": i.CreatedAt, "started": i.StartedAt, "closed": i.ClosedAt} {
				if x.After(asOf) {
					t.Errorf("as of %v, %s was %s at %v", asOf, i.ID, what, x)
				}
			}
			if i.Closed() != (!i.ClosedAt.IsZero()) {
				t.Errorf("as of %v, %s has status %s and closed_at %v", asOf, i.ID, i.Status, i.ClosedAt)
			}
		}
		// No queue, a day of work, no agent limit.
		return forecast.Run(g, forecast.Options{
			Now: asOf, Runs: 10, Seed: 1, Grid: Levels,
			Agent: func(*graph.Issue) forecast.Sampler { return constant{q: 0, w: 24 * 60} },
			Human: func(*graph.Issue, float64) forecast.Draw { return func(*rand.Rand) float64 { return 0 } },
		})
	}
	pairs, origins, err := Backtest(ex, &cv, fc, BacktestOptions{Span: 12 * 24 * time.Hour, Step: 2 * 24 * time.Hour, WindowDays: 10, Seed: 1, Draws: 200})
	if err != nil {
		t.Fatal(err)
	}
	// Horizon day 32: origins on days 20, 22 and 24 (day 26 is less than a
	// week before it).
	if want := []time.Time{day(20), day(22), day(24)}; !reflect.DeepEqual(origins, want) || !reflect.DeepEqual(seen, want) {
		t.Fatalf("origins %v, forecast as of %v; want %v", origins, seen, want)
	}

	find := func(asOf time.Time, method, id string) *Pair {
		for n := range pairs {
			if p := &pairs[n]; p.AsOf.Equal(asOf) && p.Method == method && p.ID == id {
				return p
			}
		}
		return nil
	}
	// On day 22, t20 has just closed (two days after its creation), t21 is
	// in progress and t22 has just been created.
	var leaves []string
	for _, p := range pairs {
		if p.AsOf.Equal(day(22)) && p.Method == MethodModel && p.Level == LevelLeaf {
			leaves = append(leaves, p.ID)
		}
	}
	if !reflect.DeepEqual(leaves, []string{"t21", "t22"}) {
		t.Errorf("leaves on day 22 = %v", leaves)
	}
	t22 := find(day(22), MethodModel, "t22")
	if t22.Outcome != OutcomeResolved || *t22.ActualHours != 48 || t22.Source != "backtest" {
		t.Errorf("t22 = %+v", t22)
	}
	// No agent limit and a day of work: it lands a day later.
	if got := t22.Quantiles["p50"]; got != 24 {
		t.Errorf("t22 model P50 = %v h", got)
	}
	// The baseline: one close a day, so one bead lands within a day.
	if b := find(day(22), MethodBaseline, "t22"); b == nil || b.Quantiles["p99"] > 24 || b.Quantiles["p05"] <= 0 || b.Outcome != OutcomeResolved {
		t.Errorf("t22 baseline = %+v", b)
	}

	// The epic: forecast at every origin, its breakdown grown since.
	e := find(day(20), MethodModel, "E")
	if e == nil || e.Level != LevelHighLevel || e.Outcome != OutcomeResolved || *e.ActualHours != 12*24 || e.ScopeAdded != 9 || e.ScopeRemoved != 0 {
		t.Errorf("E on day 20 = %+v", e)
	}
	if e := find(day(24), MethodBaseline, "E"); e == nil || e.ScopeAdded != 5 {
		t.Errorf("E baseline on day 24 = %+v", e)
	}
	for _, p := range pairs {
		if p.Method != MethodModel && p.Method != MethodBaseline {
			t.Errorf("pair %+v", p)
		}
	}

	again, _, _ := Backtest(ex, &cv, fc, BacktestOptions{Span: 12 * 24 * time.Hour, Step: 2 * 24 * time.Hour, WindowDays: 10, Seed: 1, Draws: 200})
	if !reflect.DeepEqual(again, pairs) {
		t.Error("a backtest is not reproducible")
	}

	_, _, err = Backtest(ex, &cv, fc, BacktestOptions{Span: 24 * time.Hour, Step: 24 * time.Hour, WindowDays: 10, Seed: 1, Draws: 10})
	if err == nil || !strings.Contains(err.Error(), "no backtest origin") {
		t.Errorf("span too short: err = %v", err)
	}
}

func TestThroughputBaseline(t *testing.T) {
	g := graphOf(t, parse(t, history()...), day(20))
	tp := newThroughput(g, NewReality(g, day(20)), day(20), 10)
	// Tasks 9 to 18 closed in the ten days before day 20, one a day.
	if want := []int{1, 1, 1, 1, 1, 1, 1, 1, 1, 1}; !reflect.DeepEqual(tp.daily["r"], want) {
		t.Errorf("daily closes = %v", tp.daily["r"])
	}
	r := rand.New(rand.NewPCG(1, 1))
	grid := tp.draw("r", false, 3, 100, r)
	if grid[0] <= 48 || grid[len(grid)-1] > 72 {
		t.Errorf("three beads at one a day: grid %v, want within (48, 72] hours", grid)
	}
	// A repo that closed nothing: one close in the window is assumed.
	idle := &throughput{daily: map[string][]int{"r": make([]int, 10)}, all: make([]int, 10)}
	grid = idle.draw("r", false, 1, 100, r)
	if grid[len(Levels)/2] < 24 {
		t.Errorf("idle repo: grid %v, want days", grid)
	}
}
