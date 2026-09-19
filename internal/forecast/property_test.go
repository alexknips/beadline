package forecast

import (
	"math"
	"math/rand/v2"
	"reflect"
	"testing"

	"github.com/alexknips/beadline/internal/graph"
)

const eps = 1e-6

// asap returns the finish of every node in the run s last simulated, had
// every repository unlimited agents: the longest path through the graph
// with the run's own draws (the critical path method).
func asap(s *sim) []float64 {
	p := s.p
	out := make([]float64, len(p.nodes))
	done := make([]bool, len(p.nodes))
	var visit func(k int) float64
	visit = func(k int) float64 {
		if done[k] {
			return out[k]
		}
		n := &p.nodes[k]
		switch {
		case n.started:
			out[k] = s.own[k]
		default:
			ready := n.notBefore
			for _, pr := range n.preds {
				ready = math.Max(ready, visit(int(pr)))
			}
			out[k] = ready + s.queue[k] + s.own[k]
		}
		done[k] = true
		return out[k]
	}
	for k := range p.nodes {
		visit(k)
	}
	return out
}

// checkRun verifies one simulated run against the model's rules.
func checkRun(t *testing.T, s *sim, label string) {
	t.Helper()
	p := s.p
	lower := asap(s)
	unlimited := true
	for _, c := range p.caps {
		unlimited = unlimited && c == 0
	}
	for k := range p.nodes {
		n := &p.nodes[k]
		id := n.issue.ID
		if math.IsInf(s.finish[k], 0) || math.IsNaN(s.finish[k]) {
			t.Fatalf("%s: %s never finished", label, id)
		}
		// Unlimited agents are a floor: no schedule can beat the longest path.
		if s.finish[k] < lower[k]-eps {
			t.Fatalf("%s: %s finished at %v, before its longest path %v", label, id, s.finish[k], lower[k])
		}
		if unlimited && math.Abs(s.finish[k]-lower[k]) > eps {
			t.Fatalf("%s: %s finished at %v with unlimited agents, not at its longest path %v", label, id, s.finish[k], lower[k])
		}
		if n.started {
			continue
		}
		// Precedence: nothing starts before what it waits for is done.
		ready := n.notBefore
		for _, pr := range n.preds {
			ready = math.Max(ready, s.finish[pr])
		}
		need := s.own[k]
		if n.work() {
			need += s.queue[k]
			if s.start[k] < ready+s.queue[k]-eps {
				t.Fatalf("%s: %s started at %v, before it was eligible at %v", label, id, s.start[k], ready+s.queue[k])
			}
		}
		if s.finish[k] < ready+need-eps {
			t.Fatalf("%s: %s finished at %v, less than %v after it was ready at %v", label, id, s.finish[k], need, ready)
		}
	}
	// Agents: a bead starts only while its repository has one free.
	for k := range p.nodes {
		n := &p.nodes[k]
		limit := p.caps[n.repo]
		if !n.work() || n.started || limit == 0 {
			continue
		}
		busy := 0
		for j := range p.nodes {
			m := &p.nodes[j]
			if j != k && m.work() && m.repo == n.repo && s.start[j] <= s.start[k] && s.start[k] < s.finish[j] {
				busy++
			}
		}
		if busy >= limit {
			t.Fatalf("%s: %s started at %v with %d of %d agents busy", label, n.issue.ID, s.start[k], busy, limit)
		}
	}
}

func TestPropertyScheduleRules(t *testing.T) {
	for gseed := uint64(0); gseed < 40; gseed++ {
		g := randomGraph(t, rand.New(rand.NewPCG(gseed, 1)), 4, 8)
		for _, c := range []int{0, 1, 2, 4} {
			o := randomOptions(1, gseed)
			o.Concurrency = map[string]int{"r0": c, "r1": c, "r2": c}
			p := newPlan(g, &o)
			s := newSim(p)
			for run := 0; run < 10; run++ {
				s.run(source(gseed, run))
				checkRun(t, s, "random graph")
			}
		}
	}
}

