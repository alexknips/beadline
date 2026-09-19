package roadmap

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"math"
	"os"
	"sort"
	"time"

	"github.com/alexknips/beadline/internal/config"
	"github.com/alexknips/beadline/internal/graph"
	"github.com/alexknips/beadline/internal/load"
)

// statusDeferred is the bd status of postponed work.
const statusDeferred = "deferred"

// Build assembles everything the graph alone says about the roadmap as of
// now: repos, goals and high-level beads with their scope, progress, status
// and target. The forecast fields stay empty; the forecaster sets them (and
// may set StatusStalled), then calls Assess again. BeadlineVersion,
// ModelVersion and Inputs are the caller's to set.
//
// Closed high-level beads are kept only when they closed within the model
// window. A bead that a goal label names is shown as that goal, not also as
// a milestone. rep may be nil; its dangling blocking edges mark work that
// waits on a repo that was not loaded.
func Build(g *graph.Graph, rep *load.Report, cfg *config.Config, now time.Time) *Roadmap {
	now = now.UTC()
	b := builder{
		now:     now,
		since:   now.AddDate(0, 0, -cfg.Model.WindowDays),
		outside: map[string][]string{},
	}
	if rep != nil {
		for _, d := range rep.Dangling {
			if d.Kind == graph.Blocking {
				b.outside[d.From] = append(b.outside[d.From], d.To)
			}
		}
	}
	r := &Roadmap{
		SchemaVersion: SchemaVersion,
		GeneratedAt:   now,
		Config:        ConfigOf(cfg),
		Inputs:        Inputs{Exports: []Export{}},
		Repos:         repos(g, cfg),
		Goals:         []Goal{},
		Milestones:    []Milestone{},
	}

	goals := g.Goals()
	isGoal := map[string]bool{}
	for _, gl := range goals {
		isGoal[gl.ID] = true
	}
	for _, gl := range goals {
		scope := map[string]*graph.Issue{}
		if gl.Issue != nil {
			for _, i := range g.Scope(gl.ID) {
				scope[i.ID] = i
			}
		}
		members := []string{}
		for _, m := range gl.Members {
			if m.ID == gl.ID {
				continue
			}
			scope[m.ID] = m
			for _, i := range g.Scope(m.ID) {
				scope[i.ID] = i
			}
			if m.HighLevel && !isGoal[m.ID] {
				members = append(members, m.ID)
			}
		}
		delete(scope, gl.ID)
		list := make([]*graph.Issue, 0, len(scope))
		for _, i := range scope {
			list = append(list, i)
		}
		sort.Slice(list, func(a, c int) bool { return list[a].ID < list[c].ID })

		goal := Goal{ID: gl.ID, Title: gl.ID, Members: members, Outlook: b.outlook(gl.Issue, list)}
		if gl.Issue != nil {
			goal.Title, goal.Repo = gl.Issue.Title, gl.Issue.Repo
		}
		if b.keep(goal.Outlook) {
			r.Goals = append(r.Goals, goal)
		}
	}

	for _, i := range g.HighLevel() {
		if isGoal[i.ID] {
			continue
		}
		m := Milestone{ID: i.ID, Title: i.Title, Repo: i.Repo, Type: i.Type, Goals: i.Goals,
			Outlook: b.outlook(i, g.Scope(i.ID))}
		if b.keep(m.Outlook) {
			r.Milestones = append(r.Milestones, m)
		}
	}
	r.Assess()
	return r
}

type builder struct {
	now, since time.Time
	outside    map[string][]string // issue ID -> blockers in no loaded repo
}

// keep drops what closed before the model window: the roadmap looks ahead.
func (b *builder) keep(o Outlook) bool {
	return o.Status != StatusDone || (o.ClosedAt != nil && !o.ClosedAt.Before(b.since))
}

// outlook derives the graph facts for a bead (own, nil for a goal whose
// bead is not loaded) from its scope, sorted by ID.
func (b *builder) outlook(own *graph.Issue, scope []*graph.Issue) Outlook {
	o := Outlook{Total: len(scope), RemainingIDs: []string{}}
	var lastClosed time.Time
	outside := map[string]bool{}
	for _, i := range scope {
		if i.Closed() {
			o.Done++
			if i.ClosedAt.After(lastClosed) {
				lastClosed = i.ClosedAt
			}
			continue
		}
		o.RemainingIDs = append(o.RemainingIDs, i.ID)
		if i.HumanGate {
			o.HumanGates++
		}
		for _, id := range b.outside[i.ID] {
			outside[id] = true
		}
	}
	if own != nil && !own.Closed() {
		for _, id := range b.outside[own.ID] {
			outside[id] = true
		}
	}
	for id := range outside {
		o.WaitsOnUnloaded = append(o.WaitsOnUnloaded, id)
	}
	sort.Strings(o.WaitsOnUnloaded)
	o.Remaining = len(o.RemainingIDs)
	if o.Total > 0 {
		o.DonePct = math.Round(1000*float64(o.Done)/float64(o.Total)) / 10
	}
	if own != nil && !own.DueAt.IsZero() {
		o.TargetDueAt = timePtr(own.DueAt)
	}

	switch {
	case own != nil && own.Closed():
		o.Status = StatusDone
		if !own.ClosedAt.IsZero() {
			o.ClosedAt = timePtr(own.ClosedAt)
		}
	case own == nil && o.Total > 0 && o.Remaining == 0:
		// A goal without its bead is done when all its work is.
		o.Status = StatusDone
		o.ClosedAt = timePtr(lastClosed)
	case own != nil && (own.Status == statusDeferred || own.DeferUntil.After(b.now)):
		o.Status = StatusDeferred
	case len(o.WaitsOnUnloaded) > 0:
		o.Status = StatusBlockedOutside
	case o.Total == 0:
		o.Status = StatusNotPlanned
	case o.Remaining == 0:
		o.Status = StatusReadyToClose
	default:
		o.Status = StatusForecast
	}
	return o
}

