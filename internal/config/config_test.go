package config

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

func ts(s string) time.Time {
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		panic(err)
	}
	return t.UTC()
}

const minimal = `repos = ["api.jsonl"]
`

func TestParseDefaults(t *testing.T) {
	c, err := Parse(minimal)
	if err != nil {
		t.Fatal(err)
	}
	want := Default()
	if !reflect.DeepEqual(c.Model, want.Model) {
		t.Errorf("model = %+v, want defaults %+v", c.Model, want.Model)
	}
	if !reflect.DeepEqual(c.Conventions.HighLevelTypes, []string{"milestone", "epic"}) {
		t.Errorf("high_level_types = %v", c.Conventions.HighLevelTypes)
	}
	if len(c.Repos) != 1 || c.Repos[0].Name != "api" || !c.Repos[0].Concurrency.Measure() {
		t.Errorf("repos = %+v, want one repo api with measured concurrency", c.Repos)
	}
	if got := c.NonDefault(); got != nil {
		t.Errorf("NonDefault = %v, want none", got)
	}
}

func TestParseEmpty(t *testing.T) {
	// No repos is fine: the command line or the working directory names them.
	c, err := Parse("")
	if err != nil {
		t.Fatal(err)
	}
	if len(c.Repos) != 0 || !reflect.DeepEqual(c.Model, Default().Model) {
		t.Errorf("config = %+v", c)
	}
}

const full = `
repos = ["api/.beads/issues.jsonl", "/abs/web.jsonl", "../hivemind"]

[expert]
agents = { api = 4, web = "measure" }
roadmap_types = ["milestone"]
goal_label = "^objective/(.+)$"
human_gate = { titles = ["^HUMAN:", "^ALEX:"], metadata = ["awaiting_signoff"], hours_prior = 12 }
hide = ["api-m9"]
history_days = 30
ignore_types = ["molecule"]
runs = 500
seed = 7
cycle_minutes_prior = 90
pooling_strength = 5
tail_cap_factor = 4
`

