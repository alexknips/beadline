package calibrate

import (
	"fmt"
	"testing"
	"time"
)

func TestGrade(t *testing.T) {
	asOf := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	horizon := asOf.Add(100 * time.Hour)
	lines := []string{
		`{"id":"done","status":"closed","close_reason":"Merged to main","created_at":"2026-08-30T00:00:00Z","closed_at":"2026-09-02T06:00:00Z"}`,
		`{"id":"late","status":"open","created_at":"2026-08-30T00:00:00Z"}`,
		`{"id":"dup","status":"closed","close_reason":"Duplicate of done","created_at":"2026-08-30T00:00:00Z","closed_at":"2026-09-02T00:00:00Z"}`,
		`{"id":"noop","status":"closed","close_reason":"done","metadata":{"gc.work_outcome":"no-op"},"created_at":"2026-08-30T00:00:00Z","closed_at":"2026-09-02T00:00:00Z"}`,
		`{"id":"parked","status":"deferred","created_at":"2026-08-30T00:00:00Z"}`,
		`{"id":"snoozed","status":"open","defer_until":"2026-10-01T00:00:00Z","created_at":"2026-08-30T00:00:00Z"}`,
		`{"id":"undated","status":"closed","created_at":"2026-08-30T00:00:00Z"}`,
		// Epic E had children a and b at as_of; b moved away and c came in.
		`{"id":"E","issue_type":"epic","status":"closed","labels":["goal:g-2"],"created_at":"2026-08-30T00:00:00Z","closed_at":"2026-09-03T00:00:00Z"}`,
		`{"id":"a","status":"closed","created_at":"2026-08-30T00:00:00Z","closed_at":"2026-09-02T00:00:00Z","dependencies":[{"depends_on_id":"E","type":"parent-child"}]}`,
		`{"id":"b","status":"closed","created_at":"2026-08-30T00:00:00Z","closed_at":"2026-09-02T00:00:00Z"}`,
		`{"id":"c","status":"closed","created_at":"2026-09-01T12:00:00Z","closed_at":"2026-09-04T00:00:00Z","dependencies":[{"depends_on_id":"E","type":"parent-child"}]}`,
	}
	// Eight closes in one minute: a bulk cleanup.
	for n := 0; n < BurstCloses; n++ {
		lines = append(lines, fmt.Sprintf(`{"id":"bulk-%d","status":"closed","close_reason":"done","created_at":"2026-08-30T00:00:00Z","closed_at":"2026-09-02T09:00:%02dZ"}`, n, n))
	}
	r := NewReality(graphOf(t, parse(t, lines...), time.Time{}), horizon)
	grid := NewGrid([]float64{0.5, 0.8, 0.95}, []float64{20, 40, 150})

	grade := func(id string, then []string) Pair {
		p := Pair{AsOf: asOf, Level: LevelLeaf, ID: id, Quantiles: grid}
		if then != nil {
			p.Level = LevelHighLevel
		}
		r.grade(&p, then)
		return p
	}
	held := func(p Pair, q float64) string {
		switch h := p.Held(q); {
		case h == nil:
			return "?"
		case *h:
			return "y"
		}
		return "n"
	}

	done := grade("done", nil)
	if done.Outcome != OutcomeResolved || *done.ActualHours != 30 || done.ElapsedHours != 100 {
		t.Errorf("done = %+v", done)
	}
	if got := held(done, 0.5) + held(done, 0.8) + held(done, 0.95); got != "nyy" {
		t.Errorf("done held %s at P50, P80, P95; want nyy", got)
	}
	late := grade("late", nil)
	if late.Outcome != OutcomeOpen || late.ActualHours != nil {
		t.Errorf("late = %+v", late)
	}
	// Open for 100 hours: P50 and P80 already missed, P95 still pending.
	if got := held(late, 0.5) + held(late, 0.8) + held(late, 0.95) + held(late, 0.99); got != "nn??" {
		t.Errorf("late held %s at P50, P80, P95, P99; want nn??", got)
	}

	for id, want := range map[string]string{
		"dup": VoidDescoped, "noop": VoidDescoped, "bulk-3": VoidDescoped,
		"parked": VoidParked, "snoozed": VoidParked, "gone": VoidVanished, "undated": VoidUndated,
	} {
		if p := grade(id, nil); p.Outcome != OutcomeVoid || p.Void != want || p.Held(0.8) != nil {
			t.Errorf("%s = %s/%s, want void/%s", id, p.Outcome, p.Void, want)
		}
	}

	e := grade("E", []string{"a", "b"})
	if e.Outcome != OutcomeResolved || *e.ActualHours != 48 || e.ScopeAdded != 1 || e.ScopeRemoved != 1 || e.FixedScope() {
		t.Errorf("E = %+v", e)
	}
	if e := grade("E", []string{"a", "c"}); !e.FixedScope() {
		t.Errorf("E with its breakdown unchanged = %+v", e)
	}
	// Goal g-2 has no bead: it closes with the last bead of its breakdown.
	g := Pair{AsOf: asOf, Level: LevelHighLevel, ID: "g-2", Goal: true, Quantiles: grid}
	r.grade(&g, []string{"E", "a"})
	if g.Outcome != OutcomeResolved || *g.ActualHours != 72 || g.ScopeAdded != 1 {
		t.Errorf("goal = %+v", g)
	}
}

func TestDescopedIsConservative(t *testing.T) {
	r := NewReality(graphOf(t, parse(t,
		`{"id":"a","status":"closed","close_reason":"Fixed the duplicate rows in the export","closed_at":"2026-09-02T00:00:00Z"}`,
		`{"id":"b","status":"closed","close_reason":"  superseded by b-2","closed_at":"2026-09-02T00:00:00Z"}`,
		`{"id":"c","status":"closed","close_reason":"Won't fix: upstream","closed_at":"2026-09-02T00:00:00Z"}`,
		`{"id":"d","status":"closed","close_reason":"stale test data cleaned up","closed_at":"2026-09-02T00:00:00Z"}`,
	), time.Time{}), time.Time{})
	for id, want := range map[string]bool{"a": false, "b": true, "c": true, "d": false} {
		if got := r.Descoped(r.Graph.Issue(id)); got != want {
			t.Errorf("Descoped(%s) = %v, want %v", id, got, want)
		}
	}
}
