package forecast

import (
	"math"
	"math/rand/v2"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/alexknips/beadline/internal/config"
	"github.com/alexknips/beadline/internal/graph"
	"github.com/alexknips/beadline/internal/load"
)

func TestChainAndSplit(t *testing.T) {
	g := newGraph(t, []*graph.Issue{{ID: "m", HighLevel: true, Title: "M", Type: "epic"}, {ID: "a"}, {ID: "b"}, {ID: "c"}},
		child("a", "m"), child("b", "m"), child("c", "m"), blocks("a", "b"), blocks("b", "c"))
	o := options(fixed{q: 10, w: 60}, 0)
	o.Concurrency = map[string]int{"r": 1}
	m := find(t, run(t, g, o), "m")
	for _, p := range []*Point{m.P50, m.P80, m.P95} {
		if got := after(p); got != 210 {
			t.Errorf("finish after %v minutes, want 3 × (10 + 60) = 210", got)
		}
		if p.AgentHours != 3.5 || p.HumanHours != 0 {
			t.Errorf("split = %v agent, %v human hours, want 3.5 and 0", p.AgentHours, p.HumanHours)
		}
	}
	if !reflect.DeepEqual(m.CriticalChain, []string{"a", "b", "c"}) {
		t.Errorf("critical chain = %v", m.CriticalChain)
	}
	if m.Status != StatusForecast || m.Title != "M" || m.Type != "epic" || m.Repo != "r" {
		t.Errorf("item = %+v", m)
	}
	if m.Total != 3 || m.Closed != 0 || !reflect.DeepEqual(m.Remaining, []string{"a", "b", "c"}) {
		t.Errorf("scope: total %d, closed %d, remaining %v", m.Total, m.Closed, m.Remaining)
	}
}

func TestConcurrencyLimitsParallelWork(t *testing.T) {
	g := newGraph(t, []*graph.Issue{{ID: "m", HighLevel: true}, {ID: "a"}, {ID: "b"}, {ID: "c"}, {ID: "d"}},
		child("a", "m"), child("b", "m"), child("c", "m"), child("d", "m"))
	for c, want := range map[int]float64{1: 240, 2: 120, 3: 120, 4: 60, 0: 60} {
		o := options(fixed{w: 60}, 0)
		o.Concurrency = map[string]int{"r": c}
		res := run(t, g, o)
		if got := after(find(t, res, "m").P80); got != want {
			t.Errorf("concurrency %d: finish after %v minutes, want %v", c, got, want)
		}
		if res.Concurrency["r"] != c {
			t.Errorf("concurrency %d reported as %v", c, res.Concurrency)
		}
	}
}

func TestDispatchByPriorityThenAge(t *testing.T) {
	g := newGraph(t, []*graph.Issue{
		{ID: "m", HighLevel: true},
		{ID: "low", Priority: 3, CreatedAt: now.Add(-3 * time.Hour)},
		{ID: "new", Priority: 0, CreatedAt: now.Add(-1 * time.Hour)},
		{ID: "old", Priority: 0, CreatedAt: now.Add(-2 * time.Hour)},
	}, child("low", "m"), child("new", "m"), child("old", "m"))
	o := options(fixed{w: 60}, 0)
	o.Concurrency = map[string]int{"r": 1}
	p := newPlan(g, &o)
	s := newSim(p)
	s.run(source(1, 0))
	for id, want := range map[string]float64{"old": 0, "new": 60, "low": 120} {
		if got := s.start[p.index[id]]; got != want {
			t.Errorf("%s started at %v, want %v", id, got, want)
		}
	}
}

