package estimate

import (
	"errors"
	"fmt"
	"hash/fnv"
	"math"
	"math/rand/v2"
	"regexp"
	"strconv"
	"time"

	"github.com/alexknips/beadline/internal/dist"
)

// Bead is what the estimator needs to know about one issue. Callers pass
// work beads only: orchestration (infra) types and high-level beads span
// other work, and their durations would distort the classes and the tail cap.
type Bead struct {
	ID       string
	Repo     string
	Type     string
	Priority int    // bd priority: 0 (P0) is the most urgent
	Status   string // bd status; "in_progress" marks a started bead

	// Zero when unknown.
	CreatedAt, StartedAt, ClosedAt time.Time
	// ReadyAt is when the bead stopped waiting for other beads (ADR-2 §1):
	// its creation, or the close of its last blocker. For a closed bead it
	// is as of its close, for an open one as of now. Zero means CreatedAt.
	ReadyAt time.Time
	// Blocked marks an open bead that still waits for an open blocker. It is
	// not ready, so its lead time has not started.
	Blocked bool

	// CloseReason and WorkOutcome (Gas City's gc.work_outcome metadata) tell
	// a delivery from a close that was not one.
	CloseReason, WorkOutcome string

	// EstimatedMinutes and P80Minutes are the numbers the bead already
	// carries (bd's estimated_minutes and the P80MetadataKey metadata), 0 when
	// unset. PlanWriteBack skips beads whose numbers are unchanged.
	EstimatedMinutes, P80Minutes int
}

// Statuses with a meaning to the estimator.
const (
	// StatusInProgress is the bd status of a bead an agent has started.
	StatusInProgress = "in_progress"
	// statusDeferred is the bd status of a parked bead: nobody works it, so
	// its age says nothing about lead times.
	statusDeferred = "deferred"
)

func (b Bead) closed() bool { return !b.ClosedAt.IsZero() || b.Status == "closed" }

// readyAt is ReadyAt, or CreatedAt when it is unknown.
func (b Bead) readyAt() time.Time {
	if b.ReadyAt.IsZero() {
		return b.CreatedAt
	}
	return b.ReadyAt
}

// Class is the unit durations are learned for: repository × issue type ×
// priority (ADR-2 §1). Priority orders lead times, from hours at P0 to days
// at P3; description length does not.
type Class struct {
	Repo, Type string
	Priority   int
}

// ClassOf returns the class of a bead.
func ClassOf(b Bead) Class { return Class{Repo: b.Repo, Type: b.Type, Priority: b.Priority} }

func (c Class) String() string { return c.Repo + "/" + c.Type + "/P" + strconv.Itoa(c.Priority) }

// Params are the estimator settings; the [model] table of beadline.toml
// supplies them (docs/design.md).
type Params struct {
	WindowDays        int     // learn from beads closed in the last WindowDays
	Seed              uint64  // seeds the draws behind every reported quantile
	LeadPriorMinutes  float64 // median of the lead-time root prior
	CyclePriorMinutes float64 // median of the cycle-time root prior
	PoolingStrength   float64 // k: pseudo-observations a node borrows from its parent
	TailCapFactor     float64 // draws are capped at this × the longest observation
	// Active returns the active (idle-masked) minutes between from and to
	// (from <= to): what every duration and every open bead's age is
	// measured in, so a stretch when nothing ran is not learned as work
	// (docs/design.md, ADR-3). nil, the default, is the plain wall-clock
	// difference: no idle time is known.
	Active func(from, to time.Time) float64
}

// active returns the active minutes between from and to: p.Active(from, to)
// when set, else the plain wall-clock difference.
func (p Params) active(from, to time.Time) float64 {
	if p.Active != nil {
		return p.Active(from, to)
	}
	return to.Sub(from).Minutes()
}

// DefaultParams matches the beadline.toml defaults. Lead time has no key of
// its own: its prior is a queue and a cycle at cycle_minutes_prior each.
func DefaultParams() Params {
	return Params{
		WindowDays:        45,
		Seed:              1,
		LeadPriorMinutes:  120,
		CyclePriorMinutes: 60,
		PoolingStrength:   10,
		TailCapFactor:     3,
	}
}

