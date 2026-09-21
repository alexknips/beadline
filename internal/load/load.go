package load

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"time"

	"github.com/alexknips/beadline/internal/config"
	"github.com/alexknips/beadline/internal/graph"
)

// blockingTypes are the dependency types bd treats as hard blockers. They all
// become graph.Blocking edges. Other types (related, discovered-from, tracks,
// ...) carry no scheduling meaning and are ignored.
var blockingTypes = map[string]bool{"blocks": true, "conditional-blocks": true, "waits-for": true}

const parentChildType = "parent-child"

// statusOpen is bd's status of work nobody has started.
const statusOpen = "open"

// Source is one repository's export stream.
type Source struct {
	Repo string    // [[repos]] name the issues belong to
	Name string    // shown in errors and the report, usually the file path
	R    io.Reader // bd export JSONL
}

// Report describes what was loaded and what looked wrong. Nothing in it
// stops a load; the caller decides what is fatal.
type Report struct {
	Repos      []RepoStats
	Dangling   []Dangling
	Duplicates []Duplicate
	Cycles     []graph.Cycle
}

// RepoStats counts the records of one source.
type RepoStats struct {
	Repo, Source string
	Issues       int            // issues added to the graph
	Infra        map[string]int // infra records dropped: by issue type, or "ephemeral" / "template"
	Other        int            // non-issue records (e.g. memories) skipped
}

// Dangling is an edge whose target is in no loaded repository. Edges to
// dropped infra records are not dangling; they are dropped with them.
type Dangling struct {
	Repo string
	From string
	To   string
	Kind graph.EdgeKind
}

// Duplicate is an issue ID seen again after it was loaded. The first copy
// is kept.
type Duplicate struct {
	ID     string
	Kept   string // repo of the copy that was kept
	Repo   string // repo of the dropped copy
	Source string
	Line   int
}

// record is the part of a bd export line beadline reads. Unknown fields are
// ignored, so newer bd versions keep loading.
type record struct {
	Type             string                     `json:"_type"`
	ID               string                     `json:"id"`
	Title            string                     `json:"title"`
	Description      string                     `json:"description"`
	IssueType        string                     `json:"issue_type"`
	Status           string                     `json:"status"`
	CloseReason      string                     `json:"close_reason"`
	Priority         int                        `json:"priority"`
	Assignee         string                     `json:"assignee"`
	Labels           []string                   `json:"labels"`
	Metadata         map[string]json.RawMessage `json:"metadata"`
	CreatedAt        *time.Time                 `json:"created_at"`
	UpdatedAt        *time.Time                 `json:"updated_at"`
	StartedAt        *time.Time                 `json:"started_at"`
	ClosedAt         *time.Time                 `json:"closed_at"`
	DueAt            *time.Time                 `json:"due_at"`
	DeferUntil       *time.Time                 `json:"defer_until"`
	EstimatedMinutes *int                       `json:"estimated_minutes"`
	Ephemeral        bool                       `json:"ephemeral"`
	IsTemplate       bool                       `json:"is_template"`
	Dependencies     []dependency               `json:"dependencies"`
}

type dependency struct {
	DependsOnID string `json:"depends_on_id"`
	Type        string `json:"type"`
	// CreatedAt is read leniently: only a rewind needs it, and a malformed
	// timestamp must not make an export unreadable.
	CreatedAt string `json:"created_at"`
}

// created returns when the dependency was created, and false when the
// export does not say.
func (d dependency) created() (time.Time, bool) {
	t, err := time.Parse(time.RFC3339Nano, d.CreatedAt)
	return t.UTC(), err == nil
}

// Load reads every repository in cfg, in config order, as ParseFiles does.
func Load(cfg *config.Config) (*graph.Graph, *Report, error) {
	ex, err := ParseFiles(cfg)
	if err != nil {
		return nil, nil, err
	}
	return ex.Graph(&cfg.Conventions, time.Time{})
}

// ParseFiles reads and parses every repository in cfg, in config order, as
// ReadAll does with bd from PATH, and fails if any cannot be read.
func ParseFiles(cfg *config.Config) (*Exports, error) {
	exports := ReadAll(context.Background(), cfg, BdExporter("bd"))
	for _, e := range exports {
		if e.Err != nil {
			return nil, e.Err
		}
	}
	return ParseExports(exports)
}

