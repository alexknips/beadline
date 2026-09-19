package forecast

import (
	"math"
	"math/rand/v2"
)

// sim is one simulated schedule of a plan. Times are minutes from now.
//
// Every run first draws the durations of all beads in plan order, then
// schedules them. The draws therefore do not depend on the schedule, and
// two plans that differ only in concurrency see the same durations for the
// same seed (common random numbers).
//
// A bead becomes ready when everything it waits for is done and its
// defer_until has passed. A container is then done at once. A gate is done
// after its human wait. A work bead becomes eligible after its queue
// latency and starts when its repository has a free agent; among eligible
// beads the lowest rank (priority, then age) goes first. Beads already
// under way hold their agent from the start.
type sim struct {
	p       *plan
	queue   []float64 // queue latency of work
	own     []float64 // work time, or human wait of a gate
	pending []int32   // open predecessors not yet done
	start   []float64 // when an agent took up a work bead
	finish  []float64
	// via is the predecessor whose finish made a bead ready, -1 when none
	// did: the bead was ready (or under way) from the start.
	via     []int32
	running []int
	waiting []rankHeap // eligible work per repository
	dirty   []bool
	touched []int
	events  eventHeap
}

func newSim(p *plan) *sim {
	n := len(p.nodes)
	return &sim{
		p:       p,
		queue:   make([]float64, n),
		own:     make([]float64, n),
		pending: make([]int32, n),
		start:   make([]float64, n),
		finish:  make([]float64, n),
		via:     make([]int32, n),
		running: make([]int, len(p.caps)),
		waiting: make([]rankHeap, len(p.caps)),
		dirty:   make([]bool, len(p.caps)),
	}
}

type evKind uint8

const (
	evEligible evKind = iota // a work bead's queue latency is over
	evDone                   // a bead is done
)

type event struct {
	t    float64
	seq  uint64
	kind evKind
	node int32
}

// run simulates one schedule with r.
func (s *sim) run(r *rand.Rand) {
	p := s.p
	for k := range p.nodes {
		n := &p.nodes[k]
		s.queue[k], s.own[k] = 0, 0
		switch {
		case n.gate:
			s.own[k] = duration(n.wait(r))
		case !n.container:
			s.queue[k] = duration(n.agent.Queue(r))
			s.own[k] = duration(n.agent.Work(r))
		}
	}
	for repo := range s.running {
		s.running[repo] = 0
		s.waiting[repo] = s.waiting[repo][:0]
	}
	s.events.reset()

	for k := range p.nodes {
		n := &p.nodes[k]
		s.start[k], s.finish[k] = math.Inf(1), math.Inf(1)
		s.via[k] = -1
		s.pending[k] = int32(len(n.preds))
		switch {
		case n.started && n.work():
			s.running[n.repo]++
			s.start[k] = 0
			s.events.push(s.own[k], evDone, int32(k))
		case n.started:
			s.events.push(s.own[k], evDone, int32(k))
		case len(n.preds) == 0:
			s.ready(int32(k), 0)
		}
	}

	for s.events.len() > 0 {
		t := s.events.min().t
		for s.events.len() > 0 && s.events.min().t == t {
			e := s.events.pop()
			switch e.kind {
			case evEligible:
				n := &p.nodes[e.node]
				s.waiting[n.repo].push(e.node, p)
				s.mark(n.repo)
			case evDone:
				s.done(e.node, t)
			}
		}
		for _, repo := range s.touched {
			s.dirty[repo] = false
			s.dispatch(repo, t)
		}
		s.touched = s.touched[:0]
	}
}

// ready handles bead k becoming ready at t.
func (s *sim) ready(k int32, t float64) {
	n := &s.p.nodes[k]
	t = math.Max(t, n.notBefore)
	if n.work() {
		s.events.push(t+s.queue[k], evEligible, k)
		return
	}
	s.events.push(t+s.own[k], evDone, k)
}

func (s *sim) done(k int32, t float64) {
	n := &s.p.nodes[k]
	s.finish[k] = t
	if n.work() {
		s.running[n.repo]--
		s.mark(n.repo)
	}
	for _, succ := range n.succs {
		if s.pending[succ]--; s.pending[succ] == 0 {
			s.via[succ] = k
			s.ready(succ, t)
		}
	}
}

// mark schedules a dispatch of repo once every event at the current time
// is in, so that beads that become eligible together compete by rank.
func (s *sim) mark(repo int) {
	if !s.dirty[repo] {
		s.dirty[repo] = true
		s.touched = append(s.touched, repo)
	}
}

// dispatch starts eligible work in repo while it has free agents.
func (s *sim) dispatch(repo int, t float64) {
	limit := s.p.caps[repo]
	for s.waiting[repo].len() > 0 && (limit == 0 || s.running[repo] < limit) {
		k := s.waiting[repo].pop(s.p)
		s.running[repo]++
		s.start[k] = t
		s.events.push(t+s.own[k], evDone, k)
	}
}

