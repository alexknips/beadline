package forecast

import (
	"math"
	"sort"
	"time"

	"github.com/alexknips/beadline/internal/graph"
)

// statusDeferred is bd's status of a parked bead.
const statusDeferred = "deferred"

// node is an open bead the simulation schedules.
type node struct {
	issue *graph.Issue
	// container: the bead has children, is high-level or is a goal's bead.
	// It has no work of its own and is done when its open children and
	// blockers are.
	container bool
	// gate: the bead waits on a person. Its own time is a human wait, which
	// holds no agent.
	gate bool
	// started: a leaf already under way. It ignores its blockers.
	started   bool
	repo      int     // index into plan.caps
	notBefore float64 // minutes until defer_until; 0 when none
	rank      int     // dispatch order among eligible work, lowest first
	preds     []int32 // open beads it waits for
	succs     []int32
	agent     Sampler // work only
	wait      Draw    // gates only
}

// work reports whether an agent executes the bead.
func (n *node) work() bool { return !n.container && !n.gate }

// plan is the part of a forecast that does not change between runs.
type plan struct {
	g   *graph.Graph
	o   *Options
	now time.Time

	nodes []node
	index map[string]int32 // schedulable open beads
	// parked beads are deferred with no future defer_until: nobody works
	// them. stuck beads are open and not parked but can never start: they
	// are in a blocking cycle or wait on a parked bead.
	parked, stuck map[string]bool

	repos []string
	caps  []int // per repo; 0 = unlimited

	goals     map[string]bool // beads that a goal label names
	ancestors map[string]map[string]bool
	blockers  map[string][]string
}

func newPlan(g *graph.Graph, o *Options) *plan {
	p := &plan{
		g: g, o: o, now: o.Now,
		index:     map[string]int32{},
		parked:    map[string]bool{},
		stuck:     map[string]bool{},
		goals:     map[string]bool{},
		ancestors: map[string]map[string]bool{},
		blockers:  map[string][]string{},
	}
	for _, goal := range g.Goals() {
		p.goals[goal.ID] = true
	}

	var open []*graph.Issue
	for _, i := range g.Issues() {
		if i.Closed() {
			continue
		}
		open = append(open, i)
		if i.Status == statusDeferred && !i.DeferUntil.After(p.now) {
			p.parked[i.ID] = true
		}
	}

	// Kahn's algorithm over the open beads: a bead is schedulable once every
	// bead it waits for is. Parked beads never are, so neither is anything
	// that waits on them or on a cycle.
	preds := map[string][]string{}
	succs := map[string][]string{}
	indeg := map[string]int{}
	for _, i := range open {
		for _, id := range p.waitsFor(i) {
			preds[i.ID] = append(preds[i.ID], id)
			succs[id] = append(succs[id], i.ID)
		}
		indeg[i.ID] = len(preds[i.ID])
	}
	var queue []string
	for _, i := range open {
		if indeg[i.ID] == 0 && !p.parked[i.ID] {
			queue = append(queue, i.ID)
		}
	}
	schedulable := map[string]bool{}
	for len(queue) > 0 {
		id := queue[0]
		queue = queue[1:]
		schedulable[id] = true
		for _, s := range succs[id] {
			if indeg[s]--; indeg[s] == 0 && !p.parked[s] {
				queue = append(queue, s)
			}
		}
	}

	repoIndex := map[string]int{}
	repo := func(name string) int {
		if n, ok := repoIndex[name]; ok {
			return n
		}
		repoIndex[name] = len(p.repos)
		p.repos = append(p.repos, name)
		p.caps = append(p.caps, max(0, o.Concurrency[name]))
		return repoIndex[name]
	}
	names := make([]string, 0, len(o.Concurrency))
	for name := range o.Concurrency {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		repo(name)
	}

	for _, i := range open {
		if !schedulable[i.ID] {
			if !p.parked[i.ID] {
				p.stuck[i.ID] = true
			}
			continue
		}
		n := node{
			issue:     i,
			container: p.container(i),
			gate:      i.HumanGate,
			repo:      repo(i.Repo),
		}
		n.started = !n.container && i.Status == graph.StatusInProgress
		if i.DeferUntil.After(p.now) {
			n.notBefore = minutes(i.DeferUntil.Sub(p.now))
		}
		switch {
		case n.gate:
			var waited float64
			if len(preds[i.ID]) == 0 || n.started {
				waited = math.Max(0, minutes(p.now.Sub(p.readyAt(i))))
			}
			n.wait = o.Human(i, waited)
		case !n.container:
			n.agent = o.Agent(i)
		}
		p.index[i.ID] = int32(len(p.nodes))
		p.nodes = append(p.nodes, n)
	}
	for k := range p.nodes {
		n := &p.nodes[k]
		for _, id := range preds[n.issue.ID] {
			pr := p.index[id]
			n.preds = append(n.preds, pr)
			p.nodes[pr].succs = append(p.nodes[pr].succs, int32(k))
		}
	}

	var work []int
	for k := range p.nodes {
		if p.nodes[k].work() {
			work = append(work, k)
		}
	}
	sort.SliceStable(work, func(a, b int) bool {
		x, y := p.nodes[work[a]].issue, p.nodes[work[b]].issue
		if x.Priority != y.Priority {
			return x.Priority < y.Priority
		}
		if !x.CreatedAt.Equal(y.CreatedAt) {
			return x.CreatedAt.Before(y.CreatedAt)
		}
		return x.ID < y.ID
	})
	for r, k := range work {
		p.nodes[k].rank = r
	}
	return p
}

