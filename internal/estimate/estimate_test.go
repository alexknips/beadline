package estimate

import (
	"fmt"
	"math"
	"math/rand/v2"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"
)

var now = time.Date(2026, 9, 19, 12, 0, 0, 0, time.UTC)

func rng(stream uint64) *rand.Rand { return rand.New(rand.NewPCG(7, stream)) }

func minutes(m float64) time.Duration { return time.Duration(m * float64(time.Minute)) }

// history returns n beads closed at random in the 30 days before closedBy,
// with log-normal cycle time and queue latency of the given medians and σ.
// Each is ready when it is created.
func history(r *rand.Rand, repo, typ string, n int, cycleMedian, queueMedian, sigma float64, closedBy time.Time) []Bead {
	beads := make([]Bead, n)
	for i := range beads {
		closed := closedBy.Add(-minutes(r.Float64() * 30 * 24 * 60))
		started := closed.Add(-minutes(cycleMedian * math.Exp(sigma*r.NormFloat64())))
		created := started.Add(-minutes(queueMedian * math.Exp(sigma*r.NormFloat64())))
		beads[i] = Bead{
			ID: fmt.Sprintf("%s-%s-%d", repo, typ, i), Repo: repo, Type: typ, Status: "closed",
			CreatedAt: created, StartedAt: started, ClosedAt: closed,
		}
	}
	return beads
}

// leads returns n beads with no start, whose lead times (ready to close)
// are log-normal with the given median and σ.
func leads(r *rand.Rand, repo string, priority, n int, median, sigma float64) []Bead {
	beads := make([]Bead, n)
	for i := range beads {
		closed := now.Add(-minutes(r.Float64() * 30 * 24 * 60))
		created := closed.Add(-minutes(median * math.Exp(sigma*r.NormFloat64())))
		beads[i] = Bead{
			ID: fmt.Sprintf("%s-P%d-%d", repo, priority, i), Repo: repo, Type: "task", Priority: priority,
			Status: "closed", CreatedAt: created, ClosedAt: closed,
		}
	}
	return beads
}

// twoRepos: api tasks take about 30 minutes after 5 minutes in the queue,
// web tasks about 4 hours after an hour.
func twoRepos() []Bead {
	r := rng(1)
	return append(history(r, "api", "task", 1000, 30, 5, 0.5, now), history(r, "web", "task", 1000, 240, 60, 0.5, now)...)
}

func learn(t *testing.T, beads []Bead) *Model {
	t.Helper()
	m, err := Learn(beads, now, DefaultParams())
	if err != nil {
		t.Fatal(err)
	}
	return m
}

func within(t *testing.T, name string, got, want, rel float64) {
	t.Helper()
	if math.Abs(got-want) > rel*want {
		t.Errorf("%s = %.2f, want %.2f ± %.0f%%", name, got, want, rel*100)
	}
}

// median returns the median lead time (ready to close) of closed beads.
func median(beads []Bead) float64 {
	var xs []float64
	for _, b := range beads {
		xs = append(xs, b.ClosedAt.Sub(b.readyAt()).Minutes())
	}
	sort.Float64s(xs)
	return xs[len(xs)/2]
}

// z80 is the standard normal 0.8-quantile.
const z80 = 0.8416212335729143

func TestLearnsKnownDistributions(t *testing.T) {
	beads := twoRepos()
	m := learn(t, beads)
	for _, tt := range []struct {
		repo  string
		cycle float64
		lead  float64
	}{{"api", 30, median(beads[:1000])}, {"web", 240, median(beads[1000:])}} {
		e := m.Estimate(Bead{ID: "open", Repo: tt.repo, Type: "task", Status: "open"})
		if e.Class != (Class{tt.repo, "task", 0}) || e.Started {
			t.Errorf("%s: class %v started %v", tt.repo, e.Class, e.Started)
		}
		// σ = 0.5 plus a little smoothing.
		within(t, tt.repo+" cycle P50", e.Cycle.P50, tt.cycle, 0.10)
		within(t, tt.repo+" cycle P80", e.Cycle.P80, tt.cycle*math.Exp(0.5*z80), 0.10)
		within(t, tt.repo+" lead P50", e.Lead.P50, tt.lead, 0.10)
		// A fresh bead of no age has its whole lead time ahead.
		within(t, tt.repo+" remaining P50", e.Remaining.P50, e.Lead.P50, 0.05)
		within(t, tt.repo+" remaining P80", e.Remaining.P80, e.Lead.P80, 0.05)
	}
	if s := m.Sample(); s.Closes != 2000 || s.Started != 2000 || s.Open != 0 || len(s.Skipped) != 0 {
		t.Errorf("sample = %+v, want 2000 delivered closes, all started", s)
	}
}

