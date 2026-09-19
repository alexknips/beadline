package dist

import (
	"math"
	"math/rand/v2"
	"sort"
)

// MinMinutes floors every observation: beads can close in the second they
// open, and a duration of zero has no logarithm.
const MinMinutes = 1.0

const (
	// minBandwidth is the smallest smoothing bandwidth, in log space.
	minBandwidth = 0.1
	// beyondTries bounds the rejection loop of SampleBeyond.
	beyondTries = 64
	// tailSpread scales the node's own bandwidth for the spread beyond the
	// tail anchor (dist.Censored), matching Silverman's own coefficient in
	// bandwidth(). A shorter spread than a full bandwidth: the anchor is
	// already the longest observation of either kind, not a shorter
	// completed duration being extrapolated past, so the added noise needs
	// less width. Chosen from the backtest (docs/design.md, ADR-2 §1): 1.0
	// (the node's plain bandwidth) leaves the most recent leaf P80 backtest
	// just over the release gate's 90% ceiling.
	tailSpread = 0.9
)

// Prior is the log-normal at the root of every backoff chain.
type Prior struct {
	Median float64 // minutes
	Sigma  float64 // standard deviation of the log
}

// NewPrior returns the root prior of ADR-1 §2: a log-normal with the given
// median in minutes and σ = 1, so P80 ≈ 2.3 × median and P99 ≈ 10 × median.
func NewPrior(median float64) Prior {
	return Prior{Median: median, Sigma: 1}
}

// Quantile returns the q-quantile of the prior in minutes, for 0 < q < 1.
func (p Prior) Quantile(q float64) float64 {
	return p.Median * math.Exp(p.Sigma*math.Sqrt2*math.Erfinv(2*q-1))
}

// Dist is one node of a backoff chain for one quantity (lead time, cycle
// time, queue latency or human-gate latency). A node with n observations
// draws from them with probability n/(n+k) and from its parent otherwise.
// The root is a Prior. Every draw of a chain is clamped to the chain's tail
// cap, except the Lindy fallback of SampleBeyond.
//
// A Dist is immutable once built and safe for concurrent use; randomness
// comes only from the *rand.Rand passed to each draw.
type Dist struct {
	parent *Dist
	prior  Prior     // root only
	logs   []float64 // own completed observations: log minutes, ascending
	// cum is the cumulative Kaplan–Meier mass of logs when some observations
	// were censored, nil when each observation weighs the same. The mass
	// left over, 1 − cum[last], is drawn beyond tail (log minutes).
	cum  []float64
	tail float64
	h    float64 // smoothing bandwidth in log space
	own  float64 // probability of drawing from own observations
	cap  float64 // tail cap in minutes, shared by the whole chain
}

// NewRoot returns the root of a chain. all holds every observation of the
// quantity in the window, across all repositories, in minutes. Draws are
// clamped to tailCapFactor × the longest of them or, with no observations,
// to the prior's P99.
func NewRoot(prior Prior, all []float64, tailCapFactor float64) *Dist {
	limit := prior.Quantile(0.99)
	if len(all) > 0 {
		longest := MinMinutes
		for _, m := range all {
			longest = math.Max(longest, m)
		}
		limit = tailCapFactor * longest
	}
	return &Dist{prior: prior, cap: limit}
}

// Child returns a node over its own observations (in minutes) that falls
// back to d. k is the pooling strength: the number of pseudo-observations
// the child borrows from d.
func (d *Dist) Child(minutes []float64, k float64) *Dist {
	logs := make([]float64, len(minutes))
	for i, m := range minutes {
		logs[i] = math.Log(math.Max(m, MinMinutes))
	}
	sort.Float64s(logs)
	c := &Dist{parent: d, logs: logs, h: bandwidth(logs), cap: d.cap}
	if n := float64(len(logs)); n > 0 {
		c.own = n / (n + k)
	}
	return c
}

// Censored returns a node over durations that ended (events) and durations
// still running (censored: how long each has lasted so far), all in minutes,
// fitted by Kaplan–Meier (ADR-2 §1). Each distinct event time carries the
// Kaplan–Meier probability mass, so beads still open no longer make the
// data look shorter than it is. When durations still run past the last
// event, the survival mass left over becomes a tail beyond the longest
// observation of either kind — a bead still running past every completed
// one is itself evidence of how far survival reaches, stronger than
// extrapolating from a shorter completed duration would give. The node
// draws from its own data with probability n/(n+k), n counting both kinds.
// Without censored durations it is Child.
func (d *Dist) Censored(events, censored []float64, k float64) *Dist {
	if len(censored) == 0 {
		return d.Child(events, k)
	}
	type obs struct {
		x     float64
		event bool
	}
	all := make([]obs, 0, len(events)+len(censored))
	evLogs := make([]float64, 0, len(events))
	for _, m := range events {
		x := math.Log(math.Max(m, MinMinutes))
		all = append(all, obs{x, true})
		evLogs = append(evLogs, x)
	}
	for _, m := range censored {
		all = append(all, obs{math.Log(math.Max(m, MinMinutes)), false})
	}
	sort.Slice(all, func(a, b int) bool { return all[a].x < all[b].x })
	sort.Float64s(evLogs)

	c := &Dist{parent: d, h: bandwidth(evLogs), cap: d.cap, tail: all[len(all)-1].x}
	c.own = float64(len(all)) / (float64(len(all)) + k)
	// Kaplan–Meier: at each distinct time, the events there take their share
	// of the survivors; a duration censored at a time is still at risk at it.
	// The mass up to a time is one minus the survival there.
	atRisk, surv := len(all), 1.0
	for i := 0; i < len(all); {
		x, died, gone := all[i].x, 0, 0
		for ; i < len(all) && all[i].x == x; i++ {
			if all[i].event {
				died++
			}
			gone++
		}
		if died > 0 {
			surv *= 1 - float64(died)/float64(atRisk)
			c.logs = append(c.logs, x)
			c.cum = append(c.cum, 1-surv)
		}
		atRisk -= gone
	}
	if c.cum == nil {
		// Nothing has ended yet: all the mass is in the tail.
		c.cum = []float64{}
	}
	return c
}

