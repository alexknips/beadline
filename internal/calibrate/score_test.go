package calibrate

import (
	"fmt"
	"math"
	"reflect"
	"testing"
)

func resolved(id string, grid Grid, actual float64) Pair {
	return Pair{ID: id, Quantiles: grid, Outcome: OutcomeResolved, ActualHours: &actual}
}

func open(id string, grid Grid, elapsed float64) Pair {
	return Pair{ID: id, Quantiles: grid, Outcome: OutcomeOpen, ElapsedHours: elapsed}
}

func TestSummarize(t *testing.T) {
	grid := NewGrid([]float64{0.5, 0.8, 0.95}, []float64{10, 20, 40})
	pairs := []Pair{
		resolved("a", grid, 5),  // held at every level
		resolved("b", grid, 15), // P80 and P95
		resolved("c", grid, 30), // P95 only
		resolved("d", grid, 50), // none
		open("e", grid, 25),     // P50 and P80 missed, P95 pending
		{ID: "f", Quantiles: grid, Outcome: OutcomeVoid, Void: VoidParked},
	}
	s := Summarize(pairs)
	if s.Pairs != 6 || s.Targets != 5 || s.Resolved != 4 || s.Open != 1 || !reflect.DeepEqual(s.Void, map[string]int{VoidParked: 1}) || !s.FewTargets {
		t.Errorf("counts = %+v", s)
	}
	type want struct {
		held, known, pending int
		share, low, high     float64
	}
	for n, w := range []want{
		{1, 5, 0, 0.2, 0.2, 0.2},
		{2, 5, 0, 0.4, 0.4, 0.4},
		{3, 4, 1, 0.75, 0.6, 0.8},
	} {
		c := s.Coverage[n]
		if c.Level != Scored[n] || c.Held != w.held || c.Known != w.known || c.Pending != w.pending ||
			*c.Share != w.share || *c.Low != w.low || *c.High != w.high {
			t.Errorf("P%.0f coverage = %+v, want %+v", Scored[n]*100, c, w)
		}
		if *c.CILow > *c.Share || *c.CIHigh < *c.Share || *c.CILow < 0 || *c.CIHigh > 1 {
			t.Errorf("P%.0f 90%% interval %v-%v does not bracket %v", Scored[n]*100, *c.CILow, *c.CIHigh, *c.Share)
		}
	}
	// P50 − actual: +5, −5, −20, −40 hours.
	if *s.BiasDays != math.Round(-12.5/24*100)/100 || *s.MedianAbsErrorDays != math.Round(12.5/24*100)/100 {
		t.Errorf("bias %v, median |error| %v", *s.BiasDays, *s.MedianAbsErrorDays)
	}
	if s.CRPSDays == nil || *s.CRPSDays <= 0 || len(s.PIT) != 5 {
		t.Errorf("crps %v, pit %v", s.CRPSDays, s.PIT)
	}
	if again := Summarize(pairs); !reflect.DeepEqual(again, s) {
		t.Error("the same pairs summarize differently: the bootstrap must be seeded")
	}

	var many []Pair
	for n := 0; n < MinTargets; n++ {
		many = append(many, resolved(fmt.Sprint(n), grid, 1))
	}
	if Summarize(many).FewTargets {
		t.Errorf("%d targets reported as few", MinTargets)
	}
	if empty := Summarize(nil); empty.Coverage[1].Share != nil || empty.BiasDays != nil {
		t.Errorf("empty summary = %+v", empty)
	}
}

func TestStretch(t *testing.T) {
	grid := NewGrid([]float64{0.5, 0.8}, []float64{10, 20})
	var pairs []Pair
	for n := 0; n < 10; n++ {
		pairs = append(pairs, resolved(fmt.Sprint(n), grid, 40)) // everything twice as late as P80
	}
	c := Summarize(pairs).Coverage[1]
	if *c.Share != 0 || c.Stretch == nil || *c.Stretch != 2 {
		t.Errorf("P80 coverage %v, stretch %v; want 0 and x2", *c.Share, c.Stretch)
	}
	// Open pairs past a stretched quantile are misses too.
	pairs = append(pairs, open("x", grid, 1000), open("y", grid, 1000), open("z", grid, 1000))
	if s := Summarize(pairs).Coverage[1].Stretch; s == nil || *s <= 2 {
		t.Errorf("with three long-open pairs, stretch = %v; want more than x2", s)
	}
	// Intervals far too wide: a factor below 1.
	pairs = nil
	for n := 0; n < 10; n++ {
		pairs = append(pairs, resolved(fmt.Sprint(n), grid, 1))
	}
	if s := Summarize(pairs).Coverage[1].Stretch; s == nil || *s >= 0.1 {
		t.Errorf("stretch = %v; want well below 1", s)
	}
}

func TestCRPS(t *testing.T) {
	pts := []point{{0.25, 5}, {0.5, 5}, {0.75, 5}}
	// A point forecast scores its absolute error.
	for _, actual := range []float64{5, 6, 2} {
		if got, want := crps(pts, actual), math.Abs(actual-5); math.Abs(got-want) > 1e-9 {
			t.Errorf("crps(point at 5, %v) = %v, want %v", actual, got, want)
		}
	}
	// Sharp and right beats wide and right.
	sharp := NewGrid(Levels, spread(10, 1.1)).points()
	wide := NewGrid(Levels, spread(10, 3)).points()
	if crps(sharp, 10) >= crps(wide, 10) {
		t.Errorf("crps sharp %v >= wide %v", crps(sharp, 10), crps(wide, 10))
	}
}

// spread returns a grid around median m, each level f times the last.
func spread(m, f float64) []float64 {
	out := make([]float64, len(Levels))
	for n, q := range Levels {
		out[n] = m * math.Pow(f, (q-0.5)*10)
	}
	return out
}

func TestPIT(t *testing.T) {
	pts := NewGrid([]float64{0.1, 0.5, 0.9}, []float64{1, 10, 100}).points()
	for actual, want := range map[float64]float64{0.5: 0.05, 1: 0.1, math.Sqrt(10): 0.3, 10: 0.5, 100: 0.9, 1000: 0.95} {
		if got := pitOf(pts, actual); math.Abs(got-want) > 1e-9 {
			t.Errorf("pit(%v) = %v, want %v", actual, got, want)
		}
	}
	flat := NewGrid([]float64{0.1, 0.5, 0.9}, []float64{4, 4, 4}).points()
	if got := pitOf(flat, 4); got != 0.5 {
		t.Errorf("pit on a point forecast at the point = %v", got)
	}
}
