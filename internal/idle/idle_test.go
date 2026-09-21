package idle

import (
	"testing"
	"time"
)

func day(n int) time.Time { return time.Date(2026, 9, n, 0, 0, 0, 0, time.UTC) }
func at(n int, h int) time.Time {
	return time.Date(2026, 9, n, h, 0, 0, 0, time.UTC)
}

func TestInferGaps(t *testing.T) {
	horizon := day(10)
	// day(1)-day(2): active. day(2)-day(6): a 4-day gap, over the 48h
	// threshold. day(6)-day(6)+1h: active. day(6)+1h-day(9): a ~71h gap,
	// also over threshold. day(9)-horizon: 1 day, under threshold.
	events := []time.Time{day(1), day(2), day(6), day(6).Add(time.Hour), day(9)}
	got := Infer(events, horizon, 48*time.Hour)
	want := []Window{
		{Start: day(2), End: day(6)},
		{Start: day(6).Add(time.Hour), End: day(9)},
	}
	if len(got) != len(want) {
		t.Fatalf("Infer() = %v, want %v", got, want)
	}
	for i := range want {
		if !got[i].Start.Equal(want[i].Start) || !got[i].End.Equal(want[i].End) {
			t.Errorf("window %d = %v, want %v", i, got[i], want[i])
		}
	}
}

func TestInferNoGapsUnderThreshold(t *testing.T) {
	horizon := day(5)
	events := []time.Time{day(1), day(2), day(3), day(4)}
	if got := Infer(events, horizon, 48*time.Hour); got != nil {
		t.Errorf("Infer() = %v, want nil", got)
	}
}

func TestInferTrailingGap(t *testing.T) {
	// Last activity was 3 days before horizon: still idle now, on top of
	// the 6-day gap that came before it.
	horizon := day(10)
	events := []time.Time{day(1), day(7)}
	got := Infer(events, horizon, 24*time.Hour)
	want := []Window{{Start: day(1), End: day(7)}, {Start: day(7), End: horizon}}
	if len(got) != len(want) {
		t.Fatalf("Infer() = %v, want %v", got, want)
	}
	for i := range want {
		if !got[i].Start.Equal(want[i].Start) || !got[i].End.Equal(want[i].End) {
			t.Errorf("window %d = %v, want %v", i, got[i], want[i])
		}
	}
}

func TestInferIgnoresFutureEvents(t *testing.T) {
	horizon := day(5)
	events := []time.Time{day(1), day(10)} // day(10) is after horizon
	got := Infer(events, horizon, 24*time.Hour)
	if len(got) != 1 || !got[0].Start.Equal(day(1)) || !got[0].End.Equal(horizon) {
		t.Errorf("Infer() = %v, want a single trailing window from day(1) to horizon", got)
	}
}

func TestMaskActive(t *testing.T) {
	horizon := day(20)
	m := New(horizon, []Window{{Start: day(5), End: day(8)}}, nil)
	// 10 days from day(1) to day(11), minus the 3-day window inside it.
	got := m.Active(day(1), day(11))
	want := (10 * 24 * time.Hour) - (3 * 24 * time.Hour)
	if got != want.Minutes() {
		t.Errorf("Active() = %v minutes, want %v", got, want.Minutes())
	}
}

func TestMaskActivePartialOverlap(t *testing.T) {
	horizon := day(20)
	m := New(horizon, []Window{{Start: day(5), End: day(15)}}, nil)
	// Window only partially overlaps [day(3), day(10)): 5 idle days of 7.
	got := m.Active(day(3), day(10))
	want := (7 * 24 * time.Hour) - (5 * 24 * time.Hour)
	if got != want.Minutes() {
		t.Errorf("Active() = %v minutes, want %v", got, want.Minutes())
	}
}

func TestMaskActiveNoWindows(t *testing.T) {
	m := New(day(20), nil, nil)
	got := m.Active(day(1), day(2))
	want := (24 * time.Hour).Minutes()
	if got != want {
		t.Errorf("Active() = %v, want %v", got, want)
	}
}

