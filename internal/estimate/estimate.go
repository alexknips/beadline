package estimate

import (
	"errors"
	"fmt"
	"hash/fnv"
	"math/rand/v2"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/alexknips/beadline/internal/dist"
)

// Bead is what the estimator needs to know about one issue. Callers pass
// work beads only: orchestration (infra) types and high-level beads span
// other work, and their durations would distort the classes and the tail cap.
type Bead struct {
	ID          string
	Repo        string
	Type        string
	Status      string // bd status; "in_progress" marks a started bead
	Description string
	Labels      []string
	Children    int // number of parent-child children

	// Zero when unknown.
	CreatedAt, StartedAt, ClosedAt time.Time

	// EstimatedMinutes and P80Minutes are the numbers the bead already
	// carries (bd's estimated_minutes and the P80MetadataKey metadata), 0 when
	// unset. PlanWriteBack skips beads whose numbers are unchanged.
	EstimatedMinutes, P80Minutes int
}

// StatusInProgress is the bd status of a bead an agent has started.
const StatusInProgress = "in_progress"

func (b Bead) closed() bool { return !b.ClosedAt.IsZero() || b.Status == "closed" }

// Size is a coarse size bucket: the size proxy of a class.
type Size int

// Size buckets.
const (
	Small Size = iota
	Medium
	Large
)

func (s Size) String() string { return [...]string{"S", "M", "L"}[s] }

// Size thresholds: description length in characters and number of children.
const (
	mediumDescription = 400
	largeDescription  = 1500
	largeChildren     = 3
)

// SizeOf buckets a bead. A size:s, size:m or size:l label (case-insensitive;
// xs and xl count as s and l) decides. Otherwise a bead is Large with a
// description of 1500 characters or 3 children, Medium with 400 characters
// or any child, and Small below that.
func SizeOf(b Bead) Size {
	for _, l := range b.Labels {
		v, ok := strings.CutPrefix(strings.ToLower(l), "size:")
		if !ok {
			continue
		}
		switch v {
		case "xs", "s":
			return Small
		case "m":
			return Medium
		case "l", "xl":
			return Large
		}
	}
	n := utf8.RuneCountInString(b.Description)
	switch {
	case n >= largeDescription || b.Children >= largeChildren:
		return Large
	case n >= mediumDescription || b.Children > 0:
		return Medium
	}
	return Small
}

// Class is the unit durations are learned for: repository × issue type ×
// size bucket.
type Class struct {
	Repo, Type string
	Size       Size
}

// ClassOf returns the class of a bead.
func ClassOf(b Bead) Class { return Class{Repo: b.Repo, Type: b.Type, Size: SizeOf(b)} }

func (c Class) String() string { return c.Repo + "/" + c.Type + "/" + c.Size.String() }

// Params are the estimator settings; the [model] table of beadline.toml
// supplies them (docs/design.md).
type Params struct {
	WindowDays        int     // learn from beads closed in the last WindowDays
	Seed              uint64  // seeds the draws behind every reported quantile
	CyclePriorMinutes float64 // median of the cycle-time root prior
	QueuePriorMinutes float64 // median of the queue-latency root prior
	PoolingStrength   float64 // k: pseudo-observations a node borrows from its parent
	TailCapFactor     float64 // draws are capped at this × the longest observation
}

// DefaultParams matches the beadline.toml defaults. Queue latency has no key
// of its own and shares cycle_minutes_prior.
func DefaultParams() Params {
	return Params{
		WindowDays:        45,
		Seed:              1,
		CyclePriorMinutes: 60,
		QueuePriorMinutes: 60,
		PoolingStrength:   10,
		TailCapFactor:     3,
	}
}

