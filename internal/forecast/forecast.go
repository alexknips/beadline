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

// ModelVersion names the revision of the forecasting model: the estimator
// and this simulation (docs/design.md, ADR-2). Snapshots record it, so
// calibration never mixes the track records of two models.
const ModelVersion = "adr-2"

// Sampler draws the durations of one open work bead, in minutes.
// estimate.Sampler satisfies it.
type Sampler interface {
	// Draw draws the wait from ready until an agent picks the bead up, and
	// the work still to go after that. A bead already under way has no wait.
	Draw(r *rand.Rand) (queue, work float64)
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
	// Grid lists quantile levels, each strictly between 0 and 1 and in
	// ascending order. When set, every dated item carries its finish at each
	// level (Item.GridHours), and Result.Leaves forecasts the close of every
	// open leaf the schedule covers: the grid that calibration scores.
	Grid []float64

	// DateFrom is where finish dates are counted from; the zero value uses
	// Now. Set it to a declared resume time when the whole system is idle
	// right now, so dates do not silently assume work resumes this instant
	// (docs/design.md, ADR-3 §2). Readiness and every other "as of now"
	// graph fact still use Now: only the calendar label of a finish moves.
	DateFrom time.Time
	// Availability is the measured fraction of calendar time the agents are
	// actually active, used to turn simulated durations (already learned
	// net of idle time, Estimator rules) back into calendar time (ADR-3
	// §1). The zero value behaves as 1: durations run wall-to-wall from
	// DateFrom, with no further correction.
	Availability float64
}

// dateFrom is DateFrom, or Now when it is unset.
func (o *Options) dateFrom() time.Time {
	if o.DateFrom.IsZero() {
		return o.Now
	}
	return o.DateFrom
}

// availability is Availability, or 1 when it is not positive.
func (o *Options) availability() float64 {
	if o.Availability <= 0 {
		return 1
	}
	return o.Availability
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
	for n, q := range o.Grid {
		if !(q > 0 && q < 1) || (n > 0 && !(q > o.Grid[n-1])) {
			errs = append(errs, fmt.Errorf("grid levels must ascend strictly between 0 and 1, got %v", o.Grid))
			break
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
	// Grid echoes Options.Grid, the levels of every GridHours.
	Grid []float64 `json:"grid,omitempty"`
	// Leaves forecasts the close of every open leaf that the schedule
	// covers, in load order; only with Options.Grid. A leaf is a bead with no
	// open children that is neither high-level nor a goal's own bead: a
	// work bead or a gate. Parked and stuck beads have no forecast.
	Leaves []Leaf `json:"leaves,omitempty"`
}

// Leaf is the forecast close of one open leaf bead.
type Leaf struct {
	ID   string `json:"id"`
	Repo string `json:"repo"`
	// GridHours is the close at each Grid level, in hours from now.
	GridHours []float64 `json:"grid_hours"`
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
	// GridHours is the finish at each Result.Grid level, in hours from now;
	// nil without Options.Grid or dates.
	GridHours []float64 `json:"grid_hours,omitempty"`
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

	// finish[k][run] is item k's finish in minutes from now, leafFinish[j][run]
	// that of leaf j.
	finish := make([][]float64, len(items))
	for k := range finish {
		if len(items[k].nodes) > 0 {
			finish[k] = make([]float64, o.Runs)
		}
	}
	var leaves []int32
	if len(o.Grid) > 0 {
		leaves = p.leaves()
	}
	leafFinish := make([][]float64, len(leaves))
	for j := range leafFinish {
		leafFinish[j] = make([]float64, o.Runs)
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
				for j, k := range leaves {
					leafFinish[j][run] = s.finish[k]
				}
			}
		}()
	}
	wg.Wait()

	// offset is how far DateFrom sits after Now, in minutes: finish[] and
	// leafFinish[] are already calendar minutes from DateFrom (sim.run
	// scales every draw by 1/Availability), but GridHours and OnTime are
	// counted from Now, as calibration expects (Estimator rules, ADR-3 §1).
	offset := minutes(p.dateFrom.Sub(o.Now))

	res := &Result{Now: o.Now, Runs: o.Runs, Seed: o.Seed, Concurrency: p.concurrency(), Items: []Item{}, Goals: []Item{}, Grid: o.Grid}
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
			out.GridHours = gridHours(finish[k], o.Grid, offset)
			if out.DueAt != nil {
				limit := minutes(out.DueAt.Sub(o.Now))
				n := 0
				for _, f := range finish[k] {
					if f+offset <= limit {
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
	for j, k := range leaves {
		i := p.nodes[k].issue
		res.Leaves = append(res.Leaves, Leaf{ID: i.ID, Repo: i.Repo, GridHours: gridHours(leafFinish[j], o.Grid, offset)})
	}
	return res, nil
}

// gridHours returns the nearest-rank quantiles of finishes (calendar
// minutes from DateFrom) at the grid levels, in hours from Now (offset
// minutes ahead of DateFrom added back in); nil for an empty grid.
func gridHours(finishes []float64, grid []float64, offset float64) []float64 {
	if len(grid) == 0 {
		return nil
	}
	sorted := append([]float64(nil), finishes...)
	sort.Float64s(sorted)
	out := make([]float64, len(grid))
	for n, q := range grid {
		out[n] = hours(math.Min(sorted[nearestRank(q, len(sorted))]+offset, maxMinutes))
	}
	return out
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
