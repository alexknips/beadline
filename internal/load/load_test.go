package load

import (
	"errors"
	"fmt"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/alexknips/beadline/internal/config"
	"github.com/alexknips/beadline/internal/graph"
)

func loadFixture(t *testing.T, name string) (*graph.Graph, *Report) {
	t.Helper()
	cfg, err := config.Load(filepath.Join("..", "..", "testdata", name, "beadline.toml"))
	if err != nil {
		t.Fatal(err)
	}
	g, rep, err := Load(cfg)
	if err != nil {
		t.Fatal(err)
	}
	return g, rep
}

func ids(issues []*graph.Issue) []string {
	out := []string{}
	for _, i := range issues {
		out = append(out, i.ID)
	}
	return out
}

func ts(s string) time.Time {
	t, err := time.Parse(time.RFC3339Nano, s)
	if err != nil {
		panic(err)
	}
	return t
}

func TestMultiRepoFixture(t *testing.T) {
	g, rep := loadFixture(t, "multirepo")

	wantIDs := []string{
		"api-m1", "api-e1", "api-1", "api-2", "api-3", "api-4", "api-5",
		"web-e1", "web-1", "web-2", "web-3", "web-e2", "web-4",
		"hq-g1", "hq-h1",
	}
	if got := ids(g.Issues()); !reflect.DeepEqual(got, wantIDs) {
		t.Errorf("issues = %v\nwant     %v", got, wantIDs)
	}

	if len(rep.Dangling) != 0 || len(rep.Duplicates) != 0 || len(rep.Cycles) != 0 {
		t.Errorf("clean fixture reported problems: %+v", rep)
	}
	wantStats := []RepoStats{
		{Repo: "api", Issues: 7, Infra: map[string]int{"molecule": 1, "ephemeral": 1, "template": 1}},
		{Repo: "web", Issues: 6, Infra: map[string]int{}},
		{Repo: "hq", Issues: 2, Infra: map[string]int{"convoy": 1}, Other: 1},
	}
	for n := range rep.Repos {
		rep.Repos[n].Source = filepath.Base(rep.Repos[n].Source)
		wantStats[n].Source = wantStats[n].Repo + ".jsonl"
	}
	if !reflect.DeepEqual(rep.Repos, wantStats) {
		t.Errorf("repo stats = %+v\nwant         %+v", rep.Repos, wantStats)
	}

	t.Run("hierarchy", func(t *testing.T) {
		if got := ids(g.Descendants("api-m1")); !reflect.DeepEqual(got, []string{"api-4", "api-5", "api-e1", "api-1", "api-2", "api-3"}) {
			t.Errorf("Descendants(api-m1) = %v", got)
		}
		if got := g.Issue("api-3").Parents; !reflect.DeepEqual(got, []string{"api-e1"}) {
			t.Errorf("api-3 parents = %v (the molecule's parent-child edge must be dropped)", got)
		}
		if got := ids(g.HighLevel()); !reflect.DeepEqual(got, []string{"api-m1", "api-e1", "web-e1", "web-e2"}) {
			t.Errorf("HighLevel = %v", got)
		}
	})

	t.Run("blocking edges", func(t *testing.T) {
		blocked := map[string][]string{
			"api-2": {"api-1"},          // discovered-from and related are ignored
			"api-3": nil,                // its blocker is a molecule: dropped, not dangling
			"api-4": {"api-3"},          //
			"api-5": {"api-4"},          //
			"web-1": {"api-4"},          // cross-repo
			"web-2": {"hq-h1"},          // cross-repo, on a human gate
			"web-3": {"web-1", "web-2"}, // waits-for and conditional-blocks
		}
		for id, want := range blocked {
			if got := g.Issue(id).BlockedBy; !reflect.DeepEqual(got, want) {
				t.Errorf("%s.BlockedBy = %v, want %v", id, got, want)
			}
		}
		if got := g.Issue("api-4").Blocks; !reflect.DeepEqual(got, []string{"api-5", "web-1"}) {
			t.Errorf("api-4.Blocks = %v", got)
		}
	})

	t.Run("conventions", func(t *testing.T) {
		var gates []string
		for _, i := range g.Issues() {
			if i.HumanGate {
				gates = append(gates, i.ID)
			}
		}
		// api-4 has hold_reason "" (a cleared hold): not a gate.
		if want := []string{"api-5", "web-3", "hq-h1"}; !reflect.DeepEqual(gates, want) {
			t.Errorf("human gates = %v, want %v", gates, want)
		}
		goals := g.Goals()
		if len(goals) != 2 || goals[0].ID != "hq-g1" || goals[1].ID != "hq-g2" {
			t.Fatalf("goals = %+v", goals)
		}
		if goals[0].Issue == nil || goals[0].Issue.Repo != "hq" {
			t.Errorf("goal hq-g1 should resolve to its bead in repo hq: %+v", goals[0].Issue)
		}
		if got := ids(goals[0].Members); !reflect.DeepEqual(got, []string{"api-m1", "web-e1"}) {
			t.Errorf("hq-g1 members = %v", got)
		}
		if goals[1].Issue != nil {
			t.Errorf("goal hq-g2 is not loaded, got %+v", goals[1].Issue)
		}
	})

	t.Run("fields", func(t *testing.T) {
		api1 := g.Issue("api-1")
		want := graph.Issue{
			ID: "api-1", Repo: "api", Title: "Token issuance", Description: "Issue signed access tokens.",
			Type: "task", Status: "closed", CloseReason: "done", Priority: 2, Assignee: "agent-1",
			CreatedAt: ts("2026-09-01T09:00:00Z"), UpdatedAt: ts("2026-09-01T09:00:00Z"),
			StartedAt: ts("2026-09-01T10:00:00Z"), ClosedAt: ts("2026-09-01T12:30:00Z"),
			Parents: []string{"api-e1"}, Blocks: []string{"api-2"},
		}
		if !reflect.DeepEqual(*api1, want) {
			t.Errorf("api-1 =\n%+v\nwant\n%+v", *api1, want)
		}
		if !api1.Closed() || g.Issue("api-3").Closed() {
			t.Error("Closed")
		}

		api3 := g.Issue("api-3")
		if got := api3.CreatedAt; !got.Equal(ts("2026-09-02T07:15:00.123456Z")) || got.Location() != time.UTC {
			t.Errorf("api-3 created_at = %v, want it converted to UTC", got)
		}
		if api3.EstimatedMinutes == nil || *api3.EstimatedMinutes != 90 || api3.Priority != 1 || api3.Status != graph.StatusInProgress {
			t.Errorf("api-3 = %+v", api3)
		}
		if m1 := g.Issue("api-m1"); !m1.DueAt.Equal(ts("2026-10-15T00:00:00Z")) || !m1.StartedAt.IsZero() || !m1.ClosedAt.IsZero() {
			t.Errorf("api-m1 due/started/closed = %v/%v/%v", m1.DueAt, m1.StartedAt, m1.ClosedAt)
		}
		if got := string(g.Issue("api-4").Metadata["branch"]); got != `"polecat/api-4"` {
			t.Errorf("api-4 metadata branch = %s", got)
		}
		if w := g.Issue("web-2"); w.EstimatedMinutes != nil || w.Metadata != nil || w.Labels != nil {
			t.Errorf("web-2 optional fields should be empty: %+v", w)
		}
	})
}

