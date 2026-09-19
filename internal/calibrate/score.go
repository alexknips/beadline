package calibrate

import (
	"math"
	"math/rand/v2"
	"sort"
)

// Scored are the quantiles whose coverage is reported.
var Scored = []float64{0.5, 0.8, 0.95}

// MinTargets is the number of distinct targets below which coverage is only
// indicative: one bead forecast at many moments is still one bead.
const MinTargets = 20

// Coverage is how often one quantile held: the target closed by then.
type Coverage struct {
	Level float64 `json:"level"`
	// Held counts the pairs that closed by the quantile, Known those whose
	// outcome is known: resolved, or open past the quantile. Pending are
	// open and not yet past it.
	Held    int `json:"held"`
	Known   int `json:"known"`
	Pending int `json:"pending"`
	// Share is Held / Known. Low and High bound it once the pending pairs
	// resolve: all of them late, or all of them in time.
	Share *float64 `json:"share,omitempty"`
	Low   *float64 `json:"low,omitempty"`
	High  *float64 `json:"high,omitempty"`
	// CILow and CIHigh are a 90% interval of Share that resamples targets,
	// not pairs: forecasts of one bead at several moments are not
	// independent.
	CILow  *float64 `json:"ci_low,omitempty"`
	CIHigh *float64 `json:"ci_high,omitempty"`
	// Stretch is the factor on the quantile (time from as_of) at which it
	// would have held at its nominal level: above 1, the intervals are too
	// narrow; below 1, too wide. Absent when no factor up to 1024 would do.
	Stretch *float64 `json:"stretch,omitempty"`
}

// Summary scores a set of pairs.
type Summary struct {
	Pairs int `json:"pairs"`
	// Targets counts the distinct beads among the pairs that are not void.
	Targets  int            `json:"targets"`
	Resolved int            `json:"resolved"`
	Open     int            `json:"open"`
	Void     map[string]int `json:"void,omitempty"`
	Coverage []Coverage     `json:"coverage"`
	// Over the resolved pairs, in days: the median of P50 − actual
	// (negative: forecasts too early), the median of |P50 − actual|, and the
	// mean CRPS of the quantile grid.
	BiasDays           *float64 `json:"bias_days,omitempty"`
	MedianAbsErrorDays *float64 `json:"median_abs_error_days,omitempty"`
	CRPSDays           *float64 `json:"crps_days,omitempty"`
	// PIT counts the resolved pairs by the forecast's probability of an
	// earlier close, in fifths. A calibrated forecast spreads them evenly;
	// a pile at the right means reality came later than forecast.
	PIT []int `json:"pit,omitempty"`
	// FewTargets is set below MinTargets.
	FewTargets bool `json:"few_targets,omitempty"`
}

// Summarize scores pairs.
func Summarize(pairs []Pair) Summary {
	s := Summary{Pairs: len(pairs), Coverage: []Coverage{}}
	targets := map[string]bool{}
	var bias, crpsSum []float64
	pit := make([]int, 5)
	for n := range pairs {
		p := &pairs[n]
		switch p.Outcome {
		case OutcomeVoid:
			if s.Void == nil {
				s.Void = map[string]int{}
			}
			s.Void[p.Void]++
			continue
		case OutcomeResolved:
			s.Resolved++
			pts := p.Quantiles.points()
			if q50, ok := p.Quantiles.at(0.5); ok {
				bias = append(bias, (q50-*p.ActualHours)/24)
			}
			if len(pts) > 0 {
				crpsSum = append(crpsSum, crps(pts, *p.ActualHours)/24)
				pit[min(4, int(pitOf(pts, *p.ActualHours)*5))]++
			}
		case OutcomeOpen:
			s.Open++
		}
		targets[p.ID] = true
	}
	s.Targets = len(targets)
	s.FewTargets = s.Targets < MinTargets
	for _, q := range Scored {
		s.Coverage = append(s.Coverage, coverage(pairs, q))
	}
	if len(bias) > 0 {
		abs := make([]float64, len(bias))
		for n, b := range bias {
			abs[n] = math.Abs(b)
		}
		s.BiasDays, s.MedianAbsErrorDays = round2(median(bias)), round2(median(abs))
	}
	if len(crpsSum) > 0 {
		total := 0.0
		for _, c := range crpsSum {
			total += c
		}
		s.CRPSDays = round2(total / float64(len(crpsSum)))
		s.PIT = pit
	}
	return s
}