func TestGateIsHumanTimeAndHoldsNoAgent(t *testing.T) {
	g := newGraph(t, []*graph.Issue{
		{ID: "m", HighLevel: true}, {ID: "a"}, {ID: "gate", HumanGate: true}, {ID: "b"}, {ID: "z", Priority: 4},
	}, child("a", "m"), child("gate", "m"), child("b", "m"), child("z", "m"), blocks("a", "gate"), blocks("gate", "b"))
	o := options(fixed{w: 60}, 24*60)
	o.Concurrency = map[string]int{"r": 1}
	m := find(t, run(t, g, o), "m")
	if got := after(m.P50); got != 60+24*60+60 {
		t.Errorf("finish after %v minutes, want 60 + 1440 + 60", got)
	}
	if m.P50.AgentHours != 2 || m.P50.HumanHours != 24 {
		t.Errorf("split = %v agent, %v human hours, want 2 and 24", m.P50.AgentHours, m.P50.HumanHours)
	}
	if !reflect.DeepEqual(m.CriticalChain, []string{"a", "gate", "b"}) {
		t.Errorf("critical chain = %v", m.CriticalChain)
	}

	p := newPlan(g, &o)
	s := newSim(p)
	s.run(source(1, 0))
	if got := s.start[p.index["z"]]; got != 60 {
		t.Errorf("z started at %v: the only agent should be free while the gate waits on a human", got)
	}
}

func TestGateWaitedSoFar(t *testing.T) {
	g := newGraph(t, []*graph.Issue{
		{ID: "ready", HumanGate: true, CreatedAt: now.Add(-10 * time.Hour)},
		{ID: "after", HumanGate: true, CreatedAt: now.Add(-10 * time.Hour)},
		{ID: "done", Status: "closed", ClosedAt: now.Add(-2 * time.Hour)},
		{ID: "blocked", HumanGate: true},
		{ID: "open"},
	}, blocks("done", "after"), blocks("open", "blocked"))
	o := options(fixed{w: 60}, 0)
	waited := map[string]float64{}
	o.Human = func(i *graph.Issue, w float64) Draw {
		waited[i.ID] = w
		return func(*rand.Rand) float64 { return 0 }
	}
	run(t, g, o)
	want := map[string]float64{"ready": 600, "after": 120, "blocked": 0}
	if !reflect.DeepEqual(waited, want) {
		t.Errorf("waited = %v, want %v", waited, want)
	}
}

func TestStartedBeadsHoldAnAgentAndIgnoreBlockers(t *testing.T) {
	g := newGraph(t, []*graph.Issue{
		{ID: "m", HighLevel: true},
		{ID: "s", Status: graph.StatusInProgress},
		{ID: "b", CreatedAt: now.Add(-2 * time.Hour)},
		{ID: "t", CreatedAt: now.Add(-1 * time.Hour)},
	}, child("s", "m"), child("b", "m"), child("t", "m"), blocks("b", "s"))
	o := options(fixed{q: 30, w: 60}, 0)
	o.Concurrency = map[string]int{"r": 1}
	p := newPlan(g, &o)
	s := newSim(p)
	s.run(source(1, 0))
	for id, want := range map[string][2]float64{"s": {0, 60}, "b": {60, 120}, "t": {120, 180}} {
		k := p.index[id]
		if got := [2]float64{s.start[k], s.finish[k]}; got != want {
			t.Errorf("%s ran %v, want %v", id, got, want)
		}
	}
}

func TestDeferUntil(t *testing.T) {
	g := newGraph(t, []*graph.Issue{
		{ID: "m", HighLevel: true},
		{ID: "later", DeferUntil: now.Add(5 * time.Hour)},
		{ID: "parked", Status: "deferred"},
		{ID: "resumes", Status: "deferred", DeferUntil: now.Add(time.Hour)},
	}, child("later", "m"), child("parked", "m"), child("resumes", "m"))
	m := find(t, run(t, g, options(fixed{w: 60}, 0)), "m")
	if got := after(m.P50); got != 6*60 {
		t.Errorf("finish after %v minutes, want 5 h deferred + 1 h work", got)
	}
	if m.P50.HumanHours != 5 || m.P50.AgentHours != 1 {
		t.Errorf("split = %v agent, %v human hours: waiting for defer_until is a person's choice", m.P50.AgentHours, m.P50.HumanHours)
	}
	if !reflect.DeepEqual(m.Deferred, []string{"parked"}) || m.Status != StatusForecast {
		t.Errorf("status %s, deferred %v: a parked child is left out, not a stall", m.Status, m.Deferred)
	}
}