func TestParseFull(t *testing.T) {
	c, err := Parse(full)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, r := range c.Repos {
		names = append(names, r.Name)
	}
	if !reflect.DeepEqual(names, []string{"api", "web", "hivemind"}) {
		t.Errorf("repo names = %v", names)
	}
	if got := c.Repos[0].Concurrency; got.Max != 4 || got.String() != "4" {
		t.Errorf("api concurrency = %v", got)
	}
	if got := c.Repos[1].Concurrency; !got.Measure() || got.String() != "measure" {
		t.Errorf("web concurrency = %v", got)
	}
	// Arrays in the file replace the defaults, they do not merge with them.
	if !reflect.DeepEqual(c.Conventions.HighLevelTypes, []string{"milestone"}) {
		t.Errorf("high_level_types = %v", c.Conventions.HighLevelTypes)
	}
	if !reflect.DeepEqual(c.Conventions.InfraTypes, []string{"molecule"}) {
		t.Errorf("infra_types = %v", c.Conventions.InfraTypes)
	}
	if !reflect.DeepEqual(c.Hide, []string{"api-m9"}) {
		t.Errorf("hide = %v", c.Hide)
	}
	wantModel := Model{WindowDays: 30, Simulations: 500, Seed: 7, HumanGateHoursPrior: 12,
		CycleMinutesPrior: 90, PoolingStrength: 5, TailCapFactor: 4}
	if c.Model != wantModel {
		t.Errorf("model = %+v, want %+v", c.Model, wantModel)
	}
	// An unnamed submatch is the goal ID.
	if got := c.Conventions.Goals([]string{"objective/launch", "goal:x"}); !reflect.DeepEqual(got, []string{"launch"}) {
		t.Errorf("goals = %v", got)
	}
	if !c.Conventions.IsHumanGate("ALEX: approve pricing", nil) {
		t.Error("custom title pattern not applied")
	}
	want := []string{
		"agents.api = 4",
		`roadmap_types = ["milestone"]`,
		`goal_label = "^objective/(.+)$"`,
		`human_gate.titles = ["^HUMAN:", "^ALEX:"]`,
		`human_gate.metadata = ["awaiting_signoff"]`,
		"human_gate.hours_prior = 12",
		`hide = ["api-m9"]`,
		"history_days = 30",
		`ignore_types = ["molecule"]`,
		"runs = 500",
		"seed = 7",
		"cycle_minutes_prior = 90",
		"pooling_strength = 5",
		"tail_cap_factor = 4",
	}
	if got := c.NonDefault(); !reflect.DeepEqual(got, want) {
		t.Errorf("NonDefault =\n%s\nwant\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
}

const withIdle = minimal + `
[expert]
idle_gap_hours = 36
idle_resume_at = 2026-09-22T00:00:00Z
idle = [
  { start = 2026-09-05T00:00:00Z, end = 2026-09-09T00:00:00Z, note = "host migration" },
  { start = 2026-09-15T00:00:00Z },
]
`

func TestParseIdle(t *testing.T) {
	c, err := Parse(withIdle)
	if err != nil {
		t.Fatal(err)
	}
	if c.Idle.GapHours != 36 {
		t.Errorf("idle_gap_hours = %v, want 36", c.Idle.GapHours)
	}
	if want := ts("2026-09-22T00:00:00Z"); !c.Idle.ResumeAt.Equal(want) {
		t.Errorf("idle_resume_at = %v, want %v", c.Idle.ResumeAt, want)
	}
	want := []DeclaredWindow{
		{Start: ts("2026-09-05T00:00:00Z"), End: ts("2026-09-09T00:00:00Z"), Note: "host migration"},
		{Start: ts("2026-09-15T00:00:00Z")},
	}
	if !reflect.DeepEqual(c.Idle.Declared, want) {
		t.Errorf("idle = %+v, want %+v", c.Idle.Declared, want)
	}
	wantSettings := []string{"idle_gap_hours = 36", `idle_resume_at = "2026-09-22T00:00:00Z"`, "idle = 2 windows declared"}
	if got := c.NonDefault(); !reflect.DeepEqual(got, wantSettings) {
		t.Errorf("NonDefault =\n%s\nwant\n%s", strings.Join(got, "\n"), strings.Join(wantSettings, "\n"))
	}
}

func TestDefaultIdleGapHours(t *testing.T) {
	c, err := Parse(minimal)
	if err != nil {
		t.Fatal(err)
	}
	// Inference is off by default: the release-gate backtest found no gap
	// threshold that both held calibration and helped the target case
	// (ADR-3, "open question"). Declared windows still work unmasked.
	if c.Idle.GapHours != 0 {
		t.Errorf("default idle_gap_hours = %v, want 0 (ADR-3)", c.Idle.GapHours)
	}
	if got := c.NonDefault(); got != nil {
		t.Errorf("NonDefault = %v, want none for the default idle settings", got)
	}
}

func TestPartialTablesKeepDefaults(t *testing.T) {
	c, err := Parse(minimal + "[expert]\nhuman_gate = { metadata = [\"hold_reason\"] }\n")
	if err != nil {
		t.Fatal(err)
	}
	cv := c.Conventions
	if !reflect.DeepEqual(cv.HumanGateTitlePatterns, []string{"^HUMAN:"}) || !reflect.DeepEqual(cv.HumanGateMetadataKeys, []string{"hold_reason"}) ||
		c.Model.HumanGateHoursPrior != 24 {
		t.Errorf("conventions = %+v, model = %+v", cv, c.Model)
	}
}

func TestParseErrors(t *testing.T) {
	tests := []struct {
		name, doc, want string
	}{
		{"empty repo", `repos = ["a.jsonl", ""]`, "repos[1] is empty"},
		{"old repos layout", "[[repos]]\nname = \"a\"\nexport = \"a.jsonl\"", "repos is a list of paths"},
		{"zero agents", minimal + "[expert]\nagents = { api = 0 }", "at least 1"},
		{"bad agents word", minimal + "[expert]\nagents = { api = \"lots\" }", `got "lots"`},
		{"typo", minimal + "[expert]\nagent = { api = 2 }", "unknown key expert.agent; did you mean expert.agents?"},
		{"nested typo", minimal + "[expert.human_gate]\ntitle = [\"^X\"]", "unknown key expert.human_gate.title; did you mean expert.human_gate.titles?"},
		{"key outside expert", minimal + "runs = 5", "unknown key runs; did you mean expert.runs?"},
		{"old key", minimal + "[conventions]\nhigh_level_types = [\"epic\"]", "unknown key conventions.high_level_types; it is expert.roadmap_types now"},
		{"nothing close", minimal + "[expert]\nfrobnicate = 1", "unknown key expert.frobnicate; valid keys: repos, expert.agents,"},
		{"bad goal regexp", minimal + "[expert]\ngoal_label = \"(\"", "expert.goal_label"},
		{"goal pattern without submatch", minimal + "[expert]\ngoal_label = \"^goal:\"", "needs a submatch"},
		{"bad gate regexp", minimal + "[expert]\nhuman_gate = { titles = [\"[\"] }", "expert.human_gate.titles"},
		{"no roadmap types", minimal + "[expert]\nroadmap_types = []", "at least one type"},
		{"roadmap and ignored", minimal + "[expert]\nroadmap_types = [\"epic\"]\nignore_types = [\"epic\"]", "both roadmap_types and ignore_types"},
		{"bad model", minimal + "[expert]\nruns = 0\ntail_cap_factor = 0.5\npooling_strength = -1", "expert.runs"},
		{"negative idle gap", minimal + "[expert]\nidle_gap_hours = -1", "expert.idle_gap_hours must not be negative"},
		{"idle window without start", minimal + "[expert]\nidle = [{ end = 2026-09-09T00:00:00Z }]", "expert.idle[0]: start is required"},
		{"idle window end before start", minimal + "[expert]\nidle = [{ start = 2026-09-09T00:00:00Z, end = 2026-09-05T00:00:00Z }]", "end (2026-09-05"},
		{"not toml", "repos = [\n", "toml"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := Parse(tt.doc)
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Errorf("err = %v, want it to contain %q", err, tt.want)
			}
		})
	}
}

