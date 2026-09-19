package config

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

const minimal = `
[[repos]]
name = "api"
export = "api.jsonl"
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
	if len(c.Repos) != 1 || !c.Repos[0].Concurrency.Measure() {
		t.Errorf("repos = %+v, want one repo with measured concurrency", c.Repos)
	}
}

func TestParseFull(t *testing.T) {
	c, err := Parse(`
[[repos]]
name = "api"
export = "api/.beads/issues.jsonl"
concurrency = 4

[[repos]]
name = "web"
export = "/abs/web.jsonl"
concurrency = "measure"

[conventions]
high_level_types = ["milestone"]
goal_label_pattern = "^objective/(.+)$"
human_gate_title_patterns = ["^HUMAN:", "^ALEX:"]
human_gate_metadata_keys = ["awaiting_signoff"]
infra_types = ["molecule"]

[model]
window_days = 30
simulations = 500
seed = 7
human_gate_hours_prior = 12
cycle_minutes_prior = 90
pooling_strength = 5
tail_cap_factor = 4
`)
	if err != nil {
		t.Fatal(err)
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
}

func TestParseErrors(t *testing.T) {
	tests := []struct {
		name, doc, want string
	}{
		{"no repos", `[model]` + "\nseed = 2", "at least one repository"},
		{"missing name", `[[repos]]` + "\nexport = \"a.jsonl\"", "name is required"},
		{"missing export", `[[repos]]` + "\nname = \"a\"", "export is required"},
		{"duplicate name", minimal + minimal, `duplicate name "api"`},
		{"zero concurrency", minimal + "concurrency = 0", "at least 1"},
		{"bad concurrency word", minimal + `concurrency = "lots"`, `got "lots"`},
		{"unknown key", minimal + "\n[conventions]\nhigh_level_type = [\"epic\"]", "unknown key(s): conventions.high_level_type"},
		{"bad goal regexp", minimal + "\n[conventions]\ngoal_label_pattern = \"(\"", "goal_label_pattern"},
		{"goal pattern without submatch", minimal + "\n[conventions]\ngoal_label_pattern = \"^goal:\"", "needs a submatch"},
		{"bad gate regexp", minimal + "\n[conventions]\nhuman_gate_title_patterns = [\"[\"]", "human_gate_title_patterns"},
		{"no high-level types", minimal + "\n[conventions]\nhigh_level_types = []", "at least one type"},
		{"high-level and infra", minimal + "\n[conventions]\nhigh_level_types = [\"epic\"]\ninfra_types = [\"epic\"]", "both high-level and infra"},
		{"bad model", minimal + "\n[model]\nsimulations = 0\ntail_cap_factor = 0.5\npooling_strength = -1", "model.simulations"},
		{"not toml", "[[repos]\n", "toml"},
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
	_, err := Parse(minimal + "\n[model]\nsimulations = 0\ntail_cap_factor = 0.5\npooling_strength = -1")
	for _, key := range []string{"simulations", "tail_cap_factor", "pooling_strength"} {
		if err == nil || !strings.Contains(err.Error(), "model."+key) {
			t.Errorf("err = %v, want it to mention model.%s", err, key)
		}
	}
}

func TestLoadResolvesExportsAgainstConfigDir(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "beadline.toml")
	doc := minimal + `
[[repos]]
name = "abs"
export = "/data/abs.jsonl"
`
	if err := os.WriteFile(path, []byte(doc), 0o644); err != nil {
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

	if _, err := Load(filepath.Join(dir, "missing.toml")); err == nil {
		t.Error("Load of a missing file succeeded")
	}
	if err := os.WriteFile(path, []byte("[[repos]]\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(path); err == nil || !strings.HasPrefix(err.Error(), path+": ") {
		t.Errorf("err = %v, want it prefixed with the file path", err)
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
	c, err := Parse(minimal + "\n[conventions]\ngoal_label_pattern = \"\"")
	if err != nil {
		t.Fatal(err)
	}
	if got := c.Conventions.Goals([]string{"goal:x"}); got != nil {
		t.Errorf("Goals with no pattern = %v", got)
	}
}