func TestMaskActiveNegativeSpanIsZero(t *testing.T) {
	m := New(day(20), nil, nil)
	if got := m.Active(day(5), day(1)); got != 0 {
		t.Errorf("Active() = %v, want 0", got)
	}
}

func TestMaskMergesOverlappingWindows(t *testing.T) {
	horizon := day(20)
	m := New(horizon,
		[]Window{{Start: day(3), End: day(6)}},
		[]Declared{{Start: day(5), End: day(9), Note: "planned maintenance"}},
	)
	ws := m.Windows()
	if len(ws) != 1 {
		t.Fatalf("Windows() = %v, want one merged window", ws)
	}
	if !ws[0].Start.Equal(day(3)) || !ws[0].End.Equal(day(9)) {
		t.Errorf("merged window = %v, want [day(3), day(9))", ws[0])
	}
	if ws[0].Note != "planned maintenance" {
		t.Errorf("merged window note = %q, want the declared window's note", ws[0].Note)
	}
}

func TestMaskDeclaredOverridesShortGap(t *testing.T) {
	// A declared window can mark idle time inference alone would miss
	// (a gap shorter than G): it needs no inference (ADR-3, scope C).
	horizon := day(20)
	m := New(horizon, nil, []Declared{{Start: day(5), End: day(5).Add(6 * time.Hour), Note: "known outage"}})
	if got := m.TotalHours(day(1), day(10)); got != 6 {
		t.Errorf("TotalHours() = %v, want 6", got)
	}
}

func TestMaskOpenDeclaredWindowRunsToHorizon(t *testing.T) {
	horizon := at(10, 12)
	m := New(horizon, nil, []Declared{{Start: day(9), Note: "still down"}})
	if !m.CurrentlyIdle() {
		t.Error("CurrentlyIdle() = false, want true for an open declared window at the horizon")
	}
	ws := m.Windows()
	if len(ws) != 1 || !ws[0].End.Equal(horizon) {
		t.Errorf("Windows() = %v, want the open window closed at the horizon", ws)
	}
}

func TestMaskClipsToHorizonNoLeakage(t *testing.T) {
	// A window (declared or inferred) that starts after the horizon, or
	// reaches past it, must not leak future knowledge into a past origin
	// (ADR-3 §3).
	horizon := day(10)
	m := New(horizon,
		[]Window{{Start: day(15), End: day(16)}},  // entirely after horizon
		[]Declared{{Start: day(8), End: day(20)}}, // starts before, reaches past
	)
	ws := m.Windows()
	if len(ws) != 1 {
		t.Fatalf("Windows() = %v, want only the declared window, clipped", ws)
	}
	if !ws[0].Start.Equal(day(8)) || !ws[0].End.Equal(horizon) {
		t.Errorf("window = %v, want [day(8), horizon)", ws[0])
	}
}

func TestMaskDeclaredStartingAfterHorizonIsInvisible(t *testing.T) {
	horizon := day(10)
	m := New(horizon, nil, []Declared{{Start: day(12), End: day(14)}})
	if len(m.Windows()) != 0 {
		t.Errorf("Windows() = %v, want none: the window starts after the horizon", m.Windows())
	}
}

func TestCurrentlyIdleFalseWhenLastWindowEndsBeforeHorizon(t *testing.T) {
	horizon := day(10)
	m := New(horizon, []Window{{Start: day(5), End: day(6)}}, nil)
	if m.CurrentlyIdle() {
		t.Error("CurrentlyIdle() = true, want false: the last window closed before the horizon")
	}
}

func TestZeroMaskMasksNothing(t *testing.T) {
	var m Mask
	if got := m.Active(day(1), day(2)); got != (24 * time.Hour).Minutes() {
		t.Errorf("Active() on zero Mask = %v, want the full span", got)
	}
	if m.CurrentlyIdle() {
		t.Error("CurrentlyIdle() on zero Mask = true, want false")
	}
}
