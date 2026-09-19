package cli

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"strings"
	"testing"

	"github.com/alexknips/beadline/internal/load"
	"github.com/alexknips/beadline/internal/roadmap"
)

const fixtures = "../../testdata/multirepo/"

// fixtureRoot runs the one command on the multirepo exports, as of the
// forecast tests' "now", writing into a temporary directory.
func fixtureRoot(t *testing.T, extra ...string) (dir string, code int, stdout, stderr string) {
	t.Helper()
	dir = t.TempDir()
	args := append([]string{fixtures + "api.jsonl", fixtures + "web.jsonl", fixtures + "hq.jsonl",
		"-o", dir, "--as-of", "2026-09-03T12:00:00Z", "--runs", "300"}, extra...)
	code, stdout, stderr = run(args...)
	return dir, code, stdout, stderr
}

func readRoadmap(t *testing.T, path string) *roadmap.Roadmap {
	t.Helper()
	r, err := roadmap.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func TestRootWritesTheRoadmap(t *testing.T) {
	dir, code, stdout, stderr := fixtureRoot(t, "--agents", "api=3,hq=1")
	if code != ExitOK {
		t.Fatalf("exit %d: %s", code, stderr)
	}
	page, js := filepath.Join(dir, "roadmap.html"), filepath.Join(dir, "roadmap.json")
	if stderr != "wrote "+page+" and "+js+"\n" {
		t.Errorf("stderr = %q", stderr)
	}
	for _, want := range []string{
		"Roadmap as of 2026-09-03 12:00 UTC: 3 repos, 15 beads (",
		"  api-m1  Public API v1\n      plan for 2026-",
		" (80% chance) · 50/50: 2026-",
		" left · waits on a person",
		"  hq-g1  Public launch (goal)\n",
	} {
		if !strings.Contains(stdout, want) {
			t.Errorf("summary lacks %q:\n%s", want, stdout)
		}
	}
	if strings.Contains(stdout, "P80") || strings.Contains(stdout, "P50") || strings.Contains(stdout, "almost surely") {
		t.Errorf("summary uses quantile names:\n%s", stdout)
	}

	r := readRoadmap(t, js)
	if r.ModelVersion != modelVersion || r.BeadlineVersion == "" || len(r.Inputs.Exports) != 3 || r.Inputs.Exports[0].Live {
		t.Errorf("provenance = %q %q %+v", r.ModelVersion, r.BeadlineVersion, r.Inputs)
	}
	var m1 *roadmap.Milestone
	for n := range r.Milestones {
		if r.Milestones[n].ID == "api-m1" {
			m1 = &r.Milestones[n]
		}
	}
	if m1 == nil || m1.P50 == nil || m1.P80 == nil || m1.P95 == nil || m1.P80.Before(*m1.P50) ||
		m1.AgentHours == nil || len(m1.CriticalChain) == 0 || m1.Schedule == "" {
		t.Fatalf("api-m1 = %+v", m1)
	}
	if got := r.Repos[0]; got.Name != "api" || got.Concurrency == nil || *got.Concurrency != 3 || got.RatePerDay == nil {
		t.Errorf("repo api = %+v", got)
	}
	if want := []string{"agents.api = 3", "agents.hq = 1", "runs = 300"}; !reflect.DeepEqual(r.Config.Settings, want) {
		t.Errorf("settings = %v, want %v", r.Config.Settings, want)
	}
	html, err := os.ReadFile(page)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"<p>Settings: agents.api = 3 · agents.hq = 1 · runs = 300.</p>", `<rect class="bar`} {
		if !strings.Contains(string(html), want) {
			t.Errorf("page lacks %q", want)
		}
	}

	// Same inputs and seed: the same roadmap.
	dir2, _, _, _ := fixtureRoot(t, "--agents", "api=3,hq=1")
	again, _ := os.ReadFile(filepath.Join(dir2, "roadmap.json"))
	first, _ := os.ReadFile(js)
	if string(again) != string(first) {
		t.Error("the same inputs and seed gave another roadmap.json")
	}
}

