package calibrate

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/alexknips/beadline/internal/forecast"
	"github.com/alexknips/beadline/internal/graph"
	"github.com/alexknips/beadline/internal/roadmap"
)

// Schema names the snapshot format. A reader rejects a newer version
// rather than misread it; adding a field keeps the version.
const Schema = "beadline.snapshot/v1"

const schemaPrefix = "beadline.snapshot/v"

// Levels is the quantile grid of every snapshot and backtest forecast:
// enough levels for the PIT and CRPS, including the three whose coverage is
// reported.
var Levels = []float64{0.05, 0.10, 0.20, 0.25, 0.30, 0.40, 0.50, 0.60, 0.70, 0.75, 0.80, 0.90, 0.95, 0.99}

// Grid is a forecast as quantiles of the time to close, in hours after the
// snapshot's as_of, keyed by level: "p05" … "p99".
type Grid map[string]float64

// levelKey is the Grid key of a level: 0.05 is "p05".
func levelKey(q float64) string { return fmt.Sprintf("p%02d", int(math.Round(q*100))) }

// NewGrid pairs levels with values.
func NewGrid(levels, hours []float64) Grid {
	if len(levels) == 0 || len(levels) != len(hours) {
		return nil
	}
	g := make(Grid, len(levels))
	for n, q := range levels {
		g[levelKey(q)] = hours[n]
	}
	return g
}

// point is one level of a grid.
type point struct{ q, hours float64 }

// points returns the grid's levels in ascending order; keys that are not
// levels are ignored.
func (g Grid) points() []point {
	var out []point
	for k, v := range g {
		n, err := strconv.Atoi(strings.TrimPrefix(k, "p"))
		if err != nil || !strings.HasPrefix(k, "p") || n <= 0 || n >= 100 {
			continue
		}
		out = append(out, point{q: float64(n) / 100, hours: v})
	}
	sort.Slice(out, func(a, b int) bool { return out[a].q < out[b].q })
	return out
}

// at returns the value at level q.
func (g Grid) at(q float64) (float64, bool) {
	v, ok := g[levelKey(q)]
	return v, ok
}

// Snapshot is one forecast run, frozen: what was forecast, as of when, from
// which inputs, with which model and configuration. It is written once and
// never changed; ContentSHA256 lets a reader tell.
type Snapshot struct {
	Schema      string    `json:"schema"`
	GeneratedAt time.Time `json:"generated_at"` // when the snapshot was written
	// AsOf is the forecast's now: the data horizon. Quantiles count from it.
	AsOf            time.Time `json:"as_of"`
	BeadlineVersion string    `json:"beadline_version"`
	BeadlineCommit  string    `json:"beadline_commit,omitempty"`
	ModelVersion    string    `json:"model_version"`
	Seed            uint64    `json:"seed"`
	Runs            int       `json:"runs"`
	// Config is beadline.toml as used, and ConfigSHA256 the hash of its
	// JSON here.
	Config       roadmap.Config `json:"config"`
	ConfigSHA256 string         `json:"config_sha256"`
	Inputs       roadmap.Inputs `json:"inputs"`
	// Items are the open high-level beads and goals, with or without a
	// forecast; Leaves the open leaf beads the schedule covered.
	Items  []Target `json:"items"`
	Leaves []Target `json:"leaves"`
	// ContentSHA256 is the SHA-256 of the snapshot's canonical JSON without
	// this field: its top-level members in key order, compact, as Go's
	// encoding/json writes them.
	ContentSHA256 string `json:"content_sha256"`
}

// Target is one forecast in a snapshot. Leaves carry only an ID, a repo and
// quantiles.
type Target struct {
	ID    string `json:"id"`
	Title string `json:"title,omitempty"`
	Repo  string `json:"repo,omitempty"`
	Type  string `json:"type,omitempty"`
	Goal  bool   `json:"goal,omitempty"`
	// Status is the item's forecast status; only forecast, blocked_outside
	// and stalled carry quantiles.
	Status string `json:"status,omitempty"`
	// DescendantIDs is the item's breakdown at as_of: its descendants, and
	// for a goal also its members. Comparing it with the breakdown at close
	// tells model error from scope change.
	DescendantIDs []string `json:"descendant_ids,omitempty"`
	// RemainingIDs is the open work the forecast covered.
	RemainingIDs []string `json:"remaining_ids,omitempty"`
	// P50, P80 and P95 repeat three quantiles as dates, for people.
	P50       *time.Time `json:"p50,omitempty"`
	P80       *time.Time `json:"p80,omitempty"`
	P95       *time.Time `json:"p95,omitempty"`
	Quantiles Grid       `json:"quantile_hours,omitempty"`
	// Assumptions say what the dates leave out.
	Assumptions []string `json:"assumptions,omitempty"`
}

// Meta is what a snapshot records besides the forecast.
type Meta struct {
	GeneratedAt     time.Time
	BeadlineVersion string
	BeadlineCommit  string
	Config          roadmap.Config
	Inputs          roadmap.Inputs
}