// Priority is the class key (ADR-2 §1): urgent beads land in hours, the
// backlog in days.
func TestClassesByPriority(t *testing.T) {
	r := rng(2)
	m := learn(t, append(leads(r, "api", 0, 300, 180, 0.5), leads(r, "api", 3, 300, 4860, 0.5)...))
	p0 := m.Estimate(Bead{Repo: "api", Type: "task", Priority: 0})
	p3 := m.Estimate(Bead{Repo: "api", Type: "task", Priority: 3})
	within(t, "P0 lead P50", p0.Lead.P50, 180, 0.10)
	within(t, "P3 lead P50", p3.Lead.P50, 4860, 0.10)
	if p3.Class.String() != "api/task/P3" {
		t.Errorf("class = %s, want api/task/P3", p3.Class)
	}
	// An unseen priority draws from the repository: both modes.
	p1 := m.Estimate(Bead{Repo: "api", Type: "task", Priority: 1})
	if !(p1.Lead.P50 > 180*1.2 && p1.Lead.P50 < 4860/1.2) {
		t.Errorf("P1 lead P50 = %.1f, want between the P0 (180) and P3 (4860) medians", p1.Lead.P50)
	}
}

func TestBackoff(t *testing.T) {
	m := learn(t, twoRepos())
	api := m.Estimate(Bead{Repo: "api", Type: "task"})

	t.Run("unseen class falls back to its repository", func(t *testing.T) {
		e := m.Estimate(Bead{Repo: "api", Type: "chore", Priority: 2})
		within(t, "cycle P50", e.Cycle.P50, api.Cycle.P50, 0.05)
		within(t, "lead P50", e.Lead.P50, api.Lead.P50, 0.05)
	})

	t.Run("unseen repository falls back to all repositories", func(t *testing.T) {
		e := m.Estimate(Bead{Repo: "cli", Type: "task"})
		if !(e.Cycle.P50 > 30*1.2 && e.Cycle.P50 < 240/1.2) {
			t.Errorf("cycle P50 = %.1f, want between the api (30) and web (240) medians", e.Cycle.P50)
		}
	})

	t.Run("no history is the prior", func(t *testing.T) {
		e := learn(t, nil).Estimate(Bead{Repo: "api", Type: "task"})
		// Log-normals with σ 1: cycle median 60, lead median 120.
		within(t, "cycle P50", e.Cycle.P50, 60, 0.06)
		within(t, "cycle P80", e.Cycle.P80, 60*math.Exp(z80), 0.06)
		within(t, "lead P50", e.Lead.P50, 120, 0.06)
		within(t, "lead P80", e.Lead.P80, 120*math.Exp(z80), 0.06)
	})

	t.Run("thin class is half its own at k observations", func(t *testing.T) {
		// Ten urgent api tasks near 3000 minutes: with k = 10 half the draws
		// come from them, half from the api repository (about 35 minutes).
		urgent := history(rng(2), "api", "task", 10, 3000, 1, 0.1, now)
		for i := range urgent {
			urgent[i].Priority = 1
		}
		m := learn(t, append(twoRepos(), urgent...))
		s := m.Sampler(Bead{Repo: "api", Type: "task", Priority: 1})
		r := rng(3)
		long := 0
		const n = 20000
		for i := 0; i < n; i++ {
			if q, w := s.Draw(r); q+w > 1000 {
				long++
			}
		}
		if share := float64(long) / n; math.Abs(share-0.5) > 0.04 {
			t.Errorf("share of draws from the thin class = %.3f, want about 0.5", share)
		}
	})
}

