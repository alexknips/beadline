package calibrate

import (
	"encoding/json"
	"path/filepath"
	"regexp"
	"time"

	"github.com/alexknips/beadline/internal/graph"
)

// Levels of a pair.
const (
	LevelLeaf      = "leaf"       // a leaf bead's own close
	LevelHighLevel = "high_level" // a milestone, epic or goal
)

// Methods of a pair.
const (
	MethodModel    = "model"    // beadline's forecast
	MethodBaseline = "baseline" // repo throughput only, for comparison
)

// Outcomes of a pair.
const (
	OutcomeResolved = "resolved" // the target closed: the forecast is scored
	OutcomeOpen     = "open"     // still open: right-censored
	OutcomeVoid     = "void"     // cannot be scored; Pair.Void says why
)

// Reasons a pair is void.
const (
	VoidDescoped = "descoped" // closed without being delivered
	VoidParked   = "parked"   // still open and deferred: postponed by a decision
	VoidVanished = "vanished" // no longer in the exports
	VoidUndated  = "undated"  // closed with no close time
)

// statusDeferred is bd's status of postponed work.
const statusDeferred = "deferred"

// Pair is one forecast graded against what happened.
type Pair struct {
	Source string    `json:"source"` // snapshot file name, or "backtest"
	AsOf   time.Time `json:"as_of"`
	Method string    `json:"method"`
	Level  string    `json:"level"`
	ID     string    `json:"id"`
	Repo   string    `json:"repo,omitempty"`
	Goal   bool      `json:"goal,omitempty"`
	// Quantiles is the forecast: hours after AsOf.
	Quantiles Grid   `json:"quantile_hours"`
	Outcome   string `json:"outcome"`
	Void      string `json:"void,omitempty"`
	// ActualHours is the time from AsOf to the close, for a resolved pair.
	ActualHours *float64 `json:"actual_hours,omitempty"`
	// ElapsedHours is the time from AsOf to the data horizon: how long an
	// open target has been known to stay open.
	ElapsedHours float64 `json:"elapsed_hours"`
	// ScopeAdded and ScopeRemoved count, for a high-level target, the beads
	// its breakdown gained and lost since AsOf.
	ScopeAdded   int `json:"scope_added,omitempty"`
	ScopeRemoved int `json:"scope_removed,omitempty"`
}

// Held reports whether the target closed within quantile q: true or false
// when that is known, nil when it is not (the target is open and q has not
// passed yet, the pair is void, or the grid has no level q).
func (p *Pair) Held(q float64) *bool {
	v, ok := p.Quantiles.at(q)
	if !ok {
		return nil
	}
	switch p.Outcome {
	case OutcomeResolved:
		held := *p.ActualHours <= v
		return &held
	case OutcomeOpen:
		if p.ElapsedHours > v {
			held := false
			return &held
		}
	}
	return nil
}

// FixedScope reports whether a high-level target's breakdown is the same
// as at AsOf.
func (p *Pair) FixedScope() bool { return p.ScopeAdded == 0 && p.ScopeRemoved == 0 }

// Reality is the data a forecast is graded against: the graph the exports
// show now, and their horizon.
type Reality struct {
	Graph   *graph.Graph
	Horizon time.Time
	goals   map[string]graph.Goal
	bursts  map[burst]int
}

type burst struct {
	repo string
	at   time.Time
}

// BurstCloses is the number of closes in one repo within one minute that
// marks a bulk close: an administrative cleanup, not deliveries.
const BurstCloses = 8

// NewReality prepares g for grading as of horizon.
func NewReality(g *graph.Graph, horizon time.Time) *Reality {
	r := &Reality{Graph: g, Horizon: horizon, goals: map[string]graph.Goal{}, bursts: map[burst]int{}}
	for _, gl := range g.Goals() {
		r.goals[gl.ID] = gl
	}
	for _, i := range g.Issues() {
		if i.Closed() && !i.ClosedAt.IsZero() {
			r.bursts[burst{i.Repo, i.ClosedAt.Truncate(time.Minute)}]++
		}
	}
	return r
}

// descopedReason matches the start of a close reason that says the bead
// was not delivered.
var descopedReason = regexp.MustCompile(`(?i)^\s*(duplicate|dup of|supersed|won'?t (fix|do)|wontfix|obsolete|` +
	`(not|no longer) needed|not planned|moot|abandon|descop|out of scope|closing stale|bulk[- ]?close)`)