func TestProblemsFixture(t *testing.T) {
	g, rep := loadFixture(t, "problems")

	wantDangling := []Dangling{
		{Repo: "a", From: "a-5", To: "a-99", Kind: graph.Blocking},
		{Repo: "a", From: "a-5", To: "gone-epic", Kind: graph.ParentChild},
	}
	if !reflect.DeepEqual(rep.Dangling, wantDangling) {
		t.Errorf("dangling = %+v", rep.Dangling)
	}
	if len(rep.Duplicates) != 1 {
		t.Fatalf("duplicates = %+v", rep.Duplicates)
	}
	if d := rep.Duplicates[0]; d.ID != "a-5" || d.Kept != "a" || d.Repo != "b" || d.Line != 2 || filepath.Base(d.Source) != "b.jsonl" {
		t.Errorf("duplicate = %+v", d)
	}
	if got := g.Issue("a-5"); got.Title != "Points nowhere" || !reflect.DeepEqual(got.Blocks, []string{"b-1"}) {
		t.Errorf("a-5 = %+v, want the first copy, blocking b-1 across repos", got)
	}
	wantCycles := []graph.Cycle{
		{Kind: graph.Blocking, IDs: []string{"a-1", "a-2"}, Open: true},
		{Kind: graph.Blocking, IDs: []string{"a-3", "a-4"}, Open: false},
	}
	if !reflect.DeepEqual(rep.Cycles, wantCycles) {
		t.Errorf("cycles = %+v", rep.Cycles)
	}
}

