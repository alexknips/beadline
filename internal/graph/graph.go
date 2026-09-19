package graph

import (
	"encoding/json"
	"fmt"
	"sort"
	"time"
)

// Statuses with a meaning to beadline. Any other status (open, blocked,
// deferred, hooked, a custom one) counts as not closed.
const (
	StatusClosed     = "closed"
	StatusInProgress = "in_progress"
)

// Issue is one bead. Times are UTC; a zero time means "not set".
type Issue struct {
	ID          string
	Repo        string // name of the [[repos]] entry it was loaded from
	Title       string
	Description string
	Type        string // issue_type
	Status      string
	Priority    int
	Assignee    string
	Labels      []string
	Metadata    map[string]json.RawMessage

	CreatedAt  time.Time
	UpdatedAt  time.Time
	StartedAt  time.Time
	ClosedAt   time.Time
	DueAt      time.Time
	DeferUntil time.Time

	// EstimatedMinutes is the typed estimate, nil when none was given.
	// beadline learns its own and writes them back; it never trusts these.
	EstimatedMinutes *int

	// Classification by the configured conventions.
	HighLevel bool     // its type is one of conventions.high_level_types
	HumanGate bool     // it waits on a human (title pattern or metadata key)
	Goals     []string // goal IDs from labels, sorted

	// Edges within the graph, sorted by ID. Parent-child edges give the
	// hierarchy; blocking edges give order: every BlockedBy issue must close
	// before this one can start.
	Parents   []string
	Children  []string
	BlockedBy []string
	Blocks    []string
}

// Closed reports whether the issue is done.
func (i *Issue) Closed() bool { return i.Status == StatusClosed }

// EdgeKind distinguishes the two kinds of edge beadline uses.
type EdgeKind string

const (
	ParentChild EdgeKind = "parent-child"
	Blocking    EdgeKind = "blocks"
)

// Graph is a set of issues from one or more repositories and the edges
// between them.
type Graph struct {
	issues map[string]*Issue
	order  []string
}

// New returns an empty graph.
func New() *Graph {
	return &Graph{issues: map[string]*Issue{}}
}

// Add inserts an issue. IDs must be unique across all repositories.
func (g *Graph) Add(i *Issue) error {
	if i.ID == "" {
		return fmt.Errorf("graph: issue without an ID")
	}
	if prev, ok := g.issues[i.ID]; ok {
		return fmt.Errorf("graph: duplicate issue %s (repos %s and %s)", i.ID, prev.Repo, i.Repo)
	}
	g.issues[i.ID] = i
	g.order = append(g.order, i.ID)
	return nil
}

// Link adds an edge. For ParentChild, from is the child and to the parent;
// for Blocking, from is blocked by to. Both issues must exist. Adding an
// edge twice is a no-op.
func (g *Graph) Link(kind EdgeKind, from, to string) error {
	f, t := g.issues[from], g.issues[to]
	if f == nil || t == nil {
		return fmt.Errorf("graph: %s edge %s -> %s: unknown issue", kind, from, to)
	}
	switch kind {
	case ParentChild:
		f.Parents = insert(f.Parents, to)
		t.Children = insert(t.Children, from)
	case Blocking:
		f.BlockedBy = insert(f.BlockedBy, to)
		t.Blocks = insert(t.Blocks, from)
	default:
		return fmt.Errorf("graph: unknown edge kind %q", kind)
	}
	return nil
}

// insert adds id to a sorted slice unless it is already there.
func insert(ids []string, id string) []string {
	n := sort.SearchStrings(ids, id)
	if n < len(ids) && ids[n] == id {
		return ids
	}
	ids = append(ids, "")
	copy(ids[n+1:], ids[n:])
	ids[n] = id
	return ids
}

// Issue returns the issue with this ID, or nil.
func (g *Graph) Issue(id string) *Issue { return g.issues[id] }

// Len is the number of issues.
func (g *Graph) Len() int { return len(g.order) }

// Issues returns every issue in the order they were added.
func (g *Graph) Issues() []*Issue {
	out := make([]*Issue, len(g.order))
	for n, id := range g.order {
		out[n] = g.issues[id]
	}
	return out
}

