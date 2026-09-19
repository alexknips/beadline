package graph

import "sort"

// Scope returns the work a high-level issue waits on, without the issue
// itself, sorted by ID:
//
//   - every descendant through parent-child edges, open or closed: the
//     issue's own breakdown, which is what "done %" counts against;
//   - every open blocker of the issue or of an open issue already in scope,
//     transitively, together with that blocker's open descendants: upstream
//     work that must land first even though it hangs off another parent.
//
// Closed blockers are satisfied, so they are not part of the remaining work
// and are left out. Cycles do not make it loop.
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
			blocker := g.issues[b]
			if blocker.Closed() || in[b] {
				continue
			}
			add(blocker)
			for _, d := range g.Descendants(b) {
				if !d.Closed() {
					add(d)
				}
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