// isContainer reports whether a bead is done when its work is, with no work
// of its own: it has children or is high-level.
func isContainer(i *graph.Issue) bool { return len(i.Children) > 0 || i.HighLevel }

// container is isContainer, and also true for a goal's own bead: its work
// is linked by labels, and nobody executes the bead itself.
func (p *plan) container(i *graph.Issue) bool { return isContainer(i) || p.goals[i.ID] }

// waitsFor returns the open beads an open bead waits for, sorted: its open
// children if it is a container, and its open blockers unless it is a leaf
// already under way. A parked child is left out: an epic's forecast covers
// the work someone intends to do. A parked blocker is not: nothing can
// start behind it.
func (p *plan) waitsFor(i *graph.Issue) []string {
	set := map[string]bool{}
	container := p.container(i)
	if container {
		for _, c := range i.Children {
			if !p.g.Issue(c).Closed() && !p.parked[c] {
				set[c] = true
			}
		}
	}
	if container || i.Status != graph.StatusInProgress {
		for _, b := range p.blockersOf(i) {
			if !p.g.Issue(b).Closed() {
				set[b] = true
			}
		}
	}
	out := make([]string, 0, len(set))
	for id := range set {
		out = append(out, id)
	}
	sort.Strings(out)
	return out
}

// blockersOf returns every bead that blocks i, sorted and of any status:
// its own blockers and those of its ancestors, as bd does not offer the
// children of a blocked epic as ready work. An ancestor's blocker that sits
// under that same ancestor is not inherited: an epic blocked by its own
// child waits for the child, it does not stop it.
func (p *plan) blockersOf(i *graph.Issue) []string {
	if b, ok := p.blockers[i.ID]; ok {
		return b
	}
	set := map[string]bool{}
	for _, b := range i.BlockedBy {
		set[b] = true
	}
	for a := range p.ancestorsOf(i.ID) {
		for _, b := range p.g.Issue(a).BlockedBy {
			if b != a && b != i.ID && !p.ancestorsOf(b)[a] {
				set[b] = true
			}
		}
	}
	out := make([]string, 0, len(set))
	for id := range set {
		out = append(out, id)
	}
	sort.Strings(out)
	p.blockers[i.ID] = out
	return out
}

