package config

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"github.com/BurntSushi/toml"
)

// Config is a parsed and validated beadline.toml.
type Config struct {
	Repos       []Repo      `toml:"repos"`
	Conventions Conventions `toml:"conventions"`
	Model       Model       `toml:"model"`

	// Dir is the directory relative export paths resolve against: the
	// directory of the config file, or the working directory for Parse.
	Dir string `toml:"-"`
}

// Repo is one [[repos]] entry: a beads database exported with `bd export`.
type Repo struct {
	Name        string      `toml:"name"`
	Export      string      `toml:"export"`
	Concurrency Concurrency `toml:"concurrency"`
}

// ExportPath is the export file path with relative paths resolved against
// the config directory.
func (r Repo) ExportPath(c *Config) string {
	if filepath.IsAbs(r.Export) || c.Dir == "" {
		return r.Export
	}
	return filepath.Join(c.Dir, r.Export)
}

// Concurrency is the maximum number of agents working a repository at once,
// or Measure to learn it from history. The zero value means Measure.
type Concurrency struct {
	Max int // > 0 when fixed; 0 means measure
}

// Measure reports whether concurrency is learned from history.
func (c Concurrency) Measure() bool { return c.Max == 0 }

func (c Concurrency) String() string {
	if c.Measure() {
		return "measure"
	}
	return fmt.Sprint(c.Max)
}

// UnmarshalTOML accepts a positive integer or the string "measure".
func (c *Concurrency) UnmarshalTOML(v any) error {
	switch v := v.(type) {
	case int64:
		if v < 1 {
			return fmt.Errorf("concurrency must be at least 1 or \"measure\", got %d", v)
		}
		c.Max = int(v)
		return nil
	case string:
		if v != "measure" {
			return fmt.Errorf("concurrency must be a positive integer or \"measure\", got %q", v)
		}
		c.Max = 0
		return nil
	default:
		return fmt.Errorf("concurrency must be a positive integer or \"measure\", got %v", v)
	}
}

// Conventions say how a team uses beads. They are configuration, not
// assumptions: every pattern here can be replaced.
type Conventions struct {
	// HighLevelTypes are the issue types shown on the roadmap.
	HighLevelTypes []string `toml:"high_level_types"`
	// GoalLabelPattern matches a label that links a bead to a cross-repo
	// goal. The goal ID is the submatch named "id", else the first submatch.
	GoalLabelPattern string `toml:"goal_label_pattern"`
	// HumanGateTitlePatterns mark a bead as waiting on a human by title.
	HumanGateTitlePatterns []string `toml:"human_gate_title_patterns"`
	// HumanGateMetadataKeys mark a bead as waiting on a human when the key is
	// set in its metadata to anything but "", false, 0 or null.
	HumanGateMetadataKeys []string `toml:"human_gate_metadata_keys"`
	// InfraTypes are orchestration issue types that are not work and are
	// dropped on load, together with every edge to them.
	InfraTypes []string `toml:"infra_types"`

	goalRe  *regexp.Regexp
	goalIdx int
	gateRes []*regexp.Regexp
}

// Model holds the simulation settings (docs/design.md, ADR-1 §2).
type Model struct {
	WindowDays          int     `toml:"window_days"`
	Simulations         int     `toml:"simulations"`
	Seed                uint64  `toml:"seed"`
	HumanGateHoursPrior float64 `toml:"human_gate_hours_prior"`
	CycleMinutesPrior   float64 `toml:"cycle_minutes_prior"`
	PoolingStrength     float64 `toml:"pooling_strength"`
	TailCapFactor       float64 `toml:"tail_cap_factor"`
}

// Default returns the configuration used for every key a file leaves out.
// It has no repositories.
func Default() Config {
	c := Config{
		Conventions: Conventions{
			HighLevelTypes:         []string{"milestone", "epic"},
			GoalLabelPattern:       `^goal:(?P<id>.+)$`,
			HumanGateTitlePatterns: []string{`^HUMAN:`},
			HumanGateMetadataKeys:  []string{"awaiting_signoff", "plan_first", "hold_reason"},
			InfraTypes:             []string{"molecule", "wisp", "step", "convoy", "message", "gate", "event"},
		},
		Model: Model{
			WindowDays:          45,
			Simulations:         2000,
			Seed:                1,
			HumanGateHoursPrior: 24,
			CycleMinutesPrior:   60,
			PoolingStrength:     10,
			TailCapFactor:       3,
		},
	}
	if err := c.Conventions.Compile(); err != nil {
		panic("config: invalid default conventions: " + err.Error())
	}
	return c
}

// Load reads and validates the config file at path. Relative export paths
// resolve against the file's directory.
func Load(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	c, err := Parse(string(data))
	if err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	c.Dir = filepath.Dir(path)
	return c, nil
}

// Parse decodes and validates a beadline.toml document. Keys it leaves out
// keep their Default values; unknown keys are an error, so typos surface.
func Parse(doc string) (*Config, error) {
	c := Default()
	md, err := toml.Decode(doc, &c)
	if err != nil {
		return nil, err
	}
	if undecoded := md.Undecoded(); len(undecoded) > 0 {
		keys := make([]string, len(undecoded))
		for i, k := range undecoded {
			keys[i] = k.String()
		}
		return nil, fmt.Errorf("unknown key(s): %s", strings.Join(keys, ", "))
	}
	if err := c.validate(); err != nil {
		return nil, err
	}
	return &c, nil
}

