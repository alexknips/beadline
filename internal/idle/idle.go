// Package idle finds and masks the stretches of calendar time when nothing
// was happening: the agent pool was suspended, the host was down, or a
// usage cap paused every repo at once. Left in, that wall-clock time is
// learned as if it were work (docs/design.md, ADR-3).
//
// Two sources feed a Mask: Infer finds gaps in a cross-repo activity
// timeline, and Declared windows are explicit, config-given overrides that
// need no inference. Both are clipped to the horizon they are computed as
// of, so a rolling-origin backtest never sees a window it could not yet
// know about (ADR-3 §3).
package idle

import (
	"sort"
	"time"
)

// Window is one masked interval, half-open [Start, End). A Window a Mask
// holds always has End set: it is clipped to the mask's horizon.
type Window struct {
	Start, End time.Time
	// Note is the declared window's reason, when it came from one; empty
	// for an inferred gap or when two merged windows disagree.
	Note string
}

// Declared is one explicit idle window from beadline.toml (expert.idle): a
// zero-inference override. End zero means the window was still open as of
// when the config was written; a Mask closes it at its own horizon.
type Declared struct {
	Start, End time.Time
	Note       string
}

// Infer finds gaps of more than gap in a cross-repo activity timeline:
// events is every created/updated/started/closed timestamp beadline loaded,
// of any record, work or infra, across every repo (docs/design.md, ADR-3).
// events need not be sorted or deduplicated. A gap between two consecutive
// events becomes a window, and so does a gap between the latest event and
// horizon: the system may still be idle as of the moment asked about.
func Infer(events []time.Time, horizon time.Time, gap time.Duration) []Window {
	if gap <= 0 || horizon.IsZero() {
		return nil
	}
	sorted := make([]time.Time, 0, len(events))
	for _, t := range events {
		if !t.IsZero() && !t.After(horizon) {
			sorted = append(sorted, t)
		}
	}
	sort.Slice(sorted, func(a, b int) bool { return sorted[a].Before(sorted[b]) })

	var out []Window
	var last time.Time
	for _, t := range sorted {
		if !last.IsZero() && t.Sub(last) > gap {
			out = append(out, Window{Start: last, End: t})
		}
		if t.After(last) {
			last = t
		}
	}
	if !last.IsZero() && horizon.Sub(last) > gap {
		out = append(out, Window{Start: last, End: horizon})
	}
	return out
}

// Mask is a merged, non-overlapping set of idle windows, every one clipped
// to the horizon it was built for. A zero Mask masks nothing.
type Mask struct {
	windows []Window
	horizon time.Time
}

// New merges inferred gaps and declared windows into one Mask as of
// horizon. Every window is clipped to horizon — an open declared window
// (End zero) runs idle up to it, and so does a window that would otherwise
// reach past it — so a Mask built at a past origin never leaks a window
// only known later (ADR-3 §3, the backtest's rewind).
func New(horizon time.Time, inferred []Window, declared []Declared) Mask {
	var all []Window
	for _, w := range inferred {
		if w.Start.Before(horizon) {
			all = append(all, clip(w, horizon))
		}
	}
	for _, d := range declared {
		if !d.Start.Before(horizon) {
			continue
		}
		w := Window{Start: d.Start, End: d.End, Note: d.Note}
		if w.End.IsZero() {
			w.End = horizon
		}
		all = append(all, clip(w, horizon))
	}
	sort.Slice(all, func(a, b int) bool { return all[a].Start.Before(all[b].Start) })

	var merged []Window
	for _, w := range all {
		if w.End.Before(w.Start) {
			continue
		}
		if n := len(merged); n > 0 && !w.Start.After(merged[n-1].End) {
			if w.End.After(merged[n-1].End) {
				merged[n-1].End = w.End
			}
			if merged[n-1].Note == "" {
				merged[n-1].Note = w.Note
			}
			continue
		}
		merged = append(merged, w)
	}
	return Mask{windows: merged, horizon: horizon}
}

func clip(w Window, horizon time.Time) Window {
	if w.End.IsZero() || w.End.After(horizon) {
		w.End = horizon
	}
	return w
}

// Windows returns the mask's merged windows, sorted, earliest first.
func (m Mask) Windows() []Window { return m.windows }

// CurrentlyIdle reports whether the mask's last window reaches its horizon:
// the system was still idle at the moment the mask was built.
func (m Mask) CurrentlyIdle() bool {
	n := len(m.windows)
	return n > 0 && !m.windows[n-1].End.Before(m.horizon)
}

// Active returns the active (non-idle) minutes between from and to; never
// negative. to before from returns 0, as every caller treats a non-positive
// span.
func (m Mask) Active(from, to time.Time) float64 {
	if !to.After(from) {
		return 0
	}
	active := to.Sub(from) - m.overlap(from, to)
	if active < 0 {
		active = 0
	}
	return active.Minutes()
}

// TotalHours returns the idle hours the mask covers within [from, to].
func (m Mask) TotalHours(from, to time.Time) float64 {
	if !to.After(from) {
		return 0
	}
	return m.overlap(from, to).Hours()
}

func (m Mask) overlap(from, to time.Time) time.Duration {
	var total time.Duration
	for _, w := range m.windows {
		start, end := w.Start, w.End
		if start.Before(from) {
			start = from
		}
		if end.After(to) {
			end = to
		}
		if end.After(start) {
			total += end.Sub(start)
		}
	}
	return total
}
