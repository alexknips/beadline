package calibrate

import (
	"bytes"
	"encoding/json"
	"math/rand/v2"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/alexknips/beadline/internal/config"
	"github.com/alexknips/beadline/internal/forecast"
	"github.com/alexknips/beadline/internal/graph"
	"github.com/alexknips/beadline/internal/load"
	"github.com/alexknips/beadline/internal/roadmap"
)

var now = time.Date(2026, 9, 19, 12, 0, 0, 0, time.UTC)

// constant is a sampler with fixed durations, in minutes.
type constant struct{ q, w float64 }

func (c constant) Draw(*rand.Rand) (float64, float64) { return c.q, c.w }

// parse parses JSONL lines as the export of repo "r".
func parse(t testing.TB, lines ...string) *load.Exports {
	t.Helper()
	ex, err := load.Parse(load.Source{Repo: "r", Name: "r.jsonl", R: strings.NewReader(strings.Join(lines, "\n"))})
	if err != nil {
		t.Fatal(err)
	}
	return ex
}

func graphOf(t testing.TB, ex *load.Exports, at time.Time) *graph.Graph {
	t.Helper()
	cv := config.Default().Conventions
	g, _, err := ex.Graph(&cv, at)
	if err != nil {
		t.Fatal(err)
	}
	return g
}

// run forecasts g with an agent working w minutes per bead after a queue
// of q, one agent, and human gates that take an hour.
func run(t testing.TB, g *graph.Graph, at time.Time, q, w float64) *forecast.Result {
	t.Helper()
	res, err := forecast.Run(g, forecast.Options{
		Now: at, Runs: 20, Seed: 1, Grid: Levels, Concurrency: map[string]int{"r": 1},
		Agent: func(*graph.Issue) forecast.Sampler { return constant{q, w} },
		Human: func(*graph.Issue, float64) forecast.Draw { return func(*rand.Rand) float64 { return 60 } },
	})
	if err != nil {
		t.Fatal(err)
	}
	return res
}

// fixture is a small repo: epic e with a closed and two open children, a
// goal g-1 on the epic, a parked bead and a stray open task.
var fixture = []string{
	`{"id":"e","title":"Epic <one> & \"two\"","issue_type":"epic","status":"open","labels":["goal:g-1"],"created_at":"2026-09-10T00:00:00Z"}`,
	`{"id":"a","status":"closed","created_at":"2026-09-10T00:00:00Z","closed_at":"2026-09-11T00:00:00Z","dependencies":[{"depends_on_id":"e","type":"parent-child"}]}`,
	`{"id":"b","status":"open","created_at":"2026-09-10T00:00:00Z","dependencies":[{"depends_on_id":"e","type":"parent-child"}]}`,
	`{"id":"c","status":"open","created_at":"2026-09-10T00:00:00Z","dependencies":[{"depends_on_id":"e","type":"parent-child"},{"depends_on_id":"b","type":"blocks"}]}`,
	`{"id":"p","status":"deferred","created_at":"2026-09-10T00:00:00Z","dependencies":[{"depends_on_id":"e","type":"parent-child"}]}`,
	`{"id":"x","status":"open","created_at":"2026-09-10T00:00:00Z"}`,
}