func (c *Config) validate() error {
	var errs []error
	if len(c.Repos) == 0 {
		errs = append(errs, errors.New("no [[repos]]: at least one repository is required"))
	}
	seen := map[string]bool{}
	for i, r := range c.Repos {
		switch {
		case r.Name == "":
			errs = append(errs, fmt.Errorf("repos[%d]: name is required", i))
		case seen[r.Name]:
			errs = append(errs, fmt.Errorf("repos[%d]: duplicate name %q", i, r.Name))
		}
		seen[r.Name] = true
		if r.Export == "" {
			errs = append(errs, fmt.Errorf("repos[%d] (%s): export is required", i, r.Name))
		}
	}
	if err := c.Conventions.Compile(); err != nil {
		errs = append(errs, err)
	}
	errs = append(errs, c.Model.validate()...)
	return errors.Join(errs...)
}

func (m Model) validate() []error {
	var errs []error
	positive := func(key string, v float64) {
		if !(v > 0) {
			errs = append(errs, fmt.Errorf("model.%s must be positive, got %v", key, v))
		}
	}
	positive("window_days", float64(m.WindowDays))
	positive("simulations", float64(m.Simulations))
	positive("human_gate_hours_prior", m.HumanGateHoursPrior)
	positive("cycle_minutes_prior", m.CycleMinutesPrior)
	if !(m.PoolingStrength >= 0) {
		errs = append(errs, fmt.Errorf("model.pooling_strength must not be negative, got %v", m.PoolingStrength))
	}
	if !(m.TailCapFactor >= 1) {
		errs = append(errs, fmt.Errorf("model.tail_cap_factor must be at least 1, got %v", m.TailCapFactor))
	}
	return errs
}

// Compile validates the conventions and prepares their matchers. Parse, Load
// and Default call it; code that changes Conventions must call it again.
func (cv *Conventions) Compile() error {
	var errs []error
	if len(cv.HighLevelTypes) == 0 {
		errs = append(errs, errors.New("conventions.high_level_types must name at least one type"))
	}
	infra := map[string]bool{}
	for _, t := range cv.InfraTypes {
		infra[t] = true
	}
	for _, t := range cv.HighLevelTypes {
		if infra[t] {
			errs = append(errs, fmt.Errorf("conventions: type %q is both high-level and infra", t))
		}
	}

	cv.goalRe, cv.goalIdx = nil, 0
	if cv.GoalLabelPattern != "" {
		re, err := regexp.Compile(cv.GoalLabelPattern)
		switch {
		case err != nil:
			errs = append(errs, fmt.Errorf("conventions.goal_label_pattern: %w", err))
		case re.NumSubexp() == 0:
			errs = append(errs, errors.New("conventions.goal_label_pattern needs a submatch for the goal ID, e.g. (?P<id>.+)"))
		default:
			cv.goalRe, cv.goalIdx = re, 1
			if i := re.SubexpIndex("id"); i > 0 {
				cv.goalIdx = i
			}
		}
	}

	cv.gateRes = nil
	for _, p := range cv.HumanGateTitlePatterns {
		re, err := regexp.Compile(p)
		if err != nil {
			errs = append(errs, fmt.Errorf("conventions.human_gate_title_patterns: %w", err))
			continue
		}
		cv.gateRes = append(cv.gateRes, re)
	}
	return errors.Join(errs...)
}

// IsHighLevel reports whether issues of this type appear on the roadmap.
func (cv *Conventions) IsHighLevel(issueType string) bool {
	return contains(cv.HighLevelTypes, issueType)
}

// IsInfra reports whether issues of this type are orchestration, not work.
func (cv *Conventions) IsInfra(issueType string) bool {
	return contains(cv.InfraTypes, issueType)
}

// Goals returns the goal IDs named by labels, sorted and without duplicates.
func (cv *Conventions) Goals(labels []string) []string {
	if cv.goalRe == nil {
		return nil
	}
	var goals []string
	for _, l := range labels {
		if m := cv.goalRe.FindStringSubmatch(l); m != nil && m[cv.goalIdx] != "" && !contains(goals, m[cv.goalIdx]) {
			goals = append(goals, m[cv.goalIdx])
		}
	}
	sort.Strings(goals)
	return goals
}

// IsHumanGate reports whether a bead waits on a human: its title matches a
// gate pattern or one of the gate metadata keys is set.
func (cv *Conventions) IsHumanGate(title string, metadata map[string]json.RawMessage) bool {
	for _, re := range cv.gateRes {
		if re.MatchString(title) {
			return true
		}
	}
	for _, k := range cv.HumanGateMetadataKeys {
		if v, ok := metadata[k]; ok && isSet(v) {
			return true
		}
	}
	return false
}

// isSet reports whether a metadata value means "yes": anything but "",
// false, 0 or null. A cleared hold is often left behind as "".
func isSet(v json.RawMessage) bool {
	switch string(bytes.TrimSpace(v)) {
	case "", `""`, "false", "0", "null":
		return false
	}
	return true
}

func contains(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}
