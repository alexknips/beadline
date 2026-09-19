package forecast

import (
	"errors"
	"fmt"
	"math"
	"math/rand/v2"
	"runtime"
	"sort"
	"sync"
	"sync/atomic"
	"time"

	"github.com/alexknips/beadline/internal/graph"
)

// Sampler draws the durations of one open work bead, in minutes.
// estimate.Sampler satisfies it.
type Sampler interface {
	// Queue draws the wait from ready until an agent picks the bead up.
	Queue(r *rand.Rand) float64
	// Work draws the cycle time still to go.
	Work(r *rand.Rand) float64
}

// Draw draws one duration in minutes.
type Draw func(r *rand.Rand) float64

// Options configure a forecast.
type Options struct {
	// Now is the moment the forecast starts from.
	Now time.Time
	// Runs is the number of simulated schedules.
	Runs int
	// Seed makes a forecast reproducible: run n draws from a source seeded
	// with (Seed, n), whatever the number of workers.
	Seed uint64
	// Concurrency is the number of agents that work a repository at once. A
	// repository that is missing, or set to 0, has no limit.
	Concurrency map[string]int
	// Agent returns the duration sampler of an open work bead.
	Agent func(*graph.Issue) Sampler
	// Human returns the draw of the human wait still ahead of an open gate.
	// waited is how long the gate has already been ready, in minutes: 0
	// while it still waits for its blockers or children.
	Human func(i *graph.Issue, waited float64) Draw
	// Outside maps a bead to its blockers that no loaded repository has.
	Outside map[string][]string
	// Workers bounds the goroutines that run simulations; 0 means
	// GOMAXPROCS. Results do not depend on it.
	Workers int
}

func (o *Options) validate() error {
	var errs []error
	if o.Runs < 1 {
		errs = append(errs, fmt.Errorf("runs must be at least 1, got %d", o.Runs))
	}
	if o.Agent == nil {
		errs = append(errs, errors.New("no agent sampler"))
	}
	if o.Human == nil {
		errs = append(errs, errors.New("no human-gate sampler"))
	}
	for repo, n := range o.Concurrency {
		if n < 0 {
			errs = append(errs, fmt.Errorf("concurrency of repo %s must not be negative, got %d", repo, n))
		}
	}
	return errors.Join(errs...)
}

// Status says what an item's forecast means.
type Status string

const (
	// StatusForecast: open work remains and all of it can be scheduled.
	StatusForecast Status = "forecast"
	// StatusReadyToClose: all of the item's work is closed, the item is not.
	StatusReadyToClose Status = "ready_to_close"
	// StatusNotPlanned: the item has no children and no blockers.
	StatusNotPlanned Status = "not_planned"
	// StatusDeferred: the item itself is deferred (bd status deferred, or a
	// defer_until in the future), or all of its remaining work is parked.
	StatusDeferred Status = "deferred"
	// StatusBlockedOutside: some of the item's work waits on a bead that no
	// loaded repository has. The dates cover the loaded work only.
	StatusBlockedOutside Status = "blocked_outside"
	// StatusStalled: some of the item's work can never start: it is in a
	// blocking cycle or waits on a deferred bead. The dates cover the rest.
	StatusStalled Status = "stalled"
)

// Dated reports whether items with this status carry forecast dates.
func (s Status) Dated() bool {
	return s == StatusForecast || s == StatusBlockedOutside || s == StatusStalled
}

// Result is a forecast of every open high-level bead and goal.
type Result struct {
	Now         time.Time      `json:"now"`
	Runs        int            `json:"runs"`
	Seed        uint64         `json:"seed"`
	Concurrency map[string]int `json:"concurrency"` // as simulated; 0 = unlimited
	Items       []Item         `json:"items"`       // open high-level beads, in load order
	Goals       []Item         `json:"goals"`       // goals with open work, by ID
}

// Item is the forecast of one high-level bead or goal.
type Item struct {
	ID    string `json:"id"`
	Title string `json:"title,omitempty"` // empty for a goal whose bead is not loaded
	Repo  string `json:"repo,omitempty"`
	Type  string `json:"type,omitempty"`
	// Goals are the goals the item's labels name.
	Goals  []string   `json:"goals,omitempty"`
	Status Status     `json:"status"`
	DueAt  *time.Time `json:"due_at,omitempty"`

	// Scope: the beads the item waits for. For a high-level bead, its
	// descendants and, transitively, the open beads that it or its open
	// work is blocked by, with their open descendants; for a goal, its
	// members and their scopes. Total and Closed count them.
	Total   int     `json:"total"`
	Closed  int     `json:"closed"`
	DonePct float64 `json:"done_pct"`
	// Remaining are the open beads of the scope, by ID. Deferred and Stuck
	// are the ones among them that the simulation leaves out.
	Remaining []string `json:"remaining"`
	Deferred  []string `json:"deferred,omitempty"`
	Stuck     []string `json:"stuck,omitempty"`
	// Outside are blockers of the remaining work that no loaded repository
	// has.
	Outside []string `json:"outside,omitempty"`

	// Finish dates; nil unless Status.Dated() and some work can be
	// scheduled.
	P50 *Point `json:"p50,omitempty"`
	P80 *Point `json:"p80,omitempty"`
	P95 *Point `json:"p95,omitempty"`
	// OnTime is the share of runs that finish by DueAt.
	OnTime *float64 `json:"on_time,omitempty"`
	// CriticalChain is the chain of beads that set the P80 date, first to
	// last: each one could not start before the one before it finished.
	CriticalChain []string `json:"critical_chain,omitempty"`
}