// Read builds one graph from the sources and applies the conventions:
// infra records (by type, ephemeral or template) are dropped, high-level
// types, goal labels and human gates are marked on each issue. Malformed
// JSON or a record without an ID is an error; graph problems (dangling
// edges, duplicate IDs, cycles) go into the report.
func Read(cv *config.Conventions, sources ...Source) (*graph.Graph, *Report, error) {
	ex, err := Parse(sources...)
	if err != nil {
		return nil, nil, err
	}
	return ex.Graph(cv, time.Time{})
}

// Exports are parsed bd exports, not yet a graph. Graph builds the graph as
// the exports show it, or as it stood at an earlier moment, so a backtest
// parses once and replays many moments.
type Exports struct {
	sources []parsed
	skew    Skew
}

// parsed is one source's issue records with their line numbers.
type parsed struct {
	repo, name string
	other      int // non-issue records skipped
	records    []record
	lines      []int
}

// Parse reads the sources. Malformed JSON or an issue without an ID is an
// error that names the source and line.
func Parse(sources ...Source) (*Exports, error) {
	ex := &Exports{}
	for _, src := range sources {
		p := parsed{repo: src.Repo, name: src.Name}
		err := eachLine(src.Name, src.R, func(line int, data []byte) error {
			var rec record
			if err := json.Unmarshal(data, &rec); err != nil {
				return fmt.Errorf("%s:%d: %w", src.Name, line, err)
			}
			if rec.Type != "" && rec.Type != "issue" {
				p.other++
				return nil
			}
			if rec.ID == "" {
				return fmt.Errorf("%s:%d: issue without an id", src.Name, line)
			}
			p.records = append(p.records, rec)
			p.lines = append(p.lines, line)
			return nil
		})
		if err != nil {
			return nil, err
		}
		ex.sources = append(ex.sources, p)
	}
	ex.skew = ex.detectSkew()
	return ex, nil
}

// Horizon is the latest moment the exports know about: the latest creation,
// update, start or close of any issue record, infra included. It stands in
// for the time the exports were taken.
func (ex *Exports) Horizon() time.Time {
	var h time.Time
	for _, p := range ex.sources {
		for n := range p.records {
			r := &p.records[n]
			for _, t := range []*time.Time{r.CreatedAt, r.UpdatedAt, r.StartedAt, r.ClosedAt} {
				if t != nil && t.After(h) {
					h = t.UTC()
				}
			}
		}
	}
	return h
}

// ActivityTimestamps returns every created/updated/started/closed
// timestamp of every record beadline read, work or infra alike, across
// every source, as visible as of asOf: the cross-repo activity timeline
// idle.Infer masks idle time from (docs/design.md, ADR-3). A zero asOf
// takes every record as loaded, with no rewind. Records are rewound the
// same way Graph rewinds them, so a backtest origin sees only the activity
// it could have known about then (ADR-3 §3): a later timestamp is left
// out, not just filtered, because rewind also undoes an update, start or
// close that happened after asOf. The result is not sorted.
func (ex *Exports) ActivityTimestamps(asOf time.Time) []time.Time {
	var out []time.Time
	for _, p := range ex.sources {
		for n := range p.records {
			rec := p.records[n] // copy: rewind must not mutate the parsed export
			if !asOf.IsZero() && !rec.rewind(asOf, ex.skew) {
				continue
			}
			for _, t := range []*time.Time{rec.CreatedAt, rec.UpdatedAt, rec.StartedAt, rec.ClosedAt} {
				if t != nil && (asOf.IsZero() || !t.After(asOf)) {
					out = append(out, t.UTC())
				}
			}
		}
	}
	return out
}

// Skew is a constant error in dependency created_at timestamps from Since
// on: bd stored them in the server's local time zone but exported them as
// UTC. Dependencies created at or after Since are Offset late.
type Skew struct {
	Since  time.Time
	Offset time.Duration
}

// DependencySkew returns the skew detected in the dependency timestamps;
// the zero Skew when there is none.
func (ex *Exports) DependencySkew() Skew { return ex.skew }

