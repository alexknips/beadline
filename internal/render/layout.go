package render

import (
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/alexknips/beadline/internal/roadmap"
)

// Timeline geometry in SVG user units. The script re-stacks lanes and rows
// with the same numbers when a repo filter hides some; they reach it as
// data attributes on the <svg>.
const (
	svgW       = 1000
	chartX0    = 312.0 // left edge of the time axis; lane and row labels sit left of it
	chartX1    = 976.0
	axisH      = 40 // tick labels above the first lane
	laneHeadH  = 26
	rowH       = 24
	laneGap    = 8
	barH       = 12
	labelRunes = 44 // row labels are cut to this many characters
)

// laneGoals is the data-repo of the goals lane: its rows filter by member
// repos instead.
const laneGoals = "*goals"

type timeline struct {
	W, H                   int
	X0, X1, NowX           float64
	AxisH, LaneHeadH, RowH int
	LaneGap, BarH          int
	// Fixed offsets: within a row (bar, P50 tick, text baseline), within a
	// lane header, and in the axis above the lanes.
	BarY, P50Y1, P50Y2, MidY, TextY int
	HeadTextY, EmptyY               int
	NowLabelY, TickY, GridY, NowY   int
	Ticks                           []tick
	Lanes                           []lane
}

type tick struct {
	X     float64
	Label string
}

type lane struct {
	Repo  string // data-repo: a repo name, or laneGoals
	Name  string
	Note  string
	Y, H  int
	Alt   bool // shaded background
	Rows  []row
	Empty string // shown in place of rows
}

type row struct {
	ID     string
	Repos  string // data-repos: the repos the row belongs to, as a JSON array
	Y      int    // within the lane
	Label  string // title, cut to fit
	Tip    string // multi-line details: the SVG <title> and the popover
	Aria   string // Tip on one line
	Class  string // schedule class for the bar
	Bar    *bar
	Note   string // status text: instead of a bar, or after one that needs a caveat
	NoteX  float64
	Target *marker
}

type bar struct{ X, W, P50X, P95X float64 }

type marker struct {
	X    float64 // clamped to the axis start
	Past bool    // the target has passed
	D    string  // the diamond's path
}

