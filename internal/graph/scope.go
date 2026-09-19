package graph

import "sort"

// Scope returns the work a high-level issue stands for, without the issue
// itself, sorted by ID:
//
//   - its descendants through parent-child edges, open or closed: its own
//     breakdown;
//   - every blocker of the issue or of open work in scope, transitively, open
//     or closed, with the blocker's own descendants: upstream work it needed
//     or still waits on, although that work hangs off another parent.
//
// Blocking edges are followed only from open issues: what a closed issue
// waited on is history, not part of this issue's work. So the open issues in
// scope are exactly the work that still has to land. Cycles do not make it
// loop.
func (g *Graph) Scope(id string) []*Issue {
	root := g.issues[id]
	if root == nil {
		return nil
	}
	in := map[string]bool{id: true}
	var queue []*Issue // open issues whose blockers are still to be followed
	add := func(i *Issue) {
		if in[i.ID] {
			return
		}
		in[i.ID] = true
		if !i.Closed() {
			queue = append(queue, i)
		}
	}
	for _, d := range g.Descendants(id) {
		add(d)
	}
	if !root.Closed() {
		queue = append(queue, root)
	}
	for len(queue) > 0 {
		i := queue[0]
		queue = queue[1:]
		for _, b := range i.BlockedBy {
			if in[b] {
				continue
			}
			add(g.issues[b])
			for _, d := range g.Descendants(b) {
				add(d)
			}
		}
	}
	delete(in, id)
	out := make([]*Issue, 0, len(in))
	for sid := range in {
		out = append(out, g.issues[sid])
	}
	sort.Slice(out, func(a, b int) bool { return out[a].ID < out[b].ID })
	return out
}