func TestWindow(t *testing.T) {
	recent := history(rng(4), "api", "task", 300, 30, 5, 0.5, now)
	stale := history(rng(5), "api", "task", 300, 1000, 5, 0.5, now.AddDate(0, 0, -50))
	future := history(rng(6), "api", "task", 300, 1000, 5, 0.5, now.AddDate(0, 0, 31))
	beads := append(append(recent, stale...), future...)

	m := learn(t, beads)
	within(t, "cycle P50, 45-day window", m.Estimate(Bead{Repo: "api", Type: "task"}).Cycle.P50, 30, 0.10)

	// Stale beads closed 50 to 80 days ago enter a 90-day window.
	p := DefaultParams()
	p.WindowDays = 90
	wide, err := Learn(beads, now, p)
	if err != nil {
		t.Fatal(err)
	}
	// Half the history is now stale: P80 lies in its mode (the median of a
	// 50/50 mixture could fall anywhere between the modes).
	if got := wide.Estimate(Bead{Repo: "api", Type: "task"}).Cycle.P80; got < 500 {
		t.Errorf("cycle P80 with a 90-day window = %.1f, want the stale 1000-minute beads to count", got)
	}

	// Learned as of 31 days from now, the future beads are the recent ones.
	later, err := Learn(beads, now.AddDate(0, 0, 31), DefaultParams())
	if err != nil {
		t.Fatal(err)
	}
	if got := later.Estimate(Bead{Repo: "api", Type: "task"}).Cycle.P50; got < 100 {
		t.Errorf("cycle P50 learned a month later = %.1f, want the later 1000-minute beads", got)
	}
}

// Lead time runs from ready_at: a bead blocked for ten days after its
// creation was not waiting for an agent all that time.
func TestLeadFromReady(t *testing.T) {
	beads := leads(rng(7), "api", 2, 200, 30, 0.3)
	for i := range beads {
		b := &beads[i]
		b.ReadyAt, b.CreatedAt = b.CreatedAt, b.CreatedAt.AddDate(0, 0, -10)
	}
	e := learn(t, beads).Estimate(Bead{Repo: "api", Type: "task", Priority: 2})
	within(t, "lead P50", e.Lead.P50, 30, 0.10)

	for i := range beads {
		beads[i].ReadyAt = time.Time{} // unknown: from created_at
	}
	if e := learn(t, beads).Estimate(Bead{Repo: "api", Type: "task", Priority: 2}); e.Lead.P50 < 9*24*60 {
		t.Errorf("lead P50 without ready_at = %.1f, want about the ten days since creation", e.Lead.P50)
	}
}

// Only beads with a usable start teach cycle times; every delivery teaches
// a lead time.
func TestCycleNeedsStart(t *testing.T) {
	var beads []Bead
	for i := 0; i < 200; i++ {
		closed := now.Add(-time.Duration(i+1) * time.Hour)
		b := Bead{ID: fmt.Sprint(i), Repo: "api", Type: "task", CreatedAt: closed.Add(-20 * time.Minute), ClosedAt: closed}
		if i%2 == 1 {
			b.StartedAt = closed.Add(time.Hour) // clock skew: started after closing
		}
		beads = append(beads, b)
	}
	m := learn(t, beads)
	e := m.Estimate(Bead{Repo: "api", Type: "task"})
	within(t, "lead P50", e.Lead.P50, 20, 0.05)
	within(t, "cycle P50 without starts is the prior", e.Cycle.P50, 60, 0.06)
	if s := m.Sample(); s.Closes != 200 || s.Started != 0 {
		t.Errorf("sample = %+v, want 200 closes and no usable start", s)
	}
}

func TestInstantBeadsFloorAtOneMinute(t *testing.T) {
	var beads []Bead
	for i := 0; i < 100; i++ {
		at := now.Add(-time.Duration(i+1) * time.Hour)
		beads = append(beads, Bead{ID: fmt.Sprint(i), Repo: "api", Type: "task",
			CreatedAt: at.Add(-time.Minute), StartedAt: at, ClosedAt: at})
	}
	e := learn(t, beads).Estimate(Bead{Repo: "api", Type: "task"})
	within(t, "cycle P50", e.Cycle.P50, 1, 0.05)
	if e.Cycle.P80 > 3 {
		t.Errorf("cycle P80 = %.2f, want at most the tail cap of 3 minutes", e.Cycle.P80)
	}
}