// coverage scores quantile q over the pairs.
func coverage(pairs []Pair, q float64) Coverage {
	c := Coverage{Level: q}
	type group struct{ held, known int }
	groups := map[string]*group{}
	var order []string
	for n := range pairs {
		p := &pairs[n]
		if p.Outcome == OutcomeVoid {
			continue
		}
		if _, ok := p.Quantiles.at(q); !ok {
			continue
		}
		h := p.Held(q)
		if h == nil {
			c.Pending++
			continue
		}
		c.Known++
		g := groups[p.ID]
		if g == nil {
			g = &group{}
			groups[p.ID] = g
			order = append(order, p.ID)
		}
		g.known++
		if *h {
			c.Held++
			g.held++
		}
	}
	if all := c.Known + c.Pending; all > 0 {
		c.Low, c.High = round4(float64(c.Held)/float64(all)), round4(float64(c.Held+c.Pending)/float64(all))
	}
	if c.Known == 0 {
		return c
	}
	c.Share = round4(float64(c.Held) / float64(c.Known))

	// Bootstrap over targets, seeded: the same pairs give the same interval.
	const draws = 1000
	sort.Strings(order)
	r := rand.New(rand.NewPCG(7, uint64(math.Round(q*100))))
	shares := make([]float64, 0, draws)
	for b := 0; b < draws; b++ {
		held, known := 0, 0
		for range order {
			g := groups[order[r.IntN(len(order))]]
			held += g.held
			known += g.known
		}
		shares = append(shares, float64(held)/float64(known))
	}
	sort.Float64s(shares)
	c.CILow, c.CIHigh = round4(shares[int(0.05*draws)]), round4(shares[int(0.95*draws)-1])
	c.Stretch = stretch(pairs, q)
	return c
}

// stretch returns the smallest factor 2^(k/6), k from −60 to 60, on
// quantile q at which the known pairs would have held at level q. An open
// pair is a known miss while the stretched quantile lies before its
// elapsed time, and unknown after.
func stretch(pairs []Pair, q float64) *float64 {
	for k := -60; k <= 60; k++ {
		c := math.Pow(2, float64(k)/6)
		held, known := 0, 0
		for n := range pairs {
			p := &pairs[n]
			v, ok := p.Quantiles.at(q)
			if !ok {
				continue
			}
			switch p.Outcome {
			case OutcomeResolved:
				known++
				if *p.ActualHours <= c*v {
					held++
				}
			case OutcomeOpen:
				if c*v < p.ElapsedHours {
					known++
				}
			}
		}
		if known > 0 && float64(held) >= q*float64(known) {
			return round2(c)
		}
	}
	return nil
}

// pinball is the quantile loss of value v at level q for an actual a.
func pinball(q, v, a float64) float64 {
	d := a - v
	return math.Max(q*d, (q-1)*d)
}

// crps approximates the continuous ranked probability score of a forecast
// given as a quantile grid: twice the integral of the pinball loss over the
// levels, each level's loss standing for the levels up to halfway to its
// neighbours, and the outermost levels for the tails.
func crps(pts []point, actual float64) float64 {
	total := 0.0
	for n, p := range pts {
		lo, hi := 0.0, 1.0
		if n > 0 {
			lo = (pts[n-1].q + p.q) / 2
		}
		if n < len(pts)-1 {
			hi = (p.q + pts[n+1].q) / 2
		}
		total += (hi - lo) * pinball(p.q, p.hours, actual)
	}
	return 2 * total
}

// pitOf is the forecast's probability that the close comes no later than
// actual: the grid interpolated in log time, with half the outer levels'
// mass beyond each end. An actual equal to a run of levels (a point
// forecast) sits in the middle of the run.
func pitOf(pts []point, actual float64) float64 {
	const floor = 1e-3 // hours
	lx := math.Log(math.Max(actual, floor))
	logOf := func(p point) float64 { return math.Log(math.Max(p.hours, floor)) }
	first, last := -1, -1
	for n, p := range pts {
		if logOf(p) == lx {
			if first < 0 {
				first = n
			}
			last = n
		}
	}
	switch {
	case first >= 0:
		return (pts[first].q + pts[last].q) / 2
	case lx < logOf(pts[0]):
		return pts[0].q / 2
	}
	for n := 1; n < len(pts); n++ {
		if l0, l1 := logOf(pts[n-1]), logOf(pts[n]); lx < l1 {
			return pts[n-1].q + (pts[n].q-pts[n-1].q)*(lx-l0)/(l1-l0)
		}
	}
	return (1 + pts[len(pts)-1].q) / 2
}

func median(v []float64) float64 {
	s := append([]float64(nil), v...)
	sort.Float64s(s)
	m := len(s) / 2
	if len(s)%2 == 1 {
		return s[m]
	}
	return (s[m-1] + s[m]) / 2
}

func round2(v float64) *float64 {
	r := math.Round(v*100) / 100
	return &r
}

func round4(v float64) *float64 {
	r := math.Round(v*10000) / 10000
	return &r
}