// Skew detection. Most dependencies are created with the later of their two
// beads (bd create --parent, bd dep add right after bd create), so their
// lag behind it clusters near zero. A whole number of hours of lag shared by
// at least skewMinEdges of them, and by skewMinShare of the dependencies
// created since the first of them, is a time-zone error, not a coincidence.
const (
	skewMaxHours  = 14
	skewMinEdges  = 20
	skewMinShare  = 0.1
	skewTolerance = 3 * time.Minute
)

func (ex *Exports) detectSkew() Skew {
	created := map[string]time.Time{}
	for _, p := range ex.sources {
		for n := range p.records {
			if r := &p.records[n]; r.CreatedAt != nil {
				created[r.ID] = r.CreatedAt.UTC()
			}
		}
	}
	type dep struct {
		at  time.Time
		lag time.Duration
	}
	var deps []dep
	for _, p := range ex.sources {
		for n := range p.records {
			r := &p.records[n]
			for _, d := range r.Dependencies {
				at, ok := d.created()
				a, okA := created[r.ID]
				b, okB := created[d.DependsOnID]
				if !ok || !okA || !okB {
					continue
				}
				if b.After(a) {
					a = b
				}
				deps = append(deps, dep{at: at, lag: at.Sub(a)})
			}
		}
	}
	var best Skew
	bestCount := 0
	for h := 1; h <= skewMaxHours; h++ {
		offset := time.Duration(h) * time.Hour
		var since time.Time
		count := 0
		for _, d := range deps {
			if diff := d.lag - offset; diff >= -skewTolerance && diff <= skewTolerance {
				count++
				if since.IsZero() || d.at.Before(since) {
					since = d.at
				}
			}
		}
		if count < skewMinEdges || count <= bestCount {
			continue
		}
		after := 0
		for _, d := range deps {
			if !d.at.Before(since) {
				after++
			}
		}
		if float64(count) >= skewMinShare*float64(after) {
			best, bestCount = Skew{Since: since, Offset: offset}, count
		}
	}
	return best
}

// Graph builds one graph from the parsed sources and applies the
// conventions (see Read). With a zero asOf it is the graph the exports
// show. Otherwise it is the graph as it stood at asOf, as far as the
// exports can tell (docs/design.md, "Calibration"): issues and dependencies
// created later are left out, closes and starts after asOf are undone, and
// an issue updated after asOf is open, or in progress if it had started,
// with no defer_until, since its status then is unknown. Other fields are
// taken as they are now.
func (ex *Exports) Graph(cv *config.Conventions, asOf time.Time) (*graph.Graph, *Report, error) {
	g := graph.New()
	rep := &Report{}
	infra := map[string]bool{}
	later := map[string]bool{} // created after asOf
	type edge struct {
		repo, from, to string
		kind           graph.EdgeKind
	}
	var edges []edge

	for _, p := range ex.sources {
		stats := RepoStats{Repo: p.repo, Source: p.name, Infra: map[string]int{}, Other: p.other}
		for n := range p.records {
			rec := p.records[n]
			if !asOf.IsZero() && !rec.rewind(asOf, ex.skew) {
				later[rec.ID] = true
				continue
			}
			if reason := infraReason(cv, &rec); reason != "" {
				infra[rec.ID] = true
				stats.Infra[reason]++
				continue
			}
			if prev := g.Issue(rec.ID); prev != nil {
				rep.Duplicates = append(rep.Duplicates, Duplicate{
					ID: rec.ID, Kept: prev.Repo, Repo: p.repo, Source: p.name, Line: p.lines[n],
				})
				continue
			}
			if err := g.Add(issue(cv, p.repo, &rec)); err != nil {
				return nil, nil, err
			}
			stats.Issues++
			for _, d := range rec.Dependencies {
				e := edge{repo: p.repo, from: rec.ID, to: d.DependsOnID}
				switch {
				case d.Type == parentChildType:
					e.kind = graph.ParentChild
				case blockingTypes[d.Type]:
					e.kind = graph.Blocking
				default:
					continue
				}
				edges = append(edges, e)
			}
		}
		rep.Repos = append(rep.Repos, stats)
	}

	// Edges are resolved once every source is in: they cross repositories.
	for _, e := range edges {
		switch {
		case g.Issue(e.to) != nil:
			if err := g.Link(e.kind, e.from, e.to); err != nil {
				return nil, nil, err
			}
		case !infra[e.to] && !later[e.to]:
			rep.Dangling = append(rep.Dangling, Dangling{Repo: e.repo, From: e.from, To: e.to, Kind: e.kind})
		}
	}
	rep.Cycles = g.Cycles()
	return g, rep, nil
}