// More agents never make a forecast later. Greedy list scheduling admits
// Graham's anomalies within a single run (one more agent can reorder the
// work so that some bead finishes later; about 0.2% of paired runs on these
// graphs), so the guarantee is on the quantiles a forecast reports, and,
// per run, against unlimited agents (TestPropertyScheduleRules).
func TestPropertyMoreConcurrencyNeverDelays(t *testing.T) {
	for gseed := uint64(0); gseed < 25; gseed++ {
		g := randomGraph(t, rand.New(rand.NewPCG(gseed, 2)), 4, 8)
		var prev []Item
		for _, c := range []int{1, 2, 3, 5, 0} {
			o := randomOptions(400, gseed)
			o.Concurrency = map[string]int{"r0": c, "r1": c, "r2": c}
			res := run(t, g, o)
			items := append(res.Items, res.Goals...)
			if prev != nil {
				for k, it := range items {
					was := prev[k]
					for q, pair := range [][2]*Point{{was.P50, it.P50}, {was.P80, it.P80}, {was.P95, it.P95}} {
						if pair[0] == nil {
							continue
						}
						if pair[1].At.After(pair[0].At) {
							t.Errorf("graph %d, %s, quantile %d: concurrency %d finishes %v, later than %v with fewer agents",
								gseed, it.ID, q, c, pair[1].At, pair[0].At)
						}
					}
				}
			}
			prev = items
		}
	}
}

// A blocking chain sets a floor: the item cannot finish before the chain's
// beads have run one after the other, however many agents there are and
// however much other work competes for them.
func TestPropertyBlockingChainSetsFloor(t *testing.T) {
	issues := []*graph.Issue{{ID: "m", HighLevel: true}}
	edges := [][3]string{}
	chain := []string{"c1", "c2", "c3", "c4", "c5"}
	for n, id := range chain {
		issues = append(issues, &graph.Issue{ID: id, Priority: 3})
		edges = append(edges, child(id, "m"))
		if n > 0 {
			edges = append(edges, blocks(chain[n-1], id))
		}
	}
	for _, id := range []string{"x1", "x2", "x3", "x4", "x5", "x6"} {
		issues = append(issues, &graph.Issue{ID: id, Priority: 0})
		edges = append(edges, child(id, "m"))
	}
	g := newGraph(t, issues, edges...)
	for _, c := range []int{0, 1, 2, 3} {
		o := randomOptions(1, 3)
		o.Concurrency = map[string]int{"r": c}
		p := newPlan(g, &o)
		s := newSim(p)
		m := p.items()[0]
		for run := 0; run < 200; run++ {
			s.run(source(3, run))
			floor := 0.0
			for _, id := range chain {
				k := p.index[id]
				floor += s.queue[k] + s.own[k]
			}
			finish := s.last(m.nodes)
			if finish < floor-eps {
				t.Fatalf("concurrency %d, run %d: finish %v below the chain's floor %v", c, run, finish, floor)
			}
			if got := s.finish[p.index["c5"]]; c == 0 && math.Abs(got-floor) > eps {
				t.Fatalf("unlimited agents, run %d: chain ends at %v, want exactly %v", run, got, floor)
			}
		}
	}
}

func TestDeterministic(t *testing.T) {
	g := randomGraph(t, rand.New(rand.NewPCG(9, 9)), 4, 8)
	o := randomOptions(300, 42)
	o.Concurrency = map[string]int{"r0": 2, "r1": 1}
	o.Workers = 1
	a := run(t, g, o)
	o.Workers = 8
	b := run(t, g, o)
	if !reflect.DeepEqual(a, b) {
		t.Error("the number of workers changed the result")
	}
	if c := run(t, g, o); !reflect.DeepEqual(a, c) {
		t.Error("the same seed gave a different result")
	}
	o.Seed = 43
	if d := run(t, g, o); reflect.DeepEqual(a.Items, d.Items) {
		t.Error("a different seed gave the same result")
	}
}