// Closes that were not deliveries teach nothing, and are counted by reason.
func TestSampleHygiene(t *testing.T) {
	base := now.Add(-24 * time.Hour)
	delivered := func(id, repo string, closed time.Time) Bead {
		return Bead{ID: id, Repo: repo, Type: "task", Status: "closed", CreatedAt: closed.Add(-10 * time.Hour), ClosedAt: closed}
	}
	var beads []Bead
	add := func(b Bead) { beads = append(beads, b) }
	// A bulk close: five closes of one repo in one minute.
	for i := 0; i < 5; i++ {
		add(delivered(fmt.Sprint("bulk", i), "api", base.Add(time.Duration(i)*time.Second)))
	}
	// Four in a minute, or five across two repos, are deliveries.
	for i := 0; i < 4; i++ {
		add(delivered(fmt.Sprint("four", i), "api", base.Add(time.Hour+time.Duration(i)*time.Second)))
	}
	for i := 0; i < 5; i++ {
		add(delivered(fmt.Sprint("split", i), []string{"api", "web"}[i%2], base.Add(2*time.Hour+time.Duration(i)*time.Second)))
	}
	instant := delivered("instant", "api", base.Add(3*time.Hour))
	instant.CreatedAt = instant.ClosedAt.Add(-59 * time.Second)
	add(instant)
	noop := delivered("noop", "api", base.Add(4*time.Hour))
	noop.WorkOutcome = "no-op"
	add(noop)
	abandoned := delivered("abandoned", "api", base.Add(5*time.Hour))
	abandoned.WorkOutcome = "abandoned"
	add(abandoned)
	for i, reason := range []string{"Duplicate of api-7", "superseded by api-9", "won't fix", "Closing stale beads"} {
		b := delivered(fmt.Sprint("descoped", i), "web", base.Add(time.Duration(6+i)*time.Hour))
		b.CloseReason = reason
		add(b)
	}
	// A close reason mentioning tests is a genuine merge.
	test := delivered("test", "web", base.Add(11*time.Hour))
	test.CloseReason = "Merged: add tests for the duplicate check"
	add(test)
	shipped := delivered("shipped", "web", base.Add(12*time.Hour))
	shipped.WorkOutcome = "shipped"
	add(shipped)

	s := learn(t, beads).Sample()
	want := map[string]int{SkipBulk: 5, SkipInstant: 1, SkipOutcome: 2, SkipDescoped: 4}
	if !reflect.DeepEqual(s.Skipped, want) {
		t.Errorf("skipped = %v, want %v", s.Skipped, want)
	}
	if s.Closes != 4+5+2 {
		t.Errorf("closes = %d, want 11 deliveries", s.Closes)
	}
}

func TestUndelivered(t *testing.T) {
	for _, tt := range []struct{ reason, outcome, want string }{
		{"", "", ""},
		{"Merged to main at f56ff54", "shipped", ""},
		{"all tests pass", "", ""},
		{"Duplicate of hm-12", "", SkipDescoped},
		{"  superseded by hm-3", "", SkipDescoped},
		{"Won't do", "", SkipDescoped},
		{"obsolete", "", SkipDescoped},
		{"No longer needed", "", SkipDescoped},
		{"bulk-close of stale work", "", SkipDescoped},
		{"Merged", "no-op", SkipOutcome},
		{"", "abandoned", SkipOutcome},
		{"fixed a duplicate entry", "", ""},
	} {
		if got := Undelivered(tt.reason, tt.outcome); got != tt.want {
			t.Errorf("Undelivered(%q, %q) = %q, want %q", tt.reason, tt.outcome, got, tt.want)
		}
	}
}