func TestModelErrorsAreAllReported(t *testing.T) {
	_, err := Parse(minimal + "[expert]\nruns = 0\ntail_cap_factor = 0.5\npooling_strength = -1")
	for _, key := range []string{"runs", "tail_cap_factor", "pooling_strength"} {
		if err == nil || !strings.Contains(err.Error(), "expert."+key) {
			t.Errorf("err = %v, want it to mention expert.%s", err, key)
		}
	}
}

func TestLoadResolvesReposAgainstConfigDir(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "beadline.toml")
	if err := os.WriteFile(path, []byte(`repos = ["api.jsonl", "/data/abs.jsonl", "."]`), 0o644); err != nil {
		t.Fatal(err)
	}
	c, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := c.Repos[0].ExportPath(c), filepath.Join(dir, "api.jsonl"); got != want {
		t.Errorf("relative export = %q, want %q", got, want)
	}
	if got := c.Repos[1].ExportPath(c); got != "/data/abs.jsonl" {
		t.Errorf("absolute export = %q", got)
	}
	if got := c.Repos[2].Name; got != filepath.Base(dir) {
		t.Errorf(". is named %q, want the config directory's name %q", got, filepath.Base(dir))
	}

	if _, err := Load(filepath.Join(dir, "missing.toml")); err == nil {
		t.Error("Load of a missing file succeeded")
	}
	if err := os.WriteFile(path, []byte("repos = 1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(path); err == nil || !strings.HasPrefix(err.Error(), path+": ") {
		t.Errorf("err = %v, want it prefixed with the file path", err)
	}
}