func (p Params) validate() error {
	var errs []error
	if p.WindowDays <= 0 {
		errs = append(errs, fmt.Errorf("window_days must be positive, got %d", p.WindowDays))
	}
	if !(p.LeadPriorMinutes > 0) {
		errs = append(errs, fmt.Errorf("lead-time prior must be positive, got %v", p.LeadPriorMinutes))
	}
	if !(p.CyclePriorMinutes > 0) {
		errs = append(errs, fmt.Errorf("cycle prior must be positive, got %v", p.CyclePriorMinutes))
	}
	if !(p.PoolingStrength >= 0) {
		errs = append(errs, fmt.Errorf("pooling_strength must not be negative, got %v", p.PoolingStrength))
	}
	if !(p.TailCapFactor >= 1) {
		errs = append(errs, fmt.Errorf("tail_cap_factor must be at least 1, got %v", p.TailCapFactor))
	}
	return errors.Join(errs...)
}

// Reasons a close in the window does not teach durations: it was not a
// delivery (sample hygiene, ADR-2 §1). Learn counts them, so they can be
// shown.
const (
	SkipOutcome  = "no-op or abandoned" // gc.work_outcome is no-op or abandoned
	SkipDescoped = "descoped"           // the close reason says duplicate, superseded, won't fix ...
	SkipBulk     = "bulk close"         // BulkCloses or more closes of its repo in the same minute
	SkipInstant  = "closed at once"     // closed less than InstantClose after its creation
)

// SkipReasons lists the Skip reasons in the order they are tested.
var SkipReasons = []string{SkipOutcome, SkipDescoped, SkipBulk, SkipInstant}

const (
	// BulkCloses closes of one repo within one minute are an administrative
	// cleanup, not deliveries.
	BulkCloses = 5
	// InstantClose: a bead closed this soon after its creation was filed as
	// done or by mistake. No agent worked it.
	InstantClose = time.Minute
)

// undeliveredReason matches the start of a close reason that says the bead
// was not delivered. It deliberately does not look for words such as
// "test" anywhere in the reason: those match genuine merges.
var undeliveredReason = regexp.MustCompile(`(?i)^\s*(duplicate|dup of|supersed|won'?t (fix|do)|wontfix|obsolete|` +
	`(not|no longer) needed|not planned|moot|abandon|descop|out of scope|closing stale|bulk[- ]?close)`)

// Undelivered says why a close was not a delivery, judging by what it
// states: SkipOutcome when its gc.work_outcome is no-op or abandoned,
// SkipDescoped when its close reason starts with duplicate, superseded,
// won't fix, obsolete, not needed, descoped, abandoned and the like, and ""
// otherwise.
func Undelivered(closeReason, workOutcome string) string {
	switch {
	case workOutcome == "no-op" || workOutcome == "abandoned":
		return SkipOutcome
	case undeliveredReason.MatchString(closeReason):
		return SkipDescoped
	}
	return ""
}

// Sample describes what a model learned from.
type Sample struct {
	// Closes are the delivered closes in the window: every one teaches a
	// lead time, and those with a start also a cycle time.
	Closes, Started int
	// Open are the beads still open that entered as censored lead or cycle
	// times.
	Open int
	// Skipped counts the closes in the window that were not deliveries, by
	// Skip reason.
	Skipped map[string]int
}

// Quantiles are a duration's P50 and P80 in minutes.
type Quantiles struct{ P50, P80 float64 }

// quantileDraws is the number of draws behind every reported quantile.
const quantileDraws = 4000

// Model holds the learned lead-time and cycle-time distributions of every
// class seen in the window, with their backoff chains. It is immutable and
// safe for concurrent use.
type Model struct {
	params      Params
	now         time.Time
	lead, cycle chain
	sample      Sample
}

