package graph

import (
	"reflect"
	"testing"
)

// build makes a graph from issues given as "id" or "id:closed" and edges
// given as [kind, from, to].
func build(t *testing.T, ids []string, edges [][3]string) *Graph {
	t.Helper()
	g := New()
	for _, id := range ids {
		status := "open"
		if n := len(id) - len(":closed"); n > 0 && id[n:] == ":closed" {
			id, status = id[:n], StatusClosed
		}
		if err := g.Add(&Issue{ID: id, Status: status}); err != nil {
			t.Fatal(err)
		}
	}
	for _, e := range edges {
		if err := g.Link(EdgeKind(e[0]), e[1], e[2]); err != nil {
			t.Fatal(err)
		}
	}
	return g
}

func idsOf(issues []*Issue) []string {
	out := []string{}
	for _, i := range issues {
		out = append(out, i.ID)
	}
	return out
}

func TestAddAndLink(t *testing.T) {
	g := New()
	if err := g.Add(&Issue{ID: "a", Repo: "api"}); err != nil {
		t.Fatal(err)
	}
	if err := g.Add(&Issue{ID: "a", Repo: "web"}); err == nil {
		t.Error("duplicate ID accepted")
	}
	if err := g.Add(&Issue{}); err == nil {
		t.Error("empty ID accepted")
	}
	if err := g.Link(Blocking, "a", "missing"); err == nil {
		t.Error("edge to a missing issue accepted")
	}
	if err := g.Link("related", "a", "a"); err == nil {
		t.Error("unknown edge kind accepted")
	}
	for _, id := range []string{"c", "b"} {
		if err := g.Add(&Issue{ID: id}); err != nil {
			t.Fatal(err)
		}
	}
	for _, e := range [][2]string{{"c", "a"}, {"b", "a"}, {"b", "a"}} {
		if err := g.Link(Blocking, e[0], e[1]); err != nil {
			t.Fatal(err)
		}
	}
	if got := g.Issue("a").Blocks; !reflect.DeepEqual(got, []string{"b", "c"}) {
		t.Errorf("a.Blocks = %v, want sorted and deduplicated [b c]", got)
	}
	if got := g.Issue("b").BlockedBy; !reflect.DeepEqual(got, []string{"a"}) {
		t.Errorf("b.BlockedBy = %v", got)
	}
	if err := g.Link(ParentChild, "b", "c"); err != nil {
		t.Fatal(err)
	}
	if b, c := g.Issue("b"), g.Issue("c"); !reflect.DeepEqual(b.Parents, []string{"c"}) || !reflect.DeepEqual(c.Children, []string{"b"}) {
		t.Errorf("parent-child: b.Parents = %v, c.Children = %v", b.Parents, c.Children)
	}
	if got := idsOf(g.Issues()); !reflect.DeepEqual(got, []string{"a", "c", "b"}) {
		t.Errorf("Issues order = %v, want insertion order", got)
	}
	if g.Len() != 3 || g.Issue("zz") != nil {
		t.Error("Len or Issue lookup")
	}
}

func TestDescendants(t *testing.T) {
	g := build(t, []string{"m", "e1", "e2", "t1", "t2", "t3", "other"}, [][3]string{
		{"parent-child", "e2", "m"},
		{"parent-child", "e1", "m"},
		{"parent-child", "t2", "e1"},
		{"parent-child", "t1", "e1"},
		{"parent-child", "t3", "e2"},
		{"parent-child", "t3", "e1"}, // two parents: listed once
		{"blocks", "other", "t1"},    // blocking edges are not hierarchy
	})
	if got := idsOf(g.Descendants("m")); !reflect.DeepEqual(got, []string{"e1", "t1", "t2", "t3", "e2"}) {
		t.Errorf("Descendants(m) = %v", got)
	}
	if got := idsOf(g.Descendants("t1")); len(got) != 0 {
		t.Errorf("Descendants(leaf) = %v", got)
	}
	if got := g.Descendants("missing"); got != nil {
		t.Errorf("Descendants(missing) = %v", got)
	}

	loop := build(t, []string{"a", "b"}, [][3]string{{"parent-child", "a", "b"}, {"parent-child", "b", "a"}})
	if got := idsOf(loop.Descendants("a")); !reflect.DeepEqual(got, []string{"b"}) {
		t.Errorf("Descendants in a parent-child cycle = %v", got)
	}
}