func TestStatuses(t *testing.T) {
	epic := func(id string) *graph.Issue { return &graph.Issue{ID: id, HighLevel: true} }
	g := newGraph(t, []*graph.Issue{
		epic("f"), {ID: "f1"},
		epic("rc"), {ID: "rc1", Status: "closed"},
		epic("np"),
		{ID: "df", HighLevel: true, Status: "deferred"}, {ID: "df1"},
		{ID: "du", HighLevel: true, DeferUntil: now.Add(time.Hour)}, {ID: "du1"},
		epic("onlyClosedBlocker"), {ID: "cb", Status: "closed"},
		epic("bo"), {ID: "bo1"},
		epic("boOnly"),
		epic("st"), {ID: "st1"}, {ID: "st2"}, {ID: "p1", Status: "deferred"},
		epic("cy"), {ID: "cy1"}, {ID: "cy2"},
		epic("allp"), {ID: "allp1", Status: "deferred"}, {ID: "allp2", Status: "closed"},
		{ID: "done", HighLevel: true, Status: "closed"},
	},
		child("f1", "f"), child("rc1", "rc"), child("df1", "df"), child("bo1", "bo"),
		child("st1", "st"), child("st2", "st"), blocks("p1", "st1"),
		child("cy1", "cy"), child("cy2", "cy"), blocks("cy1", "cy2"), blocks("cy2", "cy1"),
		child("allp1", "allp"), child("allp2", "allp"),
		child("du1", "du"), blocks("cb", "onlyClosedBlocker"),
	)
	o := options(fixed{w: 60}, 0)
	o.Outside = map[string][]string{"bo1": {"zz-9"}, "boOnly": {"zz-1"}}
	res := run(t, g, o)

	var ids []string
	for _, it := range res.Items {
		ids = append(ids, it.ID)
	}
	if want := []string{"f", "rc", "np", "df", "du", "onlyClosedBlocker", "bo", "boOnly", "st", "cy", "allp"}; !reflect.DeepEqual(ids, want) {
		t.Errorf("items = %v, want %v (closed ones left out)", ids, want)
	}
	for id, want := range map[string]struct {
		status Status
		dated  bool
	}{
		"f":      {StatusForecast, true},
		"rc":     {StatusReadyToClose, false},
		"np":     {StatusNotPlanned, false},
		"df":     {StatusDeferred, false},
		"bo":     {StatusBlockedOutside, true},
		"boOnly": {StatusBlockedOutside, false},
		"st":     {StatusStalled, true},
		"cy":     {StatusStalled, false},
		"allp":   {StatusDeferred, false},
		"du":     {StatusDeferred, false},
		// A closed blocker is satisfied: nothing left to plan.
		"onlyClosedBlocker": {StatusNotPlanned, false},
	} {
		it := find(t, res, id)
		if it.Status != want.status || (it.P80 != nil) != want.dated {
			t.Errorf("%s: status %s, dated %v; want %s, %v", id, it.Status, it.P80 != nil, want.status, want.dated)
		}
	}
	if rc := find(t, res, "rc"); rc.Total != 1 || rc.Closed != 1 || rc.DonePct != 100 || len(rc.Remaining) != 0 || rc.Remaining == nil {
		t.Errorf("rc = %+v", rc)
	}
	if bo := find(t, res, "bo"); !reflect.DeepEqual(bo.Outside, []string{"zz-9"}) {
		t.Errorf("bo outside = %v", bo.Outside)
	}
	st := find(t, res, "st")
	if !reflect.DeepEqual(st.Remaining, []string{"p1", "st1", "st2"}) || !reflect.DeepEqual(st.Deferred, []string{"p1"}) || !reflect.DeepEqual(st.Stuck, []string{"st1"}) {
		t.Errorf("st: remaining %v, deferred %v, stuck %v", st.Remaining, st.Deferred, st.Stuck)
	}
	if got := after(st.P50); got != 60 {
		t.Errorf("st: the schedulable rest finishes after %v minutes, want 60", got)
	}
	if cy := find(t, res, "cy"); !reflect.DeepEqual(cy.Stuck, []string{"cy1", "cy2"}) {
		t.Errorf("cy stuck = %v", cy.Stuck)
	}
}

