package load

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/alexknips/beadline/internal/config"
	"github.com/alexknips/beadline/internal/graph"
)

// blockingTypes are the dependency types bd treats as hard blockers. They all
// become graph.Blocking edges. Other types (related, discovered-from, tracks,
// ...) carry no scheduling meaning and are ignored.
var blockingTypes = map[string]bool{"blocks": true, "conditional-blocks": true, "waits-for": true}

const parentChildType = "parent-child"

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
	Dependencies     []struct {
		DependsOnID string `json:"depends_on_id"`
		Type        string `json:"type"`
	} `json:"dependencies"`
}

// Load reads every repository export named in cfg, in config order.
func Load(cfg *config.Config) (*graph.Graph, *Report, error) {
	var sources []Source
	for _, r := range cfg.Repos {
		path := r.ExportPath(cfg)
		f, err := os.Open(path)
		if err != nil {
			return nil, nil, fmt.Errorf("repo %s: %w", r.Name, err)
		}
		defer f.Close()
		sources = append(sources, Source{Repo: r.Name, Name: path, R: f})
	}
	return Read(&cfg.Conventions, sources...)
}

// Read builds one graph from the sources and applies the conventions:
// infra records (by type, ephemeral or template) are dropped, high-level
// types, goal labels and human gates are marked on each issue. Malformed
// JSON or a record without an ID is an error; graph problems (dangling
// edges, duplicate IDs, cycles) go into the report.
func Read(cv *config.Conventions, sources ...Source) (*graph.Graph, *Report, error) {
	g := graph.New()
	rep := &Report{}
	infra := map[string]bool{}
	type edge struct {
		repo, from, to string
		kind           graph.EdgeKind
	}
	var edges []edge

	for _, src := range sources {
		stats := RepoStats{Repo: src.Repo, Source: src.Name, Infra: map[string]int{}}
		err := eachLine(src.Name, src.R, func(line int, data []byte) error {
			var rec record
			if err := json.Unmarshal(data, &rec); err != nil {
				return fmt.Errorf("%s:%d: %w", src.Name, line, err)
			}
			if rec.Type != "" && rec.Type != "issue" {
				stats.Other++
				return nil
			}
			if rec.ID == "" {
				return fmt.Errorf("%s:%d: issue without an id", src.Name, line)
			}
			if reason := infraReason(cv, &rec); reason != "" {
				infra[rec.ID] = true
				stats.Infra[reason]++
				return nil
			}
			if prev := g.Issue(rec.ID); prev != nil {
				rep.Duplicates = append(rep.Duplicates, Duplicate{
					ID: rec.ID, Kept: prev.Repo, Repo: src.Repo, Source: src.Name, Line: line,
				})
				return nil
			}
			if err := g.Add(issue(cv, src.Repo, &rec)); err != nil {
				return err
			}
			stats.Issues++
			for _, d := range rec.Dependencies {
				e := edge{repo: src.Repo, from: rec.ID, to: d.DependsOnID}
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
			return nil
		})
		if err != nil {
			return nil, nil, err
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
		case !infra[e.to]:
			rep.Dangling = append(rep.Dangling, Dangling{Repo: e.repo, From: e.from, To: e.to, Kind: e.kind})
		}
	}
	rep.Cycles = g.Cycles()
	return g, rep, nil
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