// Beads still open enter the fit as censored lead times (Kaplan–Meier): a
// class whose open beads have outlasted every close is slower than its
// closes alone say.
func TestCensoredOpenBeads(t *testing.T) {
	closes := leads(rng(8), "api", 2, 100, 60, 0.3)
	var longest float64
	for _, b := range closes {
		longest = math.Max(longest, b.ClosedAt.Sub(b.CreatedAt).Minutes())
	}
	open := func(id string, readyAgo time.Duration) Bead {
		return Bead{ID: id, Repo: "api", Type: "task", Priority: 2, Status: "open",
			CreatedAt: now.Add(-readyAgo - time.Hour), ReadyAt: now.Add(-readyAgo)}
	}
	var beads []Bead
	for i := 0; i < 100; i++ {
		beads = append(beads, open(fmt.Sprint("open", i), 2*24*time.Hour))
	}
	// Neither blocked, nor deferred, nor ready before the window count.
	blocked := open("blocked", 2*24*time.Hour)
	blocked.Blocked = true
	deferred := open("deferred", 2*24*time.Hour)
	deferred.Status = "deferred"
	old := open("old", 60*24*time.Hour)
	// An in-progress bead enters even when blocked: it has started.
	running := open("running", 3*24*time.Hour)
	running.Status, running.Blocked, running.StartedAt = StatusInProgress, true, now.Add(-2*time.Hour)
	// A bead closed after now is open as of now.
	later := leads(rng(9), "api", 2, 1, 60, 0.3)[0]
	later.ClosedAt, later.ReadyAt = now.Add(time.Hour), now.Add(-24*time.Hour)
	beads = append(beads, blocked, deferred, old, running, later)

	closedOnly := learn(t, closes).Estimate(Bead{Repo: "api", Type: "task", Priority: 2})
	m := learn(t, append(append([]Bead(nil), closes...), beads...))
	if s := m.Sample(); s.Closes != 100 || s.Open != 102 {
		t.Errorf("sample = %+v, want 100 closes and 102 open beads", s)
	}
	e := m.Estimate(Bead{Repo: "api", Type: "task", Priority: 2})
	within(t, "closes alone, lead P50", closedOnly.Lead.P50, 60, 0.10)
	// About half the class is still open past every close: the P80 lies
	// beyond the longest close.
	if e.Lead.P80 <= longest {
		t.Errorf("lead P80 = %.1f, want beyond the longest close %.1f", e.Lead.P80, longest)
	}
	if e.Lead.P50 <= closedOnly.Lead.P50 {
		t.Errorf("lead P50 = %.1f with censored beads, want above %.1f from closes alone", e.Lead.P50, closedOnly.Lead.P50)
	}
}

// Every open bead's draw is conditioned on its age (ADR-2 §1).
func TestAgeConditioning(t *testing.T) {
	m := learn(t, twoRepos())
	fresh := m.Estimate(Bead{ID: "fresh", Repo: "api", Type: "task", Status: "open", CreatedAt: now})

	t.Run("a ready bead from ready_at", func(t *testing.T) {
		b := Bead{ID: "b", Repo: "api", Type: "task", Status: "open", CreatedAt: now.AddDate(0, 0, -5), ReadyAt: now.Add(-40 * time.Minute)}
		e := m.Estimate(b)
		if e.Started {
			t.Error("started, want not started")
		}
		// 40 minutes in, the total P50 exceeds the fresh lead P50.
		if total := 40 + e.Remaining.P50; total <= fresh.Lead.P50 || e.Remaining.P50 >= fresh.Remaining.P50 {
			t.Errorf("remaining P50 = %.1f, want less than a fresh bead's %.1f with a total above %.1f",
				e.Remaining.P50, fresh.Remaining.P50, fresh.Lead.P50)
		}
	})

	t.Run("a blocked bead has no age yet", func(t *testing.T) {
		e := m.Estimate(Bead{ID: "blocked", Repo: "api", Type: "task", Status: "open", CreatedAt: now.AddDate(0, 0, -5), Blocked: true})
		within(t, "remaining P50", e.Remaining.P50, fresh.Remaining.P50, 0.08)
	})

	t.Run("in progress, from started_at", func(t *testing.T) {
		b := Bead{ID: "b", Repo: "api", Type: "task", Status: StatusInProgress, CreatedAt: now.AddDate(0, 0, -5), StartedAt: now.Add(-40 * time.Minute)}
		e := m.Estimate(b)
		if !e.Started || e.Cycle != fresh.Cycle {
			t.Errorf("started = %v, cycle = %v; want started with the class cycle time %v", e.Started, e.Cycle, fresh.Cycle)
		}
		// 40 minutes into a 30-minute (median) cycle: some cycle time is
		// left, less than a fresh cycle.
		if e.Remaining.P50 <= 0 || e.Remaining.P50 >= fresh.Cycle.P50 || 40+e.Remaining.P50 <= fresh.Cycle.P50 {
			t.Errorf("remaining P50 = %.1f, want the rest of a cycle above 40 minutes", e.Remaining.P50)
		}
		for i := 0; i < 100; i++ {
			if q, _ := m.Sampler(b).Draw(rng(uint64(i))); q != 0 {
				t.Fatalf("sampled queue latency %v for a started bead, want 0", q)
			}
		}
	})

	t.Run("in progress without a start time, from ready_at", func(t *testing.T) {
		e := m.Estimate(Bead{ID: "nostart", Repo: "api", Type: "task", Status: StatusInProgress, CreatedAt: now})
		if !e.Started {
			t.Error("not started, want started from the status")
		}
		within(t, "remaining P50", e.Remaining.P50, fresh.Lead.P50, 0.08)
	})

	t.Run("released back to open", func(t *testing.T) {
		e := m.Estimate(Bead{ID: "released", Repo: "api", Type: "task", Status: "open", CreatedAt: now, StartedAt: now.Add(-time.Hour)})
		if e.Started {
			t.Error("started, want the not-started estimate")
		}
		within(t, "remaining P50", e.Remaining.P50, fresh.Remaining.P50, 0.08)
	})

	t.Run("beyond all history: Lindy", func(t *testing.T) {
		// Thirty days in, where no lead time reaches: it waits about as long
		// again, far beyond the tail cap, instead of a fresh draw of hours.
		const age = 30 * 24 * 60.0
		e := m.Estimate(Bead{ID: "old", Repo: "api", Type: "task", Status: "open", CreatedAt: now.AddDate(0, 0, -30)})
		within(t, "remaining P50", e.Remaining.P50, age, 0.08)
		within(t, "remaining P80", e.Remaining.P80, age*math.Exp(z80), 0.08)
	})
}