func TestScopeIsDescendantsAndBlocksClosure(t *testing.T) {
	g := newGraph(t, []*graph.Issue{
		{ID: "M", HighLevel: true}, {ID: "c"},
		{ID: "x", Type: "epic"}, {ID: "x1"}, {ID: "x2", Status: "closed"},
		{ID: "y"}, {ID: "z", Status: "closed"}, {ID: "w"},
		{ID: "old", Status: "closed"}, {ID: "old1", Status: "closed"},
	},
		child("c", "M"), blocks("x", "M"), blocks("y", "M"),
		child("x1", "x"), child("x2", "x"),
		blocks("z", "y"), blocks("w", "z"),
		blocks("old", "M"), child("old1", "old"),
	)
	o := options(fixed{w: 60}, 0)
	m := find(t, run(t, g, o), "M")
	// Closed blockers are satisfied and left out; an open blocker brings
	// its open descendants along, not its closed history.
	if !reflect.DeepEqual(m.Remaining, []string{"c", "x", "x1", "y"}) || m.Total != 4 || m.Closed != 0 {
		t.Errorf("remaining %v, total %d, closed %d; want [c x x1 y], 4, 0", m.Remaining, m.Total, m.Closed)
	}
	// c inherits M's blockers, as bd holds back the children of a blocked
	// epic: it starts after x1 (and so x) and y are done.
	if got := after(m.P50); got != 120 {
		t.Errorf("finish after %v minutes, want 120", got)
	}
}

func TestEpicBlockedByItsOwnChild(t *testing.T) {
	g := newGraph(t, []*graph.Issue{{ID: "e", HighLevel: true}, {ID: "c"}}, child("c", "e"), blocks("c", "e"))
	e := find(t, run(t, g, options(fixed{w: 60}, 0)), "e")
	if e.Status != StatusForecast || after(e.P50) != 60 {
		t.Errorf("status %s, finish after %v: the child must not inherit a block on itself", e.Status, after(e.P50))
	}
}

func TestGoals(t *testing.T) {
	g := newGraph(t, []*graph.Issue{
		{ID: "ae", Repo: "a", HighLevel: true, Goals: []string{"g1"}}, {ID: "a1", Repo: "a"},
		{ID: "be", Repo: "b", HighLevel: true, Goals: []string{"g1"}}, {ID: "b1", Repo: "b"},
		{ID: "g2", Title: "Launch", Repo: "hq", Type: "goal", Status: "deferred"},
		{ID: "ce", Repo: "c", HighLevel: true, Goals: []string{"g2"}}, {ID: "c1", Repo: "c"},
		{ID: "gone", Repo: "c", HighLevel: true, Status: "closed", Goals: []string{"g3"}},
		{ID: "g4", Repo: "hq", Type: "goal", DueAt: now.Add(1000 * time.Hour)},
		{ID: "d1", Repo: "c", Goals: []string{"g4"}},
	}, child("a1", "ae"), child("b1", "be"), child("c1", "ce"))
	o := randomOptions(500, 7)
	res := run(t, g, o)
	var ids []string
	for _, it := range res.Goals {
		ids = append(ids, it.ID)
	}
	if want := []string{"g1", "g2", "g4"}; !reflect.DeepEqual(ids, want) {
		t.Fatalf("goals = %v, want %v (g3 has no open work)", ids, want)
	}
	g1, ae, be := find(t, res, "g1"), find(t, res, "ae"), find(t, res, "be")
	if g1.Title != "" || g1.Status != StatusForecast || g1.Total != 4 {
		t.Errorf("g1 = %+v", g1)
	}
	if !reflect.DeepEqual(ae.Goals, []string{"g1"}) {
		t.Errorf("ae goals = %v", ae.Goals)
	}
	// Per run the goal finishes with its last member, so each of its
	// quantiles is at least as late as every member's.
	for q, pts := range [][3]*Point{{g1.P50, ae.P50, be.P50}, {g1.P80, ae.P80, be.P80}, {g1.P95, ae.P95, be.P95}} {
		if pts[0].At.Before(pts[1].At) || pts[0].At.Before(pts[2].At) {
			t.Errorf("quantile %d: goal %v before a member (%v, %v)", q, pts[0].At, pts[1].At, pts[2].At)
		}
	}
	if !g1.P50.At.After(ae.P50.At) || !g1.P50.At.After(be.P50.At) {
		t.Errorf("g1 P50 %v should be later than both members' (%v, %v): it is the max per run", g1.P50.At, ae.P50.At, be.P50.At)
	}
	if g2 := find(t, res, "g2"); g2.Status != StatusDeferred || g2.Title != "Launch" || g2.Repo != "hq" || g2.P50 != nil {
		t.Errorf("g2 = %+v", g2)
	}
	// A goal's own bead is not work: g4 finishes with d1.
	g4 := find(t, res, "g4")
	if g4.DueAt == nil || g4.OnTime == nil || *g4.OnTime != 1 || !reflect.DeepEqual(g4.CriticalChain, []string{"d1"}) {
		t.Errorf("g4 = %+v", g4)
	}
}

