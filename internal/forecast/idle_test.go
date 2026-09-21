package forecast

import (
	"math"
	"testing"
	"time"

	"github.com/alexknips/beadline/internal/graph"
)

// ADR-3 §1: Availability < 1 stretches every drawn duration, and therefore
// every finish date, by 1/Availability, uniformly — a measured duty cycle
// turning simulated (idle-masked) minutes back into calendar time.
func TestAvailabilityStretchesFinish(t *testing.T) {
	g := newGraph(t, []*graph.Issue{{ID: "m", HighLevel: true}, {ID: "a"}}, child("a", "m"))
	o := options(fixed{w: 100}, 0)
	full := run(t, g, o)
	if got := after(find(t, full, "m").P80); got != 100 {
		t.Fatalf("finish at full availability = %v minutes, want 100", got)
	}

	o.Availability = 0.5
	half := run(t, g, o)
	if got := after(find(t, half, "m").P80); got != 200 {
		t.Errorf("finish at 0.5 availability = %v minutes, want 200 (100 / 0.5)", got)
	}
}

// The agent/human split still adds up to the time from Now, even stretched.
func TestAvailabilityPreservesSplitInvariant(t *testing.T) {
	g := newGraph(t, []*graph.Issue{{ID: "m", HighLevel: true}, {ID: "a"}}, child("a", "m"))
	o := options(fixed{q: 10, w: 30}, 0)
	o.Availability = 0.4
	p := find(t, run(t, g, o), "m").P80
	// Each half is independently rounded to a hundredth (render precision),
	// so allow the rounding error of two halves, not exact equality.
	if got := p.AgentHours + p.HumanHours; math.Abs(got-after(p)/60) > 0.01 {
		t.Errorf("agent %.4f + human %.4f = %.4f h, want %.4f h (time from now)",
			p.AgentHours, p.HumanHours, got, after(p)/60)
	}
}

// ADR-3 §2: a DateFrom after Now (a declared resume while the system is
// idle right now) shifts every date by exactly that gap, and the gap
// counts as human time, not agent time: nobody is working it.
func TestDateFromShiftsFinish(t *testing.T) {
	g := newGraph(t, []*graph.Issue{{ID: "m", HighLevel: true}, {ID: "a"}}, child("a", "m"))
	o := options(fixed{w: 100}, 0)
	o.DateFrom = now.Add(6 * time.Hour)
	res := run(t, g, o)
	p := find(t, res, "m").P80
	if got := p.At.Sub(now).Minutes(); got != 6*60+100 {
		t.Errorf("finish from now = %v minutes, want %v (6h resume gap + 100 work)", got, 6*60+100)
	}
	if want := 1.67; p.AgentHours != want {
		t.Errorf("agent hours = %v, want %v (the resume gap is not agent time)", p.AgentHours, want)
	}
	if want := 6.0; p.HumanHours != want {
		t.Errorf("human hours = %v, want %v (the resume gap)", p.HumanHours, want)
	}
}

// A zero DateFrom (the default) behaves exactly as before: dates count
// from Now.
func TestDateFromZeroIsNow(t *testing.T) {
	g := newGraph(t, []*graph.Issue{{ID: "m", HighLevel: true}, {ID: "a"}}, child("a", "m"))
	o := options(fixed{w: 100}, 0)
	p := find(t, run(t, g, o), "m").P80
	if got := p.At.Sub(now).Minutes(); got != 100 {
		t.Errorf("finish from now = %v minutes, want 100", got)
	}
}

// GridHours and OnTime, which grading and roadmap.json read as "from Now",
// stay correct under both Availability and DateFrom.
func TestGridHoursAndOnTimeCountFromNow(t *testing.T) {
	g := newGraph(t, []*graph.Issue{{ID: "m", HighLevel: true}, {ID: "a"}}, child("a", "m"))
	o := options(fixed{w: 120}, 0)
	o.Availability = 0.5            // 120 / 0.5 = 240 minutes of work
	o.DateFrom = now.Add(time.Hour) // +60 minutes before work starts
	due := now.Add(6 * time.Hour)   // 360 minutes: after the 300-minute finish
	o.Grid = []float64{0.5}
	g.Issue("m").DueAt = due

	res := run(t, g, o)
	m := find(t, res, "m")
	wantHours := (60.0 + 240.0) / 60
	if len(m.GridHours) != 1 || math.Abs(m.GridHours[0]-wantHours) > 1e-6 {
		t.Errorf("grid hours = %v, want [%v]", m.GridHours, wantHours)
	}
	if m.OnTime == nil || *m.OnTime != 1 {
		t.Errorf("on_time = %v, want 1 (300 minutes < 360-minute due date)", m.OnTime)
	}
}