// last returns the latest finish among nodes.
func (s *sim) last(nodes []int32) float64 {
	f := 0.0
	for _, k := range nodes {
		f = math.Max(f, s.finish[k])
	}
	return f
}

// end returns the node among nodes that finishes last, the first of them
// on a tie.
func (s *sim) end(nodes []int32) int32 {
	e := nodes[0]
	for _, k := range nodes[1:] {
		if s.finish[k] > s.finish[e] {
			e = k
		}
	}
	return e
}

// point returns the finish of nodes in this run, split along the chain
// that set it.
func (s *sim) point(nodes []int32) Point {
	var agent, human float64
	for k := s.end(nodes); k >= 0; k = s.via[k] {
		n := &s.p.nodes[k]
		start := 0.0
		if v := s.via[k]; v >= 0 {
			start = s.finish[v]
		}
		span := s.finish[k] - start
		wait := span
		switch {
		case n.gate:
		case n.started:
			wait = 0
		default:
			// Only waiting for defer_until is human time: a person chose it.
			wait = math.Min(span, math.Max(0, n.notBefore-start))
		}
		human += wait
		agent += span - wait
	}
	return Point{At: at(s.p.now, s.last(nodes)), AgentHours: hours(agent), HumanHours: hours(human)}
}

// chain returns the IDs of the beads with time of their own on the chain
// that set the finish of nodes, first to last. Containers are left out.
func (s *sim) chain(nodes []int32) []string {
	var ids []string
	for k := s.end(nodes); k >= 0; k = s.via[k] {
		if n := &s.p.nodes[k]; !n.container || n.gate {
			ids = append(ids, n.issue.ID)
		}
	}
	for a, b := 0, len(ids)-1; a < b; a, b = a+1, b-1 {
		ids[a], ids[b] = ids[b], ids[a]
	}
	return ids
}

// duration cleans a draw: never negative, never NaN, at most a century.
func duration(m float64) float64 {
	if !(m > 0) {
		return 0
	}
	return math.Min(m, maxMinutes)
}

// eventHeap is a binary min-heap of events by time, then insertion order.
type eventHeap struct {
	ev  []event
	seq uint64
}

func (h *eventHeap) reset()     { h.ev, h.seq = h.ev[:0], 0 }
func (h *eventHeap) len() int   { return len(h.ev) }
func (h *eventHeap) min() event { return h.ev[0] }

func (h *eventHeap) less(a, b int) bool {
	if h.ev[a].t != h.ev[b].t {
		return h.ev[a].t < h.ev[b].t
	}
	return h.ev[a].seq < h.ev[b].seq
}

func (h *eventHeap) push(t float64, kind evKind, node int32) {
	h.ev = append(h.ev, event{t: t, seq: h.seq, kind: kind, node: node})
	h.seq++
	for i := len(h.ev) - 1; i > 0; {
		parent := (i - 1) / 2
		if !h.less(i, parent) {
			break
		}
		h.ev[i], h.ev[parent] = h.ev[parent], h.ev[i]
		i = parent
	}
}

func (h *eventHeap) pop() event {
	top := h.ev[0]
	last := len(h.ev) - 1
	h.ev[0] = h.ev[last]
	h.ev = h.ev[:last]
	for i := 0; ; {
		l, r, m := 2*i+1, 2*i+2, i
		if l < last && h.less(l, m) {
			m = l
		}
		if r < last && h.less(r, m) {
			m = r
		}
		if m == i {
			break
		}
		h.ev[i], h.ev[m] = h.ev[m], h.ev[i]
		i = m
	}
	return top
}

// rankHeap is a binary min-heap of work beads by dispatch rank.
type rankHeap []int32

func (h rankHeap) len() int { return len(h) }

func (h *rankHeap) push(k int32, p *plan) {
	*h = append(*h, k)
	a := *h
	for i := len(a) - 1; i > 0; {
		parent := (i - 1) / 2
		if p.nodes[a[i]].rank >= p.nodes[a[parent]].rank {
			break
		}
		a[i], a[parent] = a[parent], a[i]
		i = parent
	}
}

func (h *rankHeap) pop(p *plan) int32 {
	a := *h
	top := a[0]
	last := len(a) - 1
	a[0] = a[last]
	a = a[:last]
	for i := 0; ; {
		l, r, m := 2*i+1, 2*i+2, i
		if l < last && p.nodes[a[l]].rank < p.nodes[a[m]].rank {
			m = l
		}
		if r < last && p.nodes[a[r]].rank < p.nodes[a[m]].rank {
			m = r
		}
		if m == i {
			break
		}
		a[i], a[m] = a[m], a[i]
		i = m
	}
	*h = a
	return top
}