func (p Params) validate() error {
	var errs []error
	if p.WindowDays <= 0 {
		errs = append(errs, fmt.Errorf("window_days must be positive, got %d", p.WindowDays))
	}
	if !(p.CyclePriorMinutes > 0) {
		errs = append(errs, fmt.Errorf("cycle prior must be positive, got %v", p.CyclePriorMinutes))
	}
	if !(p.QueuePriorMinutes > 0) {
		errs = append(errs, fmt.Errorf("queue prior must be positive, got %v", p.QueuePriorMinutes))
	}
	if !(p.PoolingStrength >= 0) {
		errs = append(errs, fmt.Errorf("pooling_strength must not be negative, got %v", p.PoolingStrength))
	}
	if !(p.TailCapFactor >= 1) {
		errs = append(errs, fmt.Errorf("tail_cap_factor must be at least 1, got %v", p.TailCapFactor))
	}
	return errors.Join(errs...)
}

// Quantiles are a duration's P50 and P80 in minutes.
type Quantiles struct{ P50, P80 float64 }

// quantileDraws is the number of draws behind every reported quantile.
const quantileDraws = 4000

// Model holds the learned cycle-time and queue-latency distributions of every
// class seen in the window, with their backoff chains. It is immutable and
// safe for concurrent use.
type Model struct {
	params       Params
	now          time.Time
	cycle, queue chain
}

// Learn builds the model from the beads closed in the window
// (now − WindowDays, now]. Cycle time runs from started_at to closed_at, or
// from created_at when the bead has no usable start; queue latency runs from
// created_at to started_at. Beads closed after now are ignored, so a model
// can be learned as of a past moment.
func Learn(beads []Bead, now time.Time, p Params) (*Model, error) {
	if err := p.validate(); err != nil {
		return nil, fmt.Errorf("estimate: %w", err)
	}
	since := now.AddDate(0, 0, -p.WindowDays)
	cycle, queue := newObservations(), newObservations()
	for _, b := range beads {
		if b.ClosedAt.IsZero() || !b.ClosedAt.After(since) || b.ClosedAt.After(now) {
			continue
		}
		c := ClassOf(b)
		start := b.StartedAt
		if start.IsZero() || start.After(b.ClosedAt) {
			start = b.CreatedAt
		}
		if !start.IsZero() && !start.After(b.ClosedAt) {
			cycle.add(c, b.ClosedAt.Sub(start).Minutes())
		}
		if !b.CreatedAt.IsZero() && !b.StartedAt.IsZero() &&
			!b.StartedAt.Before(b.CreatedAt) && !b.StartedAt.After(b.ClosedAt) {
			queue.add(c, b.StartedAt.Sub(b.CreatedAt).Minutes())
		}
	}
	return &Model{
		params: p,
		now:    now,
		cycle:  cycle.chain("cycle", dist.NewPrior(p.CyclePriorMinutes), p),
		queue:  queue.chain("queue", dist.NewPrior(p.QueuePriorMinutes), p),
	}, nil
}

// Sampler draws the durations of one open bead. The forecaster makes one per
// bead and calls it once per simulation run.
type Sampler struct {
	queue, cycle *dist.Dist
	started      bool
	elapsed      float64 // minutes since started_at
}

// Sampler returns the sampler of an open bead, from the most specific node of
// its class's backoff chain.
func (m *Model) Sampler(b Bead) Sampler {
	c := ClassOf(b)
	s := Sampler{queue: m.queue.node(c).d, cycle: m.cycle.node(c).d}
	if b.Status == StatusInProgress {
		s.started = true
		if !b.StartedAt.IsZero() && m.now.After(b.StartedAt) {
			s.elapsed = m.now.Sub(b.StartedAt).Minutes()
		}
	}
	return s
}

// Queue draws the minutes before an agent starts the bead: zero once started.
func (s Sampler) Queue(r *rand.Rand) float64 {
	if s.started {
		return 0
	}
	return s.queue.Sample(r)
}

// Work draws the remaining cycle time in minutes. For a started bead it is
// conditioned on the time already spent (ADR-1 §2).
func (s Sampler) Work(r *rand.Rand) float64 {
	if s.started {
		return s.cycle.SampleBeyond(r, s.elapsed)
	}
	return s.cycle.Sample(r)
}