func TestOnTime(t *testing.T) {
	for due, want := range map[time.Duration]float64{90 * time.Minute: 1, 30 * time.Minute: 0} {
		g := newGraph(t, []*graph.Issue{{ID: "m", HighLevel: true, DueAt: now.Add(due)}, {ID: "a"}}, child("a", "m"))
		m := find(t, run(t, g, options(fixed{w: 60}, 0)), "m")
		if m.OnTime == nil || *m.OnTime != want || !m.DueAt.Equal(now.Add(due)) {
			t.Errorf("due in %v: on time %v, want %v", due, m.OnTime, want)
		}
	}
}

func TestOnTimeIsShareOfRuns(t *testing.T) {
	g := newGraph(t, []*graph.Issue{{ID: "m", HighLevel: true}, {ID: "a"}}, child("a", "m"))
	o := randomOptions(1000, 5)
	m := find(t, run(t, g, o), "m")
	// Dates are rounded to the second: allow the P80 run's own finish.
	due := m.P80.At.Add(time.Second)
	g = newGraph(t, []*graph.Issue{{ID: "m", HighLevel: true, DueAt: due}, {ID: "a"}}, child("a", "m"))
	m = find(t, run(t, g, o), "m")
	if m.OnTime == nil || math.Abs(*m.OnTime-0.8) > 0.001 {
		t.Errorf("due at the P80 date: on time %v, want 0.8", *m.OnTime)
	}
}

func TestEmptyGraph(t *testing.T) {
	res := run(t, newGraph(t, nil), options(fixed{}, 0))
	if res.Items == nil || res.Goals == nil || len(res.Items)+len(res.Goals) != 0 {
		t.Errorf("empty graph: items %v, goals %v; want empty, not nil (JSON [])", res.Items, res.Goals)
	}
}

func TestValidate(t *testing.T) {
	g := newGraph(t, nil)
	_, err := Run(g, Options{Concurrency: map[string]int{"r": -1}})
	if err == nil {
		t.Fatal("invalid options accepted")
	}
	for _, want := range []string{"runs", "agent sampler", "human-gate sampler", "concurrency of repo r"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not mention %s", err, want)
		}
	}
}

func TestMeasureConcurrency(t *testing.T) {
	h := func(n int) time.Time { return now.Add(time.Duration(n) * time.Hour) }
	g := newGraph(t, []*graph.Issue{
		{ID: "a", StartedAt: h(-5), ClosedAt: h(-1), Status: "closed"},
		{ID: "b", StartedAt: h(-4), ClosedAt: h(-2), Status: "closed"},
		{ID: "c", StartedAt: h(-3), Status: graph.StatusInProgress},
		{ID: "gate", StartedAt: h(-4), HumanGate: true},
		{ID: "epic", StartedAt: h(-4), HighLevel: true},
		{ID: "old1", StartedAt: h(-800), ClosedAt: h(-700), Status: "closed"},
		{ID: "old2", StartedAt: h(-800), ClosedAt: h(-700), Status: "closed"},
		{ID: "old3", StartedAt: h(-800), ClosedAt: h(-700), Status: "closed"},
		{ID: "old4", StartedAt: h(-800), ClosedAt: h(-700), Status: "closed"},
		{ID: "s1", Repo: "s", StartedAt: h(-5), ClosedAt: h(-4), Status: "closed"},
		{ID: "s2", Repo: "s", StartedAt: h(-4), ClosedAt: h(-3), Status: "closed"},
	})
	got := MeasureConcurrency(g, []string{"r", "s", "empty"}, now, 10*24*time.Hour)
	if want := map[string]int{"r": 3, "s": 1, "empty": 1}; !reflect.DeepEqual(got, want) {
		t.Errorf("MeasureConcurrency = %v, want %v (gates, epics, old and back-to-back beads do not count)", got, want)
	}
}