// A bead not started draws its remaining lead time, of which a cycle time
// is work and the rest waiting.
func TestDrawSplitsLeadTime(t *testing.T) {
	m := learn(t, twoRepos())
	s := m.Sampler(Bead{Repo: "web", Type: "task", Status: "open"})
	r := rng(10)
	var waits, total []float64
	for i := 0; i < 20000; i++ {
		q, w := s.Draw(r)
		if q < 0 || w <= 0 {
			t.Fatalf("draw %v + %v, want a wait of at least 0 and some work", q, w)
		}
		waits = append(waits, q)
		total = append(total, q+w)
	}
	sort.Float64s(waits)
	sort.Float64s(total)
	e := m.Estimate(Bead{Repo: "web", Type: "task"})
	within(t, "total P50", total[len(total)/2], e.Lead.P50, 0.05)
	if w := waits[len(waits)/2]; w <= 0 || w >= e.Lead.P50 {
		t.Errorf("median wait %.1f, want part of the lead time %.1f", w, e.Lead.P50)
	}
}

func TestEstimatesOpenBeadsInOrder(t *testing.T) {
	m := learn(t, twoRepos())
	beads := []Bead{
		{ID: "a", Repo: "api", Type: "task", Status: "open"},
		{ID: "done", Repo: "api", Type: "task", Status: "closed", ClosedAt: now},
		{ID: "b", Repo: "web", Type: "task", Status: StatusInProgress},
		{ID: "closed-no-date", Repo: "web", Type: "task", Status: "closed"},
		{ID: "c", Repo: "web", Type: "bug", Status: "blocked"},
	}
	var ids []string
	for _, e := range m.Estimates(beads) {
		ids = append(ids, e.ID)
	}
	if got := strings.Join(ids, ","); got != "a,b,c" {
		t.Errorf("estimated %s, want a,b,c", got)
	}
}

func TestDeterministic(t *testing.T) {
	beads := twoRepos()
	open := []Bead{
		{ID: "a", Repo: "api", Type: "task"},
		{ID: "b", Repo: "web", Type: "task", Status: StatusInProgress, StartedAt: now.Add(-time.Hour)},
		{ID: "c", Repo: "cli", Type: "bug", ReadyAt: now.Add(-time.Hour)},
	}
	first := learn(t, beads).Estimates(open)

	shuffled := append([]Bead(nil), beads...)
	rng(9).Shuffle(len(shuffled), func(i, j int) { shuffled[i], shuffled[j] = shuffled[j], shuffled[i] })
	if again := learn(t, shuffled).Estimates(open); !reflect.DeepEqual(first, again) {
		t.Errorf("estimates depend on input order:\n%+v\n%+v", first, again)
	}

	p := DefaultParams()
	p.Seed = 2
	other, err := Learn(beads, now, p)
	if err != nil {
		t.Fatal(err)
	}
	if reflect.DeepEqual(first, other.Estimates(open)) {
		t.Error("a different seed gave identical estimates")
	}
}

func TestInvalidParams(t *testing.T) {
	p := Params{PoolingStrength: -1, TailCapFactor: 0.5}
	_, err := Learn(nil, now, p)
	if err == nil {
		t.Fatal("Learn accepted invalid params")
	}
	for _, want := range []string{"window_days", "lead-time prior", "cycle prior", "pooling_strength", "tail_cap_factor"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not mention %s", err, want)
		}
	}
}