// Estimate is the internal per-bead estimate. It feeds the simulation and
// the optional write-back and is never shown.
type Estimate struct {
	ID, Repo string
	Class    Class
	Started  bool
	// Cycle is the full cycle time: what the write-back records.
	Cycle Quantiles
	// Queue is the latency before start; zero for a started bead.
	Queue Quantiles
	// Remaining is the cycle time still to go; Cycle for a bead not started.
	Remaining Quantiles
}

// Estimates returns the estimate of every open bead, in input order.
func (m *Model) Estimates(beads []Bead) []Estimate {
	var out []Estimate
	for _, b := range beads {
		if !b.closed() {
			out = append(out, m.Estimate(b))
		}
	}
	return out
}

// Estimate returns the estimate of one open bead. Quantiles come from 4,000
// seeded draws of the same sampler the simulation uses. Beads that share a
// node share its numbers; the remaining time of a started bead is drawn with
// a source seeded by its ID.
func (m *Model) Estimate(b Bead) Estimate {
	c := ClassOf(b)
	s := m.Sampler(b)
	e := Estimate{ID: b.ID, Repo: b.Repo, Class: c, Started: s.started, Cycle: m.cycle.node(c).q}
	e.Remaining = e.Cycle
	if s.started {
		r := seeded(m.params.Seed, "remaining", b.Repo+"/"+b.ID)
		e.Remaining = quantiles(func() float64 { return s.Work(r) })
	} else {
		e.Queue = m.queue.node(c).q
	}
	return e
}

func seeded(seed uint64, quantity, key string) *rand.Rand {
	h := fnv.New64a()
	h.Write([]byte(quantity + "\x00" + key))
	return rand.New(rand.NewPCG(seed, h.Sum64()))
}

func quantiles(sample func() float64) Quantiles {
	q := dist.Quantiles(quantileDraws, sample, 0.5, 0.8)
	return Quantiles{P50: q[0], P80: q[1]}
}

// observations collects durations per backoff level.
type observations struct {
	all     []float64
	repos   map[string][]float64
	classes map[Class][]float64
}

func newObservations() *observations {
	return &observations{repos: map[string][]float64{}, classes: map[Class][]float64{}}
}

func (o *observations) add(c Class, m float64) {
	o.all = append(o.all, m)
	o.repos[c.Repo] = append(o.repos[c.Repo], m)
	o.classes[c] = append(o.classes[c], m)
}

// chain is the backoff tree of one quantity: class → repo → all → prior.
type chain struct {
	all     node
	repos   map[string]node
	classes map[Class]node
}

// node is a distribution with its precomputed quantiles.
type node struct {
	d *dist.Dist
	q Quantiles
}

func (o *observations) chain(quantity string, prior dist.Prior, p Params) chain {
	build := func(key string, d *dist.Dist) node {
		r := seeded(p.Seed, quantity, key)
		return node{d: d, q: quantiles(func() float64 { return d.Sample(r) })}
	}
	root := dist.NewRoot(prior, o.all, p.TailCapFactor)
	c := chain{
		all:     build("all", root.Child(o.all, p.PoolingStrength)),
		repos:   make(map[string]node, len(o.repos)),
		classes: make(map[Class]node, len(o.classes)),
	}
	for repo, obs := range o.repos {
		c.repos[repo] = build("repo:"+repo, c.all.d.Child(obs, p.PoolingStrength))
	}
	for cl, obs := range o.classes {
		c.classes[cl] = build("class:"+cl.String(), c.repos[cl.Repo].d.Child(obs, p.PoolingStrength))
	}
	return c
}

// node returns the most specific node for a class. An unseen class draws
// purely from its repository, an unseen repository from all repositories.
func (c chain) node(cl Class) node {
	if n, ok := c.classes[cl]; ok {
		return n
	}
	if n, ok := c.repos[cl.Repo]; ok {
		return n
	}
	return c.all
}