// Descoped reports whether a closed bead was closed without being
// delivered: its close reason starts by saying so (duplicate, superseded,
// won't fix, obsolete, not needed, descoped, abandoned ...), its
// gc.work_outcome metadata is no-op or abandoned, or it closed in a bulk
// close of BurstCloses or more beads of its repo in one minute.
func (r *Reality) Descoped(i *graph.Issue) bool {
	if descopedReason.MatchString(i.CloseReason) {
		return true
	}
	if raw, ok := i.Metadata["gc.work_outcome"]; ok {
		var outcome string
		if json.Unmarshal(raw, &outcome) == nil && (outcome == "no-op" || outcome == "abandoned") {
			return true
		}
	}
	return !i.ClosedAt.IsZero() && r.bursts[burst{i.Repo, i.ClosedAt.Truncate(time.Minute)}] >= BurstCloses
}

// grade sets the outcome of p from reality. breakdown is a high-level
// target's breakdown at AsOf, nil for a leaf.
func (r *Reality) grade(p *Pair, breakdownThen []string) {
	p.ElapsedHours = max(0, r.Horizon.Sub(p.AsOf).Hours())
	p.Outcome, p.Void, p.ActualHours = OutcomeOpen, "", nil
	p.ScopeAdded, p.ScopeRemoved = 0, 0

	var closedAt time.Time
	var now []string // the breakdown now
	var issue *graph.Issue
	if gl, ok := r.goals[p.ID]; ok && p.Goal {
		now = goalBreakdown(r.Graph, gl)
		issue = gl.Issue
		if issue == nil {
			// A goal without its bead closes with its last bead.
			all := len(now) > 0
			for _, id := range now {
				i := r.Graph.Issue(id)
				if !i.Closed() || i.ClosedAt.IsZero() {
					all = false
					break
				}
				if i.ClosedAt.After(closedAt) {
					closedAt = i.ClosedAt
				}
			}
			if !all {
				closedAt = time.Time{}
			}
		}
	} else {
		issue = r.Graph.Issue(p.ID)
		if issue == nil {
			p.Outcome, p.Void = OutcomeVoid, VoidVanished
			return
		}
		if p.Level == LevelHighLevel {
			now = breakdown(r.Graph, p.ID)
		}
	}
	if breakdownThen != nil {
		then := map[string]bool{}
		for _, id := range breakdownThen {
			then[id] = true
		}
		for _, id := range now {
			if then[id] {
				delete(then, id)
			} else {
				p.ScopeAdded++
			}
		}
		p.ScopeRemoved = len(then)
	}

	if issue != nil && !issue.Closed() && (issue.Status == statusDeferred || issue.DeferUntil.After(r.Horizon)) {
		// Like a descoped close, a deferral is a decision, not a delivery
		// the model could have foreseen. It also covers beads imported with
		// a backdated created_at, which look open long before they existed.
		p.Outcome, p.Void = OutcomeVoid, VoidParked
		return
	}
	if issue != nil && issue.Closed() {
		switch {
		case r.Descoped(issue):
			p.Outcome, p.Void = OutcomeVoid, VoidDescoped
			return
		case issue.ClosedAt.IsZero():
			p.Outcome, p.Void = OutcomeVoid, VoidUndated
			return
		}
		closedAt = issue.ClosedAt
	}
	if !closedAt.IsZero() {
		// A close before AsOf means the forecast saw stale data: it held.
		actual := max(0, closedAt.Sub(p.AsOf).Hours())
		p.Outcome, p.ActualHours = OutcomeResolved, &actual
	}
}

// Grade grades every forecast in the snapshots against reality: leaves,
// then the items that carry quantiles.
func Grade(snaps []Loaded, r *Reality) []Pair {
	var pairs []Pair
	for _, l := range snaps {
		s := l.Snapshot
		source := filepath.Base(l.Path)
		for _, t := range s.Leaves {
			if len(t.Quantiles) == 0 {
				continue
			}
			p := Pair{Source: source, AsOf: s.AsOf, Method: MethodModel, Level: LevelLeaf, ID: t.ID, Repo: t.Repo, Quantiles: t.Quantiles}
			r.grade(&p, nil)
			pairs = append(pairs, p)
		}
		for _, t := range s.Items {
			if len(t.Quantiles) == 0 {
				continue
			}
			p := Pair{Source: source, AsOf: s.AsOf, Method: MethodModel, Level: LevelHighLevel, ID: t.ID, Repo: t.Repo,
				Goal: t.Goal, Quantiles: t.Quantiles}
			then := t.DescendantIDs
			if then == nil {
				then = []string{}
			}
			r.grade(&p, then)
			pairs = append(pairs, p)
		}
	}
	return pairs
}