// rewind turns r into the record as it stood at t and reports whether it
// existed then. A record or dependency without created_at is assumed to
// have existed.
func (r *record) rewind(t time.Time, skew Skew) bool {
	if r.CreatedAt != nil && r.CreatedAt.After(t) {
		return false
	}
	after := func(x *time.Time) bool { return x != nil && x.After(t) }
	// Unchanged since t: its status then is its status now.
	unchanged := r.UpdatedAt != nil && !after(r.UpdatedAt) && !after(r.StartedAt) && !after(r.ClosedAt)
	closed := r.ClosedAt != nil && !after(r.ClosedAt)
	if after(r.ClosedAt) {
		r.ClosedAt, r.CloseReason = nil, ""
	}
	if after(r.StartedAt) {
		r.StartedAt = nil
	}
	if after(r.UpdatedAt) {
		r.UpdatedAt = nil
	}
	switch {
	case closed:
		r.Status = graph.StatusClosed
	case unchanged:
	case r.StartedAt != nil:
		r.Status = graph.StatusInProgress
	default:
		r.Status = statusOpen
	}
	if !unchanged {
		r.DeferUntil = nil
	}
	var deps []dependency
	for _, d := range r.Dependencies {
		if at, ok := d.created(); !ok || !skew.correct(at).After(t) {
			deps = append(deps, d)
		}
	}
	r.Dependencies = deps
	return true
}

// correct removes the skew from a dependency timestamp.
func (s Skew) correct(t time.Time) time.Time {
	if s.Offset == 0 || t.Before(s.Since) {
		return t
	}
	return t.Add(-s.Offset)
}

// infraReason says why a record is orchestration rather than work, or "".
func infraReason(cv *config.Conventions, r *record) string {
	switch {
	case cv.IsInfra(r.IssueType):
		return r.IssueType
	case r.Ephemeral:
		return "ephemeral"
	case r.IsTemplate:
		return "template"
	}
	return ""
}

func issue(cv *config.Conventions, repo string, r *record) *graph.Issue {
	return &graph.Issue{
		ID:               r.ID,
		Repo:             repo,
		Title:            r.Title,
		Description:      r.Description,
		Type:             r.IssueType,
		Status:           r.Status,
		CloseReason:      r.CloseReason,
		Priority:         r.Priority,
		Assignee:         r.Assignee,
		Labels:           r.Labels,
		Metadata:         r.Metadata,
		CreatedAt:        utc(r.CreatedAt),
		UpdatedAt:        utc(r.UpdatedAt),
		StartedAt:        utc(r.StartedAt),
		ClosedAt:         utc(r.ClosedAt),
		DueAt:            utc(r.DueAt),
		DeferUntil:       utc(r.DeferUntil),
		EstimatedMinutes: r.EstimatedMinutes,
		HighLevel:        cv.IsHighLevel(r.IssueType),
		HumanGate:        cv.IsHumanGate(r.Title, r.Metadata),
		Goals:            cv.Goals(r.Labels),
	}
}

func utc(t *time.Time) time.Time {
	if t == nil {
		return time.Time{}
	}
	return t.UTC()
}

// eachLine calls fn with every non-blank line and its 1-based number. Lines
// may be of any length: descriptions and comments are unbounded.
func eachLine(name string, r io.Reader, fn func(line int, data []byte) error) error {
	br := bufio.NewReader(r)
	for line := 1; ; line++ {
		data, err := br.ReadBytes('\n')
		if len(bytes.TrimSpace(data)) > 0 {
			if ferr := fn(line, data); ferr != nil {
				return ferr
			}
		}
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("%s:%d: %w", name, line, err)
		}
	}
}