func TestRepoNames(t *testing.T) {
	tests := map[string]string{
		"/data/projects/hivemind":                     "hivemind",
		"/data/projects/hivemind/":                    "hivemind",
		"/data/projects/hivemind/.beads":              "hivemind",
		"/data/projects/hivemind/.beads/issues.jsonl": "hivemind",
		"exports/api.jsonl":                           "api",
		"exports/api.JSON":                            "api",
	}
	for path, want := range tests {
		if got := RepoName(path); got != want {
			t.Errorf("RepoName(%q) = %q, want %q", path, got, want)
		}
	}

	c := Default()
	c.Agents = map[string]Concurrency{"api": {Max: 2}, "api-2": {Max: 5}, "gone": {Max: 1}}
	c.SetRepos([]string{"a/api", "b/api.jsonl", "c/api/.beads"})
	var got []string
	for _, r := range c.Repos {
		got = append(got, r.Name+"="+r.Concurrency.String())
	}
	if want := []string{"api=2", "api-2=5", "api-3=measure"}; !reflect.DeepEqual(got, want) {
		t.Errorf("repos = %v, want %v", got, want)
	}
	if unknown := c.ApplyAgents(); !reflect.DeepEqual(unknown, []string{"gone"}) {
		t.Errorf("unknown agents = %v", unknown)
	}
}

func TestParseConcurrency(t *testing.T) {
	if c, err := ParseConcurrency("3"); err != nil || c.Max != 3 {
		t.Errorf("3: %v, %v", c, err)
	}
	if c, err := ParseConcurrency("measure"); err != nil || !c.Measure() {
		t.Errorf("measure: %v, %v", c, err)
	}
	for _, bad := range []string{"0", "-1", "many", ""} {
		if _, err := ParseConcurrency(bad); err == nil {
			t.Errorf("ParseConcurrency(%q) succeeded", bad)
		}
	}
}

func TestConventions(t *testing.T) {
	cv := Default().Conventions
	if !cv.IsHighLevel("epic") || cv.IsHighLevel("task") {
		t.Error("IsHighLevel")
	}
	if !cv.IsInfra("molecule") || cv.IsInfra("bug") {
		t.Error("IsInfra")
	}
	goals := cv.Goals([]string{"goal:b", "beadline", "goal:a", "goal:b", "goal:", "xgoal:c"})
	if !reflect.DeepEqual(goals, []string{"a", "b"}) {
		t.Errorf("Goals = %v, want [a b]", goals)
	}
	if got := cv.Goals(nil); got != nil {
		t.Errorf("Goals(nil) = %v", got)
	}

	meta := func(s string) map[string]json.RawMessage {
		var m map[string]json.RawMessage
		if err := json.Unmarshal([]byte(s), &m); err != nil {
			t.Fatal(err)
		}
		return m
	}
	gates := []struct {
		title, metadata string
		want            bool
	}{
		{"HUMAN: sign off the design", `{}`, true},
		{"Implement the loader", `{}`, false},
		{"Say HUMAN: in the middle", `{}`, false},
		{"Ship", `{"hold_reason": "awaiting-mayor-signoff"}`, true},
		{"Ship", `{"awaiting_signoff": true}`, true},
		{"Ship", `{"plan_first": 1}`, true},
		{"Ship", `{"hold_reason": ""}`, false},
		{"Ship", `{"awaiting_signoff": false}`, false},
		{"Ship", `{"plan_first": 0}`, false},
		{"Ship", `{"hold_reason": null}`, false},
		{"Ship", `{"gate_status": "GREEN"}`, false},
	}
	for _, g := range gates {
		if got := cv.IsHumanGate(g.title, meta(g.metadata)); got != g.want {
			t.Errorf("IsHumanGate(%q, %s) = %v, want %v", g.title, g.metadata, got, g.want)
		}
	}
}

func TestNoGoalPattern(t *testing.T) {
	c, err := Parse(minimal + "[expert]\ngoal_label = \"\"")
	if err != nil {
		t.Fatal(err)
	}
	if got := c.Conventions.Goals([]string{"goal:x"}); got != nil {
		t.Errorf("Goals with no pattern = %v", got)
	}
}