func TestRootJSONAndQuiet(t *testing.T) {
	_, code, stdout, stderr := fixtureRoot(t, "--json", "-q")
	if code != ExitOK || stderr != "" {
		t.Fatalf("exit %d, stderr %q", code, stderr)
	}
	if _, err := roadmap.Read(strings.NewReader(stdout)); err != nil {
		t.Errorf("--json does not print a roadmap: %v", err)
	}
	if _, code, stdout, stderr := fixtureRoot(t, "--quiet"); code != ExitOK || stdout != "" || stderr != "" {
		t.Errorf("--quiet: exit %d, stdout %q, stderr %q", code, stdout, stderr)
	}
}

func TestRootExplain(t *testing.T) {
	_, code, stdout, stderr := fixtureRoot(t, "--explain", "api-m1", "-q")
	if code != ExitOK || stderr != "" {
		t.Fatalf("exit %d, stderr %q", code, stderr)
	}
	for _, want := range []string{
		"api-m1 · Public API v1 (milestone in api)\nplan for 2026-",
		"Work: 4 of 6 beads left", "1 bead of them wait", "The median run takes",
		"Critical chain (it sets the 80% date", "Work left:\n  api-3  ",
		"api-5  HUMAN: ", "waits on a person",
	} {
		if !strings.Contains(stdout, want) {
			t.Errorf("explain lacks %q:\n%s", want, stdout)
		}
	}

	_, code, stdout, _ = fixtureRoot(t, "--explain", "api-3", "-q")
	if code != ExitOK || !strings.Contains(stdout, "api-3 · ") || !strings.Contains(stdout, "Counts towards: api-m1") ||
		!regexp.MustCompile(`about [0-9.]+ (min|h|d)`).MatchString(stdout) {
		t.Errorf("explain of a bead: exit %d\n%s", code, stdout)
	}

	dir, code, _, stderr := fixtureRoot(t, "--explain", "nope-1")
	if code != ExitUsage || !strings.Contains(stderr, "nope-1 is not a bead or goal") {
		t.Errorf("explain of an unknown ID: exit %d, stderr %q", code, stderr)
	}
	if entries, _ := os.ReadDir(dir); len(entries) != 0 {
		t.Errorf("a usage error wrote %v", entries)
	}
}

func TestRootOutputs(t *testing.T) {
	for out, want := range map[string][2]string{
		"site":              {"site/roadmap.html", "site/roadmap.json"},
		"site/index.html":   {"site/index.html", "site/index.json"},
		"site/data.json":    {"site/data.html", "site/data.json"},
		"site/deep/Map.HTM": {"site/deep/Map.HTM", "site/deep/Map.json"},
	} {
		page, js := outputs(out)
		if page != filepath.FromSlash(want[0]) || js != filepath.FromSlash(want[1]) {
			t.Errorf("outputs(%q) = %q, %q; want %q", out, page, js, want)
		}
	}

	dir := t.TempDir()
	out := filepath.Join(dir, "new", "index.html")
	code, _, stderr := run(fixtures+"api.jsonl", "-o", out, "--runs", "50", "-q", "--no-record")
	if code != ExitOK || stderr != "" {
		t.Fatalf("exit %d: %s", code, stderr)
	}
	for _, name := range []string{"index.html", "index.json"} {
		if _, err := os.Stat(filepath.Join(dir, "new", name)); err != nil {
			t.Error(err)
		}
	}
}