// Learn builds the model from the beads closed in the window
// (now − WindowDays, now] and the beads still open at now (ADR-2 §1).
//
//   - Lead time runs from ready_at to closed_at. Every delivered close
//     teaches one.
//   - Cycle time runs from started_at to closed_at, for closes with a start.
//   - Beads still open enter as right-censored observations at their current
//     age, when that age began in the window: a lead time for a ready bead
//     (not blocked, not deferred), and a cycle time for one in progress.
//     Kaplan–Meier weighs them (dist.Censored).
//   - A close that was not a delivery teaches nothing (SkipReasons).
//
// Beads closed after now count as open, so a model can be learned as of a
// past moment.
func Learn(beads []Bead, now time.Time, p Params) (*Model, error) {
	if err := p.validate(); err != nil {
		return nil, fmt.Errorf("estimate: %w", err)
	}
	since := now.AddDate(0, 0, -p.WindowDays)
	type minute struct {
		repo string
		at   time.Time
	}
	perMinute := map[minute]int{}
	closedBy := func(b Bead) bool { return !b.ClosedAt.IsZero() && !b.ClosedAt.After(now) }
	for _, b := range beads {
		if closedBy(b) {
			perMinute[minute{b.Repo, b.ClosedAt.Truncate(time.Minute)}]++
		}
	}

	lead, cycle := newObservations(), newObservations()
	s := Sample{Skipped: map[string]int{}}
	for _, b := range beads {
		c := ClassOf(b)
		ready := b.readyAt()
		if closedBy(b) {
			if !b.ClosedAt.After(since) {
				continue
			}
			reason := Undelivered(b.CloseReason, b.WorkOutcome)
			switch {
			case reason != "":
			case perMinute[minute{b.Repo, b.ClosedAt.Truncate(time.Minute)}] >= BulkCloses:
				reason = SkipBulk
			case !b.CreatedAt.IsZero() && b.ClosedAt.Sub(b.CreatedAt) < InstantClose:
				reason = SkipInstant
			}
			if reason != "" {
				s.Skipped[reason]++
				continue
			}
			s.Closes++
			if !ready.IsZero() && !ready.After(b.ClosedAt) {
				lead.add(c, p.active(ready, b.ClosedAt), false)
			}
			if started(b, b.ClosedAt) {
				s.Started++
				cycle.add(c, p.active(b.StartedAt, b.ClosedAt), false)
			}
			continue
		}
		if (b.closed() && b.ClosedAt.IsZero()) || b.Status == statusDeferred {
			// Closed at an unknown time, or parked.
			continue
		}
		inProgress := b.Status == StatusInProgress
		entered := false
		if inProgress && started(b, now) && b.StartedAt.After(since) {
			cycle.add(c, p.active(b.StartedAt, now), true)
			entered = true
		}
		if (inProgress || !b.Blocked) && !ready.IsZero() && ready.After(since) && !ready.After(now) {
			lead.add(c, p.active(ready, now), true)
			entered = true
		}
		if entered {
			s.Open++
		}
	}
	return &Model{
		params: p,
		now:    now,
		lead:   lead.chain("lead", dist.NewPrior(p.LeadPriorMinutes), p),
		cycle:  cycle.chain("cycle", dist.NewPrior(p.CyclePriorMinutes), p),
		sample: s,
	}, nil
}

// started reports whether b has a usable start by t: after its creation,
// not after t.
func started(b Bead, t time.Time) bool {
	return !b.StartedAt.IsZero() && !b.StartedAt.Before(b.CreatedAt) && !b.StartedAt.After(t)
}

// Sample returns what the model learned from.
func (m *Model) Sample() Sample { return m.sample }

// Sampler draws the remaining durations of one open bead. The forecaster
// makes one per bead and calls it once per simulation run.
type Sampler struct {
	lead, cycle *dist.Dist
	inProgress  bool
	started     bool    // in progress with a known start
	age         float64 // minutes since started_at when started, else since ready_at
}

