package estimate

import (
	"fmt"
	"math"
	"math/rand/v2"
	"reflect"
	"strings"
	"testing"
	"time"
)

var now = time.Date(2026, 9, 19, 12, 0, 0, 0, time.UTC)

func rng(stream uint64) *rand.Rand { return rand.New(rand.NewPCG(7, stream)) }

func minutes(m float64) time.Duration { return time.Duration(m * float64(time.Minute)) }

// history returns n beads closed at random in the 30 days before closedBy,
// with log-normal cycle time and queue latency of the given medians and σ.
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

// z80 is the standard normal 0.8-quantile.
const z80 = 0.8416212335729143

func TestLearnsKnownDistributions(t *testing.T) {
	m := learn(t, twoRepos())
	for _, tt := range []struct {
		repo         string
		cycle, queue float64
	}{{"api", 30, 5}, {"web", 240, 60}} {
		e := m.Estimate(Bead{ID: "open", Repo: tt.repo, Type: "task", Status: "open"})
		if e.Class != (Class{tt.repo, "task", Small}) || e.Started {
			t.Errorf("%s: class %v started %v", tt.repo, e.Class, e.Started)
		}
		// σ = 0.5 plus a little smoothing.
		within(t, tt.repo+" cycle P50", e.Cycle.P50, tt.cycle, 0.10)
		within(t, tt.repo+" cycle P80", e.Cycle.P80, tt.cycle*math.Exp(0.5*z80), 0.10)
		within(t, tt.repo+" queue P50", e.Queue.P50, tt.queue, 0.10)
		within(t, tt.repo+" queue P80", e.Queue.P80, tt.queue*math.Exp(0.5*z80), 0.10)
		if e.Remaining != e.Cycle {
			t.Errorf("%s: remaining %v, want the cycle time %v for a bead not started", tt.repo, e.Remaining, e.Cycle)
		}
	}
}