func newSnapshot(t testing.TB) *Snapshot {
	t.Helper()
	g := graphOf(t, parse(t, fixture...), time.Time{})
	s, err := New(run(t, g, now, 0, 60), g, Meta{GeneratedAt: now.Add(time.Minute), BeadlineVersion: "v0.0.1", BeadlineCommit: "abc",
		Config: roadmap.Config{Repos: []roadmap.ConfigRepo{{Name: "r", Export: "r.jsonl"}}}, Inputs: roadmap.Inputs{Fingerprint: "sha256:00"}})
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func TestNewSnapshot(t *testing.T) {
	s := newSnapshot(t)
	if s.Schema != Schema || !s.AsOf.Equal(now) || s.ModelVersion != forecast.ModelVersion || s.Runs != 20 || s.Seed != 1 ||
		s.BeadlineVersion != "v0.0.1" || s.BeadlineCommit != "abc" || len(s.ConfigSHA256) != 64 || s.Inputs.Fingerprint != "sha256:00" {
		t.Errorf("header = %+v", s)
	}
	if len(s.Items) != 2 || s.Items[0].ID != "e" || s.Items[1].ID != "g-1" {
		t.Fatalf("items = %+v; want the epic, then its goal", s.Items)
	}
	e, goal := s.Items[0], s.Items[1]
	if e.Goal || e.Status != "forecast" || !reflect.DeepEqual(e.DescendantIDs, []string{"a", "b", "c", "p"}) ||
		!reflect.DeepEqual(e.RemainingIDs, []string{"b", "c", "p"}) || e.Title != `Epic <one> & "two"` {
		t.Errorf("e = %+v", e)
	}
	// A goal's breakdown holds its members too.
	if !goal.Goal || !reflect.DeepEqual(goal.DescendantIDs, []string{"a", "b", "c", "e", "p"}) || goal.Quantiles["p50"] != 2 {
		t.Errorf("goal = %+v", goal)
	}
	// One agent, an hour a bead: b then c.
	if got := e.Quantiles; len(got) != len(Levels) || got["p05"] != 2 || got["p99"] != 2 {
		t.Errorf("e quantiles = %v, want 2 h at every level", got)
	}
	if e.P50 == nil || !e.P50.Equal(now.Add(2*time.Hour)) || e.P95 == nil {
		t.Errorf("e dates = %v %v %v", e.P50, e.P80, e.P95)
	}
	if want := []string{"1 parked (deferred) bead(s) are left out"}; !reflect.DeepEqual(e.Assumptions, want) {
		t.Errorf("assumptions = %q", e.Assumptions)
	}
	var leaves []string
	for _, l := range s.Leaves {
		leaves = append(leaves, l.ID)
		if l.Repo != "r" || len(l.Quantiles) != len(Levels) || l.Title != "" {
			t.Errorf("leaf %+v", l)
		}
	}
	if !reflect.DeepEqual(leaves, []string{"b", "c", "x"}) {
		t.Errorf("leaves = %v; want the open work, not the parked bead", leaves)
	}

	if _, err := New(&forecast.Result{}, graph.New(), Meta{}); err == nil {
		t.Error("a forecast without a grid made a snapshot")
	}
}

func TestSealAndRead(t *testing.T) {
	s := newSnapshot(t)
	data, err := s.Seal()
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(data, []byte(`Epic <one> & \"two\"`)) {
		t.Errorf("titles should be written as they are:\n%.400s", data)
	}
	got, err := Read(data)
	if err != nil {
		t.Fatal(err)
	}
	if got.ContentSHA256 != s.ContentSHA256 || len(s.ContentSHA256) != 64 || !reflect.DeepEqual(got.Items, s.Items) {
		t.Errorf("round trip changed the snapshot")
	}
	// Sealing is deterministic.
	again, _ := s.Seal()
	if !bytes.Equal(again, data) {
		t.Error("sealing twice gave different files")
	}

	// Whitespace does not matter; content does.
	var compact bytes.Buffer
	if err := json.Compact(&compact, data); err != nil {
		t.Fatal(err)
	}
	if _, err := Read(compact.Bytes()); err != nil {
		t.Errorf("compacted snapshot: %v", err)
	}
	tampered := bytes.Replace(data, []byte(`"p50": 2`), []byte(`"p50": 3`), 1)
	if bytes.Equal(tampered, data) {
		t.Fatal("test did not tamper")
	}
	if _, err := Read(tampered); err == nil || !strings.Contains(err.Error(), "content hash") {
		t.Errorf("tampered snapshot: err = %v", err)
	}
	newer := bytes.Replace(data, []byte(Schema), []byte("beadline.snapshot/v2"), 1)
	if _, err := Read(newer); err == nil || !strings.Contains(err.Error(), "newer") {
		t.Errorf("newer schema: err = %v", err)
	}
	if _, err := Read([]byte(`{"schema_version":1}`)); err == nil || !strings.Contains(err.Error(), "not a beadline snapshot") {
		t.Errorf("roadmap.json: err = %v", err)
	}
	// The research prototype wrote the same schema name with other fields.
	proto := `{"schema":"beadline.snapshot/v1","as_of":"2026-09-19T07:09Z","model":{"name":"lead_km"},"targets":[],"content_sha256":"00"}`
	if _, err := Read([]byte(proto)); err == nil || !strings.Contains(err.Error(), "no model_version, items, leaves") {
		t.Errorf("prototype snapshot: err = %v", err)
	}
}

func TestRecord(t *testing.T) {
	dir := filepath.Join(t.TempDir(), ".beadline", "snapshots")
	s := newSnapshot(t)
	path, existing, err := Record(dir, s)
	if err != nil || existing != "" {
		t.Fatalf("Record = %q, %q, %v", path, existing, err)
	}
	if want := "20260919T120000Z_" + s.ContentSHA256[:12] + ".json"; filepath.Base(path) != want {
		t.Errorf("file %s, want %s", filepath.Base(path), want)
	}
	if info, err := os.Stat(path); err != nil || info.Mode().Perm() != 0o444 {
		t.Errorf("file mode = %v, %v; want read-only", info.Mode(), err)
	}

	// Later the same day, same model and config: kept, nothing written.
	later := newSnapshot(t)
	later.AsOf = now.Add(5 * time.Hour)
	if p, existing, err := Record(dir, later); err != nil || p != "" || existing != path {
		t.Errorf("same day: Record = %q, %q, %v; want the first kept", p, existing, err)
	}
	// Another config, or another day: recorded.
	other := newSnapshot(t)
	other.AsOf, other.ConfigSHA256 = now.Add(time.Hour), strings.Repeat("0", 64)
	next := newSnapshot(t)
	next.AsOf = now.Add(24 * time.Hour)
	for _, s := range []*Snapshot{other, next} {
		if p, existing, err := Record(dir, s); err != nil || p == "" || existing != "" {
			t.Errorf("Record(%v) = %q, %q, %v", s.AsOf, p, existing, err)
		}
	}

	os.WriteFile(filepath.Join(dir, "notes.txt"), []byte("ignored"), 0o644)
	os.WriteFile(filepath.Join(dir, "zz-broken.json"), []byte(`{"schema":"beadline.snapshot/v1","as_of":"2026-09-19T12:00:00Z","model_version":"x","items":[],"leaves":[],"content_sha256":"00"}`), 0o644)
	snaps, problems, err := ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var order []time.Time
	for _, l := range snaps {
		order = append(order, l.Snapshot.AsOf)
	}
	if !reflect.DeepEqual(order, []time.Time{now, now.Add(time.Hour), now.Add(24 * time.Hour)}) {
		t.Errorf("snapshots as of %v, want sorted", order)
	}
	if len(problems) != 1 || !strings.Contains(problems[0].Error(), "zz-broken.json: content hash") {
		t.Errorf("problems = %v", problems)
	}
	if entries, _ := os.ReadDir(dir); len(entries) != 5 {
		t.Errorf("dir holds %d entries: temporary files left behind?", len(entries))
	}

	snaps, problems, err = ReadDir(filepath.Join(dir, "missing"))
	if snaps != nil || problems != nil || err != nil {
		t.Errorf("missing dir: %v, %v, %v", snaps, problems, err)
	}
}