func TestHighLevelAndGoals(t *testing.T) {
	g := New()
	for _, i := range []*Issue{
		{ID: "api-m1", HighLevel: true, Goals: []string{"hq-1"}},
		{ID: "api-t1", Goals: []string{"hq-1", "hq-2"}},
		{ID: "hq-1", Title: "Launch"},
		{ID: "web-e1", HighLevel: true, Goals: []string{"hq-1"}},
	} {
		if err := g.Add(i); err != nil {
			t.Fatal(err)
		}
	}
	if got := idsOf(g.HighLevel()); !reflect.DeepEqual(got, []string{"api-m1", "web-e1"}) {
		t.Errorf("HighLevel = %v", got)
	}
	goals := g.Goals()
	if len(goals) != 2 {
		t.Fatalf("Goals = %+v", goals)
	}
	if goals[0].ID != "hq-1" || goals[0].Issue == nil || goals[0].Issue.Title != "Launch" {
		t.Errorf("goal hq-1 = %+v, want its loaded bead", goals[0])
	}
	if got := idsOf(goals[0].Members); !reflect.DeepEqual(got, []string{"api-m1", "api-t1", "web-e1"}) {
		t.Errorf("hq-1 members = %v", got)
	}
	if goals[1].ID != "hq-2" || goals[1].Issue != nil || len(goals[1].Members) != 1 {
		t.Errorf("goal hq-2 = %+v, want an unloaded goal with one member", goals[1])
	}
}

func TestCycles(t *testing.T) {
	g := build(t,
		[]string{"a", "b", "c", "d", "self", "x:closed", "y:closed", "p", "q", "free"},
		[][3]string{
			// open blocking cycle a -> b -> c -> a, with d hanging off it
			{"blocks", "a", "b"}, {"blocks", "b", "c"}, {"blocks", "c", "a"}, {"blocks", "d", "a"},
			{"blocks", "self", "self"},
			// closed blocking cycle: history, not a scheduling problem
			{"blocks", "x", "y"}, {"blocks", "y", "x"},
			// parent-child cycle
			{"parent-child", "p", "q"}, {"parent-child", "q", "p"},
			// an epic waiting for its own child is not a cycle of one kind
			{"parent-child", "free", "d"}, {"blocks", "d", "free"},
		})
	want := []Cycle{
		{Kind: Blocking, IDs: []string{"a", "b", "c"}, Open: true},
		{Kind: Blocking, IDs: []string{"self"}, Open: true},
		{Kind: Blocking, IDs: []string{"x", "y"}, Open: false},
		{Kind: ParentChild, IDs: []string{"p", "q"}, Open: true},
	}
	if got := g.Cycles(); !reflect.DeepEqual(got, want) {
		t.Errorf("Cycles =\n%+v\nwant\n%+v", got, want)
	}

	acyclic := build(t, []string{"a", "b", "c"}, [][3]string{{"blocks", "c", "b"}, {"blocks", "b", "a"}, {"blocks", "c", "a"}})
	if got := acyclic.Cycles(); len(got) != 0 {
		t.Errorf("acyclic graph has cycles %+v", got)
	}
}

func TestClosed(t *testing.T) {
	for status, want := range map[string]bool{"closed": true, "open": false, "in_progress": false, "deferred": false} {
		if got := (&Issue{Status: status}).Closed(); got != want {
			t.Errorf("Closed(%s) = %v", status, got)
		}
	}
}