// layout computes the timeline: the goals lane, then one lane per repo in
// config order. Only open milestones and goals get rows; done ones are in
// the table.
func layout(r *roadmap.Roadmap, v *view) timeline {
	barY := (rowH - barH) / 2
	t := timeline{W: svgW, X0: chartX0, X1: chartX1, AxisH: axisH, LaneHeadH: laneHeadH, RowH: rowH,
		LaneGap: laneGap, BarH: barH, BarY: barY, P50Y1: barY - 3, P50Y2: barY + barH + 3, MidY: rowH / 2,
		TextY: rowH/2 + 4, HeadTextY: 17, EmptyY: laneHeadH + rowH/2 + 4,
		NowLabelY: 12, TickY: 30, GridY: axisH - 4, NowY: 16}

	now := r.GeneratedAt
	end := now
	extend := func(o *roadmap.Outlook) {
		for _, d := range []*time.Time{o.P50, o.P80, o.TargetDueAt} {
			if d != nil && d.After(end) {
				end = *d
			}
		}
	}
	for n := range r.Goals {
		if r.Goals[n].Status != roadmap.StatusDone {
			extend(&r.Goals[n].Outlook)
		}
	}
	for n := range r.Milestones {
		if r.Milestones[n].Status != roadmap.StatusDone {
			extend(&r.Milestones[n].Outlook)
		}
	}
	if end.Sub(now) < 7*24*time.Hour {
		end = now.Add(7 * 24 * time.Hour)
	}
	span := end.Sub(now)
	t0, t1 := now.Add(-span/30), end.Add(span/12)
	x := func(d time.Time) float64 {
		return chartX0 + (chartX1-chartX0)*float64(d.Sub(t0))/float64(t1.Sub(t0))
	}
	t.NowX = x(now)
	for _, d := range tickTimes(t0, t1) {
		t.Ticks = append(t.Ticks, tick{X: x(d.at), Label: d.label})
	}

	mkRow := func(id, title, repos string, o *roadmap.Outlook, tip []string) row {
		rw := row{ID: id, Repos: repos, Label: cut(title, labelRunes), Tip: strings.Join(tip, "\n"),
			Aria: strings.Join(tip, ". "), Class: scheduleClass(o.Schedule)}
		if o.P80 != nil {
			b := &bar{X: t.NowX, W: max(x(*o.P80)-t.NowX, 2), P95X: -1}
			b.P50X = b.X
			if o.P50 != nil {
				b.P50X = x(*o.P50)
			}
			if o.P95 != nil {
				b.P95X = min(x(*o.P95), chartX1)
			}
			rw.Bar = b
			// A forecast that needs a caveat says so after the bar.
			if o.Status == roadmap.StatusBlockedOutside || o.Status == roadmap.StatusStalled {
				rw.Note, rw.NoteX = v.statusText(o), max(b.X+b.W, b.P95X)+8
			}
		} else {
			rw.Note, rw.NoteX = v.statusText(o), t.NowX+8
		}
		if o.TargetDueAt != nil {
			mx := x(*o.TargetDueAt)
			m := &marker{X: max(mx, chartX0), Past: o.TargetDueAt.Before(now)}
			m.D = fmt.Sprintf("M%s %dl6 6-6 6-6-6z", num(m.X), t.MidY-6)
			rw.Target = m
		}
		return rw
	}

	y := axisH
	addLane := func(l lane) {
		l.Y = y
		l.H = laneHeadH + max(len(l.Rows), 1)*rowH
		for n := range l.Rows {
			l.Rows[n].Y = laneHeadH + n*rowH
		}
		l.Alt = len(t.Lanes)%2 == 1
		t.Lanes = append(t.Lanes, l)
		y += l.H + laneGap
	}

	goals := lane{Repo: laneGoals, Name: "Goals", Empty: "no open goals"}
	open, done := 0, 0
	for _, g := range openFirst(goalOutlooks(r)) {
		gl := &r.Goals[g]
		if gl.Status == roadmap.StatusDone {
			done++
			continue
		}
		open++
		goals.Rows = append(goals.Rows, mkRow(gl.ID, gl.Title, reposAttr(v.goalRepos(gl)...), &gl.Outlook, v.goalTip(gl)))
	}
	goals.Note = laneNote(open, done, r.Config.Model.WindowDays, nil)
	addLane(goals)

	for _, rp := range r.Repos {
		l := lane{Repo: rp.Name, Name: rp.Name, Empty: "no open milestones or epics"}
		open, done := 0, 0
		for _, n := range openFirst(milestoneOutlooks(r, rp.Name)) {
			m := &r.Milestones[n]
			if m.Status == roadmap.StatusDone {
				done++
				continue
			}
			open++
			l.Rows = append(l.Rows, mkRow(m.ID, m.Title, reposAttr(m.Repo), &m.Outlook, v.milestoneTip(m)))
		}
		l.Note = laneNote(open, done, r.Config.Model.WindowDays, rp.Concurrency)
		if rp.Error != "" {
			l.Note, l.Empty = "could not be read", "could not be read: see the note at the top"
		}
		addLane(l)
	}
	t.H = y - laneGap + 12
	return t
}

func laneNote(open, done, windowDays int, agents *float64) string {
	parts := []string{fmt.Sprintf("%d open", open)}
	if done > 0 {
		parts = append(parts, fmt.Sprintf("%d done in the last %d days", done, windowDays))
	}
	if agents != nil {
		parts = append(parts, plural(*agents, "agent"))
	}
	return strings.Join(parts, " · ")
}

type indexed struct {
	n  int
	o  *roadmap.Outlook
	id string
}

func goalOutlooks(r *roadmap.Roadmap) []indexed {
	out := make([]indexed, len(r.Goals))
	for n := range r.Goals {
		out[n] = indexed{n, &r.Goals[n].Outlook, r.Goals[n].ID}
	}
	return out
}

func milestoneOutlooks(r *roadmap.Roadmap, repo string) []indexed {
	var out []indexed
	for n := range r.Milestones {
		if r.Milestones[n].Repo == repo {
			out = append(out, indexed{n, &r.Milestones[n].Outlook, r.Milestones[n].ID})
		}
	}
	return out
}