// New builds the snapshot of a forecast of g made with Options.Grid set.
func New(res *forecast.Result, g *graph.Graph, m Meta) (*Snapshot, error) {
	if len(res.Grid) == 0 {
		return nil, errors.New("calibrate: the forecast has no quantile grid")
	}
	cfg, err := json.Marshal(m.Config)
	if err != nil {
		return nil, err
	}
	sum := sha256.Sum256(cfg)
	s := &Snapshot{
		Schema:          Schema,
		GeneratedAt:     m.GeneratedAt.UTC().Truncate(time.Second),
		AsOf:            res.Now.UTC(),
		BeadlineVersion: m.BeadlineVersion,
		BeadlineCommit:  m.BeadlineCommit,
		ModelVersion:    forecast.ModelVersion,
		Seed:            res.Seed,
		Runs:            res.Runs,
		Config:          m.Config,
		ConfigSHA256:    hex.EncodeToString(sum[:]),
		Inputs:          m.Inputs,
		Items:           []Target{},
		Leaves:          []Target{},
	}
	goals := map[string]graph.Goal{}
	for _, gl := range g.Goals() {
		goals[gl.ID] = gl
	}
	add := func(it forecast.Item, goal bool) {
		t := Target{ID: it.ID, Title: it.Title, Repo: it.Repo, Type: it.Type, Goal: goal, Status: string(it.Status),
			RemainingIDs: it.Remaining, Assumptions: assumptions(it)}
		if goal {
			t.DescendantIDs = goalBreakdown(g, goals[it.ID])
		} else {
			t.DescendantIDs = breakdown(g, it.ID)
		}
		if it.GridHours != nil && it.P50 != nil {
			t.Quantiles = NewGrid(res.Grid, it.GridHours)
			t.P50, t.P80, t.P95 = timePtr(it.P50.At), timePtr(it.P80.At), timePtr(it.P95.At)
		}
		s.Items = append(s.Items, t)
	}
	for _, it := range res.Items {
		add(it, false)
	}
	for _, it := range res.Goals {
		add(it, true)
	}
	for _, l := range res.Leaves {
		s.Leaves = append(s.Leaves, Target{ID: l.ID, Repo: l.Repo, Quantiles: NewGrid(res.Grid, l.GridHours)})
	}
	return s, nil
}

func timePtr(t time.Time) *time.Time {
	t = t.UTC()
	return &t
}

// assumptions says what an item's dates leave out.
func assumptions(it forecast.Item) []string {
	var out []string
	if len(it.Outside) > 0 {
		out = append(out, fmt.Sprintf("waits on %s, which no loaded repo has: the dates cover the loaded work only",
			strings.Join(it.Outside, ", ")))
	}
	if len(it.Stuck) > 0 {
		out = append(out, fmt.Sprintf("%d bead(s) can never start (a blocking cycle, or a parked blocker): the dates cover the rest",
			len(it.Stuck)))
	}
	if len(it.Deferred) > 0 && it.Status != forecast.StatusDeferred {
		out = append(out, fmt.Sprintf("%d parked (deferred) bead(s) are left out", len(it.Deferred)))
	}
	return out
}

// breakdown returns the IDs of a bead's descendants, sorted.
func breakdown(g *graph.Graph, id string) []string {
	out := []string{}
	for _, d := range g.Descendants(id) {
		out = append(out, d.ID)
	}
	sort.Strings(out)
	return out
}

// goalBreakdown returns a goal's members and every descendant of them and
// of its own bead, sorted.
func goalBreakdown(g *graph.Graph, gl graph.Goal) []string {
	set := map[string]bool{}
	if gl.Issue != nil {
		for _, d := range g.Descendants(gl.ID) {
			set[d.ID] = true
		}
	}
	for _, m := range gl.Members {
		if m.ID == gl.ID {
			continue
		}
		set[m.ID] = true
		for _, d := range g.Descendants(m.ID) {
			set[d.ID] = true
		}
	}
	delete(set, gl.ID)
	out := make([]string, 0, len(set))
	for id := range set {
		out = append(out, id)
	}
	sort.Strings(out)
	return out
}

// contentHash is the SHA-256 of a snapshot's JSON without content_sha256:
// the top-level members sorted by key, each as written, compacted.
func contentHash(data []byte) (string, error) {
	var top map[string]json.RawMessage
	if err := json.Unmarshal(data, &top); err != nil {
		return "", err
	}
	delete(top, "content_sha256")
	canonical, err := json.Marshal(top)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(canonical)
	return hex.EncodeToString(sum[:]), nil
}

// Seal sets ContentSHA256 and returns the snapshot's file contents.
func (s *Snapshot) Seal() ([]byte, error) {
	s.ContentSHA256 = ""
	body, err := json.Marshal(s)
	if err != nil {
		return nil, err
	}
	if s.ContentSHA256, err = contentHash(body); err != nil {
		return nil, err
	}
	var buf strings.Builder
	enc := json.NewEncoder(&buf)
	enc.SetIndent("", " ")
	enc.SetEscapeHTML(false)
	if err := enc.Encode(s); err != nil {
		return nil, err
	}
	return []byte(buf.String()), nil
}

