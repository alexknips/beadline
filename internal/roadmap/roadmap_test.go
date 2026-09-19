package roadmap

import (
	"bytes"
	"context"
	"errors"
	"flag"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/alexknips/beadline/internal/config"
	"github.com/alexknips/beadline/internal/graph"
	"github.com/alexknips/beadline/internal/load"
)

var update = flag.Bool("update", false, "rewrite the golden files in testdata")

// now is the fixed "now" of the fixture runs.
var now = time.Date(2026, 9, 19, 12, 0, 0, 0, time.UTC)

func fixture(t *testing.T, name string) (*config.Config, *graph.Graph, *load.Report) {
	t.Helper()
	cfg, err := config.Load(filepath.Join("..", "..", "testdata", name, "beadline.toml"))
	if err != nil {
		t.Fatal(err)
	}
	g, rep, err := load.Load(cfg)
	if err != nil {
		t.Fatal(err)
	}
	return cfg, g, rep
}

func golden(t *testing.T, name string, got []byte) {
	t.Helper()
	path := filepath.Join("testdata", name)
	if *update {
		if err := os.WriteFile(path, got, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("%v (run go test -update to create it)", err)
	}
	if !bytes.Equal(got, want) {
		t.Errorf("output differs from %s (run go test -update and review the diff):\n%s", path, got)
	}
}

func milestone(t *testing.T, r *Roadmap, id string) Milestone {
	t.Helper()
	for _, m := range r.Milestones {
		if m.ID == id {
			return m
		}
	}
	t.Fatalf("milestone %s not on the roadmap", id)
	return Milestone{}
}

func goal(t *testing.T, r *Roadmap, id string) Goal {
	t.Helper()
	for _, g := range r.Goals {
		if g.ID == id {
			return g
		}
	}
	t.Fatalf("goal %s not on the roadmap", id)
	return Goal{}
}

func TestBuildMultirepo(t *testing.T) {
	cfg, g, rep := fixture(t, "multirepo")
	r := Build(g, rep, cfg, now)
	r.BeadlineVersion = "test"
	in, err := Fingerprint(cfg)
	if err != nil {
		t.Fatal(err)
	}
	r.Inputs = in

	m1 := milestone(t, r, "api-m1")
	if m1.Status != StatusForecast || m1.Total != 6 || m1.Done != 2 || m1.DonePct != 33.3 || m1.HumanGates != 1 ||
		!reflect.DeepEqual(m1.RemainingIDs, []string{"api-3", "api-4", "api-5", "api-e1"}) {
		t.Errorf("api-m1 = %+v", m1.Outlook)
	}
	if m1.TargetDueAt == nil || !m1.TargetDueAt.Equal(time.Date(2026, 10, 15, 0, 0, 0, 0, time.UTC)) || m1.Schedule != "" {
		t.Errorf("api-m1 target %v schedule %q: want 2026-10-15 and no schedule before a forecast", m1.TargetDueAt, m1.Schedule)
	}
	// web-e1's own children are all open, but they wait on api-4 (and so
	// api-3) and on the human gate hq-h1 in other repos.
	e1 := milestone(t, r, "web-e1")
	if !reflect.DeepEqual(e1.RemainingIDs, []string{"api-3", "api-4", "hq-h1", "web-1", "web-2", "web-3"}) || e1.HumanGates != 2 {
		t.Errorf("web-e1 = %+v", e1.Outlook)
	}
	if e2 := milestone(t, r, "web-e2"); e2.Status != StatusDone || e2.ClosedAt == nil || e2.DonePct != 100 {
		t.Errorf("web-e2 = %+v", e2.Outlook)
	}

	g1 := goal(t, r, "hq-g1")
	if g1.Title != "Public launch" || g1.Repo != "hq" || !reflect.DeepEqual(g1.Members, []string{"api-m1", "web-e1"}) ||
		g1.Total != 12 || g1.Remaining != 10 || g1.HumanGates != 3 || g1.Status != StatusForecast {
		t.Errorf("hq-g1 = %+v", g1)
	}
	// hq-g2's bead is not loaded: it is done when all its work is.
	g2 := goal(t, r, "hq-g2")
	if g2.Title != "hq-g2" || g2.Repo != "" || g2.Status != StatusDone ||
		g2.ClosedAt == nil || !g2.ClosedAt.Equal(time.Date(2026, 8, 20, 17, 0, 0, 0, time.UTC)) {
		t.Errorf("hq-g2 = %+v", g2)
	}

	if len(r.Repos) != 3 || r.Repos[0].Issues != 7 || r.Repos[0].Open != 5 || *r.Repos[0].Concurrency != 3 ||
		r.Repos[1].Concurrency != nil || r.Repos[1].ConcurrencySource != "measured" {
		t.Errorf("repos = %+v", r.Repos)
	}

	var buf bytes.Buffer
	if err := Write(&buf, r); err != nil {
		t.Fatal(err)
	}
	golden(t, "multirepo.json", buf.Bytes())
}

// issueGraph builds a graph from issues and [kind, from, to] edges.
func issueGraph(t *testing.T, issues []*graph.Issue, edges [][3]string) *graph.Graph {
	t.Helper()
	g := graph.New()
	for _, i := range issues {
		if i.Repo == "" {
			i.Repo = "r"
		}
		if err := g.Add(i); err != nil {
			t.Fatal(err)
		}
	}
	for _, e := range edges {
		if err := g.Link(graph.EdgeKind(e[0]), e[1], e[2]); err != nil {
			t.Fatal(err)
		}
	}
	return g
}

func TestBuildStatus(t *testing.T) {
	cfg, err := config.Parse(`repos = ["r.jsonl"]`)
	if err != nil {
		t.Fatal(err)
	}
	hl := func(id, status string) *graph.Issue {
		return &graph.Issue{ID: id, Title: id, Type: "epic", Status: status, HighLevel: true}
	}
	task := func(id, status string) *graph.Issue { return &graph.Issue{ID: id, Type: "task", Status: status} }
	later := hl("later", "open")
	later.DeferUntil = now.Add(48 * time.Hour)
	recent, old := hl("recent", "closed"), hl("old", "closed")
	recent.ClosedAt, old.ClosedAt = now.AddDate(0, 0, -44), now.AddDate(0, 0, -46)

	g := issueGraph(t, []*graph.Issue{
		hl("work", "open"), task("w1", "open"), task("w2", "closed"),
		hl("finished", "in_progress"), task("f1", "closed"),
		hl("empty", "open"),
		hl("shelved", "deferred"), task("s1", "open"),
		later, task("l1", "open"),
		hl("waiting", "open"), task("x1", "open"),
		recent, old,
	}, [][3]string{
		{"parent-child", "w1", "work"}, {"parent-child", "w2", "work"},
		{"parent-child", "f1", "finished"},
		{"parent-child", "s1", "shelved"},
		{"parent-child", "l1", "later"},
		{"parent-child", "x1", "waiting"},
	})
	rep := &load.Report{Dangling: []load.Dangling{
		{From: "x1", To: "other-9", Kind: graph.Blocking},
		{From: "x1", To: "gone-epic", Kind: graph.ParentChild}, // not a wait
	}}
	r := Build(g, rep, cfg, now)
	want := map[string]string{
		"work": StatusForecast, "finished": StatusReadyToClose, "empty": StatusNotPlanned,
		"shelved": StatusDeferred, "later": StatusDeferred, "waiting": StatusBlockedOutside, "recent": StatusDone,
	}
	got := map[string]string{}
	for _, m := range r.Milestones {
		got[m.ID] = m.Status
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("statuses = %v\nwant %v", got, want)
	}
	if w := milestone(t, r, "waiting"); !reflect.DeepEqual(w.WaitsOnUnloaded, []string{"other-9"}) {
		t.Errorf("waiting.WaitsOnUnloaded = %v", w.WaitsOnUnloaded)
	}
	if err := r.Validate(); err != nil {
		t.Error(err)
	}
}

func TestAssess(t *testing.T) {
	day := func(n int) *time.Time { d := now.AddDate(0, 0, n); return &d }
	tests := []struct {
		name string
		o    Outlook
		want string
	}{
		{"no target", Outlook{Status: StatusForecast, P50: day(1), P80: day(2)}, ""},
		{"p80 before target", Outlook{Status: StatusForecast, TargetDueAt: day(5), P50: day(1), P80: day(2)}, OnTrack},
		{"p80 on target", Outlook{Status: StatusForecast, TargetDueAt: day(2), P50: day(1), P80: day(2)}, OnTrack},
		{"p50 before target, p80 after", Outlook{Status: StatusForecast, TargetDueAt: day(2), P50: day(1), P80: day(3)}, AtRisk},
		{"p50 after target", Outlook{Status: StatusForecast, TargetDueAt: day(2), P50: day(3), P80: day(4)}, Late},
		{"no forecast yet", Outlook{Status: StatusForecast, TargetDueAt: day(2)}, ""},
		{"target passed without forecast", Outlook{Status: StatusNotPlanned, TargetDueAt: day(-1)}, Late},
		{"ready to close before target", Outlook{Status: StatusReadyToClose, TargetDueAt: day(1)}, OnTrack},
		{"ready to close after target", Outlook{Status: StatusReadyToClose, TargetDueAt: day(-1)}, Late},
		{"done on time", Outlook{Status: StatusDone, TargetDueAt: day(-1), ClosedAt: day(-2)}, OnTrack},
		{"done late", Outlook{Status: StatusDone, TargetDueAt: day(-2), ClosedAt: day(-1)}, Late},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := Roadmap{GeneratedAt: now, Milestones: []Milestone{{Outlook: tt.o}}, Goals: []Goal{{Outlook: tt.o}}}
			r.Assess()
			if got := r.Milestones[0].Schedule; got != tt.want {
				t.Errorf("milestone schedule = %q, want %q", got, tt.want)
			}
			if got := r.Goals[0].Schedule; got != tt.want {
				t.Errorf("goal schedule = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestReadWriteRoundTrip(t *testing.T) {
	cfg, g, rep := fixture(t, "multirepo")
	r := Build(g, rep, cfg, now)
	p50, p80, p95 := now.Add(24*time.Hour), now.Add(60*time.Hour), now.Add(200*time.Hour)
	hours, rate := 30.5, 4.25
	m := &r.Milestones[0]
	m.P50, m.P80, m.P95, m.AgentHours, m.RatePerDay, m.CriticalChain = &p50, &p80, &p95, &hours, &rate, []string{"api-3", "api-4"}
	m.QuantileHours = map[string]float64{"p05": 2.5, "p50": 24, "p80": 60, "p95": 200, "p99": 410.25}
	r.Calibration = &Calibration{Samples: 12, P50Coverage: 0.5, P80Coverage: 0.75}
	r.Assess()

	path := filepath.Join(t.TempDir(), "roadmap.json")
	if err := WriteFile(path, r); err != nil {
		t.Fatal(err)
	}
	if fi, err := os.Stat(path); err != nil || fi.Mode().Perm() != 0o644 {
		t.Fatalf("stat %s: %v, %v", path, fi, err)
	}
	back, err := ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(back, r) {
		t.Errorf("round trip changed the roadmap:\n got %+v\nwant %+v", back, r)
	}
	if entries, _ := os.ReadDir(filepath.Dir(path)); len(entries) != 1 {
		t.Errorf("WriteFile left temporary files: %v", entries)
	}
}

func TestReadRejects(t *testing.T) {
	valid := func() map[string]string {
		return map[string]string{
			"schema_version": `1`,
			"generated_at":   `"2026-09-19T12:00:00Z"`,
			"repos":          `[{"name":"r","issues":1,"open":1,"closed":0,"concurrency_source":"measured"}]`,
			"milestones":     `[{"id":"m","title":"M","repo":"r","type":"epic","status":"forecast","total":1,"done":0,"remaining":1,"done_pct":0,"remaining_ids":["t"],"human_gates":0}]`,
		}
	}
	doc := func(fields map[string]string) string {
		var parts []string
		for k, v := range fields {
			parts = append(parts, `"`+k+`":`+v)
		}
		return "{" + strings.Join(parts, ",") + "}"
	}
	if _, err := Read(strings.NewReader(doc(valid()))); err != nil {
		t.Fatalf("valid document rejected: %v", err)
	}
	with := func(k, v string) map[string]string { f := valid(); f[k] = v; return f }
	without := func(k string) map[string]string { f := valid(); delete(f, k); return f }
	tests := []struct {
		name   string
		fields map[string]string
		want   string
	}{
		{"not a roadmap", without("schema_version"), "no schema_version"},
		{"newer schema", with("schema_version", "2"), "newer than this beadline reads"},
		{"no generated_at", without("generated_at"), "generated_at is missing"},
		{"unknown status", with("milestones", `[{"id":"m","repo":"r","status":"shipped","remaining_ids":[]}]`), `unknown status "shipped"`},
		{"unknown schedule", with("milestones", `[{"id":"m","repo":"r","status":"forecast","schedule":"eventually","remaining_ids":[]}]`), `unknown schedule "eventually"`},
		{"unknown repo", with("milestones", `[{"id":"m","repo":"elsewhere","status":"forecast","remaining_ids":[]}]`), `repo "elsewhere" is not in repos`},
		{"counts", with("milestones", `[{"id":"m","repo":"r","status":"forecast","total":2,"done":0,"remaining":1,"remaining_ids":["t"]}]`), "do not add up"},
		{"quantiles", with("milestones", `[{"id":"m","repo":"r","status":"forecast","remaining_ids":[],"p50":"2026-09-21T00:00:00Z","p80":"2026-09-20T00:00:00Z"}]`), "quantiles out of order"},
		{"grid key", with("milestones", `[{"id":"m","repo":"r","status":"forecast","remaining_ids":[],"quantile_hours":{"median":3}}]`), `key "median" is not a level`},
		{"grid order", with("milestones", `[{"id":"m","repo":"r","status":"forecast","remaining_ids":[],"quantile_hours":{"p05":3,"p50":2}}]`), "p50 (2) is before p05 (3)"},
		{"grid negative", with("milestones", `[{"id":"m","repo":"r","status":"forecast","remaining_ids":[],"quantile_hours":{"p05":-1}}]`), "p05 is -1"},
		{"duplicate id", with("goals", `[{"id":"m","title":"M","members":[],"status":"forecast","remaining_ids":[]}]`), "goal m: duplicate id"},
		{"bad concurrency", with("config", `{"repos":[{"name":"r","export":"r.jsonl","concurrency":0}]}`), "concurrency must be"},
		{"not JSON", map[string]string{"schema_version": "1,"}, "roadmap:"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := Read(strings.NewReader(doc(tt.fields)))
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Errorf("Read error = %v, want it to contain %q", err, tt.want)
			}
		})
	}
	if err := Write(&bytes.Buffer{}, &Roadmap{SchemaVersion: SchemaVersion}); err == nil {
		t.Error("Write accepted an invalid roadmap")
	}
}

func TestConcurrencyJSON(t *testing.T) {
	for c, want := range map[Concurrency]string{0: `"measure"`, 3: `3`} {
		b, err := c.MarshalJSON()
		if err != nil || string(b) != want {
			t.Errorf("Concurrency(%d) = %s, %v; want %s", c, b, err, want)
		}
		var back Concurrency
		if err := back.UnmarshalJSON(b); err != nil || back != c {
			t.Errorf("UnmarshalJSON(%s) = %d, %v", b, back, err)
		}
	}
	var c Concurrency
	for _, bad := range []string{`0`, `-1`, `"many"`, `2.5`} {
		if err := c.UnmarshalJSON([]byte(bad)); err == nil {
			t.Errorf("UnmarshalJSON(%s) accepted", bad)
		}
	}
}

func TestFingerprint(t *testing.T) {
	dir := t.TempDir()
	write := func(name, body string) {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("a.jsonl", "{\"id\":\"a-1\"}\n")
	write("b.jsonl", "{\"id\":\"b-1\"}\n")
	write("beadline.toml", `repos = ["a.jsonl", "b.jsonl"]`)
	cfg, err := config.Load(filepath.Join(dir, "beadline.toml"))
	if err != nil {
		t.Fatal(err)
	}
	first, err := Fingerprint(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(first.Fingerprint, "sha256:") || len(first.Exports) != 2 ||
		first.Exports[0].Path != "a.jsonl" || first.Exports[0].Bytes != 13 {
		t.Errorf("Fingerprint = %+v", first)
	}
	again, _ := Fingerprint(cfg)
	if again.Fingerprint != first.Fingerprint {
		t.Error("Fingerprint is not deterministic")
	}
	write("b.jsonl", "{\"id\":\"b-2\"}\n")
	if changed, _ := Fingerprint(cfg); changed.Fingerprint == first.Fingerprint || changed.Exports[0] != first.Exports[0] {
		t.Errorf("changing one export: %+v", changed)
	}
	os.Remove(filepath.Join(dir, "a.jsonl"))
	if _, err := Fingerprint(cfg); err == nil || !strings.Contains(err.Error(), "repo a") {
		t.Errorf("missing export: err = %v", err)
	}
}

func TestSetInputs(t *testing.T) {
	cfg, err := config.Load(filepath.Join("..", "..", "testdata", "multirepo", "beadline.toml"))
	if err != nil {
		t.Fatal(err)
	}
	exports := load.ReadAll(context.Background(), cfg, nil)
	g, rep, err := load.FromExports(&cfg.Conventions, exports)
	if err != nil {
		t.Fatal(err)
	}
	r := Build(g, rep, cfg, now)
	r.SetInputs(exports)
	files, err := Fingerprint(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(r.Inputs, files) {
		t.Errorf("inputs of what was read = %+v, want the files' %+v", r.Inputs, files)
	}

	exports[1].Err, exports[1].Data = errors.New("repo web: database is locked"), nil
	r.SetInputs(exports)
	if len(r.Inputs.Exports) != 2 || r.Inputs.Fingerprint == files.Fingerprint ||
		r.Repos[1].Error != "repo web: database is locked" || r.Repos[0].Error != "" {
		t.Errorf("with a failed repo: inputs %+v, repos %+v", r.Inputs, r.Repos)
	}
}

func TestHide(t *testing.T) {
	cfg, g, rep := fixture(t, "multirepo")
	r := Build(g, rep, cfg, now)
	r.Hide([]string{"api-m1", "hq-g1", "not-there"})
	for _, m := range r.Milestones {
		if m.ID == "api-m1" {
			t.Error("api-m1 is still on the roadmap")
		}
	}
	for _, g := range r.Goals {
		if g.ID == "hq-g1" {
			t.Error("hq-g1 is still on the roadmap")
		}
	}
	if len(r.Milestones) == 0 || len(r.Goals) == 0 {
		t.Errorf("Hide removed too much: %d milestones, %d goals", len(r.Milestones), len(r.Goals))
	}
}
