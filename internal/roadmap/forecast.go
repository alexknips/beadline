package roadmap

import (
	"time"

	"github.com/alexknips/beadline/internal/forecast"
	"github.com/alexknips/beadline/internal/graph"
)

// SetForecast copies a forecast of the same graph onto the roadmap: the
// forecaster's status, which knows stalled and parked work, the P50, P80
// and P95 dates, the split of the median run into agent and human time, and
// the critical chain. The scope counts stay those of Build: graph.Scope
// counts the closed blockers of open work as done, and the open set is the
// same either way. Each repo gets the agents the forecaster simulated and
// its pace over the model window. SetForecast calls Assess.
func (r *Roadmap) SetForecast(res *forecast.Result, g *graph.Graph) {
	outlooks := map[string]*Outlook{}
	for n := range r.Milestones {
		outlooks[r.Milestones[n].ID] = &r.Milestones[n].Outlook
	}
	for n := range r.Goals {
		outlooks[r.Goals[n].ID] = &r.Goals[n].Outlook
	}
	for _, items := range [][]forecast.Item{res.Items, res.Goals} {
		for _, it := range items {
			if o := outlooks[it.ID]; o != nil {
				o.setForecast(it)
			}
		}
	}

	rates := pace(g, r.GeneratedAt, r.Config.Model.WindowDays)
	for n := range r.Repos {
		rp := &r.Repos[n]
		if c := res.Concurrency[rp.Name]; c > 0 {
			f := float64(c)
			rp.Concurrency = &f
		}
		rate := rates[rp.Name]
		rp.RatePerDay = &rate
	}
	r.Assess()
}

func (o *Outlook) setForecast(it forecast.Item) {
	for _, s := range Statuses {
		if string(it.Status) == s {
			o.Status = s
		}
	}
	o.P50, o.P80, o.P95, o.AgentHours, o.HumanHours, o.CriticalChain = nil, nil, nil, nil, nil, nil
	if it.P50 == nil || it.P80 == nil || it.P95 == nil {
		return
	}
	o.P50, o.P80, o.P95 = timePtr(it.P50.At), timePtr(it.P80.At), timePtr(it.P95.At)
	agent, human := it.P50.AgentHours, it.P50.HumanHours
	o.AgentHours, o.HumanHours = &agent, &human
	o.CriticalChain = it.CriticalChain
}

// pace is each repo's work beads closed per day over the window, rounded to
// two decimals.
func pace(g *graph.Graph, now time.Time, windowDays int) map[string]float64 {
	out := map[string]float64{}
	if windowDays < 1 {
		return out
	}
	since := now.AddDate(0, 0, -windowDays)
	for _, i := range forecast.WorkBeads(g) {
		if i.Closed() && !i.ClosedAt.Before(since) && !i.ClosedAt.After(now) {
			out[i.Repo]++
		}
	}
	for repo, n := range out {
		out[repo] = float64(int(n/float64(windowDays)*100+0.5)) / 100
	}
	return out
}