// Point is one quantile of an item's finish. The run that sets it splits
// the time from now to At along its critical chain into agent time (queue
// latency, waiting for a free agent, work) and human time (gates and
// defer_until waits); the two add up to At − now.
type Point struct {
	At         time.Time `json:"at"`
	AgentHours float64   `json:"agent_hours"`
	HumanHours float64   `json:"human_hours"`
}

// Run forecasts every open high-level bead and goal of g.
func Run(g *graph.Graph, o Options) (*Result, error) {
	if err := o.validate(); err != nil {
		return nil, fmt.Errorf("forecast: %w", err)
	}
	p := newPlan(g, &o)
	items := p.items()

	// finish[k][run] is item k's finish in minutes from now.
	finish := make([][]float64, len(items))
	for k := range finish {
		if len(items[k].nodes) > 0 {
			finish[k] = make([]float64, o.Runs)
		}
	}
	workers := o.Workers
	if workers <= 0 {
		workers = runtime.GOMAXPROCS(0)
	}
	workers = min(workers, o.Runs)
	var next atomic.Int64
	var wg sync.WaitGroup
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			s := newSim(p)
			for {
				run := int(next.Add(1) - 1)
				if run >= o.Runs {
					return
				}
				s.run(source(o.Seed, run))
				for k, it := range items {
					if finish[k] != nil {
						finish[k][run] = s.last(it.nodes)
					}
				}
			}
		}()
	}
	wg.Wait()

	res := &Result{Now: o.Now, Runs: o.Runs, Seed: o.Seed, Concurrency: p.concurrency(), Items: []Item{}, Goals: []Item{}}
	traces := map[int]*sim{}
	trace := func(run int) *sim {
		if s := traces[run]; s != nil {
			return s
		}
		s := newSim(p)
		s.run(source(o.Seed, run))
		traces[run] = s
		return s
	}
	for k, it := range items {
		out := it.out
		if finish[k] != nil {
			order := runOrder(finish[k])
			point := func(q float64) *Point {
				pt := trace(order[nearestRank(q, len(order))]).point(it.nodes)
				return &pt
			}
			out.P50, out.P80, out.P95 = point(0.5), point(0.8), point(0.95)
			out.CriticalChain = trace(order[nearestRank(0.8, len(order))]).chain(it.nodes)
			if out.DueAt != nil {
				limit := minutes(out.DueAt.Sub(o.Now))
				n := 0
				for _, f := range finish[k] {
					if f <= limit {
						n++
					}
				}
				share := float64(n) / float64(len(finish[k]))
				out.OnTime = &share
			}
		}
		if it.goal {
			res.Goals = append(res.Goals, out)
		} else {
			res.Items = append(res.Items, out)
		}
	}
	return res, nil
}

// source returns the random source of one run.
func source(seed uint64, run int) *rand.Rand {
	return rand.New(rand.NewPCG(seed, uint64(run)))
}

// runOrder returns the run indices sorted by finish, ties by index.
func runOrder(finish []float64) []int {
	order := make([]int, len(finish))
	for i := range order {
		order[i] = i
	}
	sort.SliceStable(order, func(a, b int) bool { return finish[order[a]] < finish[order[b]] })
	return order
}

// nearestRank is the index of the q-quantile among n sorted values, so a
// quantile is always the outcome of an actual run.
func nearestRank(q float64, n int) int {
	i := int(math.Ceil(q*float64(n))) - 1
	return max(0, min(i, n-1))
}

func minutes(d time.Duration) float64 { return d.Minutes() }

// maxMinutes bounds durations so that times stay representable: about a
// century.
const maxMinutes = 100 * 365 * 24 * 60

func at(now time.Time, m float64) time.Time {
	return now.Add(time.Duration(math.Min(m, maxMinutes) * float64(time.Minute))).Round(time.Second)
}

func hours(m float64) float64 { return math.Round(m/60*100) / 100 }