func TestBackoff(t *testing.T) {
	m := learn(t, twoRepos())
	api := m.Estimate(Bead{Repo: "api", Type: "task"})

	t.Run("unseen class falls back to its repository", func(t *testing.T) {
		e := m.Estimate(Bead{Repo: "api", Type: "chore", Labels: []string{"size:l"}})
		within(t, "cycle P50", e.Cycle.P50, api.Cycle.P50, 0.05)
		within(t, "queue P50", e.Queue.P50, api.Queue.P50, 0.05)
	})

	t.Run("unseen repository falls back to all repositories", func(t *testing.T) {
		e := m.Estimate(Bead{Repo: "cli", Type: "task"})
		if !(e.Cycle.P50 > 30*1.2 && e.Cycle.P50 < 240/1.2) {
			t.Errorf("cycle P50 = %.1f, want between the api (30) and web (240) medians", e.Cycle.P50)
		}
	})

	t.Run("no history is the prior", func(t *testing.T) {
		e := learn(t, nil).Estimate(Bead{Repo: "api", Type: "task"})
		p80 := 60 * math.Exp(z80) // log-normal, median 60, σ 1
		within(t, "cycle P50", e.Cycle.P50, 60, 0.06)
		within(t, "cycle P80", e.Cycle.P80, p80, 0.06)
		within(t, "queue P50", e.Queue.P50, 60, 0.06)
		within(t, "queue P80", e.Queue.P80, p80, 0.06)
	})

	t.Run("thin class is half its own at k observations", func(t *testing.T) {
		// Ten large api tasks near 300 minutes: with k = 10 half the draws
		// come from them, half from the api repository (about 30 minutes).
		large := history(rng(2), "api", "task", 10, 300, 5, 0.1, now)
		for i := range large {
			large[i].Labels = []string{"size:L"}
		}
		m := learn(t, append(twoRepos(), large...))
		s := m.Sampler(Bead{Repo: "api", Type: "task", Labels: []string{"size:l"}})
		r := rng(3)
		long := 0
		const n = 20000
		for i := 0; i < n; i++ {
			if s.Work(r) > 120 {
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

func TestCycleWithoutStart(t *testing.T) {
	var beads []Bead
	for i := 0; i < 200; i++ {
		closed := now.Add(-time.Duration(i+1) * time.Hour)
		b := Bead{ID: fmt.Sprint(i), Repo: "api", Type: "task", CreatedAt: closed.Add(-20 * time.Minute), ClosedAt: closed}
		if i%2 == 1 {
			b.StartedAt = closed.Add(time.Hour) // clock skew: started after closing
		}
		beads = append(beads, b)
	}
	e := learn(t, beads).Estimate(Bead{Repo: "api", Type: "task"})
	within(t, "cycle P50 from created_at", e.Cycle.P50, 20, 0.05)
	within(t, "queue P50 without starts is the prior", e.Queue.P50, 60, 0.06)
}

func TestInstantBeadsFloorAtOneMinute(t *testing.T) {
	var beads []Bead
	for i := 0; i < 100; i++ {
		at := now.Add(-time.Duration(i+1) * time.Hour)
		beads = append(beads, Bead{ID: fmt.Sprint(i), Repo: "api", Type: "task", CreatedAt: at, StartedAt: at, ClosedAt: at})
	}
	e := learn(t, beads).Estimate(Bead{Repo: "api", Type: "task"})
	within(t, "cycle P50", e.Cycle.P50, 1, 0.05)
	within(t, "queue P50", e.Queue.P50, 1, 0.05)
	if e.Cycle.P80 > 3 {
		t.Errorf("cycle P80 = %.2f, want at most the tail cap of 3 minutes", e.Cycle.P80)
	}
}

func TestInProgress(t *testing.T) {
	m := learn(t, twoRepos())
	fresh := m.Estimate(Bead{ID: "fresh", Repo: "api", Type: "task", Status: "open"})

	t.Run("conditioned on elapsed time", func(t *testing.T) {
		b := Bead{ID: "b", Repo: "api", Type: "task", Status: StatusInProgress, StartedAt: now.Add(-40 * time.Minute)}
		e := m.Estimate(b)
		if !e.Started || e.Queue != (Quantiles{}) {
			t.Errorf("started = %v, queue = %v; want started with no queue latency", e.Started, e.Queue)
		}
		if e.Cycle != fresh.Cycle {
			t.Errorf("cycle = %v, want the class cycle time %v", e.Cycle, fresh.Cycle)
		}
		// 40 minutes in, the total P50 exceeds 40 and the class P50 (30).
		if total := 40 + e.Remaining.P50; total <= fresh.Cycle.P50 || e.Remaining.P50 <= 0 {
			t.Errorf("remaining P50 = %.1f, want positive with a total above %.1f", e.Remaining.P50, fresh.Cycle.P50)
		}
		if e.Remaining.P50 >= fresh.Cycle.P50 {
			t.Errorf("remaining P50 = %.1f, want less than a fresh bead's %.1f", e.Remaining.P50, fresh.Cycle.P50)
		}
		if q := m.Sampler(b).Queue(rng(8)); q != 0 {
			t.Errorf("sampled queue latency %v for a started bead, want 0", q)
		}
	})

	t.Run("beyond all history draws afresh", func(t *testing.T) {
		e := m.Estimate(Bead{ID: "stuck", Repo: "api", Type: "task", Status: StatusInProgress, StartedAt: now.AddDate(0, 0, -30)})
		within(t, "remaining P50", e.Remaining.P50, fresh.Cycle.P50, 0.08)
	})

	t.Run("in progress without a start time", func(t *testing.T) {
		e := m.Estimate(Bead{ID: "nostart", Repo: "api", Type: "task", Status: StatusInProgress})
		if !e.Started {
			t.Error("not started, want started from the status")
		}
		within(t, "remaining P50", e.Remaining.P50, fresh.Cycle.P50, 0.08)
	})

	t.Run("released back to open", func(t *testing.T) {
		e := m.Estimate(Bead{ID: "released", Repo: "api", Type: "task", Status: "open", StartedAt: now.Add(-time.Hour)})
		if e.Started || e.Queue != fresh.Queue || e.Remaining != fresh.Cycle {
			t.Errorf("estimate %+v, want the not-started estimate", e)
		}
	})
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
		{ID: "c", Repo: "cli", Type: "bug"},
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

func TestSizeOf(t *testing.T) {
	long := strings.Repeat("x", largeDescription)
	medium := strings.Repeat("é", mediumDescription) // characters, not bytes
	for _, tt := range []struct {
		name string
		b    Bead
		want Size
	}{
		{"empty", Bead{}, Small},
		{"short description", Bead{Description: "fix the typo"}, Small},
		{"medium description", Bead{Description: medium}, Medium},
		{"medium description in bytes only", Bead{Description: strings.Repeat("é", mediumDescription/2)}, Small},
		{"long description", Bead{Description: long}, Large},
		{"one child", Bead{Children: 1}, Medium},
		{"three children", Bead{Children: largeChildren}, Large},
		{"label beats description", Bead{Description: long, Labels: []string{"area:x", "size:s"}}, Small},
		{"label is case-insensitive", Bead{Labels: []string{"Size:M"}}, Medium},
		{"xl label", Bead{Labels: []string{"size:xl"}}, Large},
		{"xs label", Bead{Children: 5, Labels: []string{"size:xs"}}, Small},
		{"unknown size label is ignored", Bead{Children: 5, Labels: []string{"size:huge"}}, Large},
	} {
		if got := SizeOf(tt.b); got != tt.want {
			t.Errorf("%s: SizeOf = %v, want %v", tt.name, got, tt.want)
		}
	}
}

func TestInvalidParams(t *testing.T) {
	p := Params{PoolingStrength: -1, TailCapFactor: 0.5}
	_, err := Learn(nil, now, p)
	if err == nil {
		t.Fatal("Learn accepted invalid params")
	}
	for _, want := range []string{"window_days", "cycle prior", "queue prior", "pooling_strength", "tail_cap_factor"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not mention %s", err, want)
		}
	}
}