// statusRank orders rows without a forecast: what needs attention first.
var statusRank = map[string]int{
	roadmap.StatusBlockedOutside: 1, roadmap.StatusStalled: 2, roadmap.StatusReadyToClose: 3,
	roadmap.StatusForecast: 4, roadmap.StatusNotPlanned: 5, roadmap.StatusDeferred: 6, roadmap.StatusDone: 7,
}

// openFirst orders rows: forecast ones by P80, then P50, then the others by
// status, done last (most recent first); ties by ID. It returns indexes.
func openFirst(items []indexed) []int {
	sort.SliceStable(items, func(a, b int) bool {
		oa, ob := items[a].o, items[b].o
		fa, fb := oa.P80 != nil && oa.Status != roadmap.StatusDone, ob.P80 != nil && ob.Status != roadmap.StatusDone
		switch {
		case fa != fb:
			return fa
		case fa && !oa.P80.Equal(*ob.P80):
			return oa.P80.Before(*ob.P80)
		case fa && oa.P50 != nil && ob.P50 != nil && !oa.P50.Equal(*ob.P50):
			return oa.P50.Before(*ob.P50)
		case !fa && statusRank[oa.Status] != statusRank[ob.Status]:
			return statusRank[oa.Status] < statusRank[ob.Status]
		case oa.Status == roadmap.StatusDone && oa.ClosedAt != nil && ob.ClosedAt != nil && !oa.ClosedAt.Equal(*ob.ClosedAt):
			return oa.ClosedAt.After(*ob.ClosedAt)
		}
		return items[a].id < items[b].id
	})
	out := make([]int, len(items))
	for n, it := range items {
		out[n] = it.n
	}
	return out
}

func scheduleClass(s string) string {
	if s == "" {
		return "s-none"
	}
	return "s-" + s
}

type tickTime struct {
	at    time.Time
	label string
}

// tickTimes picks calendar-aligned ticks: days, weeks (Mondays), months,
// quarters or years, whichever gives a readable count for the span.
func tickTimes(t0, t1 time.Time) []tickTime {
	days := t1.Sub(t0).Hours() / 24
	day := time.Date(t0.Year(), t0.Month(), t0.Day(), 0, 0, 0, 0, time.UTC)
	var step func(time.Time) time.Time
	start := day
	label := func(d time.Time, first bool) string { return d.Format("Jan 2") }
	switch {
	case days <= 12:
		step = func(d time.Time) time.Time { return d.AddDate(0, 0, 1) }
	case days <= 24:
		step = func(d time.Time) time.Time { return d.AddDate(0, 0, 2) }
	case days <= 160:
		n := 7
		if days > 80 {
			n = 14
		}
		start = day.AddDate(0, 0, (8-int(day.Weekday()))%7) // next Monday, or today if Monday
		step = func(d time.Time) time.Time { return d.AddDate(0, 0, n) }
	default:
		months := 1
		switch {
		case days > 1500:
			months = 12
		case days > 450:
			months = 3
		}
		start = time.Date(t0.Year(), t0.Month(), 1, 0, 0, 0, 0, time.UTC)
		for (int(start.Month())-1)%months != 0 {
			start = start.AddDate(0, -1, 0)
		}
		step = func(d time.Time) time.Time { return d.AddDate(0, months, 0) }
		label = func(d time.Time, first bool) string {
			switch {
			case months == 12:
				return d.Format("2006")
			case first || d.Month() == time.January:
				return d.Format("Jan 2006")
			}
			return d.Format("Jan")
		}
	}
	for start.Before(t0) {
		start = step(start)
	}
	var out []tickTime
	for d := start; !d.After(t1); d = step(d) {
		out = append(out, tickTime{d, label(d, len(out) == 0)})
	}
	return out
}

// cut shortens s to n characters, ending in an ellipsis when it was longer.
func cut(s string, n int) string {
	r := []rune(strings.TrimSpace(s))
	if len(r) <= n {
		return string(r)
	}
	return strings.TrimRight(string(r[:n-1]), " ") + "…"
}
