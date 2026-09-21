package cli

import (
	"math"
	"time"

	"github.com/alexknips/beadline/internal/config"
	"github.com/alexknips/beadline/internal/idle"
	"github.com/alexknips/beadline/internal/roadmap"
)

// minAvailability floors the measured duty cycle (ADR-3 §1): without it, a
// model window that is entirely idle would divide every date by zero.
const minAvailability = 0.02

// idleMask builds the idle-time mask a forecast learns net of (ADR-3):
// declared windows, plus gaps inferred from the cross-repo activity
// timeline when expert.idle_gap_hours is positive. now is the horizon —
// idle.New clips every window to it, so a backtest origin at an earlier
// now never sees a window only known later (ADR-3 §3).
func idleMask(idl config.Idle, activity []time.Time, now time.Time) idle.Mask {
	gap := time.Duration(idl.GapHours * float64(time.Hour))
	declared := make([]idle.Declared, len(idl.Declared))
	for i, d := range idl.Declared {
		declared[i] = idle.Declared{Start: d.Start, End: d.End, Note: d.Note}
	}
	return idle.New(now, idle.Infer(activity, now, gap), declared)
}

// availability is the measured duty cycle over [since, now]: active
// minutes over calendar minutes, floored at minAvailability (ADR-3 §1).
func availability(m idle.Mask, since, now time.Time) float64 {
	calendar := now.Sub(since).Minutes()
	if !(calendar > 0) {
		return 1
	}
	a := m.Active(since, now) / calendar
	return math.Max(a, minAvailability)
}

// idleSummary is the roadmap.json report of the mask a forecast used: the
// total hours it covers in the model window, the measured availability,
// whether the data shows the city idle right now, and the declared
// windows exactly as configured (ADR-3, "never silent").
func idleSummary(idl config.Idle, m idle.Mask, since, now time.Time, avail float64) *roadmap.Idle {
	out := &roadmap.Idle{
		GapHours:      idl.GapHours,
		TotalHours:    round2(m.TotalHours(since, now)),
		Availability:  round2(avail),
		CurrentlyIdle: m.CurrentlyIdle(),
	}
	if out.CurrentlyIdle && idl.ResumeAt.After(now) {
		resume := idl.ResumeAt.UTC()
		out.ResumeAt = &resume
	}
	for _, d := range idl.Declared {
		w := roadmap.IdleWindow{Start: d.Start.UTC(), Note: d.Note}
		if !d.End.IsZero() {
			end := d.End.UTC()
			w.End = &end
		}
		out.Declared = append(out.Declared, w)
	}
	return out
}

func round2(f float64) float64 { return math.Round(f*100) / 100 }