// N returns the number of the node's own completed observations.
func (d *Dist) N() int { return len(d.logs) }

// Sample draws one duration in minutes.
func (d *Dist) Sample(r *rand.Rand) float64 {
	n := d
	for n.parent != nil && r.Float64() >= n.own {
		n = n.parent
	}
	var x float64
	switch {
	case n.parent == nil:
		x = math.Log(n.prior.Median) + n.prior.Sigma*r.NormFloat64()
	case n.cum == nil:
		x = n.logs[r.IntN(len(n.logs))] + n.h*r.NormFloat64()
	default:
		u := r.Float64()
		j := sort.Search(len(n.cum), func(j int) bool { return n.cum[j] > u })
		if j == len(n.cum) {
			// Survival mass beyond the data: somewhere past the longest
			// observation, spread by a fraction of the same bandwidth as
			// every other draw of this node (tailSpread).
			x = n.tail + tailSpread*n.h*math.Abs(r.NormFloat64())
		} else {
			x = n.logs[j] + n.h*r.NormFloat64()
		}
	}
	return math.Min(math.Exp(x), d.cap)
}

// SampleBeyond draws the remaining minutes of a duration that has already
// lasted elapsed minutes (ADR-2 §1): a draw conditioned on exceeding
// elapsed, by rejection with up to 64 tries, minus elapsed. When history
// cannot cover the age, so that every try is rejected, the duration has
// outlasted almost all of it, and the remaining time is Lindy's.
func (d *Dist) SampleBeyond(r *rand.Rand, elapsed float64) float64 {
	if elapsed <= 0 {
		return d.Sample(r)
	}
	if elapsed < d.cap {
		for i := 0; i < beyondTries; i++ {
			if x := d.Sample(r); x > elapsed {
				return x - elapsed
			}
		}
	}
	return Lindy(r, elapsed)
}

// Lindy draws the rest of a duration that has lasted elapsed minutes and is
// expected to last about as long again: elapsed × e^Z with Z ~ N(0, 1), so
// the median remaining time is elapsed. It is not clamped at the tail cap: a
// bead that has waited a month is not forecast to take hours.
func Lindy(r *rand.Rand, elapsed float64) float64 {
	return math.Max(MinMinutes, elapsed*math.Exp(r.NormFloat64()))
}

// Quantiles returns the qs-quantiles of n draws of sample.
func Quantiles(n int, sample func() float64, qs ...float64) []float64 {
	xs := make([]float64, n)
	for i := range xs {
		xs[i] = sample()
	}
	sort.Float64s(xs)
	out := make([]float64, len(qs))
	for i, q := range qs {
		out[i] = Quantile(xs, q)
	}
	return out
}

// Quantile returns the q-quantile of ascending values, interpolating
// linearly between order statistics. It returns NaN for no values.
func Quantile(sorted []float64, q float64) float64 {
	if len(sorted) == 0 {
		return math.NaN()
	}
	pos := q * float64(len(sorted)-1)
	i := int(pos)
	if i >= len(sorted)-1 {
		return sorted[len(sorted)-1]
	}
	return sorted[i] + (pos-float64(i))*(sorted[i+1]-sorted[i])
}

// bandwidth is Silverman's rule of thumb, 0.9 · min(sd, IQR/1.34) · n^(-1/5),
// with a floor of minBandwidth.
func bandwidth(logs []float64) float64 {
	n := float64(len(logs))
	if n < 2 {
		return minBandwidth
	}
	var mean, ss float64
	for _, x := range logs {
		mean += x
	}
	mean /= n
	for _, x := range logs {
		ss += (x - mean) * (x - mean)
	}
	sd := math.Sqrt(ss / (n - 1))
	iqr := Quantile(logs, 0.75) - Quantile(logs, 0.25)
	return math.Max(0.9*math.Min(sd, iqr/1.34)*math.Pow(n, -0.2), minBandwidth)
}