func read(t *testing.T, cv *config.Conventions, lines ...string) (*graph.Graph, *Report, error) {
	t.Helper()
	return Read(cv, Source{Repo: "r", Name: "r.jsonl", R: strings.NewReader(strings.Join(lines, "\n"))})
}

func TestReadErrors(t *testing.T) {
	cv := config.Default().Conventions
	tests := []struct {
		name  string
		lines []string
		want  string
	}{
		{"malformed json", []string{`{"id":"r-1"}`, `{"id":`}, "r.jsonl:2:"},
		{"no id", []string{"", `{"_type":"issue","title":"x"}`}, "r.jsonl:2: issue without an id"},
		{"bad time", []string{`{"id":"r-1","created_at":"yesterday"}`}, "r.jsonl:1:"},
		{"metadata not an object", []string{`{"id":"r-1","metadata":"{}"}`}, "r.jsonl:1:"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, _, err := read(t, &cv, tt.lines...)
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Errorf("err = %v, want it to contain %q", err, tt.want)
			}
		})
	}

	if _, _, err := Read(&cv, Source{Repo: "r", Name: "r.jsonl", R: failingReader{}}); err == nil || !strings.Contains(err.Error(), "r.jsonl:1: disk on fire") {
		t.Errorf("reader error = %v", err)
	}

	cfg := config.Default()
	cfg.Repos = []config.Repo{{Name: "gone", Export: filepath.Join(t.TempDir(), "missing.jsonl")}}
	if _, _, err := Load(&cfg); err == nil || !strings.HasPrefix(err.Error(), "repo gone: ") {
		t.Errorf("missing export: err = %v", err)
	}
}

type failingReader struct{}

func (failingReader) Read([]byte) (int, error) { return 0, errors.New("disk on fire") }

func TestReadEdgeCases(t *testing.T) {
	cv := config.Default().Conventions
	long := strings.Repeat("x", 1<<20) // descriptions are unbounded
	g, rep, err := read(t, &cv,
		`{"id":"r-1","title":"no _type field: older exports","description":"`+long+`"}`,
		"   ",
		`{"id":"r-2","title":"self","dependencies":[{"depends_on_id":"r-2","type":"blocks"}]}`+"\r",
		`{"id":"r-3","title":"meta null","metadata":null,"labels":null}`,
	)
	if err != nil {
		t.Fatal(err)
	}
	if g.Len() != 3 || len(g.Issue("r-1").Description) != len(long) {
		t.Errorf("loaded %d issues", g.Len())
	}
	if want := []graph.Cycle{{Kind: graph.Blocking, IDs: []string{"r-2"}, Open: true}}; !reflect.DeepEqual(rep.Cycles, want) {
		t.Errorf("self-blocking bead: cycles = %+v", rep.Cycles)
	}

	// Conventions come from the caller: with no infra types a molecule is work.
	cv.InfraTypes = nil
	if err := cv.Compile(); err != nil {
		t.Fatal(err)
	}
	g, _, err = read(t, &cv, `{"id":"r-1","issue_type":"molecule"}`)
	if err != nil || g.Issue("r-1") == nil {
		t.Errorf("molecule with no infra types: %v, %v", g.Issue("r-1"), err)
	}
}