// Sampler returns the sampler of an open bead, from the most specific nodes
// of its class's backoff chains. Its draws are conditioned on its age
// (ADR-2 §1): the time since started_at for a bead in progress, since
// ready_at for any other bead that is ready, and none for a blocked bead,
// whose lead time starts when its blockers close.
func (m *Model) Sampler(b Bead) Sampler {
	c := ClassOf(b)
	s := Sampler{lead: m.lead.node(c).d, cycle: m.cycle.node(c).d, inProgress: b.Status == StatusInProgress}
	since := time.Time{}
	switch {
	case s.inProgress && !b.StartedAt.IsZero():
		s.started, since = true, b.StartedAt
	case s.inProgress || !b.Blocked:
		since = b.readyAt()
	}
	if !since.IsZero() && m.now.After(since) {
		s.age = m.params.active(since, m.now)
	}
	return s
}

// Draw draws the rest of the bead's lead time, in minutes, split into the
// wait until an agent takes it up and the work that follows. A bead in
// progress has no wait: it draws the rest of its cycle time, or of its lead
// time when its start is unknown. Any other bead draws the rest of its lead
// time, of which a cycle time (at most all of it) is work. Measured lead
// times already hold the wait for a free agent, so only a configured agent
// limit makes the split matter.
func (s Sampler) Draw(r *rand.Rand) (queue, work float64) {
	switch {
	case s.started:
		return 0, s.cycle.SampleBeyond(r, s.age)
	case s.inProgress:
		return 0, s.lead.SampleBeyond(r, s.age)
	}
	lead := s.lead.SampleBeyond(r, s.age)
	work = math.Min(lead, s.cycle.Sample(r))
	return lead - work, work
}

// Estimate is the internal per-bead estimate. It feeds the optional
// write-back and is never shown.
type Estimate struct {
	ID, Repo string
	Class    Class
	Started  bool // in progress
	// Cycle is the full cycle time: what the write-back records.
	Cycle Quantiles
	// Lead is the full lead time, from ready to close.
	Lead Quantiles
	// Remaining is the time still to go at the bead's age: the rest of its
	// lead time, or of its cycle time when it is in progress.
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
// node share its Cycle and Lead; Remaining is drawn with a source seeded by
// the bead's ID.
func (m *Model) Estimate(b Bead) Estimate {
	c := ClassOf(b)
	s := m.Sampler(b)
	r := seeded(m.params.Seed, "remaining", b.Repo+"/"+b.ID)
	return Estimate{
		ID: b.ID, Repo: b.Repo, Class: c, Started: s.inProgress,
		Cycle: m.cycle.node(c).q,
		Lead:  m.lead.node(c).q,
		Remaining: quantiles(func() float64 {
			q, w := s.Draw(r)
			return q + w
		}),
	}
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

// series are the durations of one backoff level: completed ones, and
// censored ones that are still running.
type series struct{ events, censored []float64 }

func (s *series) add(m float64, censored bool) {
	if censored {
		s.censored = append(s.censored, m)
	} else {
		s.events = append(s.events, m)
	}
}

// observations collects durations per backoff level.
type observations struct {
	all     series
	repos   map[string]*series
	classes map[Class]*series
}

func newObservations() *observations {
	return &observations{repos: map[string]*series{}, classes: map[Class]*series{}}
}

func (o *observations) add(c Class, m float64, censored bool) {
	if o.repos[c.Repo] == nil {
		o.repos[c.Repo] = &series{}
	}
	if o.classes[c] == nil {
		o.classes[c] = &series{}
	}
	o.all.add(m, censored)
	o.repos[c.Repo].add(m, censored)
	o.classes[c].add(m, censored)
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
	// The tail cap counts censored durations too: a bead open for a month
	// shows that a month is possible.
	longest := append(append([]float64(nil), o.all.events...), o.all.censored...)
	root := dist.NewRoot(prior, longest, p.TailCapFactor)
	c := chain{
		all:     build("all", root.Censored(o.all.events, o.all.censored, p.PoolingStrength)),
		repos:   make(map[string]node, len(o.repos)),
		classes: make(map[Class]node, len(o.classes)),
	}
	for repo, s := range o.repos {
		c.repos[repo] = build("repo:"+repo, c.all.d.Censored(s.events, s.censored, p.PoolingStrength))
	}
	for cl, s := range o.classes {
		c.classes[cl] = build("class:"+cl.String(), c.repos[cl.Repo].d.Censored(s.events, s.censored, p.PoolingStrength))
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
