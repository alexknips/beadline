package forecast

import (
	"math/rand/v2"
	"sort"
	"time"

	"github.com/alexknips/beadline/internal/dist"
	"github.com/alexknips/beadline/internal/graph"
)

// WorkBeads returns the beads an agent executes, of any status, in load
// order: leaves that are neither high-level, nor a human gate, nor a goal's
// own bead. Only they teach the estimator agent durations; gates teach the
// human-gate lag (GateLags) instead, so sign-off time never leaks into
// agent cycle time.
func WorkBeads(g *graph.Graph) []*graph.Issue {
	goals := map[string]bool{}
	for _, goal := range g.Goals() {
		goals[goal.ID] = true
	}
	var out []*graph.Issue
	for _, i := range g.Issues() {
		if !isContainer(i) && !i.HumanGate && !goals[i.ID] {
			out = append(out, i)
		}
	}
	return out
}

// MeasureConcurrency returns, for each repository, the most work beads that
// were in progress at the same moment in the window before now. A bead is
// in progress from started_at until closed_at, or until now while it is
// open; beads without started_at are not counted. A repository with no
// such bead gets 1.
func MeasureConcurrency(g *graph.Graph, repos []string, now time.Time, window time.Duration) map[string]int {
	since := now.Add(-window)
	type edge struct {
		t     time.Time
		delta int
	}
	edges := map[string][]edge{}
	for _, i := range WorkBeads(g) {
		if i.StartedAt.IsZero() {
			continue
		}
		end := now
		if i.Closed() {
			end = i.ClosedAt
		}
		// Clip to the window: only overlap inside it counts.
		start := i.StartedAt
		if start.Before(since) {
			start = since
		}
		if end.After(now) {
			end = now
		}
		if !end.After(start) {
			continue
		}
		edges[i.Repo] = append(edges[i.Repo], edge{start, 1}, edge{end, -1})
	}
	out := make(map[string]int, len(repos))
	for _, repo := range repos {
		e := edges[repo]
		// At equal times ends go first: back-to-back beads do not overlap.
		sort.Slice(e, func(a, b int) bool {
			if !e[a].t.Equal(e[b].t) {
				return e[a].t.Before(e[b].t)
			}
			return e[a].delta < e[b].delta
		})
		peak, n := 1, 0
		for _, x := range e {
			n += x.delta
			peak = max(peak, n)
		}
		out[repo] = peak
	}
	return out
}

// GateLags returns the human waits of the gates closed in the window
// before now, in minutes: from when each gate became ready (its creation,
// or the latest close among its blockers and, for a container, its
// children) to its own close. It is the sign-off lag, learned apart from
// agent cycle time.
func GateLags(g *graph.Graph, now time.Time, window time.Duration) []float64 {
	r := NewReadiness(g)
	since := now.Add(-window)
	var out []float64
	for _, i := range g.Issues() {
		if !i.HumanGate || !i.Closed() || i.ClosedAt.IsZero() || !i.ClosedAt.After(since) || i.ClosedAt.After(now) {
			continue
		}
		// Only what closed before the gate made it ready.
		if ready, _ := r.At(i, i.ClosedAt); !ready.IsZero() && !ready.After(i.ClosedAt) {
			out = append(out, minutes(i.ClosedAt.Sub(ready)))
		}
	}
	return out
}

// Readiness tells when beads stopped waiting for other beads: the ready_at
// that lead times run from (ADR-2 §1). It follows the simulation: a bead
// waits for its own blockers and for those it inherits from its ancestors,
// and a container for its children too. It caches what it derives from the
// graph and is not safe for concurrent use.
type Readiness struct{ p *plan }

// NewReadiness returns the readiness of g's beads.
func NewReadiness(g *graph.Graph) *Readiness {
	p := &plan{g: g, goals: map[string]bool{}, ancestors: map[string]map[string]bool{}, blockers: map[string][]string{}}
	for _, goal := range g.Goals() {
		p.goals[goal.ID] = true
	}
	return &Readiness{p: p}
}

// At returns when i became ready as far as the moment now can tell: its
// creation, or the latest close by now among the beads it waits for.
// blocked reports that one of them was still open at now, so i was not
// ready then.
func (r *Readiness) At(i *graph.Issue, now time.Time) (ready time.Time, blocked bool) {
	p := r.p
	p.now = now
	open := func(id string) bool {
		b := p.g.Issue(id)
		return !b.Closed() || b.ClosedAt.After(now)
	}
	for _, b := range p.blockersOf(i) {
		blocked = blocked || open(b)
	}
	if p.container(i) {
		for _, c := range i.Children {
			blocked = blocked || open(c)
		}
	}
	return p.readyAt(i), blocked
}

// HumanLag returns the Human option from observed gate lags (GateLags), as
// ADR-1 §2 prescribes for every duration: a smoothed bootstrap over the
// lags that backs off, with pooling strength k, to a log-normal root prior
// with median priorMinutes, clamped to tailCapFactor × the longest lag. A
// gate that has already waited draws the rest of a lag longer than that.
func HumanLag(lags []float64, priorMinutes, k, tailCapFactor float64) func(*graph.Issue, float64) Draw {
	d := dist.NewRoot(dist.NewPrior(priorMinutes), lags, tailCapFactor).Child(lags, k)
	return func(_ *graph.Issue, waited float64) Draw {
		if waited > 0 {
			return func(r *rand.Rand) float64 { return d.SampleBeyond(r, waited) }
		}
		return d.Sample
	}
}