func TestGraphAsOf(t *testing.T) {
	cv := config.Default().Conventions
	ex, err := Parse(Source{Repo: "r", Name: "r.jsonl", R: strings.NewReader(strings.Join([]string{
		// Closed exactly at the moment: closed then.
		`{"id":"r-1","status":"closed","close_reason":"done","created_at":"2026-09-01T08:00:00Z","started_at":"2026-09-01T10:00:00Z","closed_at":"2026-09-02T12:00:00Z","updated_at":"2026-09-02T12:00:00Z",` +
			`"dependencies":[{"depends_on_id":"r-4","type":"blocks","created_at":"2026-09-03T09:00:00Z"}]}`,
		// Not updated since: its deferral stands.
		`{"id":"r-2","status":"deferred","defer_until":"2026-09-10T00:00:00Z","created_at":"2026-09-01T08:00:00Z","updated_at":"2026-09-01T09:00:00Z"}`,
		// Updated since: whether it was deferred then is unknown.
		`{"id":"r-3","status":"deferred","defer_until":"2026-09-10T00:00:00Z","created_at":"2026-09-01T08:00:00Z","updated_at":"2026-09-03T08:00:00Z"}`,
		// Created later: absent, and edges to it are not dangling.
		`{"id":"r-4","status":"open","created_at":"2026-09-03T08:00:00Z","updated_at":"2026-09-03T08:00:00Z"}`,
		`{"id":"r-5","status":"in_progress","created_at":"2026-09-01T08:00:00Z","started_at":"2026-09-01T11:00:00Z","updated_at":"2026-09-03T08:00:00Z",` +
			`"dependencies":[{"depends_on_id":"r-1","type":"blocks","created_at":"2026-09-01T09:00:00Z"},{"depends_on_id":"r-99","type":"blocks","created_at":"2026-09-01T09:00:00Z"}]}`,
		// Started and closed later: open then, and its parent link came later too.
		`{"id":"r-6","status":"closed","close_reason":"done","created_at":"2026-09-01T08:00:00Z","started_at":"2026-09-02T13:00:00Z","closed_at":"2026-09-03T10:00:00Z","updated_at":"2026-09-03T10:00:00Z",` +
			`"dependencies":[{"depends_on_id":"r-2","type":"parent-child","created_at":"2026-09-02T13:00:00Z"}]}`,
		// Infra records count towards the horizon.
		`{"id":"r-w","issue_type":"wisp","status":"open","created_at":"2026-09-01T08:00:00Z","updated_at":"2026-09-03T11:00:00Z"}`,
		// A dependency with no usable timestamp is assumed to have existed.
		`{"id":"r-7","status":"open","created_at":"2026-09-01T08:00:00Z","dependencies":[{"depends_on_id":"r-2","type":"blocks","created_at":"last tuesday"}]}`,
	}, "\n"))})
	if err != nil {
		t.Fatal(err)
	}
	if h := ex.Horizon(); !h.Equal(ts("2026-09-03T11:00:00Z")) {
		t.Errorf("Horizon = %v", h)
	}
	if s := ex.DependencySkew(); s != (Skew{}) {
		t.Errorf("DependencySkew = %+v, want none", s)
	}

	at := ts("2026-09-02T12:00:00Z")
	g, rep, err := ex.Graph(&cv, at)
	if err != nil {
		t.Fatal(err)
	}
	if got := ids(g.Issues()); !reflect.DeepEqual(got, []string{"r-1", "r-2", "r-3", "r-5", "r-6", "r-7"}) {
		t.Fatalf("issues as of %v = %v", at, got)
	}
	if want := []Dangling{{Repo: "r", From: "r-5", To: "r-99", Kind: graph.Blocking}}; !reflect.DeepEqual(rep.Dangling, want) {
		t.Errorf("dangling = %+v", rep.Dangling)
	}
	status := func(id string) string { return g.Issue(id).Status }
	if r1 := g.Issue("r-1"); status("r-1") != "closed" || r1.CloseReason != "done" || !r1.ClosedAt.Equal(at) || r1.BlockedBy != nil {
		t.Errorf("r-1 = %+v", r1)
	}
	if r2 := g.Issue("r-2"); status("r-2") != "deferred" || r2.DeferUntil.IsZero() {
		t.Errorf("r-2 = %+v, want its deferral kept", r2)
	}
	if r3 := g.Issue("r-3"); status("r-3") != "open" || !r3.DeferUntil.IsZero() {
		t.Errorf("r-3 = %+v, want open with no defer_until", r3)
	}
	if r5 := g.Issue("r-5"); status("r-5") != graph.StatusInProgress || !reflect.DeepEqual(r5.BlockedBy, []string{"r-1"}) {
		t.Errorf("r-5 = %+v", r5)
	}
	if r6 := g.Issue("r-6"); status("r-6") != "open" || !r6.StartedAt.IsZero() || !r6.ClosedAt.IsZero() || r6.CloseReason != "" ||
		r6.Parents != nil || !r6.UpdatedAt.IsZero() {
		t.Errorf("r-6 = %+v", r6)
	}
	if r7 := g.Issue("r-7"); !reflect.DeepEqual(r7.BlockedBy, []string{"r-2"}) {
		t.Errorf("r-7 = %+v", r7)
	}

	// The zero moment is the graph as exported; parsing once serves both.
	now, _, err := ex.Graph(&cv, time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	if now.Len() != 7 || now.Issue("r-6").Status != "closed" || !reflect.DeepEqual(now.Issue("r-6").Parents, []string{"r-2"}) ||
		now.Issue("r-3").Status != "deferred" {
		t.Errorf("graph as exported changed by an earlier rewind: %v", ids(now.Issues()))
	}
}

// ActivityTimestamps feeds idle.Infer's cross-repo timeline (ADR-3): every
// created/updated/started/closed timestamp of every record, work or infra,
// and no leakage past the asOf a backtest origin rewinds to.
func TestActivityTimestamps(t *testing.T) {
	ex, err := Parse(Source{Repo: "r", Name: "r.jsonl", R: strings.NewReader(strings.Join([]string{
		`{"id":"r-1","status":"closed","close_reason":"done","created_at":"2026-09-01T08:00:00Z","started_at":"2026-09-01T10:00:00Z","closed_at":"2026-09-02T12:00:00Z","updated_at":"2026-09-02T12:00:00Z"}`,
		// Infra record: still counts as activity, even though the loader
		// drops it from the graph.
		`{"id":"r-w","issue_type":"wisp","status":"open","created_at":"2026-09-01T09:00:00Z","updated_at":"2026-09-03T11:00:00Z"}`,
		// Created and closed after the rewind point below: invisible then.
		`{"id":"r-2","status":"closed","close_reason":"done","created_at":"2026-09-05T08:00:00Z","closed_at":"2026-09-05T09:00:00Z","updated_at":"2026-09-05T09:00:00Z"}`,
	}, "\n"))})
	if err != nil {
		t.Fatal(err)
	}

	all := ex.ActivityTimestamps(time.Time{})
	sort.Slice(all, func(a, b int) bool { return all[a].Before(all[b]) })
	want := []time.Time{
		ts("2026-09-01T08:00:00Z"), ts("2026-09-01T09:00:00Z"), ts("2026-09-01T10:00:00Z"),
		ts("2026-09-02T12:00:00Z"), ts("2026-09-02T12:00:00Z"),
		ts("2026-09-03T11:00:00Z"),
		ts("2026-09-05T08:00:00Z"), ts("2026-09-05T09:00:00Z"), ts("2026-09-05T09:00:00Z"),
	}
	if !reflect.DeepEqual(all, want) {
		t.Errorf("ActivityTimestamps(zero) = %v, want %v", all, want)
	}

	// Rewound to before r-2 existed and before r-w's last update: neither
	// leaks in, exactly as Graph(asOf) would not show them (no leakage,
	// ADR-3 §3).
	asOf := ts("2026-09-02T12:00:00Z")
	rewound := ex.ActivityTimestamps(asOf)
	sort.Slice(rewound, func(a, b int) bool { return rewound[a].Before(rewound[b]) })
	wantRewound := []time.Time{
		ts("2026-09-01T08:00:00Z"), ts("2026-09-01T09:00:00Z"), ts("2026-09-01T10:00:00Z"),
		ts("2026-09-02T12:00:00Z"), ts("2026-09-02T12:00:00Z"),
	}
	if !reflect.DeepEqual(rewound, wantRewound) {
		t.Errorf("ActivityTimestamps(%v) = %v, want %v (no future leakage)", asOf, rewound, wantRewound)
	}
	for _, when := range rewound {
		if when.After(asOf) {
			t.Errorf("ActivityTimestamps(%v) leaked a timestamp after asOf: %v", asOf, when)
		}
	}
}

func TestDependencySkew(t *testing.T) {
	cv := config.Default().Conventions
	start := ts("2026-06-15T00:00:00Z")
	var lines []string
	add := func(n int, lag time.Duration) {
		for k := 0; k < n; k++ {
			id := len(lines) // two lines per pair
			created := start.Add(time.Duration(id) * time.Hour)
			lines = append(lines, fmt.Sprintf(`{"id":"r-%d","status":"open","created_at":%q}`, 2*id, created.Format(time.RFC3339)))
			lines = append(lines, fmt.Sprintf(`{"id":"r-%d","status":"open","created_at":%q,"dependencies":[{"depends_on_id":"r-%d","type":"parent-child","created_at":%q}]}`,
				2*id+1, created.Format(time.RFC3339), 2*id, created.Add(lag).Format(time.RFC3339)))
		}
	}
	add(10, 30*time.Second) // before the time-zone error: no lag
	skewed := len(lines)
	add(25, 2*time.Hour+time.Minute)
	parse := func() *Exports {
		ex, err := Parse(Source{Repo: "r", Name: "r.jsonl", R: strings.NewReader(strings.Join(lines, "\n"))})
		if err != nil {
			t.Fatal(err)
		}
		return ex
	}
	ex := parse()
	want := Skew{Since: start.Add(time.Duration(skewed)*time.Hour + 2*time.Hour + time.Minute), Offset: 2 * time.Hour}
	if got := ex.DependencySkew(); got != want {
		t.Fatalf("DependencySkew = %+v, want %+v", got, want)
	}
	// An hour after the child was created, its parent link existed: the
	// exported timestamp is two hours late.
	child := fmt.Sprintf("r-%d", 2*skewed+1)
	created := start.Add(time.Duration(skewed) * time.Hour)
	g, _, err := ex.Graph(&cv, created.Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if got := g.Issue(child).Parents; len(got) != 1 {
		t.Errorf("%s parents an hour after creation = %v, want the corrected link", child, got)
	}

	// Too few edges share the lag: no skew.
	lines = lines[:skewed]
	add(19, 2*time.Hour)
	if got := parse().DependencySkew(); got != (Skew{}) {
		t.Errorf("19 skewed edges: DependencySkew = %+v, want none", got)
	}
}