// ancestorsOf returns every bead above id through parent-child edges.
func (p *plan) ancestorsOf(id string) map[string]bool {
	if a, ok := p.ancestors[id]; ok {
		return a
	}
	set := map[string]bool{}
	stack := append([]string(nil), p.g.Issue(id).Parents...)
	for len(stack) > 0 {
		a := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		if set[a] || a == id {
			continue
		}
		set[a] = true
		stack = append(stack, p.g.Issue(a).Parents...)
	}
	p.ancestors[id] = set
	return set
}

// readyAt returns when a bead stopped waiting for others: its creation, or
// the latest close among its blockers and, for a container, its children.
func (p *plan) readyAt(i *graph.Issue) time.Time {
	t := i.CreatedAt
	later := func(id string) {
		if c := p.g.Issue(id).ClosedAt; c.After(t) && !c.After(p.now) {
			t = c
		}
	}
	for _, b := range p.blockersOf(i) {
		later(b)
	}
	if p.container(i) {
		for _, c := range i.Children {
			later(c)
		}
	}
	return t
}

// leaves returns the schedulable open beads that are leaves, in plan (load)
// order: no open child, not high-level, not a goal's own bead. A bead whose
// children are all closed is one: it still has to be closed.
func (p *plan) leaves() []int32 {
	var out []int32
	for k := range p.nodes {
		i := p.nodes[k].issue
		if i.HighLevel || p.goals[i.ID] {
			continue
		}
		leaf := true
		for _, c := range i.Children {
			if !p.g.Issue(c).Closed() {
				leaf = false
				break
			}
		}
		if leaf {
			out = append(out, int32(k))
		}
	}
	return out
}

// itemPlan is one roadmap item before simulation.
type itemPlan struct {
	out   Item
	goal  bool
	nodes []int32 // schedulable open beads of the scope and the roots
}

// items returns the open high-level beads in load order, then the goals
// with open work by ID.
func (p *plan) items() []itemPlan {
	var out []itemPlan
	for _, i := range p.g.HighLevel() {
		if i.Closed() {
			continue
		}
		it := p.item(i, nil)
		it.out.ID, it.out.Title, it.out.Repo, it.out.Type = i.ID, i.Title, i.Repo, i.Type
		it.out.Goals = i.Goals
		out = append(out, it)
	}
	for _, goal := range p.g.Goals() {
		root := goal.Issue
		if root != nil && root.Closed() {
			continue
		}
		var members []*graph.Issue
		for _, m := range goal.Members {
			if m != root {
				members = append(members, m)
			}
		}
		it := p.item(root, members)
		if root == nil && len(it.out.Remaining) == 0 {
			continue
		}
		it.goal = true
		it.out.ID = goal.ID
		if root != nil {
			it.out.Title, it.out.Repo, it.out.Type = root.Title, root.Repo, root.Type
		}
		out = append(out, it)
	}
	return out
}