func TestGateLags(t *testing.T) {
	h := func(n int) time.Time { return now.Add(time.Duration(n) * time.Hour) }
	g := newGraph(t, []*graph.Issue{
		{ID: "gate", HumanGate: true, Status: "closed", CreatedAt: h(-10), ClosedAt: h(-2)},
		{ID: "x", Status: "closed", ClosedAt: h(-8)},
		{ID: "signoff", HumanGate: true, HighLevel: true, Status: "closed", CreatedAt: h(-20), ClosedAt: h(-1)},
		{ID: "work", Status: "closed", ClosedAt: h(-5)},
		{ID: "plain", Status: "closed", CreatedAt: h(-10), ClosedAt: h(-1)},
		{ID: "ancient", HumanGate: true, Status: "closed", CreatedAt: h(-900), ClosedAt: h(-899)},
		{ID: "waiting", HumanGate: true},
	}, blocks("x", "gate"), child("work", "signoff"))
	got := GateLags(g, now, 30*24*time.Hour)
	if want := []float64{360, 240}; !reflect.DeepEqual(got, want) {
		t.Errorf("GateLags = %v, want %v: from ready (last blocker or child closed) to close", got, want)
	}
}

// ready_at is the creation, or the last close among what a bead waits
// for: its own blockers, its ancestors' blockers, and a container's
// children (ADR-2 §1).
func TestReadiness(t *testing.T) {
	h := func(n int) time.Time { return now.Add(time.Duration(n) * time.Hour) }
	g := newGraph(t, []*graph.Issue{
		{ID: "a", Status: "closed", ClosedAt: h(-6)},
		{ID: "b", Status: "closed", ClosedAt: h(-3)},
		{ID: "late", Status: "closed", ClosedAt: h(2)},
		{ID: "free", CreatedAt: h(-30)},
		{ID: "x", CreatedAt: h(-30)},
		{ID: "y", CreatedAt: h(-30)},
		{ID: "open"},
		{ID: "epic", HighLevel: true, CreatedAt: h(-40)},
		{ID: "task", CreatedAt: h(-30)},
		{ID: "e2", HighLevel: true, CreatedAt: h(-40)},
		{ID: "t2", Status: "closed", CreatedAt: h(-30), ClosedAt: h(-1)},
	}, blocks("a", "x"), blocks("b", "x"), blocks("a", "y"), blocks("late", "y"), blocks("open", "y"),
		child("task", "epic"), blocks("b", "epic"), child("t2", "e2"))
	r := NewReadiness(g)
	for _, tt := range []struct {
		id      string
		at      time.Time
		ready   time.Time
		blocked bool
	}{
		{"free", now, h(-30), false},
		{"x", now, h(-3), false},
		{"x", h(-4), h(-6), true},   // as of before b closed
		{"y", now, h(-6), true},     // "late" closes after now, "open" is open
		{"task", now, h(-3), false}, // inherits the epic's blocker
		{"e2", now, h(-1), false},   // a container waits for its children
	} {
		ready, blocked := r.At(g.Issue(tt.id), tt.at)
		if !ready.Equal(tt.ready) || blocked != tt.blocked {
			t.Errorf("%s as of %s: ready %s, blocked %v; want %s, %v", tt.id, tt.at.Format(time.RFC3339),
				ready.Format(time.RFC3339), blocked, tt.ready.Format(time.RFC3339), tt.blocked)
		}
	}
}