// fakeRepos makes directories with .beads, each exported by a fake bd as
// the fixture of the same name; a directory whose name starts with "broken"
// fails to export.
func fakeRepos(t *testing.T, names ...string) (dir string, calls *[]string) {
	t.Helper()
	dir = t.TempDir()
	for _, n := range names {
		if err := os.MkdirAll(filepath.Join(dir, n, ".beads"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	calls = &[]string{}
	src, err := filepath.Abs(fixtures)
	if err != nil {
		t.Fatal(err)
	}
	saved := newExporter
	t.Cleanup(func() { newExporter = saved })
	newExporter = func(bd string) load.Exporter {
		return func(_ context.Context, repo string) ([]byte, error) {
			*calls = append(*calls, bd+" "+filepath.Base(repo))
			if strings.HasPrefix(filepath.Base(repo), "broken") {
				return nil, errors.New("bd export: exit status 1: database is locked")
			}
			return os.ReadFile(filepath.Join(src, filepath.Base(repo)+".jsonl"))
		}
	}
	return dir, calls
}

func chdir(t *testing.T, dir string) {
	t.Helper()
	wd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(dir); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chdir(wd) })
}

func TestRootReadsRepositoriesLive(t *testing.T) {
	dir, calls := fakeRepos(t, "api", "web", "hq")
	out := t.TempDir()
	code, _, stderr := run(filepath.Join(dir, "api"), filepath.Join(dir, "web", ".beads"), filepath.Join(dir, "hq"),
		"-o", out, "--runs", "50", "-q", "--bd", "/opt/bd", "--no-record")
	if code != ExitOK || stderr != "" {
		t.Fatalf("exit %d: %s", code, stderr)
	}
	if len(*calls) != 3 || !contains(*calls, "/opt/bd web") {
		t.Errorf("bd calls = %v", *calls)
	}
	r := readRoadmap(t, filepath.Join(out, "roadmap.json"))
	var names []string
	for _, rp := range r.Repos {
		names = append(names, rp.Name)
	}
	if !reflect.DeepEqual(names, []string{"api", "web", "hq"}) || !r.Inputs.Exports[1].Live {
		t.Errorf("repos = %v, inputs = %+v", names, r.Inputs.Exports)
	}
}

func TestRootPartialFailure(t *testing.T) {
	dir, _ := fakeRepos(t, "api", "broken")
	out := t.TempDir()
	code, stdout, stderr := run(filepath.Join(dir, "api"), filepath.Join(dir, "broken"), "-o", out, "--runs", "50")
	if code != ExitFailure {
		t.Errorf("exit %d, want %d", code, ExitFailure)
	}
	if !strings.Contains(stderr, "error: repo broken: bd export: exit status 1: database is locked\n") || !strings.Contains(stdout, "Roadmap as of") {
		t.Errorf("stdout %q, stderr %q", stdout, stderr)
	}
	r := readRoadmap(t, filepath.Join(out, "roadmap.json"))
	if r.Repos[1].Error == "" || r.Repos[0].Error != "" || len(r.Inputs.Exports) != 1 {
		t.Errorf("repos = %+v, inputs = %+v", r.Repos, r.Inputs)
	}
	page, _ := os.ReadFile(filepath.Join(out, "roadmap.html"))
	if !strings.Contains(string(page), "Some repos could not be read") {
		t.Error("the page has no banner for the repo that failed")
	}

	// A malformed export fails its repo the same way.
	bad := filepath.Join(t.TempDir(), "bad.jsonl")
	if err := os.WriteFile(bad, []byte("{not json\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	code, _, stderr = run(fixtures+"api.jsonl", bad, "-o", out, "--runs", "50", "-q")
	if code != ExitFailure || !strings.Contains(stderr, "error: repo bad: "+bad+":1: ") {
		t.Errorf("malformed export: exit %d, stderr %q", code, stderr)
	}

	// Nothing readable: nothing written.
	empty := t.TempDir()
	code, _, stderr = run(filepath.Join(dir, "broken"), "-o", empty)
	if entries, _ := os.ReadDir(empty); code != ExitFailure || len(entries) != 0 || !strings.Contains(stderr, "nothing written") {
		t.Errorf("all repos failed: exit %d, wrote %v, stderr %q", code, entries, stderr)
	}
}

func TestRootDefaults(t *testing.T) {
	orig := wdOf(t)
	// No PATH and no config: the current directory, when it has .beads.
	dir, calls := fakeRepos(t, "api")
	chdir(t, filepath.Join(dir, "api"))
	code, _, stderr := run("--runs", "50", "-q")
	if code != ExitOK || stderr != "" || len(*calls) != 1 {
		t.Fatalf("in a repo: exit %d, stderr %q, calls %v", code, stderr, *calls)
	}
	if r := readRoadmap(t, "roadmap.json"); r.Repos[0].Name != "api" {
		t.Errorf("repo = %+v", r.Repos[0])
	}

	// No PATH, ./beadline.toml lists the repos and sets expert keys.
	cfgDir := t.TempDir()
	for _, f := range []string{"api.jsonl", "web.jsonl", "hq.jsonl"} {
		data, err := os.ReadFile(filepath.Join(orig, fixtures, f))
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(cfgDir, f), data, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	toml := "repos = [\"api.jsonl\", \"web.jsonl\", \"hq.jsonl\"]\n[expert]\nruns = 40\nhide = [\"web-e1\"]\n"
	if err := os.WriteFile(filepath.Join(cfgDir, "beadline.toml"), []byte(toml), 0o644); err != nil {
		t.Fatal(err)
	}
	chdir(t, cfgDir)
	if code, _, stderr := run("-q"); code != ExitOK || stderr != "" {
		t.Fatalf("with beadline.toml: exit %d, stderr %q", code, stderr)
	}
	r := readRoadmap(t, "roadmap.json")
	if len(r.Repos) != 3 || r.Config.Model.Simulations != 40 {
		t.Errorf("repos %d, runs %d", len(r.Repos), r.Config.Model.Simulations)
	}
	for _, m := range r.Milestones {
		if m.ID == "web-e1" {
			t.Error("hidden web-e1 is on the roadmap")
		}
	}

	// A typo in the config is an error that names the right key.
	if err := os.WriteFile("beadline.toml", []byte(toml+"agent = { api = 2 }\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if code, _, stderr := run("-q"); code != ExitFailure || !strings.Contains(stderr, "unknown key expert.agent; did you mean expert.agents?") {
		t.Errorf("typo: exit %d, stderr %q", code, stderr)
	}

	// Neither: a usage error that says what to do.
	chdir(t, t.TempDir())
	if code, _, stderr := run(); code != ExitUsage || !strings.Contains(stderr, "no repos: run beadline in a repository with .beads") {
		t.Errorf("nowhere: exit %d, stderr %q", code, stderr)
	}
}

func TestRootAgentWarnings(t *testing.T) {
	_, code, _, stderr := fixtureRoot(t, "--agents", "nope=2", "-q")
	if code != ExitOK || !strings.Contains(stderr, "warning: agents set for nope, which is not loaded (repos: api, web, hq); ignored") {
		t.Errorf("exit %d, stderr %q", code, stderr)
	}
}

func wdOf(t *testing.T) string {
	t.Helper()
	wd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	return wd
}

func TestRootRecordsSnapshots(t *testing.T) {
	src, err := filepath.Abs(fixtures)
	if err != nil {
		t.Fatal(err)
	}
	paths := []string{filepath.Join(src, "api.jsonl"), filepath.Join(src, "web.jsonl"), filepath.Join(src, "hq.jsonl")}
	chdir(t, t.TempDir())
	snaps := filepath.Join(".beadline", "snapshots")
	count := func() int {
		entries, _ := os.ReadDir(snaps)
		return len(entries)
	}

	for _, args := range [][]string{{"--no-record"}, {"--as-of", "2026-09-03"}} {
		if code, _, stderr := run(append(append([]string{}, paths...), append(args, "--runs", "50", "-q")...)...); code != ExitOK || stderr != "" || count() != 0 {
			t.Errorf("%v: exit %d, stderr %q, %d snapshots", args, code, stderr, count())
		}
	}
	code, _, stderr := run(append(paths, "--runs", "50")...)
	if code != ExitOK || !strings.Contains(stderr, "recorded snapshot "+snaps+string(filepath.Separator)) || count() != 1 {
		t.Fatalf("first run: exit %d, stderr %q, %d snapshots", code, stderr, count())
	}
	if code, _, stderr = run(append(paths, "--runs", "50")...); code != ExitOK || !strings.Contains(stderr, "kept today's snapshot ") || count() != 1 {
		t.Errorf("second run: exit %d, stderr %q, %d snapshots", code, stderr, count())
	}

	code, stdout, stderr := run(append([]string{"check"}, paths...)...)
	if code != ExitOK || stderr != "" || !strings.Contains(stdout, "1 snapshot graded against data as of 2026-09-03") {
		t.Errorf("check: exit %d, stderr %q\n%s", code, stderr, stdout)
	}
}