// item plans the item rooted at root (nil for a goal whose bead is not
// loaded) with extra members.
func (p *plan) item(root *graph.Issue, members []*graph.Issue) itemPlan {
	var it itemPlan
	scope := p.scope(root, members)
	var outside []string
	seenOutside := map[string]bool{}
	addOutside := func(i *graph.Issue) {
		for _, id := range p.o.Outside[i.ID] {
			if !seenOutside[id] {
				seenOutside[id] = true
				outside = append(outside, id)
			}
		}
	}
	stuck := false
	scheduled := false
	for _, i := range scope {
		it.out.Total++
		if i.Closed() {
			it.out.Closed++
			continue
		}
		it.out.Remaining = append(it.out.Remaining, i.ID)
		addOutside(i)
		switch {
		case p.parked[i.ID]:
			it.out.Deferred = append(it.out.Deferred, i.ID)
		case p.stuck[i.ID]:
			it.out.Stuck = append(it.out.Stuck, i.ID)
			stuck = true
		default:
			it.nodes = append(it.nodes, p.index[i.ID])
			scheduled = true
		}
	}
	if root != nil {
		addOutside(root)
		if n, ok := p.index[root.ID]; ok {
			it.nodes = append(it.nodes, n)
		}
		stuck = stuck || p.stuck[root.ID]
		if !root.DueAt.IsZero() {
			due := root.DueAt
			it.out.DueAt = &due
		}
	}
	if it.out.Total > 0 {
		it.out.DonePct = math.Round(float64(it.out.Closed)/float64(it.out.Total)*1000) / 10
	}
	sort.Strings(it.out.Remaining)
	sort.Strings(it.out.Deferred)
	sort.Strings(it.out.Stuck)
	sort.Strings(outside)
	it.out.Outside = outside
	if it.out.Remaining == nil {
		it.out.Remaining = []string{}
	}

	switch {
	case root != nil && (root.Status == statusDeferred || root.DeferUntil.After(p.now)):
		it.out.Status = StatusDeferred
	case len(it.out.Remaining) > 0 && len(it.out.Deferred) == len(it.out.Remaining):
		// All that is left is parked.
		it.out.Status = StatusDeferred
	case it.out.Total == 0 && len(outside) > 0:
		it.out.Status = StatusBlockedOutside
	case it.out.Total == 0:
		it.out.Status = StatusNotPlanned
	case len(it.out.Remaining) == 0:
		it.out.Status = StatusReadyToClose
	case stuck:
		it.out.Status = StatusStalled
	case len(outside) > 0:
		it.out.Status = StatusBlockedOutside
	default:
		it.out.Status = StatusForecast
	}
	if !it.out.Status.Dated() || !scheduled {
		it.nodes = nil
	}
	return it
}

// scope returns the beads an item waits for, in discovery order, without
// the root. It follows the rule of the roadmap's scope, so counts and
// remaining sets agree:
//
//   - every descendant of the root and of the members through parent-child
//     edges, open or closed: the item's own breakdown, which done % counts;
//     for a goal, the members themselves too;
//   - every open blocker of the root or of an open bead already in scope,
//     transitively, with that blocker's open descendants: upstream work
//     that must land first even though it hangs off another parent.
//
// Closed blockers are satisfied and left out. Related, tracks and
// discovered-from links never enter a scope: the loader drops them. The
// blockers a bead inherits from its ancestors (blockersOf) still hold it
// back in the simulation, so they move the dates, but they are only in the
// scope when one of these rules brings them in.
func (p *plan) scope(root *graph.Issue, members []*graph.Issue) []*graph.Issue {
	seen := map[string]bool{}
	var out, queue []*graph.Issue
	add := func(i *graph.Issue) {
		if seen[i.ID] {
			return
		}
		seen[i.ID] = true
		out = append(out, i)
		if !i.Closed() {
			queue = append(queue, i)
		}
	}
	if root != nil {
		seen[root.ID] = true
		for _, d := range p.g.Descendants(root.ID) {
			add(d)
		}
		if !root.Closed() {
			queue = append(queue, root)
		}
	}
	for _, m := range members {
		add(m)
		for _, d := range p.g.Descendants(m.ID) {
			add(d)
		}
	}
	for len(queue) > 0 {
		i := queue[0]
		queue = queue[1:]
		for _, id := range i.BlockedBy {
			b := p.g.Issue(id)
			if b.Closed() || seen[id] {
				continue
			}
			add(b)
			for _, d := range p.g.Descendants(id) {
				if !d.Closed() {
					add(d)
				}
			}
		}
	}
	return out
}

// concurrency returns the agents per repository as simulated.
func (p *plan) concurrency() map[string]int {
	out := make(map[string]int, len(p.repos))
	for r, name := range p.repos {
		out[name] = p.caps[r]
	}
	return out
}
