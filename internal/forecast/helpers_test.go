package forecast

import (
	"fmt"
	"math"
	"math/rand/v2"
	"testing"
	"time"

	"github.com/alexknips/beadline/internal/graph"
)

var now = time.Date(2026, 9, 19, 12, 0, 0, 0, time.UTC)

// fixed is a sampler with constant durations.
type fixed struct{ q, w float64 }

func (f fixed) Draw(*rand.Rand) (float64, float64) { return f.q, f.w }

// lognormal is a sampler with log-normal queue and work times.
type lognormal struct{ q, w, sigma float64 }

func (l lognormal) Draw(r *rand.Rand) (float64, float64) {
	return l.q * math.Exp(l.sigma*r.NormFloat64()), l.w * math.Exp(l.sigma*r.NormFloat64())
}

// newGraph builds a graph. Issues default to repo "r", status "open" and
// creation a day before now. Edges are [kind, from, to] as in graph.Link.
func newGraph(t testing.TB, issues []*graph.Issue, edges ...[3]string) *graph.Graph {
	t.Helper()
	g := graph.New()
	for _, i := range issues {
		if i.Repo == "" {
			i.Repo = "r"
		}
		if i.Status == "" {
			i.Status = "open"
		}
		if i.CreatedAt.IsZero() {
			i.CreatedAt = now.Add(-24 * time.Hour)
		}
		if err := g.Add(i); err != nil {
			t.Fatal(err)
		}
	}
	for _, e := range edges {
		if err := g.Link(graph.EdgeKind(e[0]), e[1], e[2]); err != nil {
			t.Fatal(err)
		}
	}
	return g
}

func child(c, parent string) [3]string   { return [3]string{"parent-child", c, parent} }
func blocks(blocker, x string) [3]string { return [3]string{"blocks", x, blocker} }

// options returns options with constant agent and human durations.
func options(agent fixed, human float64) Options {
	return Options{
		Now:   now,
		Runs:  50,
		Seed:  1,
		Agent: func(*graph.Issue) Sampler { return agent },
		Human: func(*graph.Issue, float64) Draw { return func(*rand.Rand) float64 { return human } },
	}
}

func run(t testing.TB, g *graph.Graph, o Options) *Result {
	t.Helper()
	res, err := Run(g, o)
	if err != nil {
		t.Fatal(err)
	}
	return res
}

func find(t testing.TB, res *Result, id string) Item {
	t.Helper()
	for _, it := range append(append([]Item(nil), res.Items...), res.Goals...) {
		if it.ID == id {
			return it
		}
	}
	t.Fatalf("no item %s in %+v", id, res)
	return Item{}
}

// after returns the minutes from now to a point.
func after(p *Point) float64 {
	if p == nil {
		return math.NaN()
	}
	return p.At.Sub(now).Minutes()
}

// randomGraph builds a random work graph: epics in several repos, each with
// leaf children, random blocking edges from earlier to later beads (so it
// is acyclic), some gates, some started beads and some cross-repo edges.
func randomGraph(t testing.TB, r *rand.Rand, epics, leaves int) *graph.Graph {
	var issues []*graph.Issue
	var edges [][3]string
	var ids []string
	for e := 0; e < epics; e++ {
		eid := fmt.Sprintf("e%d", e)
		repo := fmt.Sprintf("r%d", e%3)
		issues = append(issues, &graph.Issue{ID: eid, Repo: repo, HighLevel: true, Type: "epic"})
		for l := 0; l < leaves; l++ {
			id := fmt.Sprintf("%s.%d", eid, l)
			i := &graph.Issue{ID: id, Repo: repo, Priority: r.IntN(4), CreatedAt: now.Add(-time.Duration(r.IntN(1000)) * time.Minute)}
			switch r.IntN(10) {
			case 0:
				i.HumanGate = true
			case 1:
				i.Status = graph.StatusInProgress
			}
			issues = append(issues, i)
			edges = append(edges, child(id, eid))
			for _, prev := range ids {
				if r.Float64() < 1.5/float64(len(ids)+1) {
					edges = append(edges, blocks(prev, id))
				}
			}
			ids = append(ids, id)
		}
	}
	return newGraph(t, issues, edges...)
}

// randomOptions draws log-normal agent and gate durations with sampler
// parameters that vary per bead.
func randomOptions(runs int, seed uint64) Options {
	return Options{
		Now:  now,
		Runs: runs,
		Seed: seed,
		Agent: func(i *graph.Issue) Sampler {
			scale := float64(10 + len(i.ID)*7%50)
			return lognormal{q: scale / 2, w: scale * 3, sigma: 0.8}
		},
		Human: func(*graph.Issue, float64) Draw {
			return func(r *rand.Rand) float64 { return 600 * math.Exp(0.7*r.NormFloat64()) }
		},
	}
}