func TestWorkBeads(t *testing.T) {
	g := newGraph(t, []*graph.Issue{
		{ID: "t", Status: "closed"}, {ID: "gate", HumanGate: true}, {ID: "e", HighLevel: true},
		{ID: "p"}, {ID: "c"}, {ID: "goal"}, {ID: "m", Goals: []string{"goal"}},
	}, child("c", "p"))
	var ids []string
	for _, i := range WorkBeads(g) {
		ids = append(ids, i.ID)
	}
	if want := []string{"t", "c", "m"}; !reflect.DeepEqual(ids, want) {
		t.Errorf("WorkBeads = %v, want %v: no gates, containers or goal beads", ids, want)
	}
}

// The multi-repo fixture end to end: loader output in, forecast out.
func TestMultirepoFixture(t *testing.T) {
	cfg, err := config.Load("../../testdata/multirepo/beadline.toml")
	if err != nil {
		t.Fatal(err)
	}
	g, rep, err := load.Load(cfg)
	if err != nil {
		t.Fatal(err)
	}
	o := options(fixed{w: 60}, 600)
	o.Concurrency = map[string]int{"api": 3, "web": 2, "hq": 1}
	o.Outside = map[string][]string{}
	for _, d := range rep.Dangling {
		if d.Kind == graph.Blocking {
			o.Outside[d.From] = append(o.Outside[d.From], d.To)
		}
	}
	res := run(t, g, o)

	var ids []string
	for _, it := range append(res.Items, res.Goals...) {
		ids = append(ids, it.ID)
	}
	if want := []string{"api-m1", "api-e1", "web-e1", "hq-g1"}; !reflect.DeepEqual(ids, want) {
		t.Errorf("items and goals = %v, want %v", ids, want)
	}

	// api-3 is under way (60 min), api-4 waits for it (60), then the human
	// review api-5 (600).
	m1 := find(t, res, "api-m1")
	if after(m1.P50) != 720 || m1.P50.AgentHours != 2 || m1.P50.HumanHours != 10 {
		t.Errorf("api-m1 P50 = %+v", m1.P50)
	}
	if !reflect.DeepEqual(m1.CriticalChain, []string{"api-3", "api-4", "api-5"}) {
		t.Errorf("api-m1 critical chain = %v", m1.CriticalChain)
	}
	if m1.Total != 6 || m1.Closed != 2 || m1.DonePct != 33.3 || m1.OnTime == nil || *m1.OnTime != 1 {
		t.Errorf("api-m1 = %+v", m1)
	}

	// web-3 is a gate by metadata; web-2 waits for the pricing approval
	// hq-h1 in hq, a gate by title.
	web := find(t, res, "web-e1")
	if after(web.P50) != 1260 || web.P50.AgentHours != 1 || web.P50.HumanHours != 20 {
		t.Errorf("web-e1 P50 = %+v", web.P50)
	}
	if !reflect.DeepEqual(web.CriticalChain, []string{"hq-h1", "web-2", "web-3"}) {
		t.Errorf("web-e1 critical chain = %v", web.CriticalChain)
	}
	if want := []string{"api-3", "api-4", "hq-h1", "web-1", "web-2", "web-3"}; !reflect.DeepEqual(web.Remaining, want) {
		t.Errorf("web-e1 remaining = %v, want %v", web.Remaining, want)
	}

	goal := find(t, res, "hq-g1")
	if goal.Title != "Public launch" || goal.Total != 12 || after(goal.P50) != 1260 {
		t.Errorf("hq-g1 = %+v", goal)
	}
}

func TestHumanLag(t *testing.T) {
	median := func(d Draw) float64 {
		r := rand.New(rand.NewPCG(1, 2))
		xs := make([]float64, 4001)
		for i := range xs {
			xs[i] = d(r)
		}
		sort.Float64s(xs)
		return xs[len(xs)/2]
	}
	// No history: the prior's median.
	if got := median(HumanLag(nil, 24*60, 10, 3)(nil, 0)); math.Abs(got-24*60)/(24*60) > 0.1 {
		t.Errorf("median with no history = %v minutes, want about the 24 h prior", got)
	}
	// Plenty of history pulls it to the observed lags.
	lags := make([]float64, 200)
	for i := range lags {
		lags[i] = 120
	}
	human := HumanLag(lags, 24*60, 10, 3)
	if got := median(human(nil, 0)); math.Abs(got-120)/120 > 0.1 {
		t.Errorf("median with 200 lags of 2 h = %v minutes", got)
	}
	// A gate that has waited 100 minutes has about 20 left.
	if got := median(human(nil, 100)); got > 60 {
		t.Errorf("remaining after 100 minutes of a 120-minute lag = %v minutes", got)
	}
}

