package graph

import (
	"reflect"
	"testing"
)

func TestScope(t *testing.T) {
	// m has one child e; e has children t1 (closed) and t2. The real work
	// hangs off blocking edges, as with a milestone whose epics live
	// elsewhere: m waits on b1, t2 waits on b2 (closed, so satisfied) and
	// on x, an epic under another parent with children x1 (open) and x2
	// (closed). x1 in turn waits on y. u is unrelated.
	g := build(t,
		[]string{"m", "e", "t1:closed", "t2", "b1", "b2:closed", "p", "x", "x1", "x2:closed", "y", "u"},
		[][3]string{
			{"parent-child", "e", "m"}, {"parent-child", "t1", "e"}, {"parent-child", "t2", "e"},
			{"blocks", "m", "b1"}, {"blocks", "t2", "b2"}, {"blocks", "t2", "x"},
			{"parent-child", "x", "p"}, {"parent-child", "x1", "x"}, {"parent-child", "x2", "x"},
			{"blocks", "x1", "y"},
			{"blocks", "t1", "u"}, // a closed bead's blockers are history
		})
	want := []string{"b1", "e", "t1", "t2", "x", "x1", "y"}
	if got := idsOf(g.Scope("m")); !reflect.DeepEqual(got, want) {
		t.Errorf("Scope(m) = %v, want %v", got, want)
	}
	if got := idsOf(g.Scope("u")); len(got) != 0 {
		t.Errorf("Scope(leaf) = %v, want empty", got)
	}
	if got := g.Scope("missing"); got != nil {
		t.Errorf("Scope(missing) = %v", got)
	}
}

func TestScopeCycles(t *testing.T) {
	// A blocking cycle through the scope, and one back to the root, must not
	// loop, and the root is never in its own scope.
	g := build(t, []string{"m", "a", "b"}, [][3]string{
		{"parent-child", "a", "m"}, {"blocks", "a", "b"}, {"blocks", "b", "a"}, {"blocks", "b", "m"},
	})
	if got := idsOf(g.Scope("m")); !reflect.DeepEqual(got, []string{"a", "b"}) {
		t.Errorf("Scope(m) = %v", got)
	}
}

func TestScopeClosedRoot(t *testing.T) {
	// A closed issue keeps its breakdown but no longer waits on anything.
	g := build(t, []string{"m:closed", "a:closed", "b"}, [][3]string{
		{"parent-child", "a", "m"}, {"blocks", "m", "b"},
	})
	if got := idsOf(g.Scope("m")); !reflect.DeepEqual(got, []string{"a"}) {
		t.Errorf("Scope(closed m) = %v", got)
	}
}