// fileTime names snapshot files: they sort by as_of.
const fileTime = "20060102T150405Z"

// Record writes s into dir, creating dir if needed, as a new read-only file
// named after its as_of and content hash. It never replaces a file. A dir
// that already holds a snapshot as of the same UTC day, from the same model
// and configuration, keeps it: one snapshot a day is enough to grade, and
// more would weigh the days a forecast ran often. Record then writes
// nothing and returns that snapshot's path as existing.
func Record(dir string, s *Snapshot) (path, existing string, err error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", "", err
	}
	day := s.AsOf.UTC().Format("20060102")
	entries, err := os.ReadDir(dir)
	if err != nil {
		return "", "", err
	}
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasPrefix(name, day+"T") || !strings.HasSuffix(name, ".json") {
			continue
		}
		prev, err := readHeader(filepath.Join(dir, name))
		if err == nil && prev.ModelVersion == s.ModelVersion && prev.ConfigSHA256 == s.ConfigSHA256 &&
			prev.AsOf.UTC().Format("20060102") == day {
			return "", filepath.Join(dir, name), nil
		}
	}

	data, err := s.Seal()
	if err != nil {
		return "", "", err
	}
	path = filepath.Join(dir, s.AsOf.UTC().Format(fileTime)+"_"+s.ContentSHA256[:12]+".json")
	// Write a temporary file, then link it into place: the snapshot appears
	// whole or not at all, and an existing file is never replaced.
	tmp, err := os.CreateTemp(dir, ".snapshot-*.tmp")
	if err != nil {
		return "", "", err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return "", "", err
	}
	if err := tmp.Close(); err != nil {
		return "", "", err
	}
	if err := os.Chmod(tmp.Name(), 0o444); err != nil {
		return "", "", err
	}
	if err := os.Link(tmp.Name(), path); err != nil {
		return "", "", err
	}
	return path, "", nil
}

// readHeader reads a snapshot's top-level fields without checking it.
func readHeader(path string) (*Snapshot, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var s Snapshot
	if err := json.Unmarshal(data, &s); err != nil {
		return nil, err
	}
	return &s, nil
}

// Loaded is a snapshot read from a file.
type Loaded struct {
	Path     string
	Snapshot *Snapshot
}

// Read decodes and checks one snapshot: its schema, its layout and its
// content hash.
func Read(data []byte) (*Snapshot, error) {
	var top map[string]json.RawMessage
	if err := json.Unmarshal(data, &top); err != nil {
		return nil, err
	}
	var schema string
	json.Unmarshal(top["schema"], &schema)
	switch {
	case !strings.HasPrefix(schema, schemaPrefix):
		return nil, fmt.Errorf("not a beadline snapshot (schema %q)", schema)
	case schema != Schema:
		return nil, fmt.Errorf("schema %s is newer than this beadline reads (%s); upgrade beadline", schema, Schema)
	}
	var missing []string
	for _, key := range []string{"as_of", "model_version", "items", "leaves", "content_sha256"} {
		if _, ok := top[key]; !ok {
			missing = append(missing, key)
		}
	}
	if len(missing) > 0 {
		return nil, fmt.Errorf("not laid out as beadline writes %s: no %s", Schema, strings.Join(missing, ", "))
	}
	var s Snapshot
	if err := json.Unmarshal(data, &s); err != nil {
		return nil, err
	}
	sum, err := contentHash(data)
	if err != nil {
		return nil, err
	}
	if sum != s.ContentSHA256 {
		return nil, errors.New("content hash does not match: the file was changed after it was recorded")
	}
	return &s, nil
}

// ReadDir reads every *.json snapshot in dir, sorted by as_of and then by
// name. A missing dir holds none. A file that is not a valid snapshot is a
// problem, reported and skipped, not an error: the others still grade.
func ReadDir(dir string) (snaps []Loaded, problems []error, err error) {
	entries, err := os.ReadDir(dir)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil, nil
	}
	if err != nil {
		return nil, nil, err
	}
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		path := filepath.Join(dir, e.Name())
		data, err := os.ReadFile(path)
		if err != nil {
			return nil, nil, err
		}
		s, err := Read(data)
		if err != nil {
			problems = append(problems, fmt.Errorf("%s: %w", path, err))
			continue
		}
		snaps = append(snaps, Loaded{Path: path, Snapshot: s})
	}
	sort.SliceStable(snaps, func(a, b int) bool {
		if !snaps[a].Snapshot.AsOf.Equal(snaps[b].Snapshot.AsOf) {
			return snaps[a].Snapshot.AsOf.Before(snaps[b].Snapshot.AsOf)
		}
		return snaps[a].Path < snaps[b].Path
	})
	return snaps, problems, nil
}