func TestGridAndLeaves(t *testing.T) {
	g := newGraph(t, []*graph.Issue{
		{ID: "m", HighLevel: true},
		{ID: "a"}, {ID: "b"}, {ID: "h", HumanGate: true}, {ID: "d", Status: "closed"},
		{ID: "p"}, {ID: "p1", Status: "closed"}, // all children closed: a leaf still to close
		{ID: "x"},
		{ID: "q", Status: "deferred"}, {ID: "s"}, // parked, and stuck behind it
	}, child("a", "m"), child("b", "m"), child("h", "m"), child("d", "m"), blocks("a", "b"),
		child("p1", "p"), blocks("q", "s"))
	o := options(fixed{w: 60}, 30)
	o.Concurrency = map[string]int{"r": 1}

	res := run(t, g, o)
	if res.Leaves != nil || res.Grid != nil || find(t, res, "m").GridHours != nil {
		t.Errorf("no grid asked for: leaves %v, grid %v, m %v", res.Leaves, res.Grid, find(t, res, "m").GridHours)
	}

	o.Grid = []float64{0.1, 0.5, 0.9}
	res = run(t, g, o)
	if !reflect.DeepEqual(res.Grid, o.Grid) {
		t.Errorf("grid = %v", res.Grid)
	}
	if got := find(t, res, "m").GridHours; !reflect.DeepEqual(got, []float64{2, 2, 2}) {
		t.Errorf("m grid = %v, want a then b on one agent: 2 h at every level", got)
	}
	// One agent: a, then b (it outranks x once a closes), then x. The gate
	// waits 30 minutes and holds no agent; p closes as soon as it is ready.
	want := []Leaf{
		{ID: "a", Repo: "r", GridHours: []float64{1, 1, 1}},
		{ID: "b", Repo: "r", GridHours: []float64{2, 2, 2}},
		{ID: "h", Repo: "r", GridHours: []float64{0.5, 0.5, 0.5}},
		{ID: "p", Repo: "r", GridHours: []float64{0, 0, 0}},
		{ID: "x", Repo: "r", GridHours: []float64{3, 3, 3}},
	}
	if !reflect.DeepEqual(res.Leaves, want) {
		t.Errorf("leaves = %+v\nwant     %+v", res.Leaves, want)
	}

	for _, bad := range [][]float64{{0, 0.5}, {0.5, 1}, {0.5, 0.5}, {0.8, 0.2}} {
		o.Grid = bad
		if _, err := Run(g, o); err == nil || !strings.Contains(err.Error(), "grid") {
			t.Errorf("grid %v: err = %v", bad, err)
		}
	}
}

func TestGridIsNearestRank(t *testing.T) {
	g := newGraph(t, []*graph.Issue{{ID: "m", HighLevel: true}, {ID: "a"}}, child("a", "m"))
	o := randomOptions(400, 3)
	o.Grid = []float64{0.05, 0.5, 0.8, 0.95, 0.99}
	res := run(t, g, o)
	m := find(t, res, "m")
	for n, p := range []*Point{m.P50, m.P80, m.P95} {
		if got, want := m.GridHours[n+1], hours(after(p)); math.Abs(got-want) > 0.01 {
			t.Errorf("grid level %v = %v h, but the point says %v h", o.Grid[n+1], got, want)
		}
	}
	if !sort.Float64sAreSorted(m.GridHours) || !reflect.DeepEqual(res.Leaves[0].GridHours, m.GridHours) {
		t.Errorf("grid %v, leaf a %v: want ascending, and a's close is m's finish", m.GridHours, res.Leaves[0].GridHours)
	}
}