// HighLevel returns the high-level issues in the order they were added.
func (g *Graph) HighLevel() []*Issue {
	var out []*Issue
	for _, id := range g.order {
		if i := g.issues[id]; i.HighLevel {
			out = append(out, i)
		}
	}
	return out
}

// Descendants returns every issue below id through parent-child edges,
// without id itself, in depth-first order with children visited by ID.
// Parent-child cycles do not make it loop.
func (g *Graph) Descendants(id string) []*Issue {
	var out []*Issue
	seen := map[string]bool{id: true}
	var walk func(string)
	walk = func(id string) {
		for _, c := range g.issues[id].Children {
			if !seen[c] {
				seen[c] = true
				out = append(out, g.issues[c])
				walk(c)
			}
		}
	}
	if g.issues[id] != nil {
		walk(id)
	}
	return out
}

// Goal is a cross-repo goal: every issue whose labels name it.
type Goal struct {
	ID string
	// Issue is the goal's own bead when it was loaded, else nil (goal beads
	// often live in a coordination repo that is not forecast).
	Issue *Issue
	// Members are the labelled issues in the order they were added.
	Members []*Issue
}

// Goals returns every goal named by a label, sorted by ID.
func (g *Graph) Goals() []Goal {
	byID := map[string]*Goal{}
	var ids []string
	for _, id := range g.order {
		i := g.issues[id]
		for _, gid := range i.Goals {
			goal := byID[gid]
			if goal == nil {
				goal = &Goal{ID: gid, Issue: g.issues[gid]}
				byID[gid] = goal
				ids = append(ids, gid)
			}
			goal.Members = append(goal.Members, i)
		}
	}
	sort.Strings(ids)
	out := make([]Goal, len(ids))
	for n, gid := range ids {
		out[n] = *byID[gid]
	}
	return out
}

// Cycle is a set of issues that depend on each other through edges of one
// kind: a strongly connected component with more than one issue, or an
// issue with an edge to itself.
type Cycle struct {
	Kind EdgeKind
	IDs  []string // sorted
	// Open reports whether any member is not closed. A blocking cycle with
	// open members can never be scheduled; a closed one is history.
	Open bool
}

// Cycles returns every blocking cycle, then every parent-child cycle, each
// group sorted by first ID.
func (g *Graph) Cycles() []Cycle {
	var out []Cycle
	for _, kind := range []EdgeKind{Blocking, ParentChild} {
		next := func(i *Issue) []string { return i.BlockedBy }
		if kind == ParentChild {
			next = func(i *Issue) []string { return i.Parents }
		}
		for _, scc := range g.components(next) {
			if len(scc) == 1 && !contains(next(g.issues[scc[0]]), scc[0]) {
				continue
			}
			sort.Strings(scc)
			c := Cycle{Kind: kind, IDs: scc}
			for _, id := range scc {
				if !g.issues[id].Closed() {
					c.Open = true
				}
			}
			out = append(out, c)
		}
	}
	sort.SliceStable(out, func(a, b int) bool {
		if out[a].Kind != out[b].Kind {
			return out[a].Kind == Blocking
		}
		return out[a].IDs[0] < out[b].IDs[0]
	})
	return out
}

// components returns the strongly connected components of the graph whose
// edges are given by next (Tarjan's algorithm).
func (g *Graph) components(next func(*Issue) []string) [][]string {
	index := map[string]int{}
	low := map[string]int{}
	onStack := map[string]bool{}
	var stack []string
	var out [][]string

	var visit func(string)
	visit = func(v string) {
		index[v] = len(index)
		low[v] = index[v]
		stack = append(stack, v)
		onStack[v] = true
		for _, w := range next(g.issues[v]) {
			if _, seen := index[w]; !seen {
				visit(w)
				low[v] = min(low[v], low[w])
			} else if onStack[w] {
				low[v] = min(low[v], index[w])
			}
		}
		if low[v] == index[v] {
			var scc []string
			for {
				w := stack[len(stack)-1]
				stack = stack[:len(stack)-1]
				onStack[w] = false
				scc = append(scc, w)
				if w == v {
					break
				}
			}
			out = append(out, scc)
		}
	}
	for _, id := range g.order {
		if _, seen := index[id]; !seen {
			visit(id)
		}
	}
	return out
}

func contains(ids []string, id string) bool {
	for _, v := range ids {
		if v == id {
			return true
		}
	}
	return false
}