func timePtr(t time.Time) *time.Time {
	t = t.UTC()
	return &t
}

func repos(g *graph.Graph, cfg *config.Config) []Repo {
	out := make([]Repo, len(cfg.Repos))
	index := map[string]int{}
	for n, rc := range cfg.Repos {
		out[n] = Repo{Name: rc.Name, ConcurrencySource: "measured"}
		if !rc.Concurrency.Measure() {
			c := float64(rc.Concurrency.Max)
			out[n].Concurrency, out[n].ConcurrencySource = &c, "configured"
		}
		index[rc.Name] = n
	}
	for _, i := range g.Issues() {
		n, ok := index[i.Repo]
		if !ok {
			continue
		}
		out[n].Issues++
		if i.Closed() {
			out[n].Closed++
		} else {
			out[n].Open++
		}
	}
	return out
}

// ConfigOf echoes cfg as roadmap.json and snapshots record it.
func ConfigOf(cfg *config.Config) Config {
	c := Config{
		Repos: make([]ConfigRepo, len(cfg.Repos)),
		Conventions: Conventions{
			HighLevelTypes:         cfg.Conventions.HighLevelTypes,
			GoalLabelPattern:       cfg.Conventions.GoalLabelPattern,
			HumanGateTitlePatterns: cfg.Conventions.HumanGateTitlePatterns,
			HumanGateMetadataKeys:  cfg.Conventions.HumanGateMetadataKeys,
			InfraTypes:             cfg.Conventions.InfraTypes,
		},
		Model:    Model(cfg.Model),
		Hide:     cfg.Hide,
		Settings: cfg.NonDefault(),
	}
	for n, rc := range cfg.Repos {
		c.Repos[n] = ConfigRepo{Name: rc.Name, Export: rc.Export, Concurrency: Concurrency(rc.Concurrency.Max)}
	}
	return c
}

// Fingerprint hashes the configured exports. Call it right after loading, so
// it describes the files that were read.
func Fingerprint(cfg *config.Config) (Inputs, error) {
	in := Inputs{Exports: []Export{}}
	all := sha256.New()
	for _, rc := range cfg.Repos {
		f, err := os.Open(rc.ExportPath(cfg))
		if err != nil {
			return Inputs{}, fmt.Errorf("repo %s: %w", rc.Name, err)
		}
		h := sha256.New()
		n, err := io.Copy(h, f)
		f.Close()
		if err != nil {
			return Inputs{}, fmt.Errorf("repo %s: %w", rc.Name, err)
		}
		sum := hex.EncodeToString(h.Sum(nil))
		in.Exports = append(in.Exports, Export{Repo: rc.Name, Path: rc.Export, SHA256: sum, Bytes: n})
		fmt.Fprintf(all, "%s\t%s\n", rc.Name, sum)
	}
	in.Fingerprint = "sha256:" + hex.EncodeToString(all.Sum(nil))
	return in, nil
}

// SetInputs fingerprints the exports a run read, in config order, and marks
// the repos that could not be read. The fingerprint covers the exports that
// were read, as Fingerprint does for files.
func (r *Roadmap) SetInputs(exports []load.Export) {
	in := Inputs{Exports: []Export{}}
	all := sha256.New()
	failed := map[string]string{}
	for _, e := range exports {
		if e.Err != nil {
			failed[e.Repo] = e.Err.Error()
			continue
		}
		sum := sha256.Sum256(e.Data)
		hexSum := hex.EncodeToString(sum[:])
		in.Exports = append(in.Exports, Export{Repo: e.Repo, Path: e.Path, Live: e.Live, SHA256: hexSum, Bytes: int64(len(e.Data))})
		fmt.Fprintf(all, "%s\t%s\n", e.Repo, hexSum)
	}
	in.Fingerprint = "sha256:" + hex.EncodeToString(all.Sum(nil))
	r.Inputs = in
	for n := range r.Repos {
		r.Repos[n].Error = failed[r.Repos[n].Name]
	}
}

// Hide leaves the milestones and goals with these IDs off the roadmap.
func (r *Roadmap) Hide(ids []string) {
	if len(ids) == 0 {
		return
	}
	hide := map[string]bool{}
	for _, id := range ids {
		hide[id] = true
	}
	milestones := r.Milestones[:0]
	for _, m := range r.Milestones {
		if !hide[m.ID] {
			milestones = append(milestones, m)
		}
	}
	r.Milestones = milestones
	goals := r.Goals[:0]
	for _, g := range r.Goals {
		if !hide[g.ID] {
			goals = append(goals, g)
		}
	}
	r.Goals = goals
}
